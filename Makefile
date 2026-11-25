.PHONY: check lint fmt build dev-server dev-web install agent-it dictation-e2e browser-handover-e2e web-e2e screens

PNPM ?= pnpm
# golangci-lint is pinned in tools/go.mod, apart from the app's dependencies.
GOLANGCI_LINT = go tool -modfile=tools/go.mod golangci-lint

# Runs Go and TypeScript lint, the Go tests, the translation check, and the web and agent
# builds and tests. CI also runs the reference scan, ShellCheck and a standalone Go build.
# It leaves out the separate CI jobs that need more than a checkout: the secret scan
# (gitleaks), the agent runtime integration tests (`make agent-it`), the CLI isolation tests
# (root, in a container), the worker network test (privileged Docker), the backup
# integration tests, and the agent browser's tests against a real Chromium
# (.github/workflows/browser.yml).
check: lint
	go test ./...
	$(PNPM) --filter @taskto/web run i18n
	$(PNPM) --filter @taskto/web run css
	$(PNPM) run build
	$(PNPM) run test

# Go: golangci-lint (go vet, staticcheck, errcheck, gosec, ..., and gofmt/goimports).
# TypeScript: Biome (lint and format).
lint:
	$(GOLANGCI_LINT) run ./...
	$(PNPM) run lint

# Fix formatting and the fixes the linters can make safely.
fmt:
	$(GOLANGCI_LINT) fmt ./...
	$(PNPM) run format

# Integration tests against the real agent sidecar (scripted model, fake OpenAI-compatible
# server), the providers file the server feeds it, and the server binary running it.
agent-it:
	$(PNPM) --filter @taskto/agent build
	TASKTO_AGENT_IT=1 go test -count=1 ./internal/agentrt/ ./internal/providers/ ./cmd/taskto/

# The web app's end to end tests (web/e2e/app/, docs/web-e2e.md): Playwright's Chromium against
# the built server on a fresh data directory, for each scenario. No agent, no model, no network
# beyond the browser's download. CI runs them on every pull request. A failing scenario leaves
# its trace and a screenshot in web/e2e-results/ (`pnpm --filter @taskto/web exec playwright
# show-trace <trace.zip>`); TASKTO_WEB_E2E_BIN runs another binary than bin/taskto.
web-e2e: build
	$(PNPM) --filter @taskto/web exec playwright install chromium
	$(PNPM) --filter @taskto/web run e2e

# Dictation end to end (web/e2e/dictation.e2e.mjs): the server with the web app and the real
# local model (about 700 MB, downloaded on first use; TASKTO_DICTATION_IT_CACHE keeps it), and
# Playwright's Chromium with a WAV file as its microphone. Never run by CI. About 11 minutes,
# 10 of them recording the long clip in real time; TASKTO_DICTATION_E2E_LONG=0 skips it.
dictation-e2e: build
	$(PNPM) --filter @taskto/web exec playwright install chromium
	TASKTO_DICTATION_E2E=1 node --test --test-reporter=spec web/e2e/dictation.e2e.mjs

# Captures of every screen, for reviews of UI changes (web/e2e/screens.mjs, docs/design.md): the
# built server on an empty temporary data directory, sample data seeded through the API, and
# Playwright's Chromium at three sizes in a light and a dark theme, into web/screens/ (git ignores
# it). Never run by CI. About a minute. SCREENS_ARGS passes the script's flags, for example
# `make screens SCREENS_ARGS="--only desktop-light/boards"`; see the script's top for the rest.
screens: build
	$(PNPM) --filter @taskto/web exec playwright install chromium
	node web/e2e/screens.mjs $(SCREENS_ARGS)

# The agent browser's hand-over with a real model (docs/agent-browser.md): builds and runs the
# Compose stack in Docker, and needs TASKTO_HANDOVER_E2E_BASE_URL and _KEY_FILE. Never in CI.
browser-handover-e2e:
	$(PNPM) --filter @taskto/web exec playwright install chromium
	TASKTO_HANDOVER_E2E=1 node --test --test-reporter=spec web/e2e/browser-handover.e2e.mjs

install:
	$(PNPM) install

# The web app is copied into internal/webui/dist for go:embed (see internal/webui).
build:
	$(PNPM) run build
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/. internal/webui/dist/
	go build -o bin/taskto ./cmd/taskto

# With the agent runtime, so it needs Node and the agent's dependencies (make install).
dev-server:
	$(PNPM) --filter @taskto/agent build
	go run ./cmd/taskto -insecure-cookies -agent

dev-web:
	$(PNPM) --filter @taskto/web dev
