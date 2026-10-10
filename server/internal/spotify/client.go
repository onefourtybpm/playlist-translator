// Package spotify is a minimal client for the parts of the Spotify Web API this app uses:
// the authorization-code OAuth flow, the current user's profile, and reading playlists.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAccountsURL = "https://accounts.spotify.com"
	defaultAPIURL      = "https://api.spotify.com/v1"

	maxRetries   = 3
	maxRetryWait = 30 * time.Second
)

// Scopes lets the app read every playlist the user owns or collaborates on, including private ones.
var Scopes = []string{"playlist-read-private", "playlist-read-collaborative"}

type Client struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string

	HTTP        *http.Client
	AccountsURL string // overridable in tests
	APIURL      string // overridable in tests
}

func NewClient(clientID, clientSecret, redirectURI string) *Client {
	return &Client{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		HTTP:         &http.Client{Timeout: 20 * time.Second},
		AccountsURL:  defaultAccountsURL,
		APIURL:       defaultAPIURL,
	}
}

// APIError is a non-success response from Spotify.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("spotify: %d %s", e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func (c *Client) AuthURL(state string) string {
	q := url.Values{
		"client_id":     {c.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {c.RedirectURI},
		"scope":         {strings.Join(Scopes, " ")},
		"state":         {state},
	}
	return c.AccountsURL + "/authorize?" + q.Encode()
}

func (c *Client) ExchangeCode(ctx context.Context, code string) (Token, error) {
	return c.requestToken(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {c.RedirectURI},
	})
}

// Refresh trades a refresh token for a new access token. Spotify only sometimes rotates the
// refresh token, so the old one is kept when the response doesn't include a new one.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	tok, err := c.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err == nil && tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, err
}

func (c *Client) requestToken(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AccountsURL+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.ClientID, c.ClientSecret)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, parseError(resp.StatusCode, body)
	}

	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Token{}, fmt.Errorf("spotify: decode token response: %w", err)
	}
	return Token{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}, nil
}

type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (c *Client) Me(ctx context.Context, accessToken string) (User, error) {
	var u User
	err := c.get(ctx, accessToken, "/me", &u)
	if u.DisplayName == "" {
		u.DisplayName = u.ID
	}
	return u, err
}

// get fetches an API path (or an absolute "next" URL from a paged response) into out,
// waiting and retrying when Spotify rate-limits the request.
func (c *Client) get(ctx context.Context, accessToken, pathOrURL string, out any) error {
	target := pathOrURL
	if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
		target = c.APIURL + target
	}
	// Never send a user's token anywhere but the Spotify API.
	if !strings.HasPrefix(target, c.APIURL+"/") {
		return fmt.Errorf("spotify: refusing to request non-API URL %q", target)
	}

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if wait > maxRetryWait {
				return parseError(resp.StatusCode, body)
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode != http.StatusOK {
			return parseError(resp.StatusCode, body)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("spotify: decode %s: %w", req.URL.Path, err)
		}
		return nil
	}
}

func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 1 {
		return time.Second
	}
	return time.Duration(secs) * time.Second
}

// parseError understands both error shapes Spotify uses: the Web API's
// {"error":{"status":..,"message":..}} and the accounts service's {"error":..,"error_description":..}.
func parseError(status int, body []byte) error {
	var apiShape struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &apiShape) == nil && apiShape.Error.Message != "" {
		return &APIError{Status: status, Message: apiShape.Error.Message}
	}
	var oauthShape struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &oauthShape) == nil && oauthShape.Error != "" {
		msg := oauthShape.Error
		if oauthShape.Description != "" {
			msg += ": " + oauthShape.Description
		}
		return &APIError{Status: status, Message: msg}
	}
	return &APIError{Status: status, Message: http.StatusText(status)}
}
