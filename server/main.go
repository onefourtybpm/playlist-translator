// Command server runs the Playlist Translator web app: the JSON API, the Spotify sign-in flow,
// and (optionally) the built React frontend.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/spotify"
	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

type config struct {
	Addr      string
	DBPath    string
	StaticDir string

	// BaseURL is where this server is reachable; Spotify redirects to BaseURL + "/callback".
	BaseURL string
	// AppURL is where the browser is sent after signing in. In development that's the Vite
	// dev server; in production it's the same as BaseURL.
	AppURL string

	SpotifyClientID     string
	SpotifyClientSecret string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	var envFiles string
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:8080", "address to listen on")
	flag.StringVar(&cfg.DBPath, "db", "data/playlist-translator.db", "path to the SQLite database")
	flag.StringVar(&cfg.StaticDir, "static", "", "directory of the built frontend to serve (optional)")
	flag.StringVar(&envFiles, "env", "", "comma-separated KEY=VALUE files to load; real environment variables take precedence")
	flag.Parse()

	for _, path := range strings.Split(envFiles, ",") {
		if path = strings.TrimSpace(path); path != "" {
			if err := loadEnvFile(path); err != nil {
				return err
			}
		}
	}

	cfg.SpotifyClientID = os.Getenv("SPOTIFY_CLIENT_ID")
	cfg.SpotifyClientSecret = os.Getenv("SPOTIFY_CLIENT_SECRET")
	cfg.BaseURL = strings.TrimRight(envOr("BASE_URL", "http://"+cfg.Addr), "/")
	cfg.AppURL = strings.TrimRight(envOr("APP_URL", cfg.BaseURL), "/")
	if cfg.SpotifyClientID == "" || cfg.SpotifyClientSecret == "" {
		return errors.New("SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET must be set (pass -env with the path to your credentials file)")
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	srv := &server{
		cfg:     cfg,
		store:   db,
		spotify: spotify.NewClient(cfg.SpotifyClientID, cfg.SpotifyClientSecret, cfg.BaseURL+"/callback"),
	}
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "app_url", cfg.AppURL)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadEnvFile sets variables from a file of KEY=VALUE lines (blank lines, # comments, and an
// optional "export " prefix are allowed). Variables already set in the environment win.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("load env file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}
