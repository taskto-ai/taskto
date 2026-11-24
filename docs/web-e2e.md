# Web app end to end tests

The web app has unit tests (Vitest, in `web/src/`), and on top of them a handful of
**end to end tests**: a real Chromium, driven by [Playwright](https://playwright.dev), against the
real server binary with the web app built into it. They are the only tests that cross every layer
at once: the built bundle, the service worker, the HTTP API, SQLite and the knowledge base's git
repository. CI runs them on every pull request (`web-e2e` in `.github/workflows/ci.yml`).

They need **no agent runtime, no model and no network** beyond downloading the browser.

## What they cover

Each file in `web/e2e/app/` is one area. Every scenario starts a **server of its own** on a fresh
data directory (`127.0.0.1`, a free port, plain-http cookies) and a fresh browser, so scenarios
share nothing and can run in any order and in parallel.

| File | Scenario |
|---|---|
| `auth.e2e.mjs` | First-run setup with the setup token from the server's log (a wrong token is refused), the owner's session across a reload, logging out, a wrong password refused, logging back in; and setup closed once the owner exists |
| `knowledge.e2e.mjs` | Create a document, edit it, see both changes (and the creation) in its history, restore the first version |
| `boards.e2e.mjs` | On the private space's personal board: add a card, move it from *To do* to *Doing* to *Done*, comment on it, and see it in place after a reload |
| `apps.e2e.mjs` | Install the Expense tracker template as a draft, publish it, add a record to the live app, and check the server stored it as the owner's |
| `offline.e2e.mjs` | Download that app for offline use, load the page with no connection (the service worker serves the app, which opens the offline view), add a record there, see it kept on the device, then go back online and check that it reached the server |

`web/e2e/support.mjs` has what they share: starting the server, a scenario wrapper, first-run
setup through the screen, the template's install and its expense form. The other files in
`web/e2e/` (`dictation.e2e.mjs`, `browser-handover.e2e.mjs`) are manual ones that need a model or
Docker and never run in CI.

## Running them

```sh
pnpm install --frozen-lockfile   # once, and after the lockfile changes
make web-e2e
```

builds the server with the web app (`make build`), installs Playwright's Chromium and runs the
suite. The tests themselves take about fifteen seconds; the build and the browser download are most of a first run. With the server already
built and Chromium installed, run only the tests:

```sh
pnpm --filter @taskto/web run e2e                          # all
cd web && node --test "e2e/app/boards.e2e.mjs"              # one file
cd web && node --test --test-name-pattern=offline "e2e/app/*.e2e.mjs"   # by name
```

Settings, all optional:

- `TASKTO_WEB_E2E_BIN`: the server binary to test, instead of `bin/taskto`.
- `TASKTO_WEB_E2E_RESULTS`: where a failure's files go, instead of `web/e2e-results/`.

## When a scenario fails

The scenario writes, to `web/e2e-results/<scenario>/`, a Playwright **trace** (`trace.zip`: every
action, DOM snapshots and screenshots, the network) and a **screenshot** of each open page, and
prints the browser's console errors and the end of the server's log. Open the trace with

```sh
pnpm --filter @taskto/web exec playwright show-trace e2e-results/<scenario>/trace.zip
```

In CI the folder is kept as the **`web-e2e-results`** artifact of the run for 14 days, only when
the job fails.

## Writing a scenario

- Use `scenario(name, async (s) => { const { page, context, server } = s; … })` from `support.mjs`, and
  `setUp(s)` to make the owner through the setup screen. Don't share a server between scenarios.
- Find things the way a person does, by role and by the English label (`getByRole`,
  `getByLabel`), which the `t()` keys in `web/src/i18n/locales/en.json` give. The browser's
  language is English. Don't use CSS classes where a role or a label exists.
- Wait for what the person would see (`waitFor()`, a `getByText`), never for a fixed time.
- Check what the server stored through the API (`api(context, …)` uses the browser's own session),
  not only what the screen shows.
- Keep one scenario to a few seconds: they run on every pull request, and the CI job's budget is
  15 minutes, browser download included.
- Nothing here may reach the network or need a secret.
