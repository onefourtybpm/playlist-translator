package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Member is a person who can use the app. They sign in with Spotify, with Apple, or either.
type Member struct {
	ID            int64
	DisplayName   string
	IsAdmin       bool
	SpotifyUserID string // empty if they haven't signed in with Spotify
	AppleSub      string // Sign in with Apple's stable user ID; empty if not linked
	CreatedAt     time.Time
}

const memberColumns = `id, display_name, is_admin, COALESCE(spotify_user_id, ''), COALESCE(apple_sub, ''), created_at`

func scanMember(row scanner) (Member, error) {
	var m Member
	var created int64
	err := row.Scan(&m.ID, &m.DisplayName, &m.IsAdmin, &m.SpotifyUserID, &m.AppleSub, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	m.CreatedAt = time.Unix(created, 0)
	return m, err
}

func (s *Store) GetMember(ctx context.Context, id int64) (Member, error) {
	return scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE id = ?`, id))
}

func (s *Store) MemberBySpotifyID(ctx context.Context, spotifyUserID string) (Member, error) {
	return scanMember(s.db.QueryRowContext(ctx,
		`SELECT `+memberColumns+` FROM members WHERE spotify_user_id = ?`, spotifyUserID))
}

func (s *Store) MemberByAppleSub(ctx context.Context, sub string) (Member, error) {
	return scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE apple_sub = ?`, sub))
}

func (s *Store) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memberColumns+` FROM members ORDER BY display_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMember adds a member. The first member ever becomes the admin.
func (s *Store) CreateMember(ctx context.Context, m Member) (Member, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO members (display_name, is_admin, spotify_user_id, apple_sub, created_at)
		VALUES (?, NOT EXISTS (SELECT 1 FROM members), NULLIF(?, ''), NULLIF(?, ''), ?)`,
		m.DisplayName, m.SpotifyUserID, m.AppleSub, time.Now().Unix())
	if err != nil {
		return Member{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Member{}, err
	}
	return s.GetMember(ctx, id)
}

// RenameMember changes a member's display name. It returns ErrNotFound if there's no such member.
func (s *Store) RenameMember(ctx context.Context, id int64, displayName string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE members SET display_name = ? WHERE id = ?`, displayName, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) LinkSpotify(ctx context.Context, memberID int64, spotifyUserID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE members SET spotify_user_id = ? WHERE id = ?`, spotifyUserID, memberID)
	return err
}

func (s *Store) LinkApple(ctx context.Context, memberID int64, sub string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE members SET apple_sub = ? WHERE id = ?`, sub, memberID)
	return err
}

// DeleteMember removes a member, their sessions, and their Spotify connection.
func (s *Store) DeleteMember(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM spotify_accounts
		WHERE spotify_user_id = (SELECT spotify_user_id FROM members WHERE id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM members WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// NormalizeEmail is the form emails are stored and compared in.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

type AllowedEmail struct {
	Email       string
	DisplayName string
	AddedAt     time.Time
}

func (s *Store) AllowEmail(ctx context.Context, email, displayName string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO allowed_emails (email, display_name, added_at) VALUES (?, ?, ?)
		ON CONFLICT (email) DO UPDATE SET display_name = excluded.display_name`,
		NormalizeEmail(email), displayName, time.Now().Unix())
	return err
}

func (s *Store) RemoveAllowedEmail(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM allowed_emails WHERE email = ?`, NormalizeEmail(email))
	return err
}

func (s *Store) ListAllowedEmails(ctx context.Context) ([]AllowedEmail, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, display_name, added_at FROM allowed_emails ORDER BY added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllowedEmail
	for rows.Next() {
		var a AllowedEmail
		var added int64
		if err := rows.Scan(&a.Email, &a.DisplayName, &added); err != nil {
			return nil, err
		}
		a.AddedAt = time.Unix(added, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimAllowedEmail turns an allowed email into a member signed in with Apple, in one step so two
// sign-ins can't both claim it. It returns ErrNotFound if the email isn't allowed.
func (s *Store) ClaimAllowedEmail(ctx context.Context, email, appleSub string) (Member, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `DELETE FROM allowed_emails WHERE email = ? RETURNING display_name`,
		NormalizeEmail(email)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrNotFound
	}
	if err != nil {
		return Member{}, err
	}
	id, err := insertAppleMember(ctx, tx, name, appleSub)
	if err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	return s.GetMember(ctx, id)
}

func insertAppleMember(ctx context.Context, tx *sql.Tx, name, appleSub string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_requests WHERE apple_sub = ?`, appleSub); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO members (display_name, apple_sub, created_at) VALUES (?, ?, ?)`,
		name, appleSub, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type AccessRequest struct {
	ID          int64
	AppleSub    string
	Email       string
	DisplayName string
	RequestedAt time.Time
}

// RequestAccess records (or refreshes) a Sign in with Apple attempt that wasn't allowed in.
func (s *Store) RequestAccess(ctx context.Context, r AccessRequest) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO access_requests (apple_sub, email, display_name, requested_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (apple_sub) DO UPDATE SET
			email        = excluded.email,
			display_name = CASE WHEN excluded.display_name = '' THEN display_name ELSE excluded.display_name END,
			requested_at = excluded.requested_at`,
		r.AppleSub, NormalizeEmail(r.Email), r.DisplayName, time.Now().Unix())
	return err
}

func (s *Store) ListAccessRequests(ctx context.Context) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, apple_sub, email, display_name, requested_at FROM access_requests ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		var r AccessRequest
		var requested int64
		if err := rows.Scan(&r.ID, &r.AppleSub, &r.Email, &r.DisplayName, &requested); err != nil {
			return nil, err
		}
		r.RequestedAt = time.Unix(requested, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApproveAccessRequest makes the requester a member. They're in on their next sign-in.
func (s *Store) ApproveAccessRequest(ctx context.Context, id int64, displayName string) (Member, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	var sub string
	err = tx.QueryRowContext(ctx, `SELECT apple_sub FROM access_requests WHERE id = ?`, id).Scan(&sub)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrNotFound
	}
	if err != nil {
		return Member{}, err
	}
	memberID, err := insertAppleMember(ctx, tx, displayName, sub)
	if err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	return s.GetMember(ctx, memberID)
}

func (s *Store) DeleteAccessRequest(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_requests WHERE id = ?`, id)
	return err
}
