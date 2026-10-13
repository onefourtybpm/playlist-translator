package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	signer, _ := newTestSigner(t)
	c := NewClient(signer)
	c.APIURL = ts.URL
	return c
}

func TestSongsByISRCBatchesAndMapsResults(t *testing.T) {
	var batches []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("Music-User-Token") != "" {
			t.Errorf("catalog request headers: %v", r.Header)
		}
		isrcs := strings.Split(r.URL.Query().Get("filter[isrc]"), ",")
		batches = append(batches, r.URL.Query().Get("filter[isrc]"))
		// Every ISRC maps to one song with the same ID; the first is on a compilation.
		var data []map[string]any
		filters := map[string]any{}
		for i, isrc := range isrcs {
			data = append(data, map[string]any{
				"id":         isrc,
				"attributes": map[string]any{"name": "Song " + isrc, "isrc": isrc, "contentRating": "explicit"},
				"relationships": map[string]any{"albums": map[string]any{"data": []map[string]any{
					{"attributes": map[string]any{"isCompilation": i == 0}},
				}}},
			})
			filters[isrc] = []map[string]string{{"id": isrc}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"filters": map[string]any{"isrc": filters}}})
	})

	var isrcs []string
	for i := range 30 {
		isrcs = append(isrcs, fmt.Sprintf("ISRC%02d", i))
	}
	found, err := c.SongsByISRC(context.Background(), "us", isrcs)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || strings.Count(batches[0], ",") != isrcBatchSize-1 {
		t.Errorf("batches = %v, want 25 then 5", batches)
	}
	if len(found) != 30 || found["ISRC00"][0].Name != "Song ISRC00" || !found["ISRC00"][0].IsCompilation || !found["ISRC00"][0].Explicit {
		t.Errorf("found = %+v", found["ISRC00"])
	}
}

func TestCreatePlaylistAddsSongsInBatches(t *testing.T) {
	var mu sync.Mutex
	var created map[string]any
	var added [][]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Music-User-Token") != "user-token" {
			t.Errorf("missing music user token on %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/me/library/playlists":
			created = body
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"data":[{"id":"p.1","attributes":{"playParams":{"globalId":"pl.u-abc"}}}]}`)
		case "/v1/me/library/playlists/p.1/tracks":
			added = append(added, body["data"].([]any))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	pl, err := c.CreatePlaylist(context.Background(), "user-token", NewPlaylist{
		Name: "Mix", Description: "desc", Public: true, FolderID: "p.folder", SongIDs: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pl.ShareURL() != "https://music.apple.com/playlist/pl.u-abc" {
		t.Errorf("share URL = %q", pl.ShareURL())
	}
	attrs := created["attributes"].(map[string]any)
	rels := created["relationships"].(map[string]any)
	firstTracks := rels["tracks"].(map[string]any)["data"].([]any)
	parent := rels["parent"].(map[string]any)["data"].([]any)[0].(map[string]any)
	if attrs["isPublic"] != true || attrs["name"] != "Mix" || len(firstTracks) != 100 || parent["id"] != "p.folder" {
		t.Errorf("create body = %v", created)
	}
	if len(added) != 2 || len(added[0]) != 100 || len(added[1]) != 50 {
		t.Errorf("added batches = %d, want 100 then 50", len(added))
	}
	if first := added[0][0].(map[string]any); first["id"] != "100" || first["type"] != "songs" {
		t.Errorf("second batch starts with %v, want song 100", first)
	}
}

func TestErrorsAndRateLimitRetry(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/me/storefront" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errors":[{"code":"40300","title":"Forbidden","detail":"Invalid authentication"}]}`)
			return
		}
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"results":{"songs":{"data":[{"id":"1","attributes":{"name":"Found"}}]}}}`)
	})

	songs, err := c.SearchSongs(context.Background(), "us", "x", 5)
	if err != nil || len(songs) != 1 || songs[0].Name != "Found" || calls != 2 {
		t.Errorf("search after 429: songs=%v err=%v calls=%d", songs, err, calls)
	}

	_, err = c.Storefront(context.Background(), "revoked")
	if !IsUserTokenRejected(err) || !strings.Contains(err.Error(), "Invalid authentication") {
		t.Errorf("storefront error = %v, want rejected user token with Apple's detail", err)
	}
}
