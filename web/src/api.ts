export type Account = {
  id: string
  displayName: string
}

export type Me = {
  spotify: Account | null
}

export type Playlist = {
  id: string
  name: string
  description: string
  collaborative: boolean
  owner: { id: string; display_name: string }
  imageUrl: string
  url: string
  total: number
}

export type Track = {
  position: number
  spotifyId: string
  name: string
  artists: string[]
  album: string
  durationMs: number
  explicit: boolean
  isrc: string
}

export type SkippedItem = {
  position: number
  name: string
  reason: string
}

export type PlaylistResult = {
  playlist: Playlist
  readWith: Account
  tracks: Track[]
  skipped: SkippedItem[]
}

export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  })
  if (!res.ok) {
    let message = `Request failed (${res.status}).`
    try {
      const body = await res.json()
      if (typeof body?.error === 'string') message = body.error
    } catch {
      // Not JSON (e.g. the Go server isn't running and the dev proxy answered); keep the default.
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export const api = {
  me: () => request<Me>('/api/me'),
  accounts: () => request<Account[]>('/api/spotify/accounts'),
  readPlaylist: (link: string) =>
    request<PlaylistResult>('/api/spotify/playlist', {
      method: 'POST',
      body: JSON.stringify({ link }),
    }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  disconnect: () => request<void>('/api/spotify/account', { method: 'DELETE' }),
}

// Full-page navigation, not fetch: the server redirects the browser to Spotify's sign-in page.
export const spotifyLoginUrl = '/auth/spotify/login'
