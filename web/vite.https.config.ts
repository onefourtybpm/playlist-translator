// Serves the frontend over https on port 5174. Apple Music sign-in (MusicKit JS) fails from
// http pages, so the Apple side has to be developed here rather than on the http port 5173.
// Uses a self-signed certificate in .cert/ (git-ignored), created once with:
//   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
//     -subj /CN=localhost -addext subjectAltName=DNS:localhost,IP:127.0.0.1 \
//     -keyout .cert/key.pem -out .cert/cert.pem
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig, mergeConfig } from 'vite'
import base from './vite.config'

export default mergeConfig(
  base,
  defineConfig({
    root: fileURLToPath(new URL('.', import.meta.url)),
    server: {
      port: 5174,
      https: {
        key: readFileSync(new URL('.cert/key.pem', import.meta.url)),
        cert: readFileSync(new URL('.cert/cert.pem', import.meta.url)),
      },
    },
  }),
)
