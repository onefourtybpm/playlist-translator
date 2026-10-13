package apple

import (
	"bytes"
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
	defaultAPIURL = "https://api.music.apple.com"

	// isrcBatchSize is how many ISRCs go in one catalog lookup.
	isrcBatchSize = 25
	// addTracksBatchSize is how many songs go in one create or add-tracks request.
	addTracksBatchSize = 100

	maxRetries   = 3
	maxRetryWait = 30 * time.Second
)

// Client calls the Apple Music API. Catalog requests need only the developer token; library
// requests also need a music user token, which MusicKit JS gets when a user connects.
type Client struct {
	Tokens *TokenSigner
	HTTP   *http.Client
	APIURL string // overridable in tests
}

func NewClient(tokens *TokenSigner) *Client {
	return &Client{
		Tokens: tokens,
		HTTP:   &http.Client{Timeout: 20 * time.Second},
		APIURL: defaultAPIURL,
	}
}

// APIError is a non-success response from Apple.
type APIError struct {
	Status int
	Code   string
	Detail string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("apple music: %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// IsUserTokenRejected reports whether Apple refused the music user token, meaning the user
// has to connect again (it lasts about six months, and the user can revoke it).
func IsUserTokenRejected(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// Song is a catalog song, with the details needed to pick the best match for a Spotify track.
type Song struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Artist        string `json:"artist"`
	Album         string `json:"album"`
	DurationMS    int    `json:"durationMs"`
	Explicit      bool   `json:"explicit"`
	ISRC          string `json:"isrc"`
	URL           string `json:"url"`
	IsCompilation bool   `json:"-"`
	IsSingle      bool   `json:"-"`
}

type songResource struct {
	ID         string `json:"id"`
	Attributes struct {
		Name             string `json:"name"`
		ArtistName       string `json:"artistName"`
		AlbumName        string `json:"albumName"`
		DurationInMillis int    `json:"durationInMillis"`
		ContentRating    string `json:"contentRating"`
		ISRC             string `json:"isrc"`
		URL              string `json:"url"`
	} `json:"attributes"`
	Relationships struct {
		Albums struct {
			Data []struct {
				Attributes struct {
					IsCompilation bool `json:"isCompilation"`
					IsSingle      bool `json:"isSingle"`
				} `json:"attributes"`
			} `json:"data"`
		} `json:"albums"`
	} `json:"relationships"`
}

func (r songResource) song() Song {
	a := r.Attributes
	s := Song{
		ID:         r.ID,
		Name:       a.Name,
		Artist:     a.ArtistName,
		Album:      a.AlbumName,
		DurationMS: a.DurationInMillis,
		Explicit:   a.ContentRating == "explicit",
		ISRC:       a.ISRC,
		URL:        a.URL,
	}
	if albums := r.Relationships.Albums.Data; len(albums) > 0 {
		s.IsCompilation = albums[0].Attributes.IsCompilation
		s.IsSingle = albums[0].Attributes.IsSingle
	}
	return s
}

// SongsByISRC looks up catalog songs by ISRC. One recording is often on several releases
// (the original album, a single, compilations), so each ISRC can map to several songs.
func (c *Client) SongsByISRC(ctx context.Context, storefront string, isrcs []string) (map[string][]Song, error) {
	found := make(map[string][]Song)
	for start := 0; start < len(isrcs); start += isrcBatchSize {
		batch := isrcs[start:min(start+isrcBatchSize, len(isrcs))]
		q := url.Values{"filter[isrc]": {strings.Join(batch, ",")}, "include": {"albums"}}
		var resp struct {
			Data []songResource `json:"data"`
			Meta struct {
				Filters struct {
					ISRC map[string][]struct {
						ID string `json:"id"`
					} `json:"isrc"`
				} `json:"filters"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, catalogPath(storefront, "/songs")+"?"+q.Encode(), "", nil, &resp); err != nil {
			return nil, err
		}
		byID := make(map[string]Song, len(resp.Data))
		for _, r := range resp.Data {
			byID[r.ID] = r.song()
		}
		for isrc, refs := range resp.Meta.Filters.ISRC {
			for _, ref := range refs {
				if s, ok := byID[ref.ID]; ok {
					found[strings.ToUpper(isrc)] = append(found[strings.ToUpper(isrc)], s)
				}
			}
		}
	}
	return found, nil
}

// SearchSongs runs a catalog search for songs.
func (c *Client) SearchSongs(ctx context.Context, storefront, term string, limit int) ([]Song, error) {
	q := url.Values{"term": {term}, "types": {"songs"}, "limit": {strconv.Itoa(limit)}, "include[songs]": {"albums"}}
	var resp struct {
		Results struct {
			Songs struct {
				Data []songResource `json:"data"`
			} `json:"songs"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, catalogPath(storefront, "/search")+"?"+q.Encode(), "", nil, &resp); err != nil {
		return nil, err
	}
	songs := make([]Song, 0, len(resp.Results.Songs.Data))
	for _, r := range resp.Results.Songs.Data {
		songs = append(songs, r.song())
	}
	return songs, nil
}

// Storefront returns the user's Apple Music country code (such as "us"), which catalog
// lookups should use so matches are songs the user can actually play.
func (c *Client) Storefront(ctx context.Context, userToken string) (string, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/me/storefront", userToken, nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Data) == 0 {
		return "", errors.New("apple music: storefront response was empty")
	}
	return resp.Data[0].ID, nil
}

// CreateFolder creates a playlist folder at the top of the user's library.
func (c *Client) CreateFolder(ctx context.Context, userToken, name string) (string, error) {
	body := map[string]any{"attributes": map[string]string{"name": name}}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/me/library/playlist-folders", userToken, body, &resp); err != nil {
		return "", err
	}
	if len(resp.Data) == 0 {
		return "", errors.New("apple music: folder created but no ID returned")
	}
	return resp.Data[0].ID, nil
}

// LibraryPlaylist is a playlist in the user's library. GlobalID (pl.u-…) is set for public
// playlists and is what share links use.
type LibraryPlaylist struct {
	ID       string
	GlobalID string
}

// ShareURL is the public link for a public playlist.
func (p LibraryPlaylist) ShareURL() string {
	if p.GlobalID == "" {
		return ""
	}
	return "https://music.apple.com/playlist/" + p.GlobalID
}

type libraryPlaylistResource struct {
	ID         string `json:"id"`
	Attributes struct {
		PlayParams struct {
			GlobalID string `json:"globalId"`
		} `json:"playParams"`
	} `json:"attributes"`
}

// NewPlaylist describes a library playlist to create.
type NewPlaylist struct {
	Name        string
	Description string
	Public      bool
	FolderID    string   // optional
	SongIDs     []string // catalog song IDs, in order
}

// CreatePlaylist creates a library playlist with the given songs, adding them in batches.
func (c *Client) CreatePlaylist(ctx context.Context, userToken string, p NewPlaylist) (LibraryPlaylist, error) {
	first := p.SongIDs[:min(addTracksBatchSize, len(p.SongIDs))]
	relationships := map[string]any{"tracks": map[string]any{"data": songRefs(first)}}
	if p.FolderID != "" {
		relationships["parent"] = map[string]any{"data": []map[string]string{{"id": p.FolderID, "type": "library-playlist-folders"}}}
	}
	body := map[string]any{
		"attributes":    map[string]any{"name": p.Name, "description": p.Description, "isPublic": p.Public},
		"relationships": relationships,
	}
	var resp struct {
		Data []libraryPlaylistResource `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/me/library/playlists", userToken, body, &resp); err != nil {
		return LibraryPlaylist{}, err
	}
	if len(resp.Data) == 0 {
		return LibraryPlaylist{}, errors.New("apple music: playlist created but no ID returned")
	}
	pl := LibraryPlaylist{ID: resp.Data[0].ID, GlobalID: resp.Data[0].Attributes.PlayParams.GlobalID}

	for start := len(first); start < len(p.SongIDs); start += addTracksBatchSize {
		batch := p.SongIDs[start:min(start+addTracksBatchSize, len(p.SongIDs))]
		path := "/v1/me/library/playlists/" + url.PathEscape(pl.ID) + "/tracks"
		if err := c.do(ctx, http.MethodPost, path, userToken, map[string]any{"data": songRefs(batch)}, nil); err != nil {
			return pl, fmt.Errorf("add songs %d–%d: %w", start+1, start+len(batch), err)
		}
	}
	return pl, nil
}

// GetLibraryPlaylist reads a library playlist, mainly to pick up a GlobalID that wasn't in the
// create response.
func (c *Client) GetLibraryPlaylist(ctx context.Context, userToken, id string) (LibraryPlaylist, error) {
	var resp struct {
		Data []libraryPlaylistResource `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/me/library/playlists/"+url.PathEscape(id), userToken, nil, &resp); err != nil {
		return LibraryPlaylist{}, err
	}
	if len(resp.Data) == 0 {
		return LibraryPlaylist{}, &APIError{Status: http.StatusNotFound}
	}
	return LibraryPlaylist{ID: resp.Data[0].ID, GlobalID: resp.Data[0].Attributes.PlayParams.GlobalID}, nil
}

func songRefs(ids []string) []map[string]string {
	refs := make([]map[string]string, len(ids))
	for i, id := range ids {
		refs[i] = map[string]string{"id": id, "type": "songs"}
	}
	return refs
}

func catalogPath(storefront, rest string) string {
	return "/v1/catalog/" + url.PathEscape(storefront) + rest
}

// do sends a request with the developer token (and the music user token, if given), retrying
// when Apple rate-limits it. out may be nil for responses without a body.
func (c *Client) do(ctx context.Context, method, path, userToken string, body, out any) error {
	devToken, err := c.Tokens.Token()
	if err != nil {
		return err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.APIURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+devToken)
		if userToken != "" {
			req.Header.Set("Music-User-Token", userToken)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			if wait > maxRetryWait {
				return parseError(resp.StatusCode, respBody)
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return parseError(resp.StatusCode, respBody)
		}
		if out == nil || len(respBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("apple music: decode %s: %w", req.URL.Path, err)
		}
		return nil
	}
}

// retryAfter honors Apple's Retry-After header when present, otherwise backs off 1s, 2s, 4s.
func retryAfter(header string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return time.Second << attempt
}

// parseError reads Apple's {"errors":[{"code":..,"title":..,"detail":..}]} error shape.
func parseError(status int, body []byte) error {
	var shape struct {
		Errors []struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(body, &shape) == nil && len(shape.Errors) > 0 {
		e.Code = shape.Errors[0].Code
		e.Detail = shape.Errors[0].Detail
		if e.Detail == "" {
			e.Detail = shape.Errors[0].Title
		}
	}
	return e
}
