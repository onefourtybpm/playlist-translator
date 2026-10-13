package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/appleid"
	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

const (
	sessionCookie    = "pt_session"
	oauthStateCookie = "pt_oauth_state"
	appleStateCookie = "pt_apple_state"
	sessionTTL       = 90 * 24 * time.Hour
)

type server struct {
	cfg     config
	store   *store.Store
	spotify *spotify.Client

	// apple is nil when Apple Music isn't configured.
	apple *apple.Client
	// appleID is nil when Sign in with Apple isn't configured.
	appleID *appleid.Client

	appleSignInsMu sync.Mutex
	appleSignIns   map[string]*appleSignIn // in-progress Sign in with Apple, by state

	// refreshMu serializes token refreshes so two requests don't refresh the same account at once.
	refreshMu sync.Mutex
	// createMu serializes Apple playlist creation.
	createMu sync.Mutex
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /auth/spotify/login", s.handleSpotifyLogin)
	mux.HandleFunc("GET /callback", s.handleSpotifyCallback)
	mux.HandleFunc("GET /auth/apple/login", s.handleAppleSignIn)
	mux.HandleFunc("POST /auth/apple/callback", s.handleAppleSignInCallback)
	mux.HandleFunc("GET /auth/apple/finish", s.handleAppleSignInFinish)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/spotify/accounts", s.requireSession(s.handleListAccounts))
	mux.HandleFunc("DELETE /api/spotify/account", s.requireSession(s.handleDisconnect))
	mux.HandleFunc("POST /api/spotify/playlist", s.requireSession(s.handleReadPlaylist))
	mux.HandleFunc("GET /api/apple", s.requireSession(s.handleAppleStatus))
	mux.HandleFunc("GET /api/apple/developer-token", s.requireSession(s.handleAppleDeveloperToken))
	mux.HandleFunc("PUT /api/apple/connection", s.requireSession(s.handleAppleConnect))
	mux.HandleFunc("DELETE /api/apple/connection", s.requireSession(s.handleAppleDisconnect))
	mux.HandleFunc("POST /api/translate/preview", s.requireSession(s.handlePreview))
	mux.HandleFunc("POST /api/translate", s.requireSession(s.handleCreate))
	mux.HandleFunc("GET /api/admin", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("POST /api/admin/allowed-emails", s.requireAdmin(s.handleAllowEmail))
	mux.HandleFunc("DELETE /api/admin/allowed-emails/{email}", s.requireAdmin(s.handleRemoveAllowedEmail))
	mux.HandleFunc("POST /api/admin/requests/{id}/approve", s.requireAdmin(s.handleApproveRequest))
	mux.HandleFunc("DELETE /api/admin/requests/{id}", s.requireAdmin(s.handleDenyRequest))
	mux.HandleFunc("DELETE /api/admin/members/{id}", s.requireAdmin(s.handleRemoveMember))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Not found.")
	})

	if s.cfg.StaticDir != "" {
		mux.Handle("/", spaHandler(s.cfg.StaticDir))
	}
	return mux
}

// --- Spotify sign-in ---

func (s *server) handleSpotifyLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Couldn't start sign-in.")
		return
	}
	http.SetCookie(w, s.cookie(oauthStateCookie, state, 10*time.Minute))
	http.Redirect(w, r, s.spotify.AuthURL(state), http.StatusFound)
}

func (s *server) handleSpotifyCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	http.SetCookie(w, s.cookie(oauthStateCookie, "", -1))

	if e := q.Get("error"); e != "" {
		msg := "Spotify sign-in failed: " + e
		if e == "access_denied" {
			msg = "Spotify sign-in was cancelled."
		}
		s.redirectToApp(w, r, msg)
		return
	}
	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(q.Get("state"))) != 1 {
		s.redirectToApp(w, r, "Sign-in expired or came from somewhere unexpected. Please try again.")
		return
	}

	ctx := r.Context()
	tok, err := s.spotify.ExchangeCode(ctx, q.Get("code"))
	if err != nil {
		slog.Error("spotify code exchange", "err", err)
		s.redirectToApp(w, r, "Couldn't finish signing in with Spotify. Please try again.")
		return
	}
	me, err := s.spotify.Me(ctx, tok.AccessToken)
	if err != nil {
		slog.Error("spotify /me", "err", err)
		msg := "Couldn't read your Spotify profile. Please try again."
		if spotify.IsStatus(err, http.StatusForbidden) {
			// Development-mode apps reject anyone not added under User Management.
			msg = "This Spotify account isn't on the app's allowed-users list. Ask the app owner to add it."
		}
		s.redirectToApp(w, r, msg)
		return
	}

	err = s.store.UpsertSpotifyAccount(ctx, store.SpotifyAccount{
		UserID:       me.ID,
		DisplayName:  me.DisplayName,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
	})
	if err != nil {
		slog.Error("save spotify account", "err", err)
		s.redirectToApp(w, r, "Couldn't save your Spotify connection.")
		return
	}
	member, err := s.spotifyMember(r, me.ID, me.DisplayName)
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			s.redirectToApp(w, r, ue.message)
			return
		}
		slog.Error("find member for spotify account", "err", err)
		s.redirectToApp(w, r, "Couldn't sign you in.")
		return
	}
	if err := s.startSession(w, r, member); err != nil {
		slog.Error("create session", "err", err)
		s.redirectToApp(w, r, "Couldn't sign you in.")
		return
	}
	slog.Info("spotify account connected", "user", me.ID, "member", member.ID)
	s.redirectToApp(w, r, "")
}

// spotifyMember finds the member a Spotify sign-in belongs to. Someone already signed in (with
// Apple) is connecting Spotify to their membership; otherwise it's the member who signed in with
// this Spotify account before, or a new member: Spotify itself only lets in the accounts listed
// under User Management in the developer dashboard.
func (s *server) spotifyMember(r *http.Request, spotifyUserID, displayName string) (store.Member, error) {
	ctx := r.Context()
	existing, err := s.store.MemberBySpotifyID(ctx, spotifyUserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Member{}, err
	}
	current, signedIn := s.currentMember(r)
	switch {
	case err == nil && signedIn && existing.ID != current.ID:
		return store.Member{}, &userError{http.StatusConflict, fmt.Sprintf(
			"That Spotify account already belongs to %s. Sign out first to use it.", existing.DisplayName)}
	case err == nil:
		return existing, nil
	case signedIn:
		if current.SpotifyUserID != "" {
			// Switching Spotify accounts: the old one's tokens go.
			if err := s.store.DeleteSpotifyAccount(ctx, current.SpotifyUserID); err != nil {
				return store.Member{}, err
			}
		}
		if err := s.store.LinkSpotify(ctx, current.ID, spotifyUserID); err != nil {
			return store.Member{}, err
		}
		return s.store.GetMember(ctx, current.ID)
	default:
		if displayName == "" {
			displayName = spotifyUserID
		}
		return s.store.CreateMember(ctx, store.Member{DisplayName: displayName, SpotifyUserID: spotifyUserID})
	}
}

// startSession signs the browser in as member.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, member store.Member) error {
	session, err := s.store.CreateSession(r.Context(), member.ID, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, s.cookie(sessionCookie, session, sessionTTL))
	return nil
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleDisconnect deletes the signed-in member's stored Spotify tokens, as Spotify's developer
// policy requires when a user disconnects. They stay signed in, and signing in with Spotify again
// reconnects it.
func (s *server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	if member.SpotifyUserID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.store.DeleteSpotifyAccount(r.Context(), member.SpotifyUserID); err != nil {
		slog.Error("delete spotify account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't disconnect your account.")
		return
	}
	slog.Info("spotify account disconnected", "user", member.SpotifyUserID, "member", member.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) redirectToApp(w http.ResponseWriter, r *http.Request, errMsg string) {
	s.redirectToAppWith(w, r, "error", errMsg)
}

// redirectToAppWith sends the browser back to the app with a message for it to show: key is
// "error" or "notice".
func (s *server) redirectToAppWith(w http.ResponseWriter, r *http.Request, key, msg string) {
	target := s.cfg.AppURL + "/"
	if msg != "" {
		target += "?" + key + "=" + url.QueryEscape(msg)
	}
	// 303 so a redirect after Apple's form POST becomes a GET.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *server) cookie(name, value string, maxAge time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		// Lax lets the cookie ride along on Spotify's top-level redirect back to /callback,
		// while blocking it on cross-site POSTs.
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
	}
	if maxAge < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge.Seconds())
	}
	return c
}

// --- Sessions ---

type memberKey struct{}

// memberFrom returns the signed-in member of a request that went through requireSession.
func memberFrom(ctx context.Context) store.Member {
	m, _ := ctx.Value(memberKey{}).(store.Member)
	return m
}

func (s *server) currentMember(r *http.Request) (store.Member, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.Member{}, false
	}
	sess, err := s.store.GetSession(r.Context(), c.Value)
	if err == nil {
		var m store.Member
		if m, err = s.store.GetMember(r.Context(), sess.MemberID); err == nil {
			return m, true
		}
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Error("load session", "err", err)
	}
	return store.Member{}, false
}

func (s *server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := s.currentMember(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "Please sign in first.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), memberKey{}, member)))
	}
}

func (s *server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		if !memberFrom(r.Context()).IsAdmin {
			writeError(w, http.StatusForbidden, "Only the app's admin can do that.")
			return
		}
		next(w, r)
	})
}

// --- API ---

type accountJSON struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type memberJSON struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
	// SignsInWithSpotify and SignsInWithApple say how this member gets in.
	SignsInWithSpotify bool `json:"signsInWithSpotify"`
	SignsInWithApple   bool `json:"signsInWithApple"`
	// PendingRequests counts people asking to join; only sent to the admin.
	PendingRequests int `json:"pendingRequests,omitempty"`
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Member *memberJSON `json:"member"`
		// Spotify is the member's connected Spotify account, if any.
		Spotify         *accountJSON `json:"spotify"`
		SignInWithApple bool         `json:"signInWithApple"`
	}{SignInWithApple: s.appleID != nil}
	if m, ok := s.currentMember(r); ok {
		resp.Member = &memberJSON{
			ID: m.ID, DisplayName: m.DisplayName, IsAdmin: m.IsAdmin,
			SignsInWithSpotify: m.SpotifyUserID != "", SignsInWithApple: m.AppleSub != "",
		}
		if m.IsAdmin {
			if reqs, err := s.store.ListAccessRequests(r.Context()); err == nil {
				resp.Member.PendingRequests = len(reqs)
			}
		}
		if m.SpotifyUserID != "" {
			if acct, err := s.store.GetSpotifyAccount(r.Context(), m.SpotifyUserID); err == nil {
				resp.Spotify = &accountJSON{ID: acct.UserID, DisplayName: acct.DisplayName}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// memberAccountJSON names a member for display, e.g. who connected Apple Music.
func (s *server) memberAccountJSON(r *http.Request, id int64) *accountJSON {
	if m, err := s.store.GetMember(r.Context(), id); err == nil {
		return &accountJSON{ID: strconv.FormatInt(m.ID, 10), DisplayName: m.DisplayName}
	}
	return &accountJSON{ID: strconv.FormatInt(id, 10), DisplayName: "a former member"}
}

func (s *server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListSpotifyAccounts(r.Context())
	if err != nil {
		slog.Error("list spotify accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load connected accounts.")
		return
	}
	out := make([]accountJSON, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountJSON{ID: a.UserID, DisplayName: a.DisplayName})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleReadPlaylist(w http.ResponseWriter, r *http.Request) {
	playlistID, ok := decodeLink(w, r)
	if !ok {
		return
	}
	result, err := s.readPlaylist(r.Context(), memberFrom(r.Context()).SpotifyUserID, playlistID)
	if err != nil {
		s.writeSpotifyError(w, playlistID, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// userError is an error whose message is safe and useful to show in the UI.
type userError struct {
	status  int
	message string
}

func (e *userError) Error() string { return e.message }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// spaHandler serves the built frontend, falling back to index.html for client-side routes.
func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.FromSlash(filepath.Clean("/"+r.URL.Path)))
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}
