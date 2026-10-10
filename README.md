# playlist-translator

A small private web app that turns Spotify playlists into Apple Music playlists for a group of friends.
Project decisions and setup status live in `../playlist-translator-notes/project-overview.md`.

- `server/`: Go backend (Spotify sign-in, playlist reading, SQLite storage)
- `web/`: React + Vite + TypeScript frontend

## Local development

Secrets are kept outside the repo in `../shh/spotify.env`:

```
SPOTIFY_CLIENT_ID=...
SPOTIFY_CLIENT_SECRET=...
```

The Spotify app's redirect URI must include `http://127.0.0.1:8080/callback`. Use `127.0.0.1`, not
`localhost`; Spotify rejects `localhost`.

Run the Go server (port 8080):

```bash
cd server && go run . -env ../../shh/spotify.env,dev.env
```

Run the frontend (port 5173, forwards `/api` and `/auth` requests to the Go server):

```bash
cd web && npm install && npm run dev
```

Then open http://127.0.0.1:5173 (again `127.0.0.1`, not `localhost`, so the sign-in cookie matches).

Tests:

```bash
cd server && go test ./...
```

## Server flags and settings

| Flag / variable | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Listen address |
| `-db` | `data/playlist-translator.db` | SQLite file (holds OAuth tokens; created with `0600` permissions) |
| `-static` | _(none)_ | Serve a built frontend (`web/dist`) from the Go server |
| `-env` | _(none)_ | Comma-separated `KEY=VALUE` files to load; real environment variables win |
| `BASE_URL` | `http://<addr>` | Public URL of the server; Spotify redirects to `BASE_URL/callback` |
| `APP_URL` | `BASE_URL` | Where the browser goes after sign-in (the Vite server in dev) |
