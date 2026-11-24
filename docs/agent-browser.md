# Agent browser: a browser per user, a live view and hand-over

The agent can use a real browser for the user, and the user can step in when a site needs
them. This note is the design. It builds on the CLI workers' OS boundary
([cli-workers.md](cli-workers.md)): the browser runs in the same worker, as the same
Unix user, under the same rules.

The design in short:

- a headed browser on a virtual display per user;
- a channel to the browser that only that user can open;
- a live view over VNC, shown with noVNC in the page;
- an "ask for help" step with Take over, Done and Not now;
- stopping the browser when idle.

## What the user gets

- **A personal browser.** It has its own profile, kept between runs: cookies, logins and
  history. It runs headed on a virtual display, since sites block headless browsers more often.
- **A live view of it in the page.** It is a VNC stream drawn by noVNC, not a screenshot loop.
  The user can look on while the agent works, or take over with mouse and keyboard.
- **Hand-over.** On a login, captcha or 2FA the agent calls `browser_help` and ends its turn.
  The page shows a card with **Take over** (opens the live view), **Done** and **Not now**.
  Done or Not now sends a message to the conversation, so the agent carries on from there.
- **A testing browser.** It is clean and headless, with no profile: a new temporary one each
  time it starts, deleted when it stops. It is for trying apps, with nothing of the user's in it.

Both stop after a few idle minutes (default 5) and start again on the next request. An open
live view counts as use.

A live view lasts as long as the login that opened it. The session is checked when the
WebSocket opens, and again every few seconds while it is open (`auth.Service.WatchSession`), and
the auth service also ends the views of an account at once when it is disabled and of a login when
it is signed out or replaced by a password change or reset: a closed login can't go on typing into
the user's browser.

## Processes and the OS boundary

```
browser page ── WebSocket /api/me/browser/live (session, Origin check) ──▶ Go server
                                                                            │
         container mode: HTTP upgrade on the worker's socket (only the server opens it)
         local mode:     the worker in the server process
                                                                            ▼
                                                                         worker (Go)
              helper as the user (taskto cli-helper), one per process ──────┤
                                                                            ▼
   Xvfb :N ◀── Chromium (CDP on fds 3/4: a pipe the worker holds) ── x11vnc ── <home>/browser/run/vnc.sock
```

- **Everything runs as the user's Unix user.** The worker starts Xvfb, Chromium and x11vnc
  through the helper, like a CLI turn. The helper prepares the user's browser directories and
  execs the program with a clean environment. The worker, as root, never opens a path in a
  home, so a user's symlinks can't steer it. There is no sudo and no setuid program, and a
  browser process has no capabilities.
- **Per-user directories** are in the user's own directory (mode 700). `browser/profile/` is
  the personal profile. `browser/run/` holds the X cookie and the VNC socket. `browser/testing-*/`
  is the testing browser's throwaway profile.
- **Driving the browser.** Go speaks the Chrome DevTools Protocol to Chromium over
  `--remote-debugging-pipe`, a pipe the worker holds. Chromium has no debugging port, so no
  other process can reach it.
  - Playwright isn't used. It would need Node in the worker and a debugging endpoint. The
    rules allow no new Node service, and a TCP port in a shared container is reachable by every
    user.
  - The CDP client is small: commands, replies and the few events we wait for.
- **The display** is Xvfb with `-displayfd` to choose a free display. `-auth` takes a random
  cookie in `browser/run/` (mode 600). `-nolisten tcp -nolisten local` leaves no TCP socket
  and no abstract socket, and an abstract socket has no permissions. The socket in
  `/tmp/.X11-unix` is world-reachable, as on any system, but it refuses a client without the
  cookie (checked: another user's X setup is refused).
- **VNC.** x11vnc serves the display on `browser/run/vnc.sock` only: `-unixsock`,
  `-rfbport 0` and `-rfbportv6 -1`. With `-rfbport 0` alone, and even with `-noipv6`, x11vnc
  0.9.17 still listens on TCP 5900 over IPv6, where any user could connect (checked). The
  isolation tests check that the browser's processes listen on no TCP port. There is no VNC
  password: the socket's directory (mode 700) is the lock.
- **The live view's bridge.** For each live view the worker starts the helper as the user. The
  helper connects to the user's own `vnc.sock` and copies bytes between it and its stdin and
  stdout. The worker joins that to the server's connection.
  - The browser page never reaches the worker. Its WebSocket ends at the Go server, which
    checks the session and the Origin, then asks the worker for that user's stream.
  - Cross-site WebSocket hijacking is blocked by the Origin check. The upgrade is a GET, which
    the CSRF guard doesn't cover.
- **Chromium's sandbox.** Inside the worker container, Chromium runs with `--no-sandbox`. Its
  sandbox needs user namespaces, which Docker's default seccomp profile refuses to an
  unprivileged container, and `no-new-privileges` rules out the setuid sandbox. So a renderer
  exploit gets the user's Unix user: exactly what a CLI turn already has, inside the same
  boundary. That includes everything in the user's own directory, their CLI logins among
  them, but nothing of another user's, nor the worker's root (no capabilities, no setuid
  program). In local mode the sandbox stays on.
- **Profile lock.** Chromium refuses a profile whose `SingletonLock` names another host, which
  a recreated container is. The worker guarantees one personal browser per user, so the helper
  removes stale `Singleton*` files before it starts Chromium.
- **Stopping.** `Browser.close` over CDP first, so cookies are written. Then each process
  group gets SIGTERM, and SIGKILL after 5 s. The worker records the groups like turns. When it
  starts, it stops what an earlier worker left: by uid in container mode, by recorded group in
  local mode.
- **Limits.** At most `-browser-max` browsers run at once (default 4). A start over the cap
  stops the least recently used browser that has no live view open. If every one has a live
  view, it is refused. At most 4 live views of one browser are open at once.

## Modes

- **`-browser`** (default `off`) turns the feature on. It needs CLI workers, since the worker
  is the boundary. It is usable under the same rule as they are: container mode, or local mode
  on a one-user instance (`local-unsafe` for development).
- **Container mode.** The worker image adds Debian's `chromium`, `xvfb`, `x11vnc` and
  `fonts-liberation`. The worker reports whether it has them (`GET /available`).
- **Local mode.** The browsers run in the server process (`browser.LocalHost`), as its own OS
  user, in each user's CLI directory (`<data>/cli/u<id>/browser`). That is the same boundary
  the helper gives CLIs in this mode, which is none between users, hence the one-user rule.
  - The personal browser needs `chromium` (or `chromium-browser`), `Xvfb` and `x11vnc` on the
    PATH. The testing browser needs only Chromium.
  - On macOS only the testing browser can work, through `-browser-chromium` pointing at a
    Chromium or Chrome binary.
  - The sandbox stays on, so a server running as root can't start Chromium in this mode.

## The agent's tools

They are in `internal/agenttools/tools.json`, run by `internal/agentbrowser`, and listed in
[agent-tools.md](agent-tools.md#the-browser-tools).

- **Who has them.** They are for the conversation agent only (`conversation_only`), not over
  MCP: the hand-over belongs to a conversation. The sidecar offers them only while `-browser`
  is on.
- **Whose browser.** Every call acts on the conversation owner's browser; the owner comes from
  the capability, never from an argument.

| Tool | What it does |
|---|---|
| `browser_tabs` | The open tabs: id, title, URL, and whether the agent opened it |
| `browser_open` | Opens a URL in a new tab, starting the browser if needed (`browser`: `personal` or `testing`) |
| `browser_navigate` | Loads a URL in a tab |
| `browser_snapshot` | The page as text: title, URL, visible text, and interactive elements with refs. A page with a password field, in the personal browser, comes with a note to call `browser_help` |
| `browser_click` | Clicks the element with a ref (real mouse events at its centre) |
| `browser_type` | Types text into the element with a ref, optionally pressing Enter |
| `browser_press` | Presses a key (Enter, Tab, Escape, arrows, Backspace) |
| `browser_close` | Closes a tab the agent opened. The user's own tabs are left alone |
| `browser_help` | Asks the user to take over (login, captcha, 2FA) and brings the tab to the front |

- **Refs** live in a CDP isolated world, out of the page scripts' reach. They are the last
  snapshot's. A ref from before a navigation is refused with "take a new snapshot".
- **Not done on purpose:** no flags or scripts to hide automation, no captcha solving, no
  storing passwords for the agent. The user signs in themselves, in the live view.
- **CLI conversations** (Claude Code, Codex, Grok) get these tools over MCP, with a token
  for each turn, while `-browser` is on ([cli-tools.md](cli-tools.md)). `browser_tabs`,
  `browser_snapshot` and `browser_help` don't ask; the others ask the conversation's owner, as
  any write does. A CLI's `browser_help` shows the same card, kept with its turn.

## Checked with a real model

`make browser-handover-e2e` (`web/e2e/browser-handover.e2e.mjs`) runs the hand-over end to
end with a real model. It is opt-in (`TASKTO_HANDOVER_E2E=1`) and never runs in CI, since it
builds and runs the Compose stack and calls a model.

- **The stack.** Container mode with `-browser` on, built from the checkout. A fake site with a
  login (`web/e2e/fakelogin.mjs`) runs in the CLI worker's network namespace, so the browser
  reaches it on `127.0.0.1:8000` whatever network the worker is on. Its dashboard shows a random
  access code, only after signing in with a password only the test knows.
- **The model.** It is a provider account of whoever runs the test, on an OpenAI-compatible gateway:
  `TASKTO_HANDOVER_E2E_BASE_URL`, `TASKTO_HANDOVER_E2E_KEY_FILE` and `TASKTO_HANDOVER_E2E_MODEL`.
  The key goes from its file to TaskTo's API only. `TASKTO_HANDOVER_E2E_RELAY=1` relays TCP to the
  gateway from the host, for a Docker VM that can't reach it (a VM-based Docker and a gateway only on the LAN); TLS
  still ends at the gateway.
- **The user.** Playwright drives the web app in Chromium:
  - it asks the agent for the access code, saying nothing about signing in;
  - it waits for `browser_help`'s card, and checks the agent hasn't tried to sign in;
  - it presses Take over and signs in through noVNC: a click on the username field, then keys;
  - it presses Done and waits for the agent's answer, which must hold the code.
  The site's log must show exactly one sign-in, the user's.
- **Teardown.** The containers, volumes, network and built images all go, unless
  `TASKTO_HANDOVER_E2E_KEEP_IMAGES=1`.

Results on 2026-11-19/20, arm64 (Docker in a VM), with a gateway to providers' free tiers:

| Model | Result |
|---|---|
| `auto` (the gateway's router) | Passed four times in a row, in 16 to 47 s, the last two with the worker on its own network. The model opened the site, took a snapshot, called `browser_help` (reason login), and ended its turn. After Done it took a new snapshot and answered with the code |
| `gpt-oss-120b` | Passed once in an earlier manual run of the same flow. In the next run, before the note below, it pressed Sign in on the empty form and then asked for the password in the chat. A later run hit the gateway's daily quota (429) |
| `llama-3.3-70b`, `mistral-small-3.2-24b`, `deepseek-v4-flash` | The gateway answered 429 after one or two calls. This is the free tiers' quota, not the code |

What it changed:

- **A note with sign-in pages.** `browser_help`'s description alone wasn't enough for every model.
  A snapshot of the personal browser with a password field now comes with a note: call
  `browser_help`, don't ask for the password in the chat, and don't submit the form.
- **Chromium offers to save the password.** After the user signs in, Chromium's password manager
  asks to save it in the personal profile. The agent never sees that prompt's contents, but the
  saved password would sit in the profile, on the worker's disk. Turning the password manager off
  for the personal browser is an open follow-up.

## API

All under `/api/me/browser`, for the signed-in user's own browser only, like
`/api/me/cli-accounts` (details in [api.md](api.md)):

- `GET /api/me/browser`: whether it is enabled (and why not), whether each browser is running,
  the tabs, and when it stops if idle;
- `POST /api/me/browser/start` and `POST /api/me/browser/stop` (`{"browser": "personal"}`);
- `GET /api/me/browser/live`: the live view's WebSocket (binary RFB). It checks the session,
  checks that the Origin is this server, and needs the personal browser to be running.

Every route has a test that one user can't see or reach another's browser. The isolation tests
(root, Linux) cover what one user's process tries against another's:

- the VNC socket;
- the X display without the cookie;
- the profile;
- signalling the browser;
- a TCP port.

## Licences

| Component | Licence | How it is used |
|---|---|---|
| Chromium (Debian `chromium`) | BSD-3-Clause, with third-party parts under their own licences (in its `copyright` file) | A separate program in the worker image |
| Xvfb (Debian `xvfb`) | MIT/X11 | A separate program |
| x11vnc | GPL-2.0 | A separate program, not linked: talked to over a socket |
| noVNC 1.7.0 (`@novnc/novnc`) | MPL-2.0 (pako inside it: MIT) | Its unmodified `core/` and `vendor/` files are copied to `web/dist/vendor/novnc/` with `LICENSE.txt`, and loaded as ES modules. They are not bundled into our code |
| `golang.org/x/net/websocket` | BSD-3-Clause | The live view's WebSocket. Already in the module graph |

Chromium is used rather than Google Chrome:

- Chrome's terms don't let us ship it in our image;
- Debian builds Chromium for amd64 and arm64 with security updates;
- the cost is that Chromium has no proprietary codecs or Widevine, so some media and DRM sites
  won't play, and a few sites treat it differently from Chrome.

## Slices

1. This note.
2. `internal/browser`: the CDP client, tabs and page actions, the per-user manager with idle
   stop and the cap. It runs Chromium directly, and has tests against a real Chromium in an
   opt-in CI job.
3. Container mode:
   - the helper's browser ops and the VNC bridge;
   - the worker's routes and the server's client;
   - the image's packages;
   - the isolation tests as root.
4. The server:
   - `/api/me/browser` and the live view's WebSocket;
   - the agent's browser tools;
   - `-browser`;
   - cross-user tests and docs.
5. The web app:
   - the live view with noVNC;
   - the browser panel in a conversation;
   - the help card.

## Open questions

Defaults taken, still open to change:

1. **Off by default.** An agent that browses with the user's logins is a choice an operator
   makes, so `-browser` is opt-in.
2. **Network reach.** The browser reaches what the worker container reaches, as the CLIs'
   shells do: the internet and the server's per-turn proxy, nothing else. The
   worker is on a network of its own, closed to the other services, the host and its LAN
   ([cli-workers.md](cli-workers.md#the-workers-network)).
3. **A persistent profile per user, shared by their conversations.** It keeps the user's
   logins between conversations, at the cost of one conversation seeing what another left
   there, so it is a trade-off rather than a safe default. The testing browser is the clean
   alternative.
4. **Headed, not hidden.** Chromium runs headed because sites block headless browsers more often. Nothing
   tries to hide automation.
