import { useState, type FormEvent } from 'react'
import { api, type AppleStatus, type Created, type MatchResult, type Preview } from './api'

// Translator takes a Spotify playlist link, shows how each song matched in Apple Music, and
// creates the Apple Music playlist from the songs left checked.
export default function Translator({ apple }: { apple: AppleStatus }) {
  const [link, setLink] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [preview, setPreview] = useState<{ link: string; data: Preview } | null>(null)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!link.trim()) return
    setLoading(true)
    setError(null)
    setPreview(null)
    try {
      setPreview({ link, data: await api.preview(link) })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <section className="card">
        <form className="link-form" onSubmit={submit}>
          <label htmlFor="playlist-link">Spotify playlist link</label>
          <div className="link-row">
            <input
              id="playlist-link"
              type="text"
              inputMode="url"
              placeholder="https://open.spotify.com/playlist/…"
              value={link}
              onChange={(e) => setLink(e.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
            <button className="button spotify" type="submit" disabled={loading || !link.trim()}>
              {loading ? 'Matching songs…' : 'Preview'}
            </button>
          </div>
        </form>
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
      </section>

      {/* Keyed by link so a new preview starts with fresh selections. */}
      {preview && <PreviewView key={preview.link} link={preview.link} preview={preview.data} apple={apple} />}
    </>
  )
}

function PreviewView({ link, preview, apple }: { link: string; preview: Preview; apple: AppleStatus }) {
  const { playlist, readWith, results, skipped, previous } = preview
  const matched = results.filter((r) => r.song)
  const byIsrc = matched.filter((r) => r.method === 'isrc').length
  const bySearch = matched.length - byIsrc
  const notFound = results.filter((r) => !r.song)

  // Every matched song starts checked; positions identify rows since a playlist can repeat a song.
  const [included, setIncluded] = useState(() => new Set(matched.map((r) => r.track.position)))
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<Created | null>(null)

  const songIds = results.filter((r) => r.song && included.has(r.track.position)).map((r) => r.song!.id)

  function toggle(position: number) {
    setIncluded((prev) => {
      const next = new Set(prev)
      if (!next.delete(position)) next.add(position)
      return next
    })
  }

  async function create() {
    setCreating(true)
    setError(null)
    try {
      setCreated(await api.create(link, songIds))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setCreating(false)
    }
  }

  return (
    <section className="card">
      <div className="playlist-header">
        {playlist.imageUrl ? (
          <img className="cover" src={playlist.imageUrl} alt="" />
        ) : (
          <div className="cover placeholder" aria-hidden="true">
            ♪
          </div>
        )}
        <div>
          <h2>
            {playlist.url ? (
              <a href={playlist.url} target="_blank" rel="noreferrer">
                {playlist.name}
              </a>
            ) : (
              playlist.name
            )}
          </h2>
          <p className="muted">
            By <strong className="spotify-name">{playlist.owner.display_name}</strong>
            {playlist.collaborative && ' · collaborative'} · read using{' '}
            <strong className="spotify-name">{readWith.displayName}</strong>'s connection
          </p>
          <ul className="stats">
            <li>
              <strong>{byIsrc}</strong> exact {byIsrc === 1 ? 'match' : 'matches'}
            </li>
            {bySearch > 0 && (
              <li className="warn">
                <strong>{bySearch}</strong> matched by name (worth a look)
              </li>
            )}
            {notFound.length > 0 && (
              <li className="warn">
                <strong>{notFound.length}</strong> not found in Apple Music
              </li>
            )}
            {skipped.length > 0 && (
              <li className="warn">
                <strong>{skipped.length}</strong> can't be translated
              </li>
            )}
          </ul>
        </div>
      </div>

      {previous.length > 0 && !created && (
        <div className="notice">
          Already translated {previous.length === 1 ? 'once' : `${previous.length} times`}. Latest:{' '}
          <a href={previous[0].shareUrl} target="_blank" rel="noreferrer">
            {previous[0].name}
          </a>{' '}
          <span className="muted">
            ({previous[0].songCount} songs, by {previous[0].createdBy.displayName} on{' '}
            {new Date(previous[0].createdAt).toLocaleDateString()})
          </span>
          . Creating again makes a separate new playlist.
        </div>
      )}

      {created ? (
        <CreatedView created={created} notFound={notFound} />
      ) : (
        <CreateBar
          apple={apple}
          count={songIds.length}
          creating={creating}
          error={error}
          onCreate={create}
        />
      )}

      <div className="table-wrap">
        <table className="matches">
          <thead>
            <tr>
              <th className="num">#</th>
              <th>Spotify</th>
              <th>Apple Music</th>
            </tr>
          </thead>
          <tbody>
            {results.map((r) => (
              <MatchRow
                key={r.track.position}
                result={r}
                checked={included.has(r.track.position)}
                disabled={creating || created !== null}
                onToggle={() => toggle(r.track.position)}
              />
            ))}
          </tbody>
        </table>
      </div>

      {skipped.length > 0 && (
        <div className="skipped">
          <h3>Can't be translated</h3>
          <ul>
            {skipped.map((s) => (
              <li key={s.position}>
                <span className="muted">#{s.position}</span> {s.name || 'Unknown item'} — {s.reason}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function CreateBar(props: {
  apple: AppleStatus
  count: number
  creating: boolean
  error: string | null
  onCreate: () => void
}) {
  const { apple, count, creating, error, onCreate } = props
  let blocked: string | null = null
  if (!apple.configured) blocked = "Apple Music isn't set up on this server yet."
  else if (!apple.connected) blocked = 'Connect Apple Music above to create the playlist.'

  return (
    <div className="create-bar">
      {blocked ? (
        <p className="muted">{blocked}</p>
      ) : (
        <button className="button apple-button" onClick={onCreate} disabled={creating || count === 0}>
          {creating ? 'Creating…' : `Create Apple Music playlist (${count} songs)`}
        </button>
      )}
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
    </div>
  )
}

function CreatedView({ created, notFound }: { created: Created; notFound: MatchResult[] }) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(created.shareUrl)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access can be refused; the link is still visible to copy by hand.
    }
  }

  return (
    <div className="created" role="status">
      <h3>✓ Created “{created.name}” with {created.songCount} songs</h3>
      <p>Send this link to anyone with Apple Music. They can open it and add the playlist:</p>
      <div className="share-row">
        <input className="share-link" readOnly value={created.shareUrl} onFocus={(e) => e.target.select()} />
        <button className="button" onClick={copy}>
          {copied ? 'Copied' : 'Copy link'}
        </button>
        <a className="button secondary" href={created.shareUrl} target="_blank" rel="noreferrer">
          Open
        </a>
      </div>
      {notFound.length > 0 && (
        <p className="muted">
          Not in Apple Music, so left out:{' '}
          {notFound.map((r) => `${r.track.name} (${r.track.artists[0] ?? 'unknown artist'})`).join(', ')}.
        </p>
      )}
    </div>
  )
}

function MatchRow(props: { result: MatchResult; checked: boolean; disabled: boolean; onToggle: () => void }) {
  const { result, checked, disabled, onToggle } = props
  const { track, song, method } = result
  const id = `include-${track.position}`

  return (
    <tr className={song ? undefined : 'unmatched'}>
      <td className="num muted">{track.position}</td>
      <td>
        <div>
          {track.name}
          {track.explicit && <span className="badge" title="Explicit">E</span>}
        </div>
        <div className="muted small">
          {track.artists.join(', ')}
          <span className="hide-narrow"> · {track.album}</span>
        </div>
      </td>
      <td>
        {song ? (
          <label className="match" htmlFor={id}>
            <input id={id} type="checkbox" checked={checked} disabled={disabled} onChange={onToggle} />
            <span>
              <span>
                <a href={song.url} target="_blank" rel="noreferrer">
                  {song.name}
                </a>
                {song.explicit && <span className="badge" title="Explicit">E</span>}
                {method === 'search' && (
                  <span className="tag warn" title="No exact recording match; found by title and artist">
                    by name
                  </span>
                )}
              </span>
              <span className="muted small">
                {song.artist}
                <span className="hide-narrow"> · {song.album}</span>
              </span>
            </span>
          </label>
        ) : (
          <span className="warn">Not found</span>
        )}
      </td>
    </tr>
  )
}
