package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

// fakeSpotify mimics the Development Mode rules: playlist metadata is visible to anyone if the
// playlist is public (only the owner if private), but songs only to the owner or collaborators.
type fakePlaylist struct {
	owner         string
	public        bool
	collaborative bool
	collaborators []string
}

func newFakeSpotify(t *testing.T, playlists map[string]fakePlaylist) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token" {
			// Refreshing "bob-refresh" yields a fresh token for bob.
			if r.FormValue("refresh_token") != "bob-refresh" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error": "invalid_grant"}`)
				return
			}
			fmt.Fprint(w, `{"access_token": "bob-token", "expires_in": 3600}`)
			return
		}

		user := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-token")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/playlists/"), "/")
		pl, ok := playlists[parts[0]]
		canSee := ok && (pl.public || pl.owner == user)
		canRead := ok && (pl.owner == user || (pl.collaborative && contains(pl.collaborators, user)))
		switch {
		case !canSee:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error": {"status": 404, "message": "Not found."}}`)
		case len(parts) == 1:
			json.NewEncoder(w).Encode(map[string]any{
				"id": parts[0], "name": "Playlist " + parts[0], "collaborative": pl.collaborative,
				"owner": map[string]string{"id": pl.owner, "display_name": strings.ToUpper(pl.owner)},
			})
		case !canRead:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error": {"status": 403, "message": "Forbidden"}}`)
		default:
			fmt.Fprint(w, `{"next": null, "items": [{"item": {"type": "track", "id": "t1", "name": "Song",
				"artists": [{"name": "Artist"}], "album": {"name": "Album"}, "external_ids": {"isrc": "USX"}}}]}`)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func newTestServer(t *testing.T, fake *httptest.Server, accounts ...store.SpotifyAccount) *server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, a := range accounts {
		if err := db.UpsertSpotifyAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	client := spotify.NewClient("id", "secret", "http://127.0.0.1:8080/callback")
	client.AccountsURL = fake.URL
	client.APIURL = fake.URL + "/v1"
	return &server{store: db, spotify: client}
}

func account(user string, expiresIn time.Duration) store.SpotifyAccount {
	return store.SpotifyAccount{
		UserID:       user,
		DisplayName:  strings.ToUpper(user),
		AccessToken:  user + "-token",
		RefreshToken: user + "-refresh",
		ExpiresAt:    time.Now().Add(expiresIn),
	}
}

func TestReadPlaylistUsesOwnersToken(t *testing.T) {
	fake := newFakeSpotify(t, map[string]fakePlaylist{
		"bobs":         {owner: "bob", public: true},
		"bobs-private": {owner: "bob"},
		"carols":       {owner: "carol", public: true},
		"collab":       {owner: "carol", public: true, collaborative: true, collaborators: []string{"bob"}},
	})
	srv := newTestServer(t, fake, account("alice", time.Hour), account("bob", time.Hour))
	ctx := context.Background()

	for _, id := range []string{"bobs", "bobs-private", "collab"} {
		// Alice is signed in, but only Bob's token can read these songs.
		res, err := srv.readPlaylist(ctx, "alice", id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if res.ReadWith.ID != "bob" || len(res.Tracks) != 1 || res.Tracks[0].ISRC != "USX" {
			t.Errorf("%s: unexpected result %+v", id, res)
		}
	}

	_, err := srv.readPlaylist(ctx, "alice", "carols")
	var ue *userError
	if !errors.As(err, &ue) || ue.status != http.StatusUnprocessableEntity || !strings.Contains(ue.message, "CAROL") {
		t.Errorf("carols: got %v, want 'hasn't connected' error naming CAROL", err)
	}

	_, err = srv.readPlaylist(ctx, "alice", "missing")
	if !errors.As(err, &ue) || ue.status != http.StatusNotFound {
		t.Errorf("missing: got %v, want not-found error", err)
	}
}

func TestReadPlaylistRefreshesExpiredToken(t *testing.T) {
	fake := newFakeSpotify(t, map[string]fakePlaylist{"bobs": {owner: "bob", public: true}})
	expired := account("bob", -time.Hour)
	expired.AccessToken = "stale"
	srv := newTestServer(t, fake, expired)

	res, err := srv.readPlaylist(context.Background(), "bob", "bobs")
	if err != nil || res.ReadWith.ID != "bob" {
		t.Fatalf("readPlaylist() = %+v, %v", res, err)
	}
	saved, _ := srv.store.GetSpotifyAccount(context.Background(), "bob")
	if saved.AccessToken != "bob-token" || saved.RefreshToken != "bob-refresh" || time.Until(saved.ExpiresAt) < 50*time.Minute {
		t.Errorf("refreshed token not saved: %+v", saved)
	}
}

func TestSessionsAndDisconnect(t *testing.T) {
	fake := newFakeSpotify(t, nil)
	srv := newTestServer(t, fake, account("alice", time.Hour))
	ctx := context.Background()

	token, err := srv.store.CreateSession(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess, err := srv.store.GetSession(ctx, token); err != nil || sess.SpotifyUserID != "alice" {
		t.Fatalf("GetSession() = %+v, %v", sess, err)
	}

	// Disconnecting deletes the account, and its sessions with it.
	req := httptest.NewRequest(http.MethodDelete, "/api/spotify/account", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect status = %d: %s", rec.Code, rec.Body)
	}
	if _, err := srv.store.GetSpotifyAccount(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("account still present: %v", err)
	}
	if _, err := srv.store.GetSession(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session still present: %v", err)
	}
}

func TestAPIRequiresSession(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil))
	req := httptest.NewRequest(http.MethodPost, "/api/spotify/playlist", strings.NewReader(`{"link": "x"}`))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.env")
	content := "# comment\n\nPT_TEST_A=one\nexport PT_TEST_B=\"two words\"\nPT_TEST_C=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PT_TEST_C", "from-env")
	for _, k := range []string{"PT_TEST_A", "PT_TEST_B"} {
		k := k
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PT_TEST_A"); got != "one" {
		t.Errorf("PT_TEST_A = %q", got)
	}
	if got := os.Getenv("PT_TEST_B"); got != "two words" {
		t.Errorf("PT_TEST_B = %q", got)
	}
	if got := os.Getenv("PT_TEST_C"); got != "from-env" {
		t.Errorf("PT_TEST_C = %q; real environment should win", got)
	}
}
