# Spike: personal apps as saved A2UI screens

> **Since then:** apps are stored in spaces, with their rows, behind the space's
> access; the server checks every saved screen against the catalog itself, in Go
> (`internal/apps/catalog.go`), rather than in the sidecar as suggested below; the catalog
> leaves out `Image`, `Video`, `AudioPlayer` and `openUrl`; actions insert, update and
> delete rows; the demo store and `-demo-apps` are gone, and the expense tracker is the
> bundled example. The routes are in [api.md](../api.md#personal-apps). The rest of this
> page is the spike as it was.

**Verdict: go, with conditions.** An app can be stored as plain JSON and drawn again later, its buttons reach the Go
server, and the updated data redraws the screen. Nothing the app defines ever runs as code. The conditions are
below: strict validation always on, server-side validation of saved apps, and a few renderer gaps to work around.

Versions: `@a2ui/react` and `@a2ui/web_core` 0.15.0, A2UI protocol **v0.9**.

## What an app is

An app is a JSON definition (`internal/apps/demo/expenses.json`):

- **components:** the A2UI component list, built only from our catalog;
- **collection:** typed fields for its records (`string`, `number`, required or not);
- **actions:** event names mapped to what they do (only `insert` so far);
- **summaries:** values Go computes from the records (`sum`, `count`), such as a total.

Go turns a definition plus its records into A2UI messages (`createSurface`, `updateComponents`, `updateDataModel`).
When a button fires, the browser sends the A2UI client action (event name + context resolved from the form), and
Go answers with an `updateDataModel` that redraws the screen.

```
GET  /api/apps/{id}/surface   → A2UI messages that draw the app
POST /api/apps/{id}/actions   → run a client action; 422 if the app doesn't allow it
```

These routes only exist with `taskto -demo-apps` (`make dev-server` sets it): the demo store is in memory, shared
by everyone who can reach the server, with no access checks and at most 1,000 records per app.

## What was checked

| Check | Where | Result |
|---|---|---|
| A stored definition draws the same screen after a save and load | `TestStoredDefinitionDrawsTheSameSurface` | Identical messages. |
| The browser draws the saved app | `web/src/apps/apps.test.tsx` | Title, list, form and a custom `Stat` render in jsdom. |
| A real click sends the action Go expects | same, against `testdata/expenses.action.json` | Typing into the fields and clicking **Add** sends exactly the golden action. |
| Go applies it and the screen updates | `TestApplyGoldenAction` + the web test | The new row and the new total (16.75) appear. |
| Go refuses bad actions | `TestApplyRefusesBadActions`, `TestRoutes` | Unknown action, other surface, missing, blank, wrongly typed or undeclared fields, `NaN` and infinities → `ErrInvalidAction` / HTTP 422. |
| A custom component next to the basic ones | `web/src/apps/catalog.tsx` | `Stat` (a labelled total) is a React component in the same catalog as A2UI's basic Web Components. |
| Components outside the catalog, or another catalog | web tests | Refused, **but only with `STRICT_VALIDATION`** (see below). |
| The real binary | manual `curl` | Surface, a valid action, and 422 for an unknown action. |

The golden files in `internal/apps/testdata` are the contract between the two halves: Go writes and checks them
(`go test ./internal/apps -update` regenerates), and the web test draws and clicks against the same files.

## Findings and conditions

1. **Strict validation is not the default.** With the default settings the renderer accepts a component outside
   the catalog (`Script`) and silently draws nothing. With `validationConfig: STRICT_VALIDATION` it refuses it with a
   clear error. `AppView` always passes it, and a test covers it.
2. **Saved apps must also be checked on the server.** Go stores components as raw JSON; today only the browser
   validates them. Before apps can be saved by the agent, validate a definition when it's saved. The
   simplest place is the Node agent sidecar, which can run `@a2ui/web_core`'s processor with the same catalog and
   strict rules. The catalog's JSON document (`getClientCapabilities({ includeInlineCatalogs: true })`) is also what
   the agent needs in its prompt to write valid screens.
3. **The React renderer speaks v0.9.** The A2UI site advertises a v1.0 spec, and `web_core` has a `v1_0` module, but
   `@a2ui/react` only ships v0.8 and v0.9. Versions are pinned exactly; moving to v1.0 is a later upgrade.
4. **Accessibility gap:** the basic `TextField` doesn't link its `<label>` to its `<input>`, so screen readers can't
   name the field (the test finds inputs by their visible label instead). Fix by adding our own `TextField` to the
   catalog, and report it upstream.
5. **Markdown in `Text` needs a renderer.** Plain text works. `@a2ui/markdown-it` 0.2.2 declares a peer of
   `web_core` ^0.14, one minor behind; add Markdown when that's aligned.
6. **Bundle size:** the web bundle grows from about 69 KB to 196 KB gzipped (A2UI, Lit, Zod). Load the app renderer
   only on app pages.
7. **Data binding:** numbers bound to text show as plain numbers ("4.5"). Formatting (currency, dates) belongs in
   Go summaries or A2UI functions, not in the app.

## Not checked here

- How a surface sits inside the OpenAgentUI chat (an app or agent screen inside a message). A2UI surfaces are plain
  React components, so this belongs with the web app shell.
- Persistence, per-user access and sharing: the spike's store is in memory with no access checks. That came later, with apps stored in spaces.
- The agent writing a definition from a request ("build me an expense tracker"): needs the agent runtime.
- Update and delete actions, and apps with more than one collection.
