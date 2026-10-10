import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const goServer = 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    // Spotify only allows plain-http redirects to 127.0.0.1 (not "localhost"), and the session
    // cookie is set for that host, so the dev server must use it too.
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': goServer,
      '/auth': goServer,
    },
  },
})
