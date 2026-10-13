package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUsersAtTheSameTime has three creators preview and create playlists at once, the way the
// friend group might after a new playlist drop. Run with -race.
func TestUsersAtTheSameTime(t *testing.T) {
	playlists := map[string]string{ // user → their playlist ID
		"alice": "alicesplaylist12345678",
		"bob":   "bobsplaylist12345678ab",
		"carol": "carolsplaylist1234567x",
	}
	fakePlaylists := map[string]fakePlaylist{}
	for user, id := range playlists {
		fakePlaylists[id] = fakePlaylist{owner: user, public: true}
	}
	// Bob's access token has expired, so his concurrent requests all race to refresh it.
	srv := newTestServer(t, newFakeSpotify(t, fakePlaylists),
		account("alice", time.Hour), account("bob", -time.Hour), account("carol", time.Hour))
	fake, client := newFakeApple(t)
	srv.apple = client
	if code := call(t, srv, "alice", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil); code != http.StatusOK {
		t.Fatalf("connect apple: status %d", code)
	}

	// Each user gets their own session, as if signed in on their own phone.
	sessions := map[string]string{}
	for user := range playlists {
		token, err := srv.store.CreateSession(context.Background(), memberID(t, srv, user), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		sessions[user] = token
	}
	handler := srv.routes()
	send := func(user, path, body string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessions[user]})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	const rounds = 3
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []string
	)
	for user, id := range playlists {
		for range rounds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				link := fmt.Sprintf(`"https://open.spotify.com/playlist/%s"`, id)
				preview := send(user, "/api/translate/preview", `{"link":`+link+`}`)
				create := send(user, "/api/translate", `{"link":`+link+`,"songIds":["111"]}`)
				if preview != http.StatusOK || create != http.StatusCreated {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("%s: preview %d, create %d", user, preview, create))
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()

	for _, f := range failures {
		t.Error(f)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.folders) != 1 {
		t.Errorf("created folders %v, want exactly one shared folder", fake.folders)
	}
	if want := len(playlists) * rounds; len(fake.createdNames) != want {
		t.Errorf("created %d playlists, want %d", len(fake.createdNames), want)
	}
	for _, parent := range fake.createdParents {
		if parent != "p.folder1" {
			t.Errorf("playlist created in %q, want p.folder1", parent)
		}
	}
	for user, id := range playlists {
		got, err := srv.store.TranslationsOf(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != rounds {
			t.Errorf("%s: %d translations recorded, want %d", user, len(got), rounds)
		}
	}
}
