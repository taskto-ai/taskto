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
  links it, the iOS touch icon, and `theme-color`. The colours and the icons' tile are the
  website's brand ([design.md](design.md#installed-app)). The icons are drawn from one tile by
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
agent browser's live view, source maps and the worker itself are left out. The fonts' `.woff2`
files (about 370 kB, [design.md](design.md#fonts)) are in the list, so the app is set in them offline.

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
- **Offline use of apps** ([offline.md](offline.md)) rests on this: the offline view and the apps
  renderer are lazy chunks, which the install fetches with the rest, so a downloaded app can be
  drawn with no network. The downloaded records are not in the worker's cache but in IndexedDB,
  one database per signed-in user, which the page writes and the worker never touches.
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

## Updates and the offline database

The page's IndexedDB schema is versioned, and an update that waits for the person means a page and
its database move together. A database that is newer than the page (another tab already updated)
is not opened for writing: the page says the app must be updated, and the queue of records made
offline is never rewritten by code that doesn't know its shape ([offline.md](offline.md#updates-and-the-schema)).

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

## Notifications

Two parts, the second optional:

- **The inbox** (`web/src/notifications/`, the bell in the top bar and `/w/{workspace}/notifications`)
  lists what happened that a person may want to see: a card assigned to them, an action of an app
  that failed for good and waits for their retry. It works in every browser, push or not, and is
  what is true. The bell's count is asked for when the app opens, when it comes back to the
  foreground, when something was read here, when a push reached the worker, and once a minute while
  it is shown.
- **Web Push** tells a device about a new notification when the app isn't open. It is
  feature-detected (`PushManager`, `Notification`, a worker and a secure context are all asked for,
  never the user agent), explicitly opted into, and never required. **Nothing is subscribed, asked
  or pushed until the person clicks "Turn on push on this device" in Settings → Notifications**;
  the browser's permission prompt comes from that click and from nowhere else. A browser that can't
  push, or whose person said no, is told so (with how to allow it, if it was blocked) and keeps the
  inbox. **On iOS push works only in the app installed on the Home Screen** (16.4 or later): in a
  tab the page says so and offers no switch.

A push is a prompt to look at the inbox. It is **not a delivery guarantee and not a way to sync**:
the push service carries it with a lifetime of an hour, and it can arrive late, twice or never; the
app learns what is new from the server, as always, and never from a push. A push that waited past
its lifetime, or for a notification that was read meanwhile, is not sent.

**What a push says.** The worker shows `TaskTo.AI` with one line for the kind of notification, in
the person's language, and nothing else. The server sends only `{"id", "kind"}` (the
notification's own ID, and its kind: `card_assigned`, `card_due`, `card_overdue`, `app_effect_failed`, or `changed`), encrypted
to the browser (RFC 8291) and signed with the server's VAPID key (RFC 8292), both done with the
standard library (`internal/notifications/webpush.go`). No title, no name, no ID of a card or app.
The worker can't load the app's translations, so the page gives it the three lines when push is
turned on and when the language changes; they are kept in a cache of their own
(`taskto-push-strings`), which an update of the app leaves alone and which holds no one's data.

**Access is checked when it is told, not when it was made.** A notification holds a kind and IDs.
What an inbox item says (a card's title, an app's name) is worked out when it is read, and the
payload's kind when it is pushed, from what the recipient may read **then**. A person who lost
access to the space, the card or the app is told only that "something changed", in the inbox and in
the push, with no names, no IDs and a link to the workspace's home.

**The click.** A click on a push opens `/notification/{id}` (the open app's window is focused and
sent there, without a reload, by a message the app checks; else a new window). A window that shows
the login is sent there too, and the address stays through signing in, so the click isn't lost (the
screens that hold a one-time token in their address, an invite, a password reset and the first
setup, ignore the message). The app reads that
notification as the signed-in person, marks it read and goes where it leads, an ordinary route
(`/w/1/boards/3:4:9`) whose page checks access again. Someone else's notification, or one that is
gone, is "not available". Signed out, the address is kept through the login. The server answers
`/notification/{id}` with the app, like any route (a page navigation, network first).

**When subscriptions end.**

- The person turns push off on that device (the server forgets it, the browser unsubscribes). If
  either fails, push stays on, as it was, Settings says why and the button tries again: it never
  says "off" while this browser is still subscribed.
- They **sign out** of that browser: the web app removes the device first, and the server removes a
  subscription with the session it is bound to, so expiry, being signed out elsewhere and a
  password change end it as well. The next sign-in turns push on again only for the same person,
  and only if they had turned it on; a browser never hands its subscription to whoever signs in
  next (the app keeps a small flag of whose it is, and registers it again at start only for them).
  A session that expires ends its subscription even if nobody signs in: no push is sent to one
  after its session's expiry, and the server removes them every hour.
- The push service answers a push with `404` or `410`.
- They leave their last workspace. Leaving one of several ends that workspace's notifications
  (and the pushes queued for them) and no more. A person in no workspace can't register a device,
  and the app doesn't try when it opens.

### Email

Email is the third way to be told, after the inbox and push, and is **off for everyone** until a
person chooses, in Settings → Notifications, for each kind of notification and each workspace:
*email me soon after*, *include in a daily summary*, or neither. It needs a server that can send mail
and knows its own address ([install.md](install.md#email): `smtp_host` and `public_url`); without
them the choices can't be turned on, and every choice can still be turned off.

**A confirmed address.** The first time a person turns an email choice on (and again if their address
changes), a link is mailed to the address on their account. Nothing else is mailed to it until they
have opened the link while signed in: a typo in an address can't be sent notifications. The link
works once, expires after 24 hours, is limited to one every 5 minutes, and holds a random token that
the server stores only hashed and never logs. The settings show whether the address is confirmed,
waiting for the link, or not, and can send another link.

A mail says what a push says and no more: that a card was assigned to you, or that an app action
needs you, in the person's language, and a link back to the notification (`/notification/{id}`), which
the app opens only for the signed-in person and checks again. It never holds a title, a name, a
comment or the ID of a card or an app. The summary says only how many notifications are unread and
links to the inbox; it counts none the person may no longer read, and so does an instant mail:
access is checked when the mail is made, not when the notification was. A summary counts what came
after the kind was turned to it, and one notification in one summary only. At most 20 instant mails
go to a person in an hour (the rest are in the inbox); a notification that was read, or is older than
a day, is not mailed. The summary is sent at the hour (and in the time zone) the person chose, once a
day, by the [general scheduler](api.md#notification-email); each run schedules the next from the
person's own date, so daylight saving time doesn't move it. Leaving the workspace ends it.

**At most once.** A mail that arrives twice is worse than one that doesn't (the inbox has it either
way), so before a message is handed to the SMTP server the attempt is written to a ledger, under a
key for what it is (the notification, or the summary's occurrence). A key with a row is not sent
again, except when the server definitely refused it (a reply code, a refused connection or sign-in):
that is tried again, a few times, with growing waits. When the outcome is unknown (the server stopped
with the attempt written, or the connection was lost after the message was sent) it is counted as
sent and never repeated, so in that rare case a mail may be lost. The hourly limit counts the same
ledger, so concurrent sends can't pass it and deleting notifications doesn't reset it. Right before
the submission, the choice, the address, the person's membership and access to what it is about (and
for a summary, that it is still the person's current one) are checked again, so opting out or
losing access cancels a mail that is already on its way. Instant mails are sent by a worker of
their own, so a slow SMTP server never delays a push.

The server side is `internal/notifications` ([api.md](api.md#notifications)); what a self-hosted
server must reach is in [install.md](install.md#web-push-notifications).

## What the tests don't cover

The tests run in jsdom and in a desktop Chromium that emulates a phone's viewport and touch.
Only a real device shows: that iOS offers Add to Home Screen and opens the installed app
under a translucent status bar with the right insets; that Android's install prompt and
maskable icon look right; that the keyboard handling (`viewport.ts`) keeps the composer and
top bar in place on iOS; that the safe areas clear a notch in landscape; what a browser keeps of
IndexedDB after a restart or under storage pressure (the rest is in [offline.md](offline.md#what-the-tests-dont-cover)); that an update
installed while the app is in the background prompts when it returns; and that the app opens
offline after a phone's own restart.

Push is tested without a push service of a browser's maker: the Go tests run a test push service
and decrypt what it receives as a browser would (including RFC 8291's own example), the web tests
fake the browser's push objects, and the worker's tests drive its `push` and `notificationclick`
handlers. Only a real device shows: that Chrome, Firefox and Safari accept the server's VAPID
identity and deliver, that an iPhone shows the notification of the installed app (and in what
language), that a click opens the right window, and what each push service does with a late or
repeated message.

## Device capabilities in apps

Personal apps can ask the phone for a few things (field types and their rules are in
[api.md](api.md#location-signature-and-barcode-fields)). Each is asked for **on a person's action, in the
foreground, once**, is feature-detected, and has a way round that is always on screen when the feature
isn't. Nothing runs in the background, and nothing here is needed to use an app.

| What | Feature used | When it can't be used | Offline |
|---|---|---|---|
| Location | `navigator.geolocation.getCurrentPosition`, once, on *Capture location* or when a form whose field says `capture: "submit"` is submitted. Never `watchPosition` (a test searches the source for it), never a background or periodic read. The accuracy and the device's time of the reading are kept. | Denied, unsupported, an insecure page, no fix, a timeout: each has its own message and the person **types the coordinates**, stored as entered by hand. A form that needs a location and can't get one isn't sent. | Works where the device has a fix (GPS needs no connection); the record is saved when the app can reach the server. |
| Signature | A canvas with pointer events and `touch-action: none` **on the pad only**, so the page scrolls everywhere else. Strokes are kept as fractions of the pad's size, so turning the device redraws them. | A **typed name** confirmed with a checkbox, for the keyboard, switch devices and screen readers. | Saving uploads a PNG to the server; offline, the picture is kept on the device with the record, like a photo, and sent with it ([offline.md](offline.md)). |
| Barcode and QR | The camera (`getUserMedia`) with the browser's own `BarcodeDetector` when it has one (it isn't in every browser), else zxing-wasm (MIT; it is ZXing-C++, Apache-2.0, compiled to WebAssembly) through the `barcode-detector` package (MIT), loaded the first time and served from the app's own assets, never from a CDN. Their licence notices are built into `/assets/THIRD_PARTY_NOTICES.txt`, beside the `.wasm` (see below). | **Typed entry**, always shown: a blocked, missing or insecure camera, or a decoder that can't load or can't run (it says it isn't supported, or fails every frame from the start): the camera is turned off and the field says why. | Scanning works offline: the decoder's `.wasm` (about 950 kB) is one of the precached files. |
| Dictation | The chat box's recorder (`getUserMedia`, 16 kHz WAV) and the workspace's transcription route ([dictation.md](dictation.md)), beside free-text fields. | Off, with the reason, where the workspace has dictation off or the microphone is blocked; **the keyboard's own dictation key** still works and is named in the note. | **Needs a connection**: the speech goes to the server. Offline, the button is disabled and says so. |

Browsers show their own prompt for the location, the camera and the microphone, and remember the answer
per site; what the app adds is the words for each refusal. The server's side checks every value again
(`internal/apps/capture.go`), whatever a client sent.

**Licences of what is shipped.** The decoder is the one third-party code the build adds for these fields:
`barcode-detector` (MIT), `zxing-wasm` (MIT) and ZXing-C++ (Apache-2.0), which the `.wasm` file is. Each licence
asks for its notice to go with copies of the code, so the build writes `assets/THIRD_PARTY_NOTICES.txt`
(`web/pwa/notices.ts`) from the installed packages' own `LICENSE` files and their versions, and the Apache-2.0 text kept
in `web/licenses/`, and the server serves it next to the decoder's `.wasm`. The service worker does not precache it.
A test checks that every notice is in it in full.
