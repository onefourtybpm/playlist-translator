package translate

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
)

type fakeCatalog struct {
	byISRC map[string][]apple.Song
	search map[string][]apple.Song // keyed by search term
	err    error

	mu       sync.Mutex // Match searches concurrently
	searches []string
}

func (f *fakeCatalog) SongsByISRC(_ context.Context, _ string, isrcs []string) (map[string][]apple.Song, error) {
	out := make(map[string][]apple.Song)
	for _, isrc := range isrcs {
		if songs, ok := f.byISRC[isrc]; ok {
			out[isrc] = songs
		}
	}
	return out, nil
}

func (f *fakeCatalog) SearchSongs(_ context.Context, _, term string, _ int) ([]apple.Song, error) {
	f.mu.Lock()
	f.searches = append(f.searches, term)
	f.mu.Unlock()
	return f.search[term], f.err
}

func TestMatchPrefersOriginalRelease(t *testing.T) {
	// The real catalog has "Never Gonna Give You Up" on several compilations and the original album.
	cat := &fakeCatalog{byISRC: map[string][]apple.Song{
		"GBARL9300135": {
			{ID: "comp", Name: "Never Gonna Give You Up", Album: "80s Dance", IsCompilation: true, DurationMS: 212747},
			{ID: "orig", Name: "Never Gonna Give You Up", Album: "Whenever You Need Somebody", DurationMS: 213573},
		},
		"USUM71703861": {
			{ID: "comp", Name: "Cut To The Feeling", Album: "CSD 2023", IsCompilation: true},
			{ID: "album", Name: "Cut To The Feeling", Album: "Emotion (10th Anniversary Edition)"},
			{ID: "single", Name: "Cut To The Feeling", Album: "Cut To The Feeling - Single", IsSingle: true},
		},
	}}
	tracks := []spotify.Track{
		{Name: "Never Gonna Give You Up", Album: "Whenever You Need Somebody", ISRC: "gbarl9300135", DurationMS: 213573},
		// Spotify has this on the single, so the single wins over the anniversary album.
		{Name: "Cut To The Feeling", Album: "Cut To The Feeling", ISRC: "USUM71703861"},
	}
	results, err := Match(context.Background(), cat, "us", tracks)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"orig", "single"} {
		if results[i].Song == nil || results[i].Song.ID != want || results[i].Method != ByISRC {
			t.Errorf("track %d: got %+v, want %s by ISRC", i, results[i], want)
		}
	}
	if len(cat.searches) != 0 {
		t.Errorf("searched %v, want no searches when every ISRC matched", cat.searches)
	}
}

func TestMatchPrefersMatchingExplicitVersion(t *testing.T) {
	cat := &fakeCatalog{byISRC: map[string][]apple.Song{"X": {
		{ID: "clean", Album: "A", Explicit: false},
		{ID: "explicit", Album: "A", Explicit: true},
	}}}
	results, _ := Match(context.Background(), cat, "us", []spotify.Track{{Album: "A", ISRC: "X", Explicit: true}})
	if results[0].Song == nil || results[0].Song.ID != "explicit" {
		t.Errorf("got %+v, want explicit version", results[0].Song)
	}
}

func TestMatchFallsBackToSearch(t *testing.T) {
	cat := &fakeCatalog{search: map[string][]apple.Song{
		"song a artist": {
			{ID: "remix", Name: "Song A (Club Remix)", Artist: "Artist", DurationMS: 200_000},
			{ID: "other-artist", Name: "Song A", Artist: "Someone Else", DurationMS: 200_000},
			{ID: "right", Name: "Song A (feat. Guest)", Artist: "Artist & Guest", DurationMS: 201_000},
		},
		"song b artist": {
			{ID: "too-long", Name: "Song B", Artist: "Artist", DurationMS: 400_000},
		},
	}}
	tracks := []spotify.Track{
		{Name: "Song A - 2011 Remaster", Artists: []string{"Artist", "Guest"}, DurationMS: 200_000, ISRC: "NOT-IN-APPLE"},
		{Name: "Song B", Artists: []string{"Artist"}, DurationMS: 200_000},
	}
	results, err := Match(context.Background(), cat, "us", tracks)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Song == nil || results[0].Song.ID != "right" || results[0].Method != BySearch {
		t.Errorf("song A: got %+v, want 'right' by search", results[0])
	}
	if results[1].Song != nil {
		t.Errorf("song B: got %+v, want no match (length differs by minutes)", results[1].Song)
	}
}

func TestMatchReturnsSearchErrors(t *testing.T) {
	cat := &fakeCatalog{err: errors.New("apple is down")}
	if _, err := Match(context.Background(), cat, "us", []spotify.Track{{Name: "x", Artists: []string{"y"}}}); err == nil {
		t.Error("want error when search fails")
	}
}

func TestNormalizeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Don't Stop Me Now - Remastered 2011": "dont stop me now",
		"Song (feat. Someone)":                "song",
		"Song [with Someone]":                 "song",
		"Song feat. Someone":                  "song",
		"Rock & Roll":                         "rock and roll",
		"Song (Live)":                         "song live",
		"Song (Acoustic Version)":             "song acoustic version",
	} {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if normalizeAlbum("Cut To The Feeling - Single") != normalizeAlbum("Cut To The Feeling") {
		t.Error("album ' - Single' label not ignored")
	}
}
