import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { initLocale } from './i18n'
import './styles.css'
import { applyTheme, storedTheme } from './theme'

const root = document.getElementById('root')
if (!root) throw new Error('index.html has no #root element')

// Before the first render, so a chosen theme doesn't flash the system's.
applyTheme(storedTheme())
// And in the user's language from the first paint: their last pick here, or the browser's.
// Its file is small, but a stalled network mustn't hold the app back: after a moment the app
// starts in English and switches when the file arrives.
await Promise.race([initLocale(), new Promise((resolve) => setTimeout(resolve, 1500))])

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
