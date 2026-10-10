package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParsePlaylistID(t *testing.T) {
	const id = "37i9dQZF1DXcBWIGoYBM5M"
	valid := []string{
		id,
		"  " + id + "\n",
		"spotify:playlist:" + id,
		"https://open.spotify.com/playlist/" + id,
		"https://open.spotify.com/playlist/" + id + "?si=abc123&pi=xyz",
		"https://open.spotify.com/intl-de/playlist/" + id,
		"open.spotify.com/playlist/" + id,
		"https://open.spotify.com/user/someone/playlist/" + id,
		"https://open.spotify.com/embed/playlist/" + id,
	}
	for _, in := range valid {
		got, err := ParsePlaylistID(in)
		if err != nil || got != id {
			t.Errorf("ParsePlaylistID(%q) = %q, %v; want %q", in, got, err, id)
		}
	}

	invalid := map[string]error{
		"":                                              ErrInvalidPlaylistLink,
		"hello":                                         ErrInvalidPlaylistLink,
		"https://open.spotify.com/album/" + id:          ErrInvalidPlaylistLink,
		"https://open.spotify.com/playlist/abc":         ErrInvalidPlaylistLink,
		"https://music.apple.com/us/playlist/x":         ErrInvalidPlaylistLink,
		"https://evil.com/playlist/" + id:               ErrInvalidPlaylistLink,
		"https://spotify.link/AbCdEf":                   ErrShortLink,
		"spotify:playlist:" + id + "/../../me":          ErrInvalidPlaylistLink,
		"https://open.spotify.com/playlist/" + id + "x": ErrInvalidPlaylistLink,
	}
	for in, want := range invalid {
		if _, err := ParsePlaylistID(in); !errors.Is(err, want) {
			t.Errorf("ParsePlaylistID(%q) error = %v; want %v", in, err, want)
		}
	}
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c := NewClient("id", "secret", "http://127.0.0.1:8080/callback")
	c.AccountsURL = ts.URL
	c.APIURL = ts.URL + "/v1"
	return c
}

func TestGetPlaylistTracks(t *testing.T) {
	var c *Client
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		switch r.URL.Query().Get("offset") {
		case "":
			// First page uses the new "item" field name and covers every skip case.
			fmt.Fprintf(w, `{"next": %q, "items": [
				{"is_local": false, "item": {"type": "track", "id": "t1", "name": "Song One",
					"duration_ms": 200000, "explicit": true,
					"artists": [{"name": "A"}, {"name": "B"}], "album": {"name": "Album"},
					"external_ids": {"isrc": "usabc1234567"}}},
				{"is_local": true, "item": {"type": "track", "id": null, "name": "My Demo", "is_local": true,
					"artists": [], "album": {"name": ""}, "external_ids": {}}},
				{"is_local": false, "item": {"type": "episode", "id": "e1", "name": "Some Podcast"}},
				{"is_local": false, "item": null}
			]}`, c.APIURL+"/playlists/pl/items?offset=4&limit=50")
		case "4":
			// Second page uses the pre-2026 "track" field name.
			fmt.Fprint(w, `{"next": null, "items": [
				{"is_local": false, "track": {"type": "track", "id": "t2", "name": "Song Two",
					"artists": [{"name": "C"}], "album": {"name": "Other"}, "external_ids": {}}}
			]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})

	tracks, skipped, err := c.GetPlaylistTracks(context.Background(), "tok", "pl")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2: %+v", len(tracks), tracks)
	}
	first := tracks[0]
	if first.Position != 1 || first.SpotifyID != "t1" || first.ISRC != "USABC1234567" ||
		strings.Join(first.Artists, ",") != "A,B" || first.Album != "Album" || !first.Explicit || first.DurationMS != 200000 {
		t.Errorf("unexpected first track: %+v", first)
	}
	if tracks[1].Position != 5 || tracks[1].Name != "Song Two" || tracks[1].ISRC != "" {
		t.Errorf("unexpected second track: %+v", tracks[1])
	}

	wantSkipped := []SkippedItem{
		{Position: 2, Name: "My Demo", Reason: "Local file (not in Spotify's catalog)"},
		{Position: 3, Name: "Some Podcast", Reason: "Podcast episode"},
		{Position: 4, Reason: "No longer available on Spotify"},
	}
	if fmt.Sprint(skipped) != fmt.Sprint(wantSkipped) {
		t.Errorf("skipped = %+v\nwant      %+v", skipped, wantSkipped)
	}
}

func TestGetRetriesAfterRateLimit(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"id": "u1", "display_name": "User One"}`)
	})
	me, err := c.Me(context.Background(), "tok")
	if err != nil || me.DisplayName != "User One" || calls.Load() != 2 {
		t.Fatalf("Me() = %+v, %v after %d calls", me, err, calls.Load())
	}
}

func TestGetRefusesToSendTokenOffAPI(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"next": "https://evil.example.com/steal", "items": []}`)
	})
	if _, _, err := c.GetPlaylistTracks(context.Background(), "tok", "pl"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestAPIErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error": "invalid_grant", "error_description": "Refresh token revoked"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error": {"status": 403, "message": "Forbidden"}}`)
	})

	_, err := c.GetPlaylist(context.Background(), "tok", "pl")
	if !IsStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("GetPlaylist error = %v", err)
	}
	_, err = c.Refresh(context.Background(), "old")
	if !IsStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "Refresh token revoked") {
		t.Errorf("Refresh error = %v", err)
	}
}

func TestRefreshKeepsOldRefreshToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "id" || pass != "secret" || r.FormValue("refresh_token") != "old" {
			t.Errorf("bad refresh request: %s:%s %v", user, pass, r.Form)
		}
		fmt.Fprint(w, `{"access_token": "new-access", "expires_in": 3600}`)
	})
	tok, err := c.Refresh(context.Background(), "old")
	if err != nil || tok.AccessToken != "new-access" || tok.RefreshToken != "old" {
		t.Fatalf("Refresh() = %+v, %v", tok, err)
	}
}

func TestGetPlaylistMetadata(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id": "pl", "name": "Road Trip", "collaborative": false,
			"owner": {"id": "u1", "display_name": ""},
			"external_urls": {"spotify": "https://open.spotify.com/playlist/pl"},
			"images": [{"url": "https://img/1"}], "items": {"total": 42}}`)
	})
	p, err := c.GetPlaylist(context.Background(), "tok", "pl")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Road Trip" || p.Owner.ID != "u1" || p.Owner.DisplayName != "u1" || p.Total != 42 || p.ImageURL != "https://img/1" {
		t.Errorf("unexpected playlist: %+v", p)
	}
}
