import { useEffect, useState } from 'react'
import { api, spotifyLoginUrl, type Account, type AppleStatus, type Me } from './api'
import AppleConnection from './AppleConnection'
import Translator from './Translator'

// The server reports sign-in failures by redirecting back with ?error=...; read it once and
// tidy the URL so a refresh doesn't show it again.
function takeErrorFromUrl(): string | null {
  const params = new URLSearchParams(window.location.search)
  const error = params.get('error')
  if (error) window.history.replaceState(null, '', window.location.pathname)
  return error
}

export default function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [error, setError] = useState<string | null>(takeErrorFromUrl)

  useEffect(() => {
    api.me().then(setMe, (e: Error) => {
      setError(e.message)
      setMe({ spotify: null })
    })
  }, [])

  async function signOut() {
    await api.logout()
    setMe({ spotify: null })
  }

  async function disconnect() {
    const ok = window.confirm(
      'Disconnect Spotify? This removes your Spotify connection from the app, so nobody in the group can translate your playlists until you connect again. Playlists already translated stay in Apple Music.',
    )
    if (!ok) return
    try {
      await api.disconnect()
      setMe({ spotify: null })
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div className="page">
      <header className="topbar">
        <h1>Playlist Translator</h1>
        {me?.spotify && (
          <div className="topbar-account">
            <span>
              <span className="service-spotify">Spotify:</span> <strong>{me.spotify.displayName}</strong>
            </span>
            <button
              className="link-button"
              onClick={signOut}
              title="Signs out this browser only. Your playlists stay available to the group."
            >
              Sign out
            </button>
            <button className="link-button" onClick={disconnect}>
              Disconnect
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

      {me === null ? (
        <p className="muted">Loading…</p>
      ) : me.spotify ? (
        <SignedIn />
      ) : (
        <SignIn />
      )}
    </div>
  )
}

function SignIn() {
  return (
    <section className="card signin">
      <h2>Turn Spotify playlists into Apple Music playlists</h2>
      <p className="muted">
        Connect the Spotify account that owns your playlists. You only need to do this once; after
        that, anyone in the group can translate your playlists.
      </p>
      <a className="button spotify" href={spotifyLoginUrl}>
        Connect Spotify
      </a>
    </section>
  )
}

function SignedIn() {
  const [apple, setApple] = useState<AppleStatus | null>(null)
  const [appleError, setAppleError] = useState<string | null>(null)

  useEffect(() => {
    api.appleStatus().then(setApple, (e: Error) => setAppleError(e.message))
  }, [])

  return (
    <>
      <ConnectedAccounts />
      {apple && <AppleConnection status={apple} onChange={setApple} />}
      {appleError && (
        <p className="form-error" role="alert">
          {appleError}
        </p>
      )}
      <Translator apple={apple ?? { configured: false, connected: false, canManage: false }} />
    </>
  )
}

function ConnectedAccounts() {
  const [accounts, setAccounts] = useState<Account[] | null>(null)

  useEffect(() => {
    api.accounts().then(setAccounts, () => setAccounts([]))
  }, [])

  if (!accounts) return null
  return (
    <section className="connected">
      <span className="muted">Connected Spotify accounts:</span>
      {accounts.map((a) => (
        <span key={a.id} className="chip">
          {a.displayName}
        </span>
      ))}
    </section>
  )
}
