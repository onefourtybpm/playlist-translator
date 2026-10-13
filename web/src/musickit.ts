import { api } from './api'

// Apple's MusicKit JS v3, loaded from Apple's CDN on first use. Only the small part of its API
// this app uses is typed here.
const scriptUrl = 'https://js-cdn.music.apple.com/musickit/v3/musickit.js'

export type MusicKitInstance = {
  isAuthorized: boolean
  storefrontId: string
  developerToken: string
  musicUserToken: string
  authorize(): Promise<string>
  unauthorize(): Promise<void>
  api: {
    music(
      path: string,
      query?: Record<string, string>,
      options?: { fetchOptions?: RequestInit },
    ): Promise<{ data: unknown }>
  }
}

type MusicKitGlobal = {
  configure(options: { developerToken: string; app: { name: string; build: string } }): Promise<MusicKitInstance>
  getInstance(): MusicKitInstance
}

declare global {
  interface Window {
    MusicKit?: MusicKitGlobal
  }
}

let instance: Promise<MusicKitInstance> | null = null

// getMusicKit loads and configures MusicKit JS once, with a developer token from our server.
export function getMusicKit(): Promise<MusicKitInstance> {
  instance ??= load().catch((e) => {
    instance = null // let a later click retry
    throw e
  })
  return instance
}

async function load(): Promise<MusicKitInstance> {
  const [{ token }] = await Promise.all([api.appleDeveloperToken(), loadScript()])
  return window.MusicKit!.configure({
    developerToken: token,
    app: { name: 'Playlist Translator', build: '1.0.0' },
  })
}

function loadScript(): Promise<void> {
  if (window.MusicKit) return Promise.resolve()
  return new Promise((resolve, reject) => {
    document.addEventListener('musickitloaded', () => resolve(), { once: true })
    const script = document.createElement('script')
    script.src = scriptUrl
    script.async = true
    script.onerror = () => reject(new Error("Couldn't load Apple's MusicKit script."))
    document.head.appendChild(script)
  })
}
