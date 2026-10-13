package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/appleid"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

// Sign in with Apple takes three steps, because Apple returns the result as a cross-site form
// POST and browsers don't send our SameSite=Lax cookies on those:
//
//  1. GET /auth/apple/login remembers a state and nonce, sets the state cookie, and sends the
//     browser to Apple.
//  2. Apple POSTs the authorization code to /auth/apple/callback. We note it against the state
//     and redirect (303) to step 3.
//  3. GET /auth/apple/finish is a top-level GET, so the browser now sends our cookies: we check
//     the state cookie, exchange the code with Apple, and sign the person in.

const appleSignInTTL = 10 * time.Minute

type appleSignIn struct {
	nonce   string
	expires time.Time
	code    string
	name    string // from Apple's first-sign-in "user" field, if any
}

func (s *server) handleAppleSignIn(w http.ResponseWriter, r *http.Request) {
	if s.appleID == nil {
		s.redirectToApp(w, r, "Sign in with Apple isn't set up on this server.")
		return
	}
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusInternalServerError, "Couldn't start sign-in.")
		return
	}
	now := time.Now()
	s.appleSignInsMu.Lock()
	if s.appleSignIns == nil {
		s.appleSignIns = map[string]*appleSignIn{}
	}
	for k, v := range s.appleSignIns {
		if now.After(v.expires) {
			delete(s.appleSignIns, k)
		}
	}
	s.appleSignIns[state] = &appleSignIn{nonce: nonce, expires: now.Add(appleSignInTTL)}
	s.appleSignInsMu.Unlock()

	c := s.cookie(appleStateCookie, state, appleSignInTTL)
	c.Path = "/auth/apple"
	http.SetCookie(w, c)
	http.Redirect(w, r, s.appleID.AuthorizeURL(state, nonce), http.StatusFound)
}

func (s *server) handleAppleSignInCallback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.redirectToApp(w, r, "Sign in with Apple sent back something unexpected. Please try again.")
		return
	}
	if e := r.PostFormValue("error"); e != "" {
		msg := "Sign in with Apple failed: " + e
		if e == "user_cancelled_authorize" {
			msg = "Sign in with Apple was cancelled."
		}
		s.redirectToApp(w, r, msg)
		return
	}
	state := r.PostFormValue("state")
	s.appleSignInsMu.Lock()
	pending, ok := s.appleSignIns[state]
	if ok && pending.code == "" && time.Now().Before(pending.expires) {
		pending.code = r.PostFormValue("code")
		pending.name = appleid.ParseFirstSignInName(r.PostFormValue("user"))
	} else {
		ok = false
	}
	s.appleSignInsMu.Unlock()
	if !ok {
		s.redirectToApp(w, r, "Sign-in expired. Please try again.")
		return
	}
	http.Redirect(w, r, s.cfg.BaseURL+"/auth/apple/finish?state="+url.QueryEscape(state), http.StatusSeeOther)
}

func (s *server) handleAppleSignInFinish(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	clear := s.cookie(appleStateCookie, "", -1)
	clear.Path = "/auth/apple"
	http.SetCookie(w, clear)

	s.appleSignInsMu.Lock()
	pending := s.appleSignIns[state]
	delete(s.appleSignIns, state)
	s.appleSignInsMu.Unlock()

	cookie, err := r.Cookie(appleStateCookie)
	if s.appleID == nil || pending == nil || pending.code == "" || err != nil || cookie.Value != state ||
		time.Now().After(pending.expires) {
		s.redirectToApp(w, r, "Sign-in expired or came from somewhere unexpected. Please try again.")
		return
	}

	id, err := s.appleID.Exchange(r.Context(), pending.code, pending.nonce)
	if err != nil {
		slog.Error("sign in with apple", "err", err)
		s.redirectToApp(w, r, "Couldn't finish signing in with Apple. Please try again.")
		return
	}

	member, err := s.appleMember(r, id, pending.name)
	if errors.Is(err, errAccessRequested) {
		slog.Info("access requested", "email", id.Email, "private_email", id.PrivateEmail)
		s.redirectToAppWith(w, r, "notice",
			"Thanks! Your request to join was sent. Once it's approved, sign in with Apple again.")
		return
	}
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			s.redirectToApp(w, r, ue.message)
			return
		}
		slog.Error("find member for apple sign-in", "err", err)
		s.redirectToApp(w, r, "Couldn't sign you in.")
		return
	}
	if err := s.startSession(w, r, member); err != nil {
		slog.Error("create session", "err", err)
		s.redirectToApp(w, r, "Couldn't sign you in.")
		return
	}
	slog.Info("signed in with apple", "member", member.ID)
	s.redirectToApp(w, r, "")
}

var errAccessRequested = errors.New("access requested")

// appleMember finds the member a Sign in with Apple belongs to: whoever signed in with this Apple
// Account before; the signed-in member, who is adding Apple as a way to sign in; or a new member,
// if the admin allowed this email. Anyone else is recorded as an access request for the admin.
func (s *server) appleMember(r *http.Request, id appleid.Identity, name string) (store.Member, error) {
	ctx := r.Context()
	existing, err := s.store.MemberByAppleSub(ctx, id.Subject)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Member{}, err
	}

	if current, ok := s.currentMember(r); ok {
		if current.AppleSub != "" {
			return store.Member{}, &userError{http.StatusConflict,
				"You already sign in with a different Apple Account. Sign out first to use this one."}
		}
		if err := s.store.LinkApple(ctx, current.ID, id.Subject); err != nil {
			return store.Member{}, err
		}
		return s.store.GetMember(ctx, current.ID)
	}

	if id.Email != "" && id.EmailVerified {
		member, err := s.store.ClaimAllowedEmail(ctx, id.Email, id.Subject)
		if err == nil {
			return member, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.Member{}, err
		}
	}

	if name == "" {
		name, _, _ = strings.Cut(id.Email, "@")
	}
	if err := s.store.RequestAccess(ctx, store.AccessRequest{
		AppleSub: id.Subject, Email: id.Email, DisplayName: cleanName(name),
	}); err != nil {
		return store.Member{}, err
	}
	return store.Member{}, errAccessRequested
}

// cleanName tidies a display name that came from a person or a browser.
func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > maxNameLength {
		name = string(r[:maxNameLength])
	}
	return name
}

const maxNameLength = 60
