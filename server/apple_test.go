package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

// fakeApple accepts the music user tokens "good" and "friend" (two different libraries) and
// records library changes.
type fakeApple struct {
	mu              sync.Mutex
	folders         []string
	folderOwner     map[string]string // folder ID → user token of the library it's in
	deletedFolders  map[string]bool
	createAttempts  int
	createdParents  []string
	createdNames    []string
	createdSongs    [][]string
	userTokenHeader []string
}

func newFakeApple(t *testing.T) (*fakeApple, *apple.Client) {
	t.Helper()
	f := &fakeApple{deletedFolders: map[string]bool{}, folderOwner: map[string]string{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		userToken := r.Header.Get("Music-User-Token")
		if strings.HasPrefix(r.URL.Path, "/v1/me/") && userToken != "good" && userToken != "friend" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errors":[{"code":"40300","detail":"Invalid authentication"}]}`)
			return
		}
		var body struct {
			Attributes    map[string]any `json:"attributes"`
			Relationships struct {
				Tracks struct{ Data []struct{ ID string } } `json:"tracks"`
				Parent struct{ Data []struct{ ID string } } `json:"parent"`
			} `json:"relationships"`
		}
		json.NewDecoder(r.Body).Decode(&body)

		switch {
		case r.URL.Path == "/v1/me/storefront":
			fmt.Fprint(w, `{"data":[{"id":"gb"}]}`)
		case r.URL.Path == "/v1/catalog/gb/songs" || r.URL.Path == "/v1/catalog/us/songs":
			// Spotify's fake playlist has one song, ISRC "USX".
			fmt.Fprint(w, `{"data":[{"id":"111","attributes":{"name":"Song","artistName":"Artist","albumName":"Album","isrc":"USX"}}],
				"meta":{"filters":{"isrc":{"USX":[{"id":"111"}]}}}}`)
		case strings.HasPrefix(r.URL.Path, "/v1/me/library/playlist-folders/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/me/library/playlist-folders/")
			if f.folderOwner[id] != userToken || f.deletedFolders[id] {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"errors":[{"code":"40400"}]}`)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
		case r.URL.Path == "/v1/me/library/playlist-folders":
			id := fmt.Sprintf("p.folder%d", len(f.folders)+1)
			f.folders = append(f.folders, id)
			f.folderOwner[id] = userToken
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
		case r.URL.Path == "/v1/me/library/playlists":
			f.createAttempts++
			parent := ""
			if len(body.Relationships.Parent.Data) > 0 {
				parent = body.Relationships.Parent.Data[0].ID
			}
			if parent != "" && (f.deletedFolders[parent] || f.folderOwner[parent] != userToken) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"errors":[{"code":"40400","detail":"Parent not found"}]}`)
				return
			}
			var songs []string
			for _, d := range body.Relationships.Tracks.Data {
				songs = append(songs, d.ID)
			}
			f.createdParents = append(f.createdParents, parent)
			f.createdNames = append(f.createdNames, fmt.Sprint(body.Attributes["name"]))
			f.createdSongs = append(f.createdSongs, songs)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"data":[{"id":"p.pl%d","attributes":{"playParams":{"globalId":"pl.u-%d"}}}]}`, len(f.createdNames), len(f.createdNames))
		default:
			t.Errorf("fake apple: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	signer, err := apple.NewTokenSigner("TEAM", "KEY", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	client := apple.NewClient(signer)
	client.APIURL = ts.URL
	return f, client
}

// call sends a request as the given user (who gets a fresh session) and decodes the JSON reply.
func call(t *testing.T, srv *server, user, method, path, body string, out any) int {
	t.Helper()
	token, err := srv.store.CreateSession(context.Background(), memberID(t, srv, user), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code
}

func TestAppleConnectionPermissions(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("alice", time.Hour), account("bob", time.Hour))
	_, srv.apple = newFakeApple(t)

	var status appleStatusJSON
	if code := call(t, srv, "bob", "GET", "/api/apple", "", &status); code != 200 || status.Connected || !status.CanManage {
		t.Fatalf("before connecting: %d %+v", code, status)
	}
	if code := call(t, srv, "alice", "PUT", "/api/apple/connection", `{"musicUserToken":"bad"}`, nil); code != http.StatusBadGateway {
		t.Errorf("connect with rejected token: status %d, want 502", code)
	}
	if code := call(t, srv, "alice", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, &status); code != 200 ||
		!status.Connected || status.Storefront != "gb" || status.ConnectedBy.DisplayName != "ALICE" || !status.CanManage {
		t.Fatalf("alice connects: %d %+v", code, status)
	}

	// Bob sees the connection but can't replace or remove it.
	if call(t, srv, "bob", "GET", "/api/apple", "", &status); status.CanManage || !status.Connected {
		t.Errorf("bob's view: %+v", status)
	}
	if code := call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil); code != http.StatusForbidden {
		t.Errorf("bob replaces: status %d, want 403", code)
	}
	if code := call(t, srv, "bob", "DELETE", "/api/apple/connection", "", nil); code != http.StatusForbidden {
		t.Errorf("bob disconnects: status %d, want 403", code)
	}
	if code := call(t, srv, "alice", "DELETE", "/api/apple/connection", "", nil); code != http.StatusNoContent {
		t.Errorf("alice disconnects: status %d, want 204", code)
	}
}

func TestPreviewAndCreate(t *testing.T) {
	fakeSpotify := newFakeSpotify(t, map[string]fakePlaylist{
		"bobsplaylist12345678ab": {owner: "bob", public: true},
	})
	srv := newTestServer(t, fakeSpotify, account("alice", time.Hour), account("bob", time.Hour))
	fake, client := newFakeApple(t)
	srv.apple = client
	link := `"https://open.spotify.com/playlist/bobsplaylist12345678ab"`

	// Creating needs a connected Apple Music account.
	if code := call(t, srv, "bob", "POST", "/api/translate", `{"link":`+link+`,"songIds":["111"]}`, nil); code != http.StatusConflict {
		t.Fatalf("create before connecting: status %d, want 409", code)
	}
	call(t, srv, "alice", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil)

	var preview previewJSON
	if code := call(t, srv, "bob", "POST", "/api/translate/preview", `{"link":`+link+`}`, &preview); code != 200 {
		t.Fatalf("preview: status %d", code)
	}
	if len(preview.Results) != 1 || preview.Results[0].Song == nil || preview.Results[0].Song.ID != "111" || preview.Storefront != "gb" {
		t.Fatalf("preview = %+v", preview)
	}

	var created struct {
		ShareURL  string `json:"shareUrl"`
		SongCount int    `json:"songCount"`
	}
	if code := call(t, srv, "bob", "POST", "/api/translate", `{"link":`+link+`,"songIds":["111"]}`, &created); code != http.StatusCreated {
		t.Fatalf("create: status %d", code)
	}
	if created.ShareURL != "https://music.apple.com/playlist/pl.u-1" || created.SongCount != 1 {
		t.Errorf("created = %+v", created)
	}
	if len(fake.folders) != 1 || fake.createdParents[0] != "p.folder1" || fake.createdNames[0] != "Playlist bobsplaylist12345678ab" {
		t.Errorf("folders %v, parents %v, names %v", fake.folders, fake.createdParents, fake.createdNames)
	}

	// The folder is reused; if it's been deleted in the Music app, a new one is made.
	call(t, srv, "bob", "POST", "/api/translate", `{"link":`+link+`,"songIds":["111"]}`, nil)
	fake.mu.Lock()
	fake.deletedFolders["p.folder1"] = true
	fake.mu.Unlock()
	if code := call(t, srv, "bob", "POST", "/api/translate", `{"link":`+link+`,"songIds":["111"]}`, nil); code != http.StatusCreated {
		t.Fatalf("create after folder deleted: status %d", code)
	}
	if fmt.Sprint(fake.createdParents) != "[p.folder1 p.folder1 p.folder2]" {
		t.Errorf("parents = %v, want folder reused then replaced", fake.createdParents)
	}

	// Earlier translations show up in the next preview, newest first.
	call(t, srv, "bob", "POST", "/api/translate/preview", `{"link":`+link+`}`, &preview)
	if len(preview.Previous) != 3 || preview.Previous[0].ShareURL != "https://music.apple.com/playlist/pl.u-3" || preview.Previous[0].CreatedBy.DisplayName != "BOB" {
		t.Errorf("previous = %+v", preview.Previous)
	}

	// Song IDs must look like catalog IDs.
	if code := call(t, srv, "bob", "POST", "/api/translate", `{"link":`+link+`,"songIds":["p.library-id"]}`, nil); code != http.StatusBadRequest {
		t.Errorf("bad song id: status %d, want 400", code)
	}
}

func TestCreateReportsExpiredAppleConnection(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, map[string]fakePlaylist{"bobsplaylist12345678ab": {owner: "bob", public: true}}), account("bob", time.Hour))
	_, srv.apple = newFakeApple(t)
	// Saved directly: the token has since been revoked or expired.
	srv.store.SetAppleAccount(context.Background(), store.AppleAccount{MusicUserToken: "expired", Storefront: "us", ConnectedBy: memberID(t, srv, "bob"), ConnectedAt: time.Now()})

	var resp struct{ Error string }
	code := call(t, srv, "bob", "POST", "/api/translate", `{"link":"bobsplaylist12345678ab","songIds":["111"]}`, &resp)
	if code != http.StatusBadGateway || !strings.Contains(resp.Error, "BOB needs to connect Apple Music again") {
		t.Errorf("status %d, error %q", code, resp.Error)
	}
}

func TestSwitchingAppleAccountsStartsNewFolder(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, map[string]fakePlaylist{"bobsplaylist12345678ab": {owner: "bob", public: true}}), account("bob", time.Hour))
	fake, client := newFakeApple(t)
	srv.apple = client
	create := func() {
		t.Helper()
		if code := call(t, srv, "bob", "POST", "/api/translate", `{"link":"bobsplaylist12345678ab","songIds":["111"]}`, nil); code != http.StatusCreated {
			t.Fatalf("create: status %d", code)
		}
	}

	call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil)
	create()
	// Reconnecting the same account keeps its folder.
	call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil)
	create()
	// Switching to the friend's account forgets the old folder instead of trying it first.
	call(t, srv, "bob", "DELETE", "/api/apple/connection", "", nil)
	call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"friend"}`, nil)
	create()
	// Reconnecting straight to a different account (no disconnect) does the same.
	call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil)
	create()

	// Creating would recover from a stale folder by retrying, so check no attempt was wasted on one.
	if fake.createAttempts != 4 {
		t.Errorf("create attempts = %d, want 4 (none against the other library's folder)", fake.createAttempts)
	}
	if got := fmt.Sprint(fake.createdParents); got != "[p.folder1 p.folder1 p.folder2 p.folder3]" {
		t.Errorf("parents = %s", got)
	}
	if fake.folderOwner["p.folder2"] != "friend" || fake.folderOwner["p.folder3"] != "good" {
		t.Errorf("folder owners = %v", fake.folderOwner)
	}
}
