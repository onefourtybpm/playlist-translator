package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
	"github.com/onefourtybpm/playlist-translator/server/internal/translate"
)

const (
	// appleFolderName is the library folder translated playlists go in.
	appleFolderName = "Playlist Translator"
	// appleTokenLifetime is roughly how long a music user token lasts. Apple doesn't document
	// it; about six months is what developers report.
	appleTokenLifetime = 180 * 24 * time.Hour
	// defaultStorefront is used for previews before an Apple Music account is connected.
	defaultStorefront = "us"
	maxPlaylistSongs  = 5000
)

var catalogSongID = regexp.MustCompile(`^[0-9]{1,20}$`)

// --- Apple Music connection ---

type appleStatusJSON struct {
	Configured  bool         `json:"configured"`
	Connected   bool         `json:"connected"`
	Storefront  string       `json:"storefront,omitempty"`
	ConnectedBy *accountJSON `json:"connectedBy,omitempty"`
	ConnectedAt *time.Time   `json:"connectedAt,omitempty"`
	ReconnectBy *time.Time   `json:"reconnectBy,omitempty"`
	// CanManage is true when the signed-in member may connect, replace, or disconnect the account:
	// anyone while nothing is connected, afterwards only whoever connected it.
	CanManage bool `json:"canManage"`
	// CanSwitch is true when the signed-in member may replace the connected account with their
	// own: members who sign in with Apple can make their Apple Music the one playlists go in.
	CanSwitch bool `json:"canSwitch"`
}

func (s *server) appleStatus(r *http.Request) (appleStatusJSON, error) {
	status := appleStatusJSON{Configured: s.apple != nil}
	acct, err := s.store.GetAppleAccount(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		status.CanManage = status.Configured
		return status, nil
	}
	if err != nil {
		return status, err
	}
	reconnectBy := acct.ConnectedAt.Add(appleTokenLifetime)
	status.Connected = true
	status.Storefront = acct.Storefront
	status.ConnectedBy = s.memberAccountJSON(r, acct.ConnectedBy)
	status.ConnectedAt = &acct.ConnectedAt
	status.ReconnectBy = &reconnectBy
	member := memberFrom(r.Context())
	status.CanManage = status.Configured && acct.ConnectedBy == member.ID
	status.CanSwitch = status.Configured && !status.CanManage && member.AppleSub != ""
	return status, nil
}

func (s *server) handleAppleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.appleStatus(r)
	if err != nil {
		slog.Error("load apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load the Apple Music connection.")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleAppleDeveloperToken gives the browser a developer token so MusicKit JS can ask the user
// to connect their Apple Music account.
func (s *server) handleAppleDeveloperToken(w http.ResponseWriter, r *http.Request) {
	if s.apple == nil {
		writeError(w, http.StatusServiceUnavailable, "Apple Music isn't set up on this server.")
		return
	}
	token, err := s.apple.Tokens.Token()
	if err != nil {
		slog.Error("sign apple developer token", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't start connecting to Apple Music.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleAppleConnect saves the music user token MusicKit JS got in the browser, after checking
// with Apple that it works.
func (s *server) handleAppleConnect(w http.ResponseWriter, r *http.Request) {
	status, err := s.appleStatus(r)
	if err != nil {
		slog.Error("load apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load the Apple Music connection.")
		return
	}
	if !status.Configured {
		writeError(w, http.StatusServiceUnavailable, "Apple Music isn't set up on this server.")
		return
	}
	if !status.CanManage && !status.CanSwitch {
		writeError(w, http.StatusForbidden, fmt.Sprintf("Only %s can change the Apple Music connection.", status.ConnectedBy.DisplayName))
		return
	}
	var req struct {
		MusicUserToken string `json:"musicUserToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil || req.MusicUserToken == "" {
		writeError(w, http.StatusBadRequest, "Missing the Apple Music sign-in token.")
		return
	}

	storefront, err := s.apple.Storefront(r.Context(), req.MusicUserToken)
	if err != nil {
		slog.Error("check apple music user token", "err", err)
		msg := "Couldn't reach Apple Music. Please try again."
		if apple.IsUserTokenRejected(err) {
			msg = "Apple Music didn't accept that sign-in. Please connect again."
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}
	// The saved "Playlist Translator" folder belongs to whichever library it was made in. If the
	// newly connected account is a different one (or the folder was deleted), forget it so the
	// next playlist goes in a fresh folder in this library.
	keepFolder := true
	if prev, err := s.store.GetAppleAccount(r.Context()); err == nil && prev.FolderID != "" {
		exists, err := s.apple.FolderExists(r.Context(), req.MusicUserToken, prev.FolderID)
		if err != nil {
			// Creating a playlist recovers from a missing folder too, so this isn't fatal.
			slog.Warn("check apple playlist folder", "folder", prev.FolderID, "err", err)
		}
		keepFolder = exists || err != nil
	}

	memberID := memberFrom(r.Context()).ID
	err = s.store.SetAppleAccount(r.Context(), store.AppleAccount{
		MusicUserToken: req.MusicUserToken,
		Storefront:     storefront,
		ConnectedBy:    memberID,
		ConnectedAt:    time.Now(),
	})
	if err == nil && !keepFolder {
		err = s.store.SetAppleFolder(r.Context(), "")
	}
	if err != nil {
		slog.Error("save apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't save the Apple Music connection.")
		return
	}
	slog.Info("apple music connected", "by", memberID, "storefront", storefront, "kept_folder", keepFolder)
	s.handleAppleStatus(w, r)
}

func (s *server) handleAppleDisconnect(w http.ResponseWriter, r *http.Request) {
	status, err := s.appleStatus(r)
	if err != nil {
		slog.Error("load apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load the Apple Music connection.")
		return
	}
	if status.Connected && !status.CanManage {
		writeError(w, http.StatusForbidden, fmt.Sprintf("Only %s can change the Apple Music connection.", status.ConnectedBy.DisplayName))
		return
	}
	if err := s.store.DeleteAppleAccount(r.Context()); err != nil {
		slog.Error("delete apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't disconnect Apple Music.")
		return
	}
	slog.Info("apple music disconnected", "by", memberFrom(r.Context()).ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- Translating ---

type translationJSON struct {
	Name      string      `json:"name"`
	ShareURL  string      `json:"shareUrl"`
	SongCount int         `json:"songCount"`
	CreatedBy accountJSON `json:"createdBy"`
	CreatedAt time.Time   `json:"createdAt"`
}

type previewJSON struct {
	Playlist   spotify.Playlist      `json:"playlist"`
	ReadWith   accountJSON           `json:"readWith"`
	Results    []translate.Result    `json:"results"`
	Skipped    []spotify.SkippedItem `json:"skipped"`
	Storefront string                `json:"storefront"`
	Previous   []translationJSON     `json:"previous"`
}

// handlePreview reads a Spotify playlist and matches its songs in Apple Music, without creating
// anything.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.apple == nil {
		writeError(w, http.StatusServiceUnavailable, "Apple Music isn't set up on this server.")
		return
	}
	playlistID, ok := decodeLink(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	result, err := s.readPlaylist(ctx, memberFrom(ctx).SpotifyUserID, playlistID)
	if err != nil {
		s.writeSpotifyError(w, playlistID, err)
		return
	}

	storefront := defaultStorefront
	if acct, err := s.store.GetAppleAccount(ctx); err == nil {
		storefront = acct.Storefront
	}
	results, err := translate.Match(ctx, s.apple, storefront, result.Tracks)
	if err != nil {
		slog.Error("match songs", "playlist", playlistID, "err", err)
		writeError(w, http.StatusBadGateway, "Something went wrong talking to Apple Music. Please try again.")
		return
	}

	previous, err := s.store.TranslationsOf(ctx, playlistID)
	if err != nil {
		slog.Error("list translations", "err", err)
	}
	prevJSON := make([]translationJSON, 0, len(previous))
	for _, t := range previous {
		prevJSON = append(prevJSON, translationJSON{
			Name: t.Name, ShareURL: t.ShareURL, SongCount: t.SongCount,
			CreatedBy: *s.memberAccountJSON(r, t.CreatedBy), CreatedAt: t.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, previewJSON{
		Playlist:   result.Playlist,
		ReadWith:   result.ReadWith,
		Results:    results,
		Skipped:    result.Skipped,
		Storefront: storefront,
		Previous:   prevJSON,
	})
}

// handleCreate creates a public playlist in the connected Apple Music library from the songs
// chosen in the preview, and returns its share link.
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.apple == nil {
		writeError(w, http.StatusServiceUnavailable, "Apple Music isn't set up on this server.")
		return
	}
	var req struct {
		Link    string   `json:"link"`
		SongIDs []string `json:"songIds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Send a playlist link and the songs to add.")
		return
	}
	playlistID, err := spotify.ParsePlaylistID(req.Link)
	if err != nil {
		writeError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if len(req.SongIDs) == 0 || len(req.SongIDs) > maxPlaylistSongs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("A playlist needs between 1 and %d songs.", maxPlaylistSongs))
		return
	}
	for _, id := range req.SongIDs {
		if !catalogSongID.MatchString(id) {
			writeError(w, http.StatusBadRequest, "That song list doesn't look right. Preview the playlist again.")
			return
		}
	}

	ctx := r.Context()
	acct, err := s.store.GetAppleAccount(ctx)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "Apple Music isn't connected yet.")
		return
	}
	if err != nil {
		slog.Error("load apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load the Apple Music connection.")
		return
	}

	// The name comes from Spotify rather than the browser, and reading it also confirms a
	// connected account can see the playlist.
	accounts, err := s.store.ListSpotifyAccounts(ctx)
	if err != nil {
		slog.Error("list spotify accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load connected accounts.")
		return
	}
	meta, err := s.findPlaylistMetadata(ctx, accounts, playlistID)
	if err != nil {
		s.writeSpotifyError(w, playlistID, err)
		return
	}

	// One at a time, so a double-click can't create two playlists or two folders. Reload the
	// account once it's our turn: the request before us may have just created the folder.
	s.createMu.Lock()
	defer s.createMu.Unlock()
	acct, err = s.store.GetAppleAccount(ctx)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "Apple Music isn't connected yet.")
		return
	}
	if err != nil {
		slog.Error("load apple account", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load the Apple Music connection.")
		return
	}

	pl, err := s.createApplePlaylist(r, acct, apple.NewPlaylist{
		Name:        meta.Name,
		Description: fmt.Sprintf("From %s's Spotify playlist. Made with Playlist Translator.", meta.Owner.DisplayName),
		Public:      true,
		SongIDs:     req.SongIDs,
	})
	if err != nil {
		slog.Error("create apple playlist", "playlist", playlistID, "err", err)
		msg := "Something went wrong creating the Apple Music playlist. Please try again."
		if apple.IsUserTokenRejected(err) {
			msg = s.reconnectMessage(r, acct)
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}

	memberID := memberFrom(ctx).ID
	err = s.store.AddTranslation(ctx, store.Translation{
		SpotifyPlaylistID: playlistID,
		Name:              meta.Name,
		ApplePlaylistID:   pl.ID,
		ShareURL:          pl.ShareURL(),
		SongCount:         len(req.SongIDs),
		CreatedBy:         memberID,
		CreatedAt:         time.Now(),
	})
	if err != nil {
		slog.Error("save translation", "err", err)
	}
	slog.Info("apple playlist created", "playlist", playlistID, "apple_playlist", pl.ID, "songs", len(req.SongIDs), "by", memberID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":      meta.Name,
		"shareUrl":  pl.ShareURL(),
		"songCount": len(req.SongIDs),
	})
}

// createApplePlaylist creates the playlist in the "Playlist Translator" folder, creating the
// folder first if needed (or again, if it was deleted), and makes sure it has a share link.
func (s *server) createApplePlaylist(r *http.Request, acct store.AppleAccount, p apple.NewPlaylist) (apple.LibraryPlaylist, error) {
	ctx := r.Context()
	folderID := acct.FolderID
	for attempt := 0; ; attempt++ {
		if folderID == "" {
			id, err := s.apple.CreateFolder(ctx, acct.MusicUserToken, appleFolderName)
			if err != nil {
				return apple.LibraryPlaylist{}, fmt.Errorf("create folder: %w", err)
			}
			if err := s.store.SetAppleFolder(ctx, id); err != nil {
				slog.Error("save apple folder", "err", err)
			}
			folderID = id
		}
		p.FolderID = folderID
		pl, err := s.apple.CreatePlaylist(ctx, acct.MusicUserToken, p)
		var apiErr *apple.APIError
		if err != nil && pl.ID == "" && attempt == 0 && acct.FolderID != "" && errors.As(err, &apiErr) &&
			(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
			// The saved folder may have been deleted in the Music app; make a new one.
			slog.Warn("apple playlist folder unusable, creating a new one", "folder", folderID, "err", err)
			folderID = ""
			continue
		}
		if err != nil {
			return pl, err
		}

		// The share ID normally comes back right away, but there may be a short delay.
		for wait := 0; pl.GlobalID == "" && wait < 3; wait++ {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return pl, ctx.Err()
			}
			if again, err := s.apple.GetLibraryPlaylist(ctx, acct.MusicUserToken, pl.ID); err == nil {
				pl.GlobalID = again.GlobalID
			}
		}
		if pl.GlobalID == "" {
			return pl, errors.New("playlist created but Apple didn't give it a share link")
		}
		return pl, nil
	}
}

func (s *server) reconnectMessage(r *http.Request, acct store.AppleAccount) string {
	who := s.memberAccountJSON(r, acct.ConnectedBy).DisplayName
	return fmt.Sprintf("Apple Music stopped accepting the connection (it lasts about six months). %s needs to connect Apple Music again.", who)
}

// decodeLink reads {"link": "..."} from the request and parses the playlist ID, writing an
// error response if it can't.
func decodeLink(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Send a playlist link.")
		return "", false
	}
	playlistID, err := spotify.ParsePlaylistID(req.Link)
	if err != nil {
		writeError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return "", false
	}
	return playlistID, true
}

// writeSpotifyError reports a failure to read a playlist from Spotify.
func (s *server) writeSpotifyError(w http.ResponseWriter, playlistID string, err error) {
	var ue *userError
	if errors.As(err, &ue) {
		writeError(w, ue.status, ue.message)
		return
	}
	slog.Error("read playlist", "playlist", playlistID, "err", err)
	writeError(w, http.StatusBadGateway, "Something went wrong talking to Spotify. Please try again.")
}
