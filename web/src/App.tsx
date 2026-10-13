import { useEffect, useState } from 'react'
import { api, appleSignInUrl, spotifyLoginUrl, type Account, type AppleStatus, type Me } from './api'
import Admin from './Admin'
import AppleConnection from './AppleConnection'
import Translator from './Translator'

// The server reports sign-in results by redirecting back with ?error=... or ?notice=...; read
// them once and tidy the URL so a refresh doesn't show them again.
function takeFromUrl(key: string): string | null {
  const params = new URLSearchParams(window.location.search)
  const value = params.get(key)
  if (value) {
    params.delete(key)
    const rest = params.toString()
    window.history.replaceState(null, '', window.location.pathname + (rest ? `?${rest}` : ''))
  }
  return value
}

export default function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [error, setError] = useState<string | null>(() => takeFromUrl('error'))
  const [notice, setNotice] = useState<string | null>(() => takeFromUrl('notice'))

  useEffect(() => {
    api.me().then(setMe, (e: Error) => {
      setError(e.message)
      setMe({ member: null, spotify: null, signInWithApple: false })
    })
  }, [])

  async function signOut() {
    await api.logout()
    setMe((prev) => ({ member: null, spotify: null, signInWithApple: prev?.signInWithApple ?? false }))
  }

  return (
    <div className="page">
      <header className="topbar">
        <h1>Playlist Translator</h1>
        {me?.member && (
          <div className="topbar-account">
            <strong>{me.member.displayName}</strong>
            <button className="link-button" onClick={signOut}>
              Sign out
            </button>
          </div>
        )}
      </header>

      {error && (
        <div className="banner error" role="alert">
          <span>{error}</span>
          <button className="link-button" onClick={() => setError(null)} aria-label="Dismiss">
            ✕
          </button>
        </div>
      )}
      {notice && (
        <div className="banner info" role="status">
          <span>{notice}</span>
          <button className="link-button" onClick={() => setNotice(null)} aria-label="Dismiss">
            ✕
          </button>
        </div>
      )}

      {me === null ? (
        <p className="muted">Loading…</p>
      ) : me.member ? (
        <SignedIn me={me} onSpotifyDisconnected={() => setMe({ ...me, spotify: null })} onError={setError} />
      ) : (
        <SignIn appleAvailable={me.signInWithApple} />
      )}
    </div>
  )
}

function SignIn({ appleAvailable }: { appleAvailable: boolean }) {
  return (
    <section className="card signin">
      <h2>Turn Spotify playlists into Apple Music playlists</h2>
      <p className="muted">
        Make playlists on Spotify? Sign in with Spotify, so the group can translate them.
        {appleAvailable &&
          ' Everyone else, sign in with Apple. The first time, the app’s admin needs to have added your email.'}
      </p>
      <div className="signin-buttons">
        <a className="button spotify" href={spotifyLoginUrl}>
          Sign in with Spotify
        </a>
        {appleAvailable && (
          <a className="apple-id" href={appleSignInUrl}>
            {/* Apple's own button artwork, as its guidelines ask, in black or (dark mode) white. */}
            <picture>
              <source media="(prefers-color-scheme: dark)" srcSet={appleButton('white')} />
              <img src={appleButton('black')} alt="Sign in with Apple" width={240} height={44} />
            </picture>
          </a>
        )}
      </div>
    </section>
  )
}

function appleButton(color: 'black' | 'white'): string {
  const params = new URLSearchParams({
    height: '44',
    width: '240',
    color,
    border: 'false',
    type: 'sign-in',
    border_radius: '50',
    scale: '3',
    locale: 'en_US',
  })
  return `https://appleid.cdn-apple.com/appleid/button?${params}`
}

function SignedIn({
  me,
  onSpotifyDisconnected,
  onError,
}: {
  me: Me
  onSpotifyDisconnected: () => void
  onError: (message: string) => void
}) {
  const [apple, setApple] = useState<AppleStatus | null>(null)
  const [appleError, setAppleError] = useState<string | null>(null)

  useEffect(() => {
    api.appleStatus().then(setApple, (e: Error) => setAppleError(e.message))
  }, [])

  return (
    <>
      <SpotifyConnection spotify={me.spotify} onDisconnected={onSpotifyDisconnected} onError={onError} />
      {apple && <AppleConnection status={apple} onChange={setApple} />}
      {appleError && (
        <p className="form-error" role="alert">
          {appleError}
        </p>
      )}
      <Translator apple={apple ?? { configured: false, connected: false, canManage: false }} />
      {me.member?.isAdmin && <Admin />}
    </>
  )
}

// SpotifyConnection shows whose Spotify playlists can be translated, and lets the signed-in
// member connect or disconnect their own Spotify account.
function SpotifyConnection({
  spotify,
  onDisconnected,
  onError,
}: {
  spotify: Account | null
  onDisconnected: () => void
  onError: (message: string) => void
}) {
  const [accounts, setAccounts] = useState<Account[] | null>(null)

  useEffect(() => {
    api.accounts().then(setAccounts, () => setAccounts([]))
  }, [spotify])

  async function disconnect() {
    const ok = window.confirm(
      'Disconnect Spotify? This removes your Spotify connection from the app, so nobody in the group can translate your playlists until you connect again. Playlists already translated stay in Apple Music.',
    )
    if (!ok) return
    try {
      await api.disconnect()
      onDisconnected()
    } catch (e) {
      onError((e as Error).message)
    }
  }

  return (
    <section className="card spotify-card">
      {accounts && accounts.length > 0 && (
        <div className="connected">
          <span className="muted">Playlists can be translated from:</span>
          {accounts.map((a) => (
            <span key={a.id} className="chip">
              {a.displayName}
            </span>
          ))}
        </div>
      )}
      {spotify ? (
        <p className="spotify-status">
          <span className="spotify-dot" aria-hidden="true" /> Your Spotify ({spotify.displayName}) is connected.{' '}
          <button className="link-button" onClick={disconnect}>
            Disconnect
          </button>
        </p>
      ) : (
        <div className="spotify-status">
          <p>Make playlists on Spotify? Connect your account so the group can translate them.</p>
          <a className="button spotify" href={spotifyLoginUrl}>
            Connect Spotify
          </a>
        </div>
      )}
    </section>
  )
}
