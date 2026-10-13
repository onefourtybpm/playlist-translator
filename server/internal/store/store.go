// Package store persists connected accounts and browser sessions in SQLite.
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

const schema = `
CREATE TABLE IF NOT EXISTS spotify_accounts (
	spotify_user_id  TEXT PRIMARY KEY,
	display_name     TEXT NOT NULL,
	access_token     TEXT NOT NULL,
	refresh_token    TEXT NOT NULL,
	token_expires_at INTEGER NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);

-- The one Apple Music account translated playlists are created in. connected_by is the Spotify
-- user who connected it; only they can replace or disconnect it.
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
`

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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// The database holds OAuth tokens, so keep it private to this user.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

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

// DeleteSpotifyAccount removes the account, its tokens, and (via cascade) its sessions.
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
	SpotifyUserID string
	ExpiresAt     time.Time
}

// CreateSession returns a new random session token. Only its hash is stored, so a leaked
// database can't be used to hijack sessions.
func (s *Store) CreateSession(ctx context.Context, spotifyUserID string, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, spotify_user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), spotifyUserID, now.Unix(), now.Add(ttl).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) GetSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var userID sql.NullString
	var expires int64
	err := s.db.QueryRowContext(ctx, `
		SELECT spotify_user_id, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	sess.SpotifyUserID = userID.String
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
	ConnectedBy    string // Spotify user ID
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

// SetAppleAccount connects (or reconnects) the Apple Music account. Reconnecting keeps the
// playlist folder, since it's in the same library.
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
	CreatedBy         string // Spotify user ID
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
