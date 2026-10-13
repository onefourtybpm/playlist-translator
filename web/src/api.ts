export type Account = {
  id: string
  displayName: string
}

export type Member = {
  id: number
  displayName: string
  isAdmin: boolean
}

export type Me = {
  member: Member | null
  // The signed-in member's connected Spotify account, if any.
  spotify: Account | null
  signInWithApple: boolean
}

export type AdminMember = Member & {
  // Which ways this member signs in.
  spotify: boolean
  apple: boolean
}

export type AllowedEmail = {
  email: string
  displayName: string
}

export type AccessRequest = {
  id: number
  email: string
  displayName: string
  requestedAt: string
}

export type AdminView = {
  members: AdminMember[]
  allowedEmails: AllowedEmail[]
  requests: AccessRequest[]
  signInWithApple: boolean
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

export type AppleStatus = {
  configured: boolean
  connected: boolean
  storefront?: string
  connectedBy?: Account
  connectedAt?: string
  reconnectBy?: string
  canManage: boolean
}

export type AppleSong = {
  id: string
  name: string
  artist: string
  album: string
  durationMs: number
  explicit: boolean
  isrc: string
  url: string
}

export type MatchResult = {
  track: Track
  song: AppleSong | null
  // 'isrc': same recording. 'search': same title and artist, found by search.
  method?: 'isrc' | 'search'
}

export type Translation = {
  name: string
  shareUrl: string
  songCount: number
  createdBy: Account
  createdAt: string
}

export type Preview = {
  playlist: Playlist
  readWith: Account
  results: MatchResult[]
  skipped: SkippedItem[]
  storefront: string
  previous: Translation[]
}

export type Created = {
  name: string
  shareUrl: string
  songCount: number
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
  appleStatus: () => request<AppleStatus>('/api/apple'),
  appleConnect: (musicUserToken: string) =>
    request<AppleStatus>('/api/apple/connection', {
      method: 'PUT',
      body: JSON.stringify({ musicUserToken }),
    }),
  appleDisconnect: () => request<void>('/api/apple/connection', { method: 'DELETE' }),
  preview: (link: string) =>
    request<Preview>('/api/translate/preview', {
      method: 'POST',
      body: JSON.stringify({ link }),
    }),
  create: (link: string, songIds: string[]) =>
    request<Created>('/api/translate', {
      method: 'POST',
      body: JSON.stringify({ link, songIds }),
    }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  disconnect: () => request<void>('/api/spotify/account', { method: 'DELETE' }),
  appleDeveloperToken: () => request<{ token: string }>('/api/apple/developer-token'),
  admin: () => request<AdminView>('/api/admin'),
  allowEmail: (email: string, displayName: string) =>
    request<AdminView>('/api/admin/allowed-emails', {
      method: 'POST',
      body: JSON.stringify({ email, displayName }),
    }),
  removeAllowedEmail: (email: string) =>
    request<AdminView>(`/api/admin/allowed-emails/${encodeURIComponent(email)}`, { method: 'DELETE' }),
  approveRequest: (id: number, displayName: string) =>
    request<AdminView>(`/api/admin/requests/${id}/approve`, {
      method: 'POST',
      body: JSON.stringify({ displayName }),
    }),
  denyRequest: (id: number) => request<AdminView>(`/api/admin/requests/${id}`, { method: 'DELETE' }),
  removeMember: (id: number) => request<AdminView>(`/api/admin/members/${id}`, { method: 'DELETE' }),
}

// Full-page navigations, not fetch: the server redirects the browser to Spotify's or Apple's
// sign-in page.
export const spotifyLoginUrl = '/auth/spotify/login'
export const appleSignInUrl = '/auth/apple/login'
