package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

type playlistResult struct {
	Playlist spotify.Playlist      `json:"playlist"`
	ReadWith accountJSON           `json:"readWith"`
	Tracks   []spotify.Track       `json:"tracks"`
	Skipped  []spotify.SkippedItem `json:"skipped"`
}

// readPlaylist finds a connected account that's allowed to read the playlist's songs and reads them.
// Spotify only returns a playlist's songs to its owner or a collaborator, so we look up the owner
// from the playlist's metadata and use that person's stored token.
func (s *server) readPlaylist(ctx context.Context, sessionUserID, playlistID string) (*playlistResult, error) {
	accounts, err := s.store.ListSpotifyAccounts(ctx)
	if err != nil {
		return nil, err
	}
	// Try the signed-in user first: they're the most likely owner, and the only one who can see
	// their own private playlists.
	for i, a := range accounts {
		if a.UserID == sessionUserID {
			accounts[0], accounts[i] = accounts[i], accounts[0]
			break
		}
	}

	meta, err := s.findPlaylistMetadata(ctx, accounts, playlistID)
	if err != nil {
		return nil, err
	}

	var candidates []store.SpotifyAccount
	for _, a := range accounts {
		if a.UserID == meta.Owner.ID {
			candidates = append(candidates, a)
		}
	}
	// A collaborator can read a collaborative playlist too, even if the owner isn't connected.
	if meta.Collaborative {
		for _, a := range accounts {
			if a.UserID != meta.Owner.ID {
				candidates = append(candidates, a)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, &userError{http.StatusUnprocessableEntity, fmt.Sprintf(
			"%q belongs to %s, who hasn't connected their Spotify account. They need to connect before their playlists can be translated.",
			meta.Name, meta.Owner.DisplayName)}
	}

	for _, a := range candidates {
		token, err := s.accessToken(ctx, a)
		if err != nil {
			slog.Warn("skipping account with unusable token", "user", a.UserID, "err", err)
			continue
		}
		tracks, skipped, err := s.spotify.GetPlaylistTracks(ctx, token, playlistID)
		if spotify.IsStatus(err, http.StatusForbidden) || spotify.IsStatus(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if tracks == nil {
			tracks = []spotify.Track{}
		}
		if skipped == nil {
			skipped = []spotify.SkippedItem{}
		}
		return &playlistResult{
			Playlist: meta,
			ReadWith: accountJSON{ID: a.UserID, DisplayName: a.DisplayName},
			Tracks:   tracks,
			Skipped:  skipped,
		}, nil
	}

	if candidates[0].UserID == meta.Owner.ID {
		return nil, &userError{http.StatusUnprocessableEntity, fmt.Sprintf(
			"Couldn't read %q with %s's Spotify connection. They may need to disconnect and connect again.",
			meta.Name, meta.Owner.DisplayName)}
	}
	return nil, &userError{http.StatusUnprocessableEntity, fmt.Sprintf(
		"None of the connected accounts can read the songs in %q.", meta.Name)}
}

// findPlaylistMetadata looks the playlist up with each connected account in turn. Public playlists
// are visible to everyone, but a private one only to its owner.
func (s *server) findPlaylistMetadata(ctx context.Context, accounts []store.SpotifyAccount, playlistID string) (spotify.Playlist, error) {
	for _, a := range accounts {
		token, err := s.accessToken(ctx, a)
		if err != nil {
			slog.Warn("skipping account with unusable token", "user", a.UserID, "err", err)
			continue
		}
		meta, err := s.spotify.GetPlaylist(ctx, token, playlistID)
		if spotify.IsStatus(err, http.StatusNotFound) || spotify.IsStatus(err, http.StatusForbidden) {
			continue
		}
		return meta, err
	}
	return spotify.Playlist{}, &userError{http.StatusNotFound,
		"Couldn't find that playlist. If it's private, its owner needs to connect their Spotify account."}
}

// accessToken returns a usable access token for the account, refreshing and saving it if it's
// about to expire.
func (s *server) accessToken(ctx context.Context, a store.SpotifyAccount) (string, error) {
	if time.Until(a.ExpiresAt) > time.Minute {
		return a.AccessToken, nil
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	// Another request may have refreshed it while we waited for the lock.
	current, err := s.store.GetSpotifyAccount(ctx, a.UserID)
	if err != nil {
		return "", err
	}
	if time.Until(current.ExpiresAt) > time.Minute {
		return current.AccessToken, nil
	}

	tok, err := s.spotify.Refresh(ctx, current.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token for %s: %w", a.UserID, err)
	}
	if err := s.store.UpdateSpotifyTokens(ctx, a.UserID, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
