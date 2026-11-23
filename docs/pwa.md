# The installable app (PWA)

TaskTo.AI's web app can be added to a phone's Home Screen or a computer's apps, opens in a
window of its own, and starts offline. It stays a normal website: everything below is an
addition the browser may ignore, and the app works the same in a tab. How to set up a server
for it (HTTPS) and what people do is in [install.md](install.md#the-app-on-phones-and-computers).

## What is installed

- **The manifest** (`web/public/manifest.webmanifest`): a stable identity (`id`, `start_url`
  and `scope` all `/`), `display: standalone`, the theme and background colours, and icons:
  the mark in 192 and 512 pixels, a **maskable** 512 (a full-bleed background with the mark
  inside the central 80%, which launchers crop a shape from) and an SVG. `web/index.html`
  links it, the iOS touch icon, and `theme-color`. The icons are drawn from one mark by
  `web/scripts/make-icons.mjs` (the PNGs are committed, so a build needs no browser). The
  manifest's names are the product's own, the same in every language.
- **The service worker** (`web/pwa/sw.template.js`, built to `/sw.js`).
- **The page's side** (`web/src/pwa/`): registration and updates (`update.ts`, `UpdateBanner`),
  the install guidance (`install.ts`, `InstallPanel`), and the keyboard (`viewport.ts`).

## The service worker

The build (`web/pwa/plugin.ts`, a Vite plugin that runs last) lists the static files of the
build: the page, the manifest, the icons, and **every** script and stylesheet under `assets/`,
the lazy-loaded ones too (the apps renderer, the language files). It writes the template to
`dist/sw.js` with that list and a version, a hash of the files' content. The noVNC files of the
agent browser's live view, source maps and the worker itself are left out.

- **Install:** downloads every file of the list into one cache, `taskto-shell-<version>`,
  past the browser's HTTP cache. If any fails, the install fails and nothing is kept: a worker
  that is installed has a *complete* app, so the app counts as ready for offline use only once
  the lazy-loaded renderer is there too.
- **Files:** a request for a file of the list, or any other under `/assets/`, is answered from
  the cache (this version's, then the previous one's), and goes to the network when it isn't
  there.
- **Page navigations:** *network first*. Online, the server answers, so a link to an app route
  (`/w/3/boards`, `/invite#…`) is always the server's page and the router keeps the route. Only
  when the network fails does the worker answer with the cached page, which the app's router
  then draws for that route. The server's own routes (`/api`, `/mcp`, `/cli-mcp`, `/healthz`)
  are never answered by the worker, offline included.
- **Never:** the worker stores only at install, from its list, and answers only those static
  files and page navigations. Nothing under `/api` is ever fetched, stored or answered by it:
  people's conversations, documents, boards and apps are not cached by the worker, so nothing of
  theirs stays in its cache after a logout. Other requests (non-`GET`, other origins) aren't
  touched. (The app itself keeps in-memory state and some `localStorage`, as it always did.)
- **Activate:** keeps this version's cache and the one before it. A page of the previous
  version may still be open in another tab and load lazy chunks by their old names, which the
  server no longer has; they stay in the previous cache until the next update.

The server serves `sw.js` and the manifest with `Cache-Control: no-cache` and an ETag, like the
page, and the manifest as `application/manifest+json` (`internal/webui`); hashed assets stay
cached for a year. `sw.js` is registered with `updateViaCache: 'none'`.

## Updates

A new version of the app installs in the background, then **waits**: the worker doesn't call
`skipWaiting()` by itself, and the page doesn't reload by itself. `UpdateBanner` says *A new
version of TaskTo.AI is ready* with **Reload to update** and **Later**. Only **Reload to update**
sends the worker a `SKIP_WAITING` message; once it has taken over (`controllerchange`) the page
that asked reloads. **Later** hides the prompt until the page is opened again. An open
installed app checks for a new version when it comes back to the foreground (at most every
half hour); the browser checks at each navigation too. Another open tab is never reloaded for
a tab that updated.

Registration happens after the page has loaded, in production builds, in a secure context
only. `vite dev` serves no worker.

## Phones: layout and keyboard

- **Safe areas:** `viewport-fit=cover`, and the top bar, the page, the drawer, the composer and
  the setup and login screens pad by `env(safe-area-inset-*)`, so nothing is under the notch,
  the status bar or the home bar. On iOS the installed app's status bar is translucent over the
  top bar.
- **Touch targets:** where the pointer is coarse, every control is at least 44 × 44 CSS pixels
  (`--tt-touch`). The app is laid out down to a 360-pixel viewport.
- **Keyboard:** inputs are 16 px on phones (iOS zooms in on smaller ones). Chrome on Android
  resizes the page for the on-screen keyboard (`interactive-widget=resizes-content`). iOS Safari
  doesn't, so while its visual viewport is much shorter than the window (and not zoomed) the app
  is sized to it (`viewport.ts`, `html[data-keyboard]`), which keeps the composer above the
  keyboard and the top bar in view.
- **Focus:** a skip link to the page; the drawer takes focus when it opens, makes the page behind
  it inert, closes on Escape and gives focus back to its button; a page picked in it takes focus.
- **Pull to refresh** is off in the installed app (`overscroll-behavior`), so a stray pull can't
  reload a page with something typed.

## What the tests don't cover

The tests run in jsdom and in a desktop Chromium that emulates a phone's viewport and touch.
Only a real device shows: that iOS offers Add to Home Screen and opens the installed app
under a translucent status bar with the right insets; that Android's install prompt and
maskable icon look right; that the keyboard handling (`viewport.ts`) keeps the composer and
top bar in place on iOS; that the safe areas clear a notch in landscape; that an update
installed while the app is in the background prompts when it returns; and that the app opens
offline after a phone's own restart.
