# Contributing to TaskTo.AI

Thanks for helping. Bug reports, fixes, docs, translations and features are all welcome.
For anything larger than a fix, open an issue first so we can agree on the approach before
you spend time on it.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Report security
issues privately, as [SECURITY.md](SECURITY.md) says, never in a public issue.

## Set up

You need:

- Go, the version in `go.mod`;
- Node.js 22.19 or newer;
- pnpm, the version in `packageManager` in `package.json` (`npx pnpm@<that version>` works
  if you don't have it);
- Docker, for the container tests and the Compose stack (optional for most changes).

```sh
make install      # JavaScript dependencies
make dev-server   # the Go server on 127.0.0.1:8080, data in ./data, with the agent runtime
make dev-web      # in another terminal: the web app with hot reload, proxying the API
```

The first start prints a setup token in the server's log; open the web app and create the
first account with it.

## Make targets

| Target | What it does |
|---|---|
| `make check` | what CI runs: lint, `go test ./...`, the translation check, the web and agent builds and their tests |
| `make lint` | golangci-lint (Go) and Biome (TypeScript) |
| `make fmt` | format Go and TypeScript, and apply the linters' safe fixes |
| `make build` | the web app and the agent, then `bin/taskto` with the web app embedded |
| `make dev-server`, `make dev-web` | the development server and the web app's dev server |
| `make agent-it` | integration tests with the real agent sidecar, as CI runs them |
| `make dictation-e2e` | dictation in a real Chromium with the real local model (~700 MB, ~11 min); not run by CI |
| `make browser-handover-e2e` | the agent browser's hand-over with a real model on an OpenAI-compatible gateway, in Docker; not run by CI |

Run `make check` before you push. CI also runs tests that need root or Docker (the CLI
workers' isolation, the worker's network, backups, the agent browser) and a secret scan;
`.github/workflows/` has how each one runs, if you want to run one locally.

## Sending a change

1. Fork the repository and branch from `main`.
2. Keep the change small and about one thing. Several small pull requests are easier to
   review than one large one.
3. Follow [AGENTS.md](AGENTS.md): it has the layout and the rules every change keeps,
   among them:
   - every new API checks the caller's access, and has a test for a cross-user attempt;
   - every string people see is translated (`t()`, and a key in `en.json`);
   - schema changes are new migrations, never edits to a merged one;
   - no secrets in the repository, an agent's environment or a log.
4. Add or update tests, and the docs the change touches (`docs/api.md` for the API).
5. Open a pull request against `main`:
   - a conventional title: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`;
   - a description that says what changed, why, and how you checked it;
   - a reason for any new dependency: we keep them few.
6. CI must be green, and a maintainer reviews it. Answer each review comment with a change
   or a reason. Pull requests are squash-merged.

Docs and code comments explain the reasoning in the repository itself: don't refer to
outside notes, chats or tickets that a reader can't open. CI's reference check
(`.github/scripts/refguard.go`) fails on an issue number written with a hash sign in the
tree; link the issue in the pull request's description instead.

## Adding a language

Translations are files in `web/src/i18n/locales/`, one per language, named by its BCP 47
tag. In short ([docs/i18n.md](docs/i18n.md#adding-a-language) has the details):

1. Copy `web/src/i18n/locales/en.json` to `web/src/i18n/locales/<tag>.json` (`de`, `fr`,
   `es-419`, `zh-Hans`, `ar`).
2. Translate the values. Keep the keys, the `{placeholders}` and the `<tags>` exactly as
   they are, and give each plural message the forms the language has. Keys you leave out
   show in English.
3. Run `pnpm --filter @taskto/web run i18n`: it fails on a broken file and lists what is
   left untranslated.
4. Open a pull request. Someone who reads the language reviews it.

The app picks the file up by its name and lays right-to-left scripts out on its own.

## Licence

TaskTo.AI is under the [MIT licence](LICENSE). By contributing, you agree that your
contribution is licensed under it too.
