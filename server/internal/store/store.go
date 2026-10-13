// Package store persists members, connected accounts and browser sessions in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

// migrations[i] takes the database from user_version i to i+1. Databases made before versioning
// are at 0 and already have the first migration's tables, which it creates only if missing.
var migrations = []string{
	// 1: Spotify accounts were the only identity.
	`
CREATE TABLE IF NOT EXISTS spotify_accounts (
	spotify_user_id  TEXT PRIMARY KEY,
	display_name     TEXT NOT NULL,
	access_token     TEXT NOT NULL,
	refresh_token    TEXT NOT NULL,
	token_expires_at INTEGER NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS apple_account (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	music_user_token TEXT NOT NULL,
	storefront       TEXT NOT NULL,
	connected_by     TEXT NOT NULL,
	connected_at     INTEGER NOT NULL,
	folder_id        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS translations (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	spotify_playlist_id TEXT NOT NULL,
	name                TEXT NOT NULL,
	apple_playlist_id   TEXT NOT NULL,
	share_url           TEXT NOT NULL,
	song_count          INTEGER NOT NULL,
	created_by          TEXT NOT NULL,
	created_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS translations_by_playlist ON translations (spotify_playlist_id, created_at);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash      TEXT PRIMARY KEY,
	spotify_user_id TEXT REFERENCES spotify_accounts(spotify_user_id) ON DELETE CASCADE,
	created_at      INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL
);
`,
	// 2: The app's own members. People sign in with Spotify or with Apple; sessions, the Apple
	// Music connection and translations now point at a member. Every Spotify account (and any
	// Spotify ID still referenced) becomes a member, and whoever connected Apple Music (else the
	// first account) becomes the admin.
	`
CREATE TABLE members (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	display_name    TEXT NOT NULL,
	is_admin        INTEGER NOT NULL DEFAULT 0,
	spotify_user_id TEXT UNIQUE,
	apple_sub       TEXT UNIQUE,
	created_at      INTEGER NOT NULL
);

INSERT INTO members (display_name, spotify_user_id, created_at)
	SELECT display_name, spotify_user_id, created_at FROM spotify_accounts ORDER BY created_at;
INSERT INTO members (display_name, spotify_user_id, created_at)
	SELECT DISTINCT id, id, CAST(strftime('%s', 'now') AS INTEGER) FROM (
		SELECT connected_by AS id FROM apple_account UNION SELECT created_by FROM translations
	) WHERE id NOT IN (SELECT spotify_user_id FROM members);
UPDATE members SET is_admin = 1 WHERE id = COALESCE(
	(SELECT m.id FROM members m JOIN apple_account a ON a.connected_by = m.spotify_user_id),
	(SELECT MIN(id) FROM members));

CREATE TABLE sessions_new (
	token_hash TEXT PRIMARY KEY,
	member_id  INTEGER NOT NULL REFERENCES members(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
INSERT INTO sessions_new SELECT s.token_hash, m.id, s.created_at, s.expires_at
	FROM sessions s JOIN members m ON m.spotify_user_id = s.spotify_user_id;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;

-- The one Apple Music account translated playlists are created in. Only the member who
-- connected it can replace or disconnect it.
CREATE TABLE apple_account_new (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	music_user_token TEXT NOT NULL,
	storefront       TEXT NOT NULL,
	connected_by     INTEGER NOT NULL,
	connected_at     INTEGER NOT NULL,
	folder_id        TEXT NOT NULL DEFAULT ''
);
INSERT INTO apple_account_new SELECT a.id, a.music_user_token, a.storefront, m.id, a.connected_at, a.folder_id
	FROM apple_account a JOIN members m ON m.spotify_user_id = a.connected_by;
DROP TABLE apple_account;
ALTER TABLE apple_account_new RENAME TO apple_account;

CREATE TABLE translations_new (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	spotify_playlist_id TEXT NOT NULL,
	name                TEXT NOT NULL,
	apple_playlist_id   TEXT NOT NULL,
	share_url           TEXT NOT NULL,
	song_count          INTEGER NOT NULL,
	created_by          INTEGER NOT NULL,
	created_at          INTEGER NOT NULL
);
INSERT INTO translations_new SELECT t.id, t.spotify_playlist_id, t.name, t.apple_playlist_id, t.share_url,
		t.song_count, m.id, t.created_at
	FROM translations t JOIN members m ON m.spotify_user_id = t.created_by;
DROP TABLE translations;
ALTER TABLE translations_new RENAME TO translations;
CREATE INDEX translations_by_playlist ON translations (spotify_playlist_id, created_at);

-- Emails the admin has let in with Sign in with Apple. A row is removed once someone claims it.
CREATE TABLE allowed_emails (
	email        TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	added_at     INTEGER NOT NULL
);

-- Sign in with Apple attempts whose email wasn't allowed, waiting for the admin.
CREATE TABLE access_requests (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	apple_sub    TEXT NOT NULL UNIQUE,
	email        TEXT NOT NULL,
	display_name TEXT NOT NULL,
	requested_at INTEGER NOT NULL
);
`,
}

// Open opens (creating if needed) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A handful of users never needs concurrent writers; one connection avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds OAuth tokens, so keep it private to this user.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for ; version < len(migrations); version++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate schema to version %d: %w", version+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate schema to version %d: %w", version+1, err)
		}
	}
	return nil
}

type SpotifyAccount struct {
	UserID       string
	DisplayName  string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func (s *Store) UpsertSpotifyAccount(ctx context.Context, a SpotifyAccount) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO spotify_accounts
			(spotify_user_id, display_name, access_token, refresh_token, token_expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (spotify_user_id) DO UPDATE SET
			display_name     = excluded.display_name,
			access_token     = excluded.access_token,
			refresh_token    = excluded.refresh_token,
			token_expires_at = excluded.token_expires_at,
			updated_at       = excluded.updated_at`,
		a.UserID, a.DisplayName, a.AccessToken, a.RefreshToken, a.ExpiresAt.Unix(), now, now)
	return err
}

func (s *Store) UpdateSpotifyTokens(ctx context.Context, userID, accessToken, refreshToken string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE spotify_accounts
		SET access_token = ?, refresh_token = ?, token_expires_at = ?, updated_at = ?
		WHERE spotify_user_id = ?`,
		accessToken, refreshToken, expiresAt.Unix(), time.Now().Unix(), userID)
	return err
}

func (s *Store) GetSpotifyAccount(ctx context.Context, userID string) (SpotifyAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT spotify_user_id, display_name, access_token, refresh_token, token_expires_at
		FROM spotify_accounts WHERE spotify_user_id = ?`, userID)
	return scanSpotifyAccount(row)
}

func (s *Store) ListSpotifyAccounts(ctx context.Context) ([]SpotifyAccount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT spotify_user_id, display_name, access_token, refresh_token, token_expires_at
		FROM spotify_accounts ORDER BY display_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []SpotifyAccount
	for rows.Next() {
		a, err := scanSpotifyAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// DeleteSpotifyAccount removes the Spotify connection and its tokens. The member stays, and can
// sign in with Spotify again to reconnect.
func (s *Store) DeleteSpotifyAccount(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM spotify_accounts WHERE spotify_user_id = ?`, userID)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanSpotifyAccount(row scanner) (SpotifyAccount, error) {
	var a SpotifyAccount
	var expires int64
	err := row.Scan(&a.UserID, &a.DisplayName, &a.AccessToken, &a.RefreshToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.ExpiresAt = time.Unix(expires, 0)
	return a, err
}

type Session struct {
	MemberID  int64
	ExpiresAt time.Time
}

// CreateSession returns a new random session token. Only its hash is stored, so a leaked
// database can't be used to hijack sessions.
func (s *Store) CreateSession(ctx context.Context, memberID int64, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, member_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), memberID, now.Unix(), now.Add(ttl).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) GetSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var expires int64
	err := s.db.QueryRowContext(ctx, `
		SELECT member_id, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&sess.MemberID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	sess.ExpiresAt = time.Unix(expires, 0)
	if time.Now().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, token)
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type AppleAccount struct {
	MusicUserToken string
	Storefront     string
	ConnectedBy    int64 // member ID
	ConnectedAt    time.Time
	FolderID       string
}

// GetAppleAccount returns the connected Apple Music account, or ErrNotFound.
func (s *Store) GetAppleAccount(ctx context.Context) (AppleAccount, error) {
	var a AppleAccount
	var connected int64
	err := s.db.QueryRowContext(ctx, `
		SELECT music_user_token, storefront, connected_by, connected_at, folder_id
		FROM apple_account WHERE id = 1`).
		Scan(&a.MusicUserToken, &a.Storefront, &a.ConnectedBy, &connected, &a.FolderID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.ConnectedAt = time.Unix(connected, 0)
	return a, err
}

// SetAppleAccount connects (or reconnects) the Apple Music account, keeping the saved playlist
// folder; callers clear it with SetAppleFolder if it isn't in the newly connected library.
func (s *Store) SetAppleAccount(ctx context.Context, a AppleAccount) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apple_account (id, music_user_token, storefront, connected_by, connected_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			music_user_token = excluded.music_user_token,
			storefront       = excluded.storefront,
			connected_by     = excluded.connected_by,
			connected_at     = excluded.connected_at`,
		a.MusicUserToken, a.Storefront, a.ConnectedBy, a.ConnectedAt.Unix())
	return err
}

func (s *Store) SetAppleFolder(ctx context.Context, folderID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE apple_account SET folder_id = ? WHERE id = 1`, folderID)
	return err
}

func (s *Store) DeleteAppleAccount(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM apple_account`)
	return err
}

type Translation struct {
	SpotifyPlaylistID string
	Name              string
	ApplePlaylistID   string
	ShareURL          string
	SongCount         int
	CreatedBy         int64 // member ID
	CreatedAt         time.Time
}

func (s *Store) AddTranslation(ctx context.Context, t Translation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO translations
			(spotify_playlist_id, name, apple_playlist_id, share_url, song_count, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.SpotifyPlaylistID, t.Name, t.ApplePlaylistID, t.ShareURL, t.SongCount, t.CreatedBy, t.CreatedAt.Unix())
	return err
}

// TranslationsOf lists earlier translations of a Spotify playlist, newest first.
func (s *Store) TranslationsOf(ctx context.Context, spotifyPlaylistID string) ([]Translation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT spotify_playlist_id, name, apple_playlist_id, share_url, song_count, created_by, created_at
		FROM translations WHERE spotify_playlist_id = ? ORDER BY created_at DESC, id DESC`, spotifyPlaylistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Translation
	for rows.Next() {
		var t Translation
		var created int64
		if err := rows.Scan(&t.SpotifyPlaylistID, &t.Name, &t.ApplePlaylistID, &t.ShareURL, &t.SongCount, &t.CreatedBy, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}
