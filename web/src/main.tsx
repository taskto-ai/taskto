import './ui/index.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { initLocale } from './i18n'
import { listenForInstall } from './pwa/install'
import { UpdateBanner } from './pwa/UpdateBanner'
import { registerServiceWorker } from './pwa/update'
import { trackKeyboard } from './pwa/viewport'
import '@fontsource-variable/bricolage-grotesque/wght.css'
import '@fontsource-variable/inter/wght.css'
import '@fontsource-variable/jetbrains-mono/wght.css'
import './styles.css'
import { applyTheme, storedTheme } from './theme'

const GALLERY_PATH = '/_gallery'

const root = document.getElementById('root')
if (!root) throw new Error('index.html has no #root element')

applyTheme(storedTheme())
await Promise.race([initLocale(), new Promise((resolve) => setTimeout(resolve, 1500))])

listenForInstall()
trackKeyboard()

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

  registerServiceWorker()
}
