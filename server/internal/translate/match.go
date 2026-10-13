// Package translate matches Spotify tracks to Apple Music catalog songs.
package translate

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
)

// Catalog is the part of the Apple Music API the matcher needs.
type Catalog interface {
	SongsByISRC(ctx context.Context, storefront string, isrcs []string) (map[string][]apple.Song, error)
	SearchSongs(ctx context.Context, storefront, term string, limit int) ([]apple.Song, error)
}

// How a track was matched.
const (
	ByISRC   = "isrc"   // same recording: Spotify and Apple share the ISRC
	BySearch = "search" // same title and artist found by search; worth a glance
)

// Result is one Spotify track and its Apple Music match, if any.
type Result struct {
	Track  spotify.Track `json:"track"`
	Song   *apple.Song   `json:"song"`
	Method string        `json:"method,omitempty"`
}

const (
	searchLimit       = 10
	searchConcurrency = 4
	// maxDurationDiffMS is how far a search result's length may be from the Spotify track's.
	maxDurationDiffMS = 10_000
)

// Match finds the best Apple Music song for each track: first by ISRC, then by searching for the
// title and artist. Tracks with no acceptable match get a nil Song.
func Match(ctx context.Context, cat Catalog, storefront string, tracks []spotify.Track) ([]Result, error) {
	var isrcs []string
	seen := make(map[string]bool)
	for _, t := range tracks {
		if isrc := strings.ToUpper(t.ISRC); isrc != "" && !seen[isrc] {
			seen[isrc] = true
			isrcs = append(isrcs, isrc)
		}
	}
	byISRC, err := cat.SongsByISRC(ctx, storefront, isrcs)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(tracks))
	var needSearch []int
	for i, t := range tracks {
		results[i].Track = t
		if best := pickBest(t, byISRC[strings.ToUpper(t.ISRC)]); best != nil {
			results[i].Song, results[i].Method = best, ByISRC
		} else {
			needSearch = append(needSearch, i)
		}
	}

	// Search for the rest, a few at a time; stop at the first error.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, searchConcurrency)
	)
	for _, i := range needSearch {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			t := tracks[i]
			songs, err := cat.SearchSongs(ctx, storefront, searchTerm(t), searchLimit)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			var candidates []apple.Song
			for _, s := range songs {
				if sameSong(t, s) {
					candidates = append(candidates, s)
				}
			}
			if best := pickBest(t, candidates); best != nil {
				results[i].Song, results[i].Method = best, BySearch
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// pickBest prefers the release Spotify uses (same album name), then non-compilations, then the
// same explicit/clean version, then the closest length.
func pickBest(t spotify.Track, candidates []apple.Song) *apple.Song {
	var best *apple.Song
	bestScore := -1
	for i := range candidates {
		if s := score(t, candidates[i]); s > bestScore {
			best, bestScore = &candidates[i], s
		}
	}
	return best
}

func score(t spotify.Track, s apple.Song) int {
	n := 0
	if normalizeAlbum(s.Album) == normalizeAlbum(t.Album) {
		n += 1000
	}
	if !s.IsCompilation {
		n += 300
	}
	if s.Explicit == t.Explicit {
		n += 100
	}
	if normalizeTitle(s.Name) == normalizeTitle(t.Name) {
		n += 50
	}
	// Up to 30 points for length, losing one per 100 ms of difference.
	n += max(0, 30-abs(s.DurationMS-t.DurationMS)/100)
	return n
}

// sameSong decides whether a search result is the same song: same title (ignoring featured
// artists and remaster labels), an overlapping artist, and a similar length. A remix or live
// version has a different title, so it doesn't count.
func sameSong(t spotify.Track, s apple.Song) bool {
	if normalizeTitle(s.Name) != normalizeTitle(t.Name) {
		return false
	}
	if t.DurationMS > 0 && s.DurationMS > 0 && abs(s.DurationMS-t.DurationMS) > maxDurationDiffMS {
		return false
	}
	appleArtist := normalize(s.Artist)
	for _, a := range t.Artists {
		if a := normalize(a); a != "" && (strings.Contains(appleArtist, a) || strings.Contains(a, appleArtist)) {
			return true
		}
	}
	return false
}

func searchTerm(t spotify.Track) string {
	term := normalizeTitle(t.Name)
	if len(t.Artists) > 0 {
		term += " " + normalize(t.Artists[0])
	}
	return term
}

var (
	// Parenthesized or bracketed notes that don't change the recording: "(feat. X)",
	// "[with Y]", "(2011 Remaster)", "(Remastered 2009)".
	notePattern = regexp.MustCompile(`(?i)\s*[\(\[][^\)\]]*\b(feat\.?|ft\.?|featuring|with|remaster(ed)?)\b[^\)\]]*[\)\]]`)
	// Spotify's " - 2011 Remaster" / " - Remastered 2009" suffixes.
	remasterSuffix = regexp.MustCompile(`(?i)\s+-\s+[^-]*\bremaster(ed)?\b.*$`)
	// Trailing "feat. X" without brackets.
	featSuffix = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring)\s+.*$`)
	// Release-type labels on album names: " - Single", " - EP", "(Deluxe Edition)".
	albumLabel = regexp.MustCompile(`(?i)\s*(-\s+(single|ep)$|[\(\[][^\)\]]*\b(deluxe|edition|remaster(ed)?|expanded|anniversary)\b[^\)\]]*[\)\]])`)
)

func normalizeTitle(s string) string {
	s = notePattern.ReplaceAllString(s, "")
	s = remasterSuffix.ReplaceAllString(s, "")
	s = featSuffix.ReplaceAllString(s, "")
	return normalize(s)
}

func normalizeAlbum(s string) string {
	return normalize(albumLabel.ReplaceAllString(s, ""))
}

// normalize lowercases, turns "&" into "and", and drops punctuation so "Don't Stop" and
// "Dont Stop" compare equal.
func normalize(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", " and "))
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			space = false
		case unicode.IsSpace(r) || r == '-' || r == '/':
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
