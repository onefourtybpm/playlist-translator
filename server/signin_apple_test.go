package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/appleid"
)

// fakeAppleID plays appleid.apple.com: each person who signs in is an Apple Account with a
// subject and an email.
type fakeAppleID struct {
	key      *rsa.PrivateKey
	accounts map[string][2]string // authorization code → {sub, email}
	nonces   map[string]string    // authorization code → nonce from the authorize request
}

func withAppleSignIn(t *testing.T, srv *server) *fakeAppleID {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAppleID{key: rsaKey, accounts: map[string][2]string{}, nonces: map[string]string{}}
	b64 := base64.RawURLEncoding.EncodeToString
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes()),
			}}})
		case "/auth/token":
			code := r.FormValue("code")
			acct, ok := f.accounts[code]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
			c, _ := json.Marshal(map[string]any{
				"iss": "https://appleid.apple.com", "aud": "com.example.web", "sub": acct[0], "email": acct[1],
				"email_verified": "true", "exp": time.Now().Add(time.Minute).Unix(), "nonce": f.nonces[code],
			})
			input := b64(h) + "." + b64(c)
			digest := sha256.Sum256([]byte(input))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
			fmt.Fprintf(w, `{"id_token":%q}`, input+"."+b64(sig))
		}
	}))
	t.Cleanup(ts.Close)

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	client := appleid.New("TEAM", "com.example.web", "K", ecKey, "https://app.example/auth/apple/callback")
	client.AuthURL = "https://appleid.example/auth/authorize"
	client.TokenURL = ts.URL + "/auth/token"
	client.KeysURL = ts.URL + "/auth/keys"
	srv.appleID = client
	srv.cfg.BaseURL = "https://app.example"
	srv.cfg.AppURL = "https://app.example"
	return f
}

// signInWithApple runs the whole browser flow as the Apple Account (sub, email) and returns where
// the browser ended up and its new session cookie, if any. sessionToken, if set, is the browser's
// existing session.
func (f *fakeAppleID) signInWithApple(t *testing.T, srv *server, sub, email, user, sessionToken string) (string, string) {
	t.Helper()
	handler := srv.routes()
	withSession := func(req *http.Request) *http.Request {
		if sessionToken != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
		}
		return req
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, withSession(httptest.NewRequest("GET", "/auth/apple/login", nil)))
	authURL, _ := url.Parse(rec.Header().Get("Location"))
	state, nonce := authURL.Query().Get("state"), authURL.Query().Get("nonce")
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == appleStateCookie {
			stateCookie = c
		}
	}
	if state == "" || stateCookie == nil || stateCookie.Value != state {
		t.Fatalf("login: redirect %q, state cookie %+v", authURL, stateCookie)
	}

	// Apple's cross-site form POST: no cookies.
	code := "code-" + sub + "-" + state[:6]
	f.accounts[code] = [2]string{sub, email}
	f.nonces[code] = nonce
	form := url.Values{"code": {code}, "state": {state}, "user": {user}}
	req := httptest.NewRequest("POST", "/auth/apple/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://app.example/auth/apple/finish?") {
		t.Fatalf("callback: %d → %q", rec.Code, rec.Header().Get("Location"))
	}

	finish, _ := url.Parse(rec.Header().Get("Location"))
	req = withSession(httptest.NewRequest("GET", finish.RequestURI(), nil))
	req.AddCookie(stateCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return rec.Header().Get("Location"), c.Value
		}
	}
	return rec.Header().Get("Location"), ""
}

func apiAs(t *testing.T, srv *server, sessionToken, method, path, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code
}

type meResponse struct {
	Member  *memberJSON
	Spotify *accountJSON
}

func TestSignInWithAppleAllowlist(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("admin", time.Hour), account("bob", time.Hour))
	fake := withAppleSignIn(t, srv)
	admin := sessionFor(t, srv, "admin")
	bob := sessionFor(t, srv, "bob")

	// Only the admin manages the allowlist.
	if code := apiAs(t, srv, bob, "POST", "/api/admin/allowed-emails", `{"email":"x@y.z","displayName":"X"}`, nil); code != http.StatusForbidden {
		t.Errorf("non-admin adding an email: %d", code)
	}
	if code := apiAs(t, srv, admin, "POST", "/api/admin/allowed-emails", `{"email":"Friend@iCloud.com","displayName":"Friend"}`, nil); code != http.StatusOK {
		t.Fatalf("allow email: %d", code)
	}

	// The friend signs in with the allowed email (Apple may change its case) and becomes a member.
	where, session := fake.signInWithApple(t, srv, "sub-friend", "friend@icloud.com", "", "")
	if session == "" || where != "https://app.example/" {
		t.Fatalf("friend sign-in: → %q, session %q", where, session)
	}
	var me meResponse
	apiAs(t, srv, session, "GET", "/api/me", "", &me)
	if me.Member == nil || me.Member.DisplayName != "Friend" || me.Member.IsAdmin || me.Spotify != nil {
		t.Errorf("friend /api/me = %+v", me)
	}
	// Members who didn't connect Spotify can still use the translator.
	var accounts []accountJSON
	if code := apiAs(t, srv, session, "GET", "/api/spotify/accounts", "", &accounts); code != http.StatusOK || len(accounts) != 2 {
		t.Errorf("accounts as friend: %d %+v", code, accounts)
	}

	// Next time, Apple's stable ID is enough, even if the email changed.
	if _, again := fake.signInWithApple(t, srv, "sub-friend", "new@example.com", "", ""); again == "" {
		t.Error("returning friend wasn't signed in")
	}
	var view struct {
		Members       []adminMemberJSON
		AllowedEmails []allowedEmailJSON
	}
	apiAs(t, srv, admin, "GET", "/api/admin", "", &view)
	if len(view.Members) != 3 || len(view.AllowedEmails) != 0 {
		t.Errorf("admin view = %+v", view)
	}
}

func TestSignInWithAppleRequestsAccess(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("admin", time.Hour))
	fake := withAppleSignIn(t, srv)
	admin := sessionFor(t, srv, "admin")

	// Hide My Email: the relay address isn't on any list, so it becomes a request.
	where, session := fake.signInWithApple(t, srv, "sub-alex", "x7f@privaterelay.appleid.com",
		`{"name":{"firstName":"Alex","lastName":"P"}}`, "")
	if session != "" || !strings.Contains(where, "notice=") {
		t.Fatalf("unknown email: → %q, session %q", where, session)
	}
	var view struct {
		Requests []accessRequestJSON
	}
	apiAs(t, srv, admin, "GET", "/api/admin", "", &view)
	if len(view.Requests) != 1 || view.Requests[0].DisplayName != "Alex P" {
		t.Fatalf("requests = %+v", view.Requests)
	}

	path := fmt.Sprintf("/api/admin/requests/%d/approve", view.Requests[0].ID)
	if code := apiAs(t, srv, admin, "POST", path, `{"displayName":"Alex"}`, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	if _, session = fake.signInWithApple(t, srv, "sub-alex", "x7f@privaterelay.appleid.com", "", ""); session == "" {
		t.Fatal("approved member wasn't signed in")
	}
	var me meResponse
	apiAs(t, srv, session, "GET", "/api/me", "", &me)
	if me.Member == nil || me.Member.DisplayName != "Alex" {
		t.Errorf("/api/me = %+v", me)
	}

	// The admin can remove them again, which signs them out.
	if code := apiAs(t, srv, admin, "DELETE", fmt.Sprintf("/api/admin/members/%d", me.Member.ID), "", nil); code != http.StatusOK {
		t.Fatalf("remove: %d", code)
	}
	if code := apiAs(t, srv, session, "GET", "/api/apple", "", nil); code != http.StatusUnauthorized {
		t.Errorf("removed member's session: %d", code)
	}
	if code := apiAs(t, srv, admin, "DELETE", fmt.Sprintf("/api/admin/members/%d", memberID(t, srv, "admin")), "", nil); code != http.StatusBadRequest {
		t.Errorf("removing the admin: %d", code)
	}
}

func TestSignInWithAppleLinksSignedInMember(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("bob", time.Hour))
	fake := withAppleSignIn(t, srv)
	bob := sessionFor(t, srv, "bob")

	// Bob, signed in with Spotify, adds Sign in with Apple; no allowlist needed.
	if _, session := fake.signInWithApple(t, srv, "sub-bob", "bob@example.com", "", bob); session == "" {
		t.Fatal("linking didn't sign in")
	}
	m, err := srv.store.MemberByAppleSub(context.Background(), "sub-bob")
	if err != nil || m.SpotifyUserID != "bob" {
		t.Errorf("member by apple = %+v, %v", m, err)
	}
}

func TestSignInWithAppleRejectsForeignState(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil))
	withAppleSignIn(t, srv)
	handler := srv.routes()

	// An attacker's own state, finished in someone else's browser (no matching state cookie).
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/apple/login", nil))
	authURL, _ := url.Parse(rec.Header().Get("Location"))
	state := authURL.Query().Get("state")
	form := url.Values{"code": {"c"}, "state": {state}}
	req := httptest.NewRequest("POST", "/auth/apple/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/apple/finish?state="+url.QueryEscape(state), nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("finish without state cookie → %q", loc)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("signed in without the state cookie")
		}
	}

	// An unknown state at the callback.
	req = httptest.NewRequest("POST", "/auth/apple/callback", strings.NewReader("code=c&state=made-up"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("unknown state → %q", loc)
	}
}

func TestSpotifySignInLinksAppleMember(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("admin", time.Hour))
	ctx := context.Background()
	friend, err := srv.store.CreateMember(ctx, storeMember("Friend", "", "sub-friend"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := srv.store.CreateSession(ctx, friend.ID, time.Hour)

	req := httptest.NewRequest("GET", "/callback", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	m, err := srv.spotifyMember(req, "friend-spotify", "Friend S")
	if err != nil || m.ID != friend.ID || m.SpotifyUserID != "friend-spotify" {
		t.Fatalf("spotifyMember() = %+v, %v", m, err)
	}

	// Someone else's Spotify account can't be attached to the friend's membership.
	if _, err := srv.spotifyMember(req, "admin", "Admin"); err == nil {
		t.Error("attaching another member's Spotify account: want error")
	}

	// Without a session, a new Spotify account becomes a new (non-admin) member.
	m, err = srv.spotifyMember(httptest.NewRequest("GET", "/callback", nil), "carol", "Carol")
	if err != nil || m.DisplayName != "Carol" || m.IsAdmin {
		t.Errorf("new member = %+v, %v", m, err)
	}
}

func TestAppleMemberCanSwitchAppleMusic(t *testing.T) {
	srv := newTestServer(t, newFakeSpotify(t, nil), account("admin", time.Hour), account("bob", time.Hour))
	_, srv.apple = newFakeApple(t)
	ctx := context.Background()
	if code := call(t, srv, "admin", "PUT", "/api/apple/connection", `{"musicUserToken":"good"}`, nil); code != http.StatusOK {
		t.Fatalf("admin connects: %d", code)
	}
	friend, _ := srv.store.CreateMember(ctx, storeMember("Friend", "", "sub-friend"))
	friendSession, _ := srv.store.CreateSession(ctx, friend.ID, time.Hour)

	// Bob signs in with Spotify only: he can't take over the connection.
	var status appleStatusJSON
	if call(t, srv, "bob", "GET", "/api/apple", "", &status); status.CanManage || status.CanSwitch {
		t.Errorf("bob: %+v", status)
	}
	if code := call(t, srv, "bob", "PUT", "/api/apple/connection", `{"musicUserToken":"friend"}`, nil); code != http.StatusForbidden {
		t.Errorf("bob switching: %d", code)
	}

	// The friend signs in with Apple, so they may switch it to their own Apple Music.
	if apiAs(t, srv, friendSession, "GET", "/api/apple", "", &status); status.CanManage || !status.CanSwitch {
		t.Fatalf("friend before switching: %+v", status)
	}
	if code := apiAs(t, srv, friendSession, "PUT", "/api/apple/connection", `{"musicUserToken":"friend"}`, &status); code != http.StatusOK {
		t.Fatalf("friend switching: %d", code)
	}
	if !status.CanManage || status.CanSwitch || status.ConnectedBy.DisplayName != "Friend" {
		t.Errorf("friend after switching: %+v", status)
	}
	// Now the admin, who signs in with Spotify, can't change it back.
	if code := call(t, srv, "admin", "DELETE", "/api/apple/connection", "", nil); code != http.StatusForbidden {
		t.Errorf("admin disconnecting the friend's account: %d", code)
	}
}
