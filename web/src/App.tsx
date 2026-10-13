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

// usePath follows the URL path, for the one extra page (/admin), without a router library.
function usePath(): [string, (to: string) => void] {
  const [path, setPath] = useState(window.location.pathname)
  useEffect(() => {
    const onPop = () => setPath(window.location.pathname)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  function navigate(to: string) {
    window.history.pushState(null, '', to)
    setPath(to)
    window.scrollTo(0, 0)
  }
  return [path, navigate]
}

export default function App() {
  const [path, navigate] = usePath()
  const [me, setMe] = useState<Me | null>(null)
  const [error, setError] = useState<string | null>(() => takeFromUrl('error'))
  const [notice, setNotice] = useState<string | null>(() => takeFromUrl('notice'))

  useEffect(() => {
    api.me().then(setMe, (e: Error) => {
      setError(e.message)
      setMe({ member: null, spotify: null, signInWithApple: false })
    })
  }, [])

  // Only the admin has an /admin page; anyone else lands on the translator.
  useEffect(() => {
    if (me && !me.member?.isAdmin && path !== '/') {
      window.history.replaceState(null, '', '/' + window.location.search)
    }
  }, [me, path])

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
            {/* In the color of the service they sign in with (Spotify if they use both). */}
            <strong className={me.member.signsInWithSpotify ? 'signed-in-spotify' : 'signed-in-apple'}>
              {me.member.displayName}
            </strong>
            {me.member.isAdmin && (
              <a
                className="link-button"
                href="/admin"
                onClick={(e) => {
                  e.preventDefault()
                  navigate('/admin')
                }}
              >
                Admin
                {(me.member.pendingRequests ?? 0) > 0 && (
                  <span className="count" aria-label={`${me.member.pendingRequests} asking to join`}>
                    {me.member.pendingRequests}
                  </span>
                )}
              </a>
            )}
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
      ) : me.member?.isAdmin && path === '/admin' ? (
        <>
          <a
            className="link-button back-link"
            href="/"
            onClick={(e) => {
              e.preventDefault()
              navigate('/')
            }}
          >
            ← Back to the translator
          </a>
          <Admin
            onRequestsChange={(n) =>
              setMe((prev) => (prev?.member ? { ...prev, member: { ...prev.member, pendingRequests: n } } : prev))
            }
          />
        </>
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
      <SpotifyConnection
        spotify={me.spotify}
        signsInWithSpotify={me.member?.signsInWithSpotify ?? false}
        onDisconnected={onSpotifyDisconnected}
        onError={onError}
      />
      {apple && <AppleConnection status={apple} onChange={setApple} />}
      {appleError && (
        <p className="form-error" role="alert">
          {appleError}
        </p>
      )}
      <Translator apple={apple ?? { configured: false, connected: false, canManage: false, canSwitch: false }} />
    </>
  )
}

// SpotifyConnection shows whose Spotify playlists can be translated. Members who sign in with
// Spotify can also connect or disconnect their own account; members who sign in with Apple only
// see the list.
function SpotifyConnection({
  spotify,
  signsInWithSpotify,
  onDisconnected,
  onError,
}: {
  spotify: Account | null
  signsInWithSpotify: boolean
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

  if (!signsInWithSpotify && !accounts?.length) return null
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
      {!signsInWithSpotify ? null : spotify ? (
        <p className="spotify-status">
          <span className="spotify-dot" aria-hidden="true" /> Your Spotify (<strong className="spotify-name">{spotify.displayName}</strong>) is connected.{' '}
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
