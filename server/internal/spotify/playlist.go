package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	ErrInvalidPlaylistLink = errors.New("that doesn't look like a Spotify playlist link")
	ErrShortLink           = errors.New("spotify.link short links aren't supported yet; open the link and copy the open.spotify.com address instead")

	playlistIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
)

// ParsePlaylistID accepts the forms people actually paste: open.spotify.com links (with or without
// a locale prefix like /intl-de/ or a ?si= share parameter), spotify:playlist: URIs, and bare IDs.
func ParsePlaylistID(input string) (string, error) {
	s := strings.TrimSpace(input)

	if rest, ok := strings.CutPrefix(s, "spotify:playlist:"); ok {
		return validID(rest)
	}
	if playlistIDPattern.MatchString(s) {
		return s, nil
	}

	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ErrInvalidPlaylistLink
	}
	switch strings.ToLower(u.Hostname()) {
	case "spotify.link":
		return "", ErrShortLink
	case "open.spotify.com", "play.spotify.com":
	default:
		return "", ErrInvalidPlaylistLink
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range segments {
		if seg == "playlist" && i+1 < len(segments) {
			return validID(segments[i+1])
		}
	}
	return "", ErrInvalidPlaylistLink
}

func validID(id string) (string, error) {
	if !playlistIDPattern.MatchString(id) {
		return "", ErrInvalidPlaylistLink
	}
	return id, nil
}

type Playlist struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Collaborative bool   `json:"collaborative"`
	Owner         User   `json:"owner"`
	ImageURL      string `json:"imageUrl"`
	URL           string `json:"url"`
	Total         int    `json:"total"`
}

// GetPlaylist returns playlist metadata, including the owner. Spotify returns metadata for any
// playlist the token can see, even when it won't return the songs.
func (c *Client) GetPlaylist(ctx context.Context, accessToken, playlistID string) (Playlist, error) {
	var raw struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		Collaborative bool   `json:"collaborative"`
		Owner         User   `json:"owner"`
		ExternalURLs  struct {
			Spotify string `json:"spotify"`
		} `json:"external_urls"`
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
		// Renamed from "tracks" to "items" in February 2026; accept either.
		Items  *struct{ Total int } `json:"items"`
		Tracks *struct{ Total int } `json:"tracks"`
	}
	if err := c.get(ctx, accessToken, "/playlists/"+url.PathEscape(playlistID), &raw); err != nil {
		return Playlist{}, err
	}

	p := Playlist{
		ID:            raw.ID,
		Name:          raw.Name,
		Description:   raw.Description,
		Collaborative: raw.Collaborative,
		Owner:         raw.Owner,
		URL:           raw.ExternalURLs.Spotify,
	}
	if p.Owner.DisplayName == "" {
		p.Owner.DisplayName = p.Owner.ID
	}
	if len(raw.Images) > 0 {
		p.ImageURL = raw.Images[0].URL
	}
	switch {
	case raw.Items != nil:
		p.Total = raw.Items.Total
	case raw.Tracks != nil:
		p.Total = raw.Tracks.Total
	}
	return p, nil
}

type Track struct {
	Position   int      `json:"position"`
	SpotifyID  string   `json:"spotifyId"`
	Name       string   `json:"name"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	DurationMS int      `json:"durationMs"`
	Explicit   bool     `json:"explicit"`
	ISRC       string   `json:"isrc"`
}

// SkippedItem is a playlist entry that can't be translated, such as a local file or podcast episode.
type SkippedItem struct {
	Position int    `json:"position"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// GetPlaylistTracks reads every entry in the playlist. Since February 2026 this only works when the
// token belongs to the playlist's owner or a collaborator; otherwise Spotify responds 403.
func (c *Client) GetPlaylistTracks(ctx context.Context, accessToken, playlistID string) ([]Track, []SkippedItem, error) {
	var tracks []Track
	var skipped []SkippedItem

	next := "/playlists/" + url.PathEscape(playlistID) + "/items?limit=50&additional_types=track,episode"
	position := 0
	for next != "" {
		var page struct {
			Items []struct {
				IsLocal bool            `json:"is_local"`
				Item    json.RawMessage `json:"item"`
				Track   json.RawMessage `json:"track"` // pre-February 2026 field name
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := c.get(ctx, accessToken, next, &page); err != nil {
			return nil, nil, err
		}

		for _, entry := range page.Items {
			position++
			obj := entry.Item
			if isNull(obj) {
				obj = entry.Track
			}
			if isNull(obj) {
				skipped = append(skipped, SkippedItem{Position: position, Reason: "No longer available on Spotify"})
				continue
			}

			var t struct {
				Type       string `json:"type"`
				ID         string `json:"id"`
				Name       string `json:"name"`
				IsLocal    bool   `json:"is_local"`
				DurationMS int    `json:"duration_ms"`
				Explicit   bool   `json:"explicit"`
				Artists    []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name string `json:"name"`
				} `json:"album"`
				ExternalIDs struct {
					ISRC string `json:"isrc"`
				} `json:"external_ids"`
			}
			if err := json.Unmarshal(obj, &t); err != nil {
				return nil, nil, fmt.Errorf("spotify: decode playlist item %d: %w", position, err)
			}

			switch {
			case entry.IsLocal || t.IsLocal:
				skipped = append(skipped, SkippedItem{Position: position, Name: t.Name, Reason: "Local file (not in Spotify's catalog)"})
			case t.Type == "episode":
				skipped = append(skipped, SkippedItem{Position: position, Name: t.Name, Reason: "Podcast episode"})
			default:
				artists := make([]string, 0, len(t.Artists))
				for _, a := range t.Artists {
					artists = append(artists, a.Name)
				}
				tracks = append(tracks, Track{
					Position:   position,
					SpotifyID:  t.ID,
					Name:       t.Name,
					Artists:    artists,
					Album:      t.Album.Name,
					DurationMS: t.DurationMS,
					Explicit:   t.Explicit,
					ISRC:       strings.ToUpper(t.ExternalIDs.ISRC),
				})
			}
		}
		next = page.Next
	}
	return tracks, skipped, nil
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}
