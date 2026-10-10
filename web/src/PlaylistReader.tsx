import { useState, type FormEvent } from 'react'
import { api, type PlaylistResult } from './api'

export default function PlaylistReader() {
  const [link, setLink] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<PlaylistResult | null>(null)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!link.trim()) return
    setLoading(true)
    setError(null)
    setResult(null)
    try {
      setResult(await api.readPlaylist(link))
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
            <button className="button" type="submit" disabled={loading || !link.trim()}>
              {loading ? 'Reading…' : 'Read playlist'}
            </button>
          </div>
        </form>
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
      </section>

      {result && <PlaylistView result={result} />}
    </>
  )
}

function PlaylistView({ result }: { result: PlaylistResult }) {
  const { playlist, readWith, tracks, skipped } = result
  const missingIsrc = tracks.filter((t) => !t.isrc).length

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
            By {playlist.owner.display_name}
            {playlist.collaborative && ' · collaborative'} · read using {readWith.displayName}'s
            connection
          </p>
          <ul className="stats">
            <li>
              <strong>{tracks.length}</strong> songs
            </li>
            <li>
              <strong>{tracks.length - missingIsrc}</strong> with ISRC
            </li>
            {missingIsrc > 0 && (
              <li className="warn">
                <strong>{missingIsrc}</strong> without ISRC (will need a name search)
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

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th className="num">#</th>
              <th>Title</th>
              <th>Artist</th>
              <th className="hide-narrow">Album</th>
              <th className="num">Length</th>
              <th>ISRC</th>
            </tr>
          </thead>
          <tbody>
            {tracks.map((t) => (
              <tr key={t.position}>
                <td className="num muted">{t.position}</td>
                <td>
                  {t.name}
                  {t.explicit && <span className="badge" title="Explicit">E</span>}
                </td>
                <td>{t.artists.join(', ')}</td>
                <td className="hide-narrow muted">{t.album}</td>
                <td className="num muted">{formatDuration(t.durationMs)}</td>
                <td className="mono">{t.isrc || <span className="warn">missing</span>}</td>
              </tr>
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

function formatDuration(ms: number): string {
  const totalSeconds = Math.round(ms / 1000)
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${minutes}:${seconds.toString().padStart(2, '0')}`
}
