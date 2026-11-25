// First of all, the design system's entry: the build emits CSS in the order modules import it, and
// ui/index.css declares the cascade layers' order (tokens, themes, base, components, screens). A
// component imported before it (through App) would put its @layer components block first, and
// make @layer base win over it (css-check fails when this is not the first import).
import './ui/index.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { initLocale } from './i18n'
import { listenForInstall } from './pwa/install'
import { UpdateBanner } from './pwa/UpdateBanner'
import { registerServiceWorker } from './pwa/update'
import { trackKeyboard } from './pwa/viewport'
// The brand's fonts, from the app's own files (SIL OFL 1.1, licences in /assets/THIRD_PARTY_NOTICES.txt):
// Inter for the interface, Bricolage Grotesque for headings (its weight axis only: the optical-size
// axis is for display sizes the app has none of), JetBrains Mono for code.
import '@fontsource-variable/bricolage-grotesque/wght.css'
import '@fontsource-variable/inter/wght.css'
import '@fontsource-variable/jetbrains-mono/wght.css'
// Then the old screens' stylesheets, after the fonts.
import './styles.css'
import { applyTheme, storedTheme } from './theme'

const GALLERY_PATH = '/_gallery'

const root = document.getElementById('root')
if (!root) throw new Error('index.html has no #root element')

// Before the first render, so a chosen theme doesn't flash the system's.
applyTheme(storedTheme())
// And in the user's language from the first paint: their last pick here, or the browser's.
// Its file is small, but a stalled network mustn't hold the app back: after a moment the app
// starts in English and switches when the file arrives.
await Promise.race([initLocale(), new Promise((resolve) => setTimeout(resolve, 1500))])

// The install prompt may fire before anything draws; the keyboard is followed from the start.
listenForInstall()
trackKeyboard()

// The design system's gallery (ui/Gallery.tsx), on the dev server only: a production build drops
// this branch and the chunk it would load, so the gallery is not in the app people get.
if (import.meta.env.DEV && window.location.pathname === GALLERY_PATH) {
  const { default: Gallery } = await import('./ui/Gallery')
  createRoot(root).render(
    <StrictMode>
      <Gallery />
    </StrictMode>,
  )
} else {
  createRoot(root).render(
    <StrictMode>
      <App />
      <UpdateBanner />
    </StrictMode>,
  )

  // Once drawn: the offline shell, and its updates (src/pwa/update.ts).
  registerServiceWorker()
}
