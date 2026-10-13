import { useEffect, useState } from 'react'
import { api, type AppleStatus } from './api'
import { getMusicKit, type MusicKitInstance } from './musickit'

// Warn this long before the Apple Music connection is expected to lapse.
const reconnectWarningMs = 14 * 24 * 60 * 60 * 1000

type Props = {
  status: AppleStatus
  onChange: (status: AppleStatus) => void
}

// AppleConnection shows which Apple Music account playlists are created in, and lets the person
// who manages it connect, reconnect, or disconnect it.
export default function AppleConnection({ status, onChange }: Props) {
  const [music, setMusic] = useState<MusicKitInstance | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Load MusicKit up front for whoever can connect: Safari only allows Apple's sign-in popup when
  // it opens straight from the click, with no loading in between.
  useEffect(() => {
    if (!status.configured || !status.canManage) return
    getMusicKit().then(setMusic, (e: Error) => setError(e.message))
  }, [status.configured, status.canManage])

  if (!status.configured) {
    return (
      <section className="card apple">
        <p className="muted">Apple Music isn't set up on this server yet.</p>
      </section>
    )
  }

  function connect() {
    if (!music) return
    // No await before this call, or Safari blocks the popup.
    const authorizing = music.authorize()
    setBusy(true)
    setError(null)
    authorizing
      .then((token) => api.appleConnect(token))
      .then(onChange)
      .catch(async (e) => {
        setError(describe(e))
        // A stale sign-in remembered by this browser would be offered again; forget it so the
        // next click asks Apple for a fresh one.
        await music.unauthorize().catch(() => {})
      })
      .finally(() => setBusy(false))
  }

  async function disconnect() {
    const ok = window.confirm(
      'Disconnect Apple Music? New playlists can’t be created until it’s connected again. Playlists already created stay in your library.',
    )
    if (!ok) return
    setBusy(true)
    setError(null)
    try {
      await api.appleDisconnect()
      await music?.unauthorize().catch(() => {})
      onChange(await api.appleStatus())
    } catch (e) {
      setError(describe(e))
    } finally {
      setBusy(false)
    }
  }

  const reconnectBy = status.reconnectBy ? new Date(status.reconnectBy) : null
  const reconnectSoon = reconnectBy !== null && reconnectBy.getTime() - Date.now() < reconnectWarningMs

  return (
    <section className="card apple">
      {status.connected ? (
        <div className="apple-row">
          <p>
            <span className="apple-dot" aria-hidden="true" /> Playlists are created in{' '}
            <strong>{status.connectedBy?.displayName}</strong>’s Apple Music
            {status.storefront && <span className="muted"> ({status.storefront.toUpperCase()})</span>}, as
            public playlists in a “Playlist Translator” folder.
          </p>
          {reconnectBy && (
            <p className={reconnectSoon ? 'warn' : 'muted'}>
              Apple’s connection lasts about six months; reconnect by around {formatDate(reconnectBy)}.
            </p>
          )}
          {status.canManage && (
            <div className="apple-actions">
              <button className="button apple-button" onClick={connect} disabled={busy || !music}>
                Reconnect
              </button>
              <button className="link-button danger" onClick={disconnect} disabled={busy}>
                Disconnect Apple Music
              </button>
            </div>
          )}
        </div>
      ) : (
        <div className="apple-row">
          <p>
            <strong>Connect Apple Music.</strong> Translated playlists are created as public playlists in
            this Apple Music account, and the app gives you a link to share. Use the account that
            should own the playlists.
          </p>
          <div className="apple-actions">
            <button className="button apple-button" onClick={connect} disabled={busy || !music}>
              {music ? 'Connect Apple Music' : 'Loading…'}
            </button>
          </div>
        </div>
      )}
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
    </section>
  )
}

function formatDate(d: Date): string {
  return d.toLocaleDateString(undefined, { month: 'long', day: 'numeric', year: 'numeric' })
}

// MusicKit rejects with objects whose useful text is in errorCode/description, not message.
function describe(e: unknown): string {
  if (e && typeof e === 'object') {
    const { errorCode, description, message } = e as Record<string, unknown>
    if (errorCode === 'AUTHORIZATION_ERROR' || errorCode === 'USER_CANCELLED') {
      return 'Apple Music sign-in didn’t finish. Please try again.'
    }
    if (typeof message === 'string' && message) return message
    if (typeof description === 'string' && description) return description
  }
  return String(e)
}
