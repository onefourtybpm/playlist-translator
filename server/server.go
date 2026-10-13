package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

const (
	sessionCookie    = "pt_session"
	oauthStateCookie = "pt_oauth_state"
	sessionTTL       = 90 * 24 * time.Hour
)

type server struct {
	cfg     config
	store   *store.Store
	spotify *spotify.Client

	// appleTokens is nil when Apple Music isn't configured.
	appleTokens *apple.TokenSigner

	// refreshMu serializes token refreshes so two requests don't refresh the same account at once.
	refreshMu sync.Mutex
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /auth/spotify/login", s.handleSpotifyLogin)
	mux.HandleFunc("GET /callback", s.handleSpotifyCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/spotify/accounts", s.requireSession(s.handleListAccounts))
	mux.HandleFunc("DELETE /api/spotify/account", s.requireSession(s.handleDisconnect))
	mux.HandleFunc("POST /api/spotify/playlist", s.requireSession(s.handleReadPlaylist))
	mux.HandleFunc("GET /api/apple/developer-token", s.requireSession(s.handleAppleDeveloperToken))
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
	session, err := s.store.CreateSession(ctx, me.ID, sessionTTL)
	if err != nil {
		slog.Error("create session", "err", err)
		s.redirectToApp(w, r, "Couldn't sign you in.")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, session, sessionTTL))
	slog.Info("spotify account connected", "user", me.ID)
	s.redirectToApp(w, r, "")
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleDisconnect deletes the signed-in user's stored Spotify tokens, as Spotify's developer
// policy requires when a user disconnects.
func (s *server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	userID := sessionFrom(r.Context()).SpotifyUserID
	if err := s.store.DeleteSpotifyAccount(r.Context(), userID); err != nil {
		slog.Error("delete spotify account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't disconnect your account.")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
	slog.Info("spotify account disconnected", "user", userID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) redirectToApp(w http.ResponseWriter, r *http.Request, errMsg string) {
	target := s.cfg.AppURL + "/"
	if errMsg != "" {
		target += "?error=" + url.QueryEscape(errMsg)
	}
	http.Redirect(w, r, target, http.StatusFound)
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

type sessionKey struct{}

func sessionFrom(ctx context.Context) store.Session {
	sess, _ := ctx.Value(sessionKey{}).(store.Session)
	return sess
}

func (s *server) currentSession(r *http.Request) (store.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.Session{}, false
	}
	sess, err := s.store.GetSession(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("load session", "err", err)
		}
		return store.Session{}, false
	}
	return sess, true
}

func (s *server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "Please connect your Spotify account first.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	}
}

// --- API ---

type accountJSON struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Spotify *accountJSON `json:"spotify"`
	}{}
	if sess, ok := s.currentSession(r); ok {
		acct, err := s.store.GetSpotifyAccount(r.Context(), sess.SpotifyUserID)
		if err == nil {
			resp.Spotify = &accountJSON{ID: acct.UserID, DisplayName: acct.DisplayName}
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	var req struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Send a playlist link.")
		return
	}
	playlistID, err := spotify.ParsePlaylistID(req.Link)
	if err != nil {
		writeError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}

	result, err := s.readPlaylist(r.Context(), sessionFrom(r.Context()).SpotifyUserID, playlistID)
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			writeError(w, ue.status, ue.message)
			return
		}
		slog.Error("read playlist", "playlist", playlistID, "err", err)
		writeError(w, http.StatusBadGateway, "Something went wrong talking to Spotify. Please try again.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAppleDeveloperToken gives the browser a developer token so MusicKit JS can ask the user
// to connect their Apple Music account.
func (s *server) handleAppleDeveloperToken(w http.ResponseWriter, r *http.Request) {
	if s.appleTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "Apple Music isn't set up on this server.")
		return
	}
	token, err := s.appleTokens.Token()
	if err != nil {
		slog.Error("sign apple developer token", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't start connecting to Apple Music.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
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
