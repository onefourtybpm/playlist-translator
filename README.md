# playlist-translator

A small private web app that turns Spotify playlists into Apple Music playlists for a group of friends.
Project decisions and setup status live in `../playlist-translator-notes/project-overview.md`.

- `server/`: Go backend (members who sign in with Spotify or Apple, playlist reading, Apple Music
  matching and playlist creation, SQLite storage)
- `web/`: React + Vite + TypeScript frontend

## Local development

Secrets are kept outside the repo in `../shh/`. `spotify.env`:

```
SPOTIFY_CLIENT_ID=...
SPOTIFY_CLIENT_SECRET=...
```

`apple.env` (optional; without it the Apple Music features are off), plus the MusicKit private key
it points to:

```
APPLE_TEAM_ID=...
APPLE_KEY_ID=...
APPLE_PRIVATE_KEY_PATH=../../shh/AuthKey_<KeyID>.p8
```

`signin-apple.env` (optional; without it people can only sign in with Spotify), plus the Sign in
with Apple key it points to. It also needs `APPLE_TEAM_ID` from `apple.env`:

```
SIGN_IN_WITH_APPLE_SERVICES_ID=...
SIGN_IN_WITH_APPLE_KEY_ID=...
SIGN_IN_WITH_APPLE_KEY_PATH=../../shh/AuthKey_<KeyID>.p8
```

Apple only returns to a real domain name, not `127.0.0.1` or `localhost`, so Sign in with Apple
can only be tried on the Pi's public URL; leave this file out of local runs. The first person to
sign in with Spotify on a new database becomes the admin.

The Spotify app's redirect URI must include `http://127.0.0.1:8080/callback`. Use `127.0.0.1`, not
`localhost`; Spotify rejects `localhost`.

Run the Go server (port 8080):

```bash
cd server && go run . -env ../../shh/spotify.env,../../shh/apple.env,dev.env
```

Run the frontend over https (port 5174, forwards `/api` and `/auth` requests to the Go server).
Apple Music sign-in doesn't work from http pages, so this is the one to use. The first time,
create a self-signed certificate in `web/.cert/` (git-ignored):

```bash
cd web && mkdir -p .cert && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 -subj /CN=localhost -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout .cert/key.pem -out .cert/cert.pem
```

```bash
cd web && npm install && npm run dev:https
```

Then open https://127.0.0.1:5174 (`127.0.0.1`, not `localhost`, so the sign-in cookie matches) and
accept the certificate warning once. `dev.env` sends the browser there after Spotify sign-in.
`npm run dev` still serves plain http on port 5173 for work that doesn't touch Apple Music.

Tests:

```bash
cd server && go test ./...
```

## Deploying to the Raspberry Pi

The app runs on a Raspberry Pi at https://playlist-translator-pi.tail5b17c8.ts.net (Tailscale Funnel).
To build and deploy:

```bash
deploy/pi/deploy.sh
```

The one-time Pi setup scripts and systemd units are in `deploy/pi/`. Setup notes are in
`../playlist-translator-notes/pi-setup.md`.

## Server flags and settings

| Flag / variable | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Listen address |
| `-db` | `data/playlist-translator.db` | SQLite file (holds OAuth tokens; created with `0600` permissions) |
| `-static` | _(none)_ | Serve a built frontend (`web/dist`) from the Go server |
| `-env` | _(none)_ | Comma-separated `KEY=VALUE` files to load; real environment variables win |
| `BASE_URL` | `http://<addr>` | Public URL of the server; Spotify redirects to `BASE_URL/callback` |
| `APP_URL` | `BASE_URL` | Where the browser goes after sign-in (the Vite server in dev) |
| `APPLE_TEAM_ID`, `APPLE_KEY_ID`, `APPLE_PRIVATE_KEY_PATH` | _(none)_ | MusicKit key for Apple Music; all three or none |
| `SIGN_IN_WITH_APPLE_SERVICES_ID`, `SIGN_IN_WITH_APPLE_KEY_ID`, `SIGN_IN_WITH_APPLE_KEY_PATH` | _(none)_ | Sign in with Apple (Services ID and its key; uses `APPLE_TEAM_ID`); all three or none. Apple returns to `BASE_URL/auth/apple/callback` |
