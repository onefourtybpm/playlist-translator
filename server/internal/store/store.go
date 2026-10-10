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
