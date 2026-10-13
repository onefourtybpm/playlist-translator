import { useEffect, useState, type FormEvent } from 'react'
import { api, type AccessRequest, type AdminView } from './api'

// Admin is the admin's view of who can use the app: people waiting to be let in, emails allowed
// to sign in with Apple, and current members.
export default function Admin({ onRequestsChange }: { onRequestsChange: (count: number) => void }) {
  const [view, setView] = useState<AdminView | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  function show(v: AdminView) {
    setView(v)
    onRequestsChange(v.requests.length)
  }

  // Reload whenever the page comes back into view: people sign in and ask to join elsewhere.
  useEffect(() => {
    const load = () => api.admin().then(show, (e: Error) => setError(e.message))
    load()
    const onVisible = () => document.visibilityState === 'visible' && load()
    document.addEventListener('visibilitychange', onVisible)
    return () => document.removeEventListener('visibilitychange', onVisible)
  }, [])

  async function run(action: () => Promise<AdminView>): Promise<boolean> {
    setBusy(true)
    setError(null)
    try {
      show(await action())
      return true
    } catch (e) {
      setError((e as Error).message)
      return false
    } finally {
      setBusy(false)
    }
  }

  if (!view) {
    return error ? (
      <section className="card">
        <p className="form-error" role="alert">
          {error}
        </p>
      </section>
    ) : null
  }

  return (
    <section className="card admin">
      <h2>Who can use the app</h2>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}

      {view.requests.length > 0 && (
        <div className="admin-block">
          <h3>Asking to join</h3>
          <ul className="admin-list">
            {view.requests.map((r) => (
              <RequestRow
                key={r.id}
                request={r}
                busy={busy}
                onApprove={(name) => run(() => api.approveRequest(r.id, name))}
                onDeny={() => run(() => api.denyRequest(r.id))}
              />
            ))}
          </ul>
        </div>
      )}

      <div className="admin-block">
        <h3>Invited to sign in with Apple</h3>
        {!view.signInWithApple && <p className="muted small">Sign in with Apple isn't set up on this server yet.</p>}
        <p className="muted small">
          Use the email of their Apple Account, and ask them to choose “Share My Email” when Apple asks. If they hide
          it, they’ll show up under “Asking to join” instead. Once they sign in, they move to Members.
        </p>
        {view.allowedEmails.length > 0 && (
          <ul className="admin-list">
            {view.allowedEmails.map((a) => (
              <li key={a.email}>
                <span className="admin-who">
                  <strong>{a.displayName}</strong>
                  <span className="muted small">{a.email} · hasn’t signed in yet</span>
                </span>
                <button
                  className="link-button"
                  disabled={busy}
                  onClick={() => run(() => api.removeAllowedEmail(a.email))}
                >
                  Remove
                </button>
              </li>
            ))}
          </ul>
        )}
        <AllowForm busy={busy} onAllow={(email, name) => run(() => api.allowEmail(email, name))} />
      </div>

      <div className="admin-block">
        <h3>Members</h3>
        <ul className="admin-list">
          {view.members.map((m) => (
            <li key={m.id}>
              <span className="admin-who">
                <strong>
                  {m.displayName}
                  {m.isAdmin && <span className="muted small"> (admin)</span>}
                </strong>
                <span className="muted small">
                  Signs in with {[m.spotify && 'Spotify', m.apple && 'Apple'].filter(Boolean).join(' or ') || 'nothing yet'}
                </span>
              </span>
              {!m.isAdmin && (
                <button
                  className="link-button"
                  disabled={busy}
                  onClick={() => {
                    if (window.confirm(`Remove ${m.displayName}? They’ll be signed out and their Spotify connection deleted.`)) {
                      run(() => api.removeMember(m.id))
                    }
                  }}
                >
                  Remove
                </button>
              )}
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}

function RequestRow({
  request,
  busy,
  onApprove,
  onDeny,
}: {
  request: AccessRequest
  busy: boolean
  onApprove: (name: string) => void
  onDeny: () => void
}) {
  const [name, setName] = useState(request.displayName)
  return (
    <li className="request">
      <span className="admin-who">
        <input
          aria-label="Name"
          className="text-input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Name"
        />
        <span className="muted small">
          {request.email || 'No email shared'} · {new Date(request.requestedAt).toLocaleDateString()}
        </span>
      </span>
      <span className="admin-actions">
        <button className="button small-button" disabled={busy || !name.trim()} onClick={() => onApprove(name)}>
          Let in
        </button>
        <button className="link-button" disabled={busy} onClick={onDeny}>
          Ignore
        </button>
      </span>
    </li>
  )
}

function AllowForm({ busy, onAllow }: { busy: boolean; onAllow: (email: string, name: string) => Promise<boolean> }) {
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (await onAllow(email, name)) {
      setEmail('')
      setName('')
    }
  }

  return (
    <form className="allow-form" onSubmit={submit}>
      <input
        aria-label="Name"
        className="text-input"
        placeholder="Name"
        value={name}
        onChange={(e) => setName(e.target.value)}
        autoComplete="off"
      />
      <input
        aria-label="Email"
        className="text-input"
        type="email"
        inputMode="email"
        placeholder="Apple Account email"
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
      />
      <button className="button" type="submit" disabled={busy || !email.trim() || !name.trim()}>
        Allow
      </button>
    </form>
  )
}
