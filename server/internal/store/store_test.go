package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestMigrateToMembers upgrades a database from before members existed, like the one on the Pi.
func TestMigrateToMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{
		migrations[0],
		`INSERT INTO spotify_accounts VALUES ('alice', 'Alice', 'a', 'a', 0, 1, 1), ('bob', 'Bob', 'b', 'b', 0, 2, 2)`,
		`INSERT INTO sessions VALUES ('hash-bob', 'bob', 1, 9999999999)`,
		`INSERT INTO apple_account VALUES (1, 'mut', 'gb', 'bob', 5, 'p.folder')`,
		// "gone" disconnected Spotify after making a playlist.
		`INSERT INTO translations (spotify_playlist_id, name, apple_playlist_id, share_url, song_count, created_by, created_at)
			VALUES ('pl1', 'Mix', 'p.1', 'https://music.apple.com/playlist/pl.u-1', 3, 'gone', 7)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	bob, err := s.MemberBySpotifyID(ctx, "bob")
	if err != nil || bob.DisplayName != "Bob" || !bob.IsAdmin {
		t.Fatalf("bob = %+v, %v; want admin, since he connected Apple Music", bob, err)
	}
	if alice, err := s.MemberBySpotifyID(ctx, "alice"); err != nil || alice.IsAdmin {
		t.Errorf("alice = %+v, %v", alice, err)
	}
	gone, err := s.MemberBySpotifyID(ctx, "gone")
	if err != nil || gone.DisplayName != "gone" {
		t.Errorf("gone = %+v, %v", gone, err)
	}

	var sessionMember int64
	if err := s.db.QueryRow(`SELECT member_id FROM sessions WHERE token_hash = 'hash-bob'`).Scan(&sessionMember); err != nil || sessionMember != bob.ID {
		t.Errorf("bob's session belongs to %d, %v", sessionMember, err)
	}
	acct, err := s.GetAppleAccount(ctx)
	if err != nil || acct.ConnectedBy != bob.ID || acct.FolderID != "p.folder" || acct.MusicUserToken != "mut" {
		t.Errorf("apple account = %+v, %v", acct, err)
	}
	ts, err := s.TranslationsOf(ctx, "pl1")
	if err != nil || len(ts) != 1 || ts[0].CreatedBy != gone.ID {
		t.Errorf("translations = %+v, %v", ts, err)
	}

	// Opening again doesn't re-run anything.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if members, _ := s.ListMembers(ctx); len(members) != 3 {
		t.Errorf("%d members after reopening", len(members))
	}
}

func TestFirstMemberIsAdmin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	first, err := s.CreateMember(ctx, Member{DisplayName: "First", SpotifyUserID: "first"})
	if err != nil || !first.IsAdmin {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := s.CreateMember(ctx, Member{DisplayName: "Second", SpotifyUserID: "second"})
	if err != nil || second.IsAdmin || second.AppleSub != "" {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

func TestAllowlistAndRequests(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.AllowEmail(ctx, " Friend@iCloud.com ", "Friend"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAllowedEmail(ctx, "someone@else.com", "sub-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim unknown email: %v", err)
	}
	// A request from the same person is cleared once they get in.
	if err := s.RequestAccess(ctx, AccessRequest{AppleSub: "sub-1", Email: "relay@privaterelay.appleid.com", DisplayName: "F"}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ClaimAllowedEmail(ctx, "friend@icloud.com", "sub-1")
	if err != nil || m.DisplayName != "Friend" || m.AppleSub != "sub-1" {
		t.Fatalf("claim = %+v, %v", m, err)
	}
	if _, err := s.ClaimAllowedEmail(ctx, "friend@icloud.com", "sub-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second claim of the same email: %v", err)
	}
	if reqs, _ := s.ListAccessRequests(ctx); len(reqs) != 0 {
		t.Errorf("requests left: %+v", reqs)
	}

	// A repeat request keeps the first name Apple sent (later sign-ins don't include it).
	s.RequestAccess(ctx, AccessRequest{AppleSub: "sub-3", Email: "a@b.c", DisplayName: "Alex"})
	s.RequestAccess(ctx, AccessRequest{AppleSub: "sub-3", Email: "a@b.c", DisplayName: ""})
	reqs, _ := s.ListAccessRequests(ctx)
	if len(reqs) != 1 || reqs[0].DisplayName != "Alex" {
		t.Fatalf("requests = %+v", reqs)
	}
	alex, err := s.ApproveAccessRequest(ctx, reqs[0].ID, "Alex P")
	if err != nil || alex.AppleSub != "sub-3" || alex.DisplayName != "Alex P" {
		t.Fatalf("approve = %+v, %v", alex, err)
	}
	if got, err := s.MemberByAppleSub(ctx, "sub-3"); err != nil || got.ID != alex.ID {
		t.Errorf("by sub = %+v, %v", got, err)
	}
}
