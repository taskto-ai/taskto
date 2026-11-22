# syntax=docker/dockerfile:1
#
# TaskTo.AI in one image: the Go server with the web app inside it, and the agent runtime's
# Node.js sidecar, which the server starts and supervises (-agent). See docs/install.md.
#
#   docker build -t taskto .
#
# The base images are pinned by tag and by digest (the multi-arch index), so a rebuild uses
# the same bases; bump both together. The runtime base is distroless: Node.js and nothing
# else, no shell or package manager, running as the nonroot user (65532).

# The JavaScript build: the web app, and the agent sidecar with its production dependencies.
# They have no native modules, so this runs on the build machine's platform for any target.
FROM --platform=$BUILDPLATFORM node:24.22.3-trixie-slim@sha256:af02d57b57719b9657a45199a79264a9437940cd55b3c7b3fc99615654f124a0 AS js
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/
COPY agent/package.json agent/
# The pnpm version is the one in package.json's packageManager, the single pin.
RUN npm install -g --no-fund --no-audit "pnpm@$(node -p "require('./package.json').packageManager.split('@')[1].split('+')[0]")" \
 && pnpm install --frozen-lockfile
COPY web/ web/
COPY agent/ agent/
# The web app's type check covers its tests, which share fixtures with the Go server.
COPY internal/apps/testdata/ internal/apps/testdata/
RUN pnpm run build \
 && pnpm --filter @taskto/agent deploy --prod --legacy /tmp/agent \
 && mkdir -p /out/data /out/agent && cp -R /tmp/agent/package.json /tmp/agent/node_modules agent/dist /out/agent/

# The Go build, cross-compiled for the target platform, with the web app embedded.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=js /src/web/dist/ internal/webui/dist/
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/taskto ./cmd/taskto

# The CLI worker (docs/cli-workers.md), a second image: `docker build --target cli-worker`.
# Compose builds it for the cli profile. It runs the official Claude Code, Codex and Grok Build
# CLIs, pinned with their sha256 in deploy/cli-workers.json, each user's as a Unix user of
# their own. Native binaries: no Node.js here.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS cli-fetch
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*
COPY deploy/cli-workers.json deploy/cli-workers.sh /src/
ARG TARGETARCH
# A download whose sha256 isn't the manifest's fails the build.
RUN sh /src/cli-workers.sh install "$TARGETARCH" /out/bin

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS cli-worker
# What the CLIs' tools run: a shell, git, ripgrep, the usual tools. tini reaps orphans: a CLI's
# background process outlives it, and a PID 1 that never reaps leaves zombies.
# The agent browser (docs/agent-browser.md): Debian's Chromium (BSD-3-Clause and its third-party
# licences, in /usr/share/doc/chromium*/copyright), Xvfb and x11vnc (GPL-2.0), separate programs
# the worker starts as each user. /tmp/.X11-unix is root's and sticky, so no user can take it.
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git less openssh-client procps ripgrep tini unzip xz-utils \
      chromium fonts-liberation x11vnc xvfb \
 && rm -rf /var/lib/apt/lists/* \
 # No setuid or setgid programs: a CLI user must have nothing to escalate with.
 && find / -xdev -perm /6000 -type f -exec chmod a-s {} + \
 && mkdir -p /homes /run/taskto-cli /tmp/.X11-unix && chmod 711 /homes /run/taskto-cli && chmod 1777 /tmp/.X11-unix
COPY --from=cli-fetch /out/bin/ /usr/local/bin/
COPY --from=go /out/taskto /usr/local/bin/taskto
# Belt and braces: the worker builds each CLI's environment from nothing and sets these itself.
ENV DISABLE_AUTOUPDATER=1 GROK_DISABLE_AUTOUPDATER=1
VOLUME /homes
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/taskto", "cli-worker"]

# The backup job, a second image: `docker build --target backup`. Compose builds it for the
# backup profile (docs/install.md#backups). restic stores each snapshot `taskto backup`
# makes in an append-only repository; the image's entrypoint is restic's. It runs as the
# server's user, to read the data volume, and makes a new volume owned by that user, as the
# server's image does, for a restore into a fresh one.
FROM restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510 AS backup
COPY --from=go /out/taskto /usr/local/bin/taskto
COPY --from=js --chown=65532:65532 --chmod=700 /out/data/ /data/
ENV TASKTO_DATA_DIR=/data \
    RESTIC_CACHE_DIR=/tmp/restic-cache
USER 65532:65532
VOLUME /data

FROM gcr.io/distroless/nodejs24-debian13:nonroot@sha256:6abfcb367ebc291572c774c1592a986be2ab26a2486f73093401327495a5962a
COPY --from=go /out/taskto /app/bin/taskto
COPY --from=js /out/agent/ /app/agent/
# The data directory belongs to the nonroot user, owner-only; a new volume mounted on it
# starts as a copy of it, with the same owner and mode.
COPY --from=js --chown=65532:65532 --chmod=700 /out/data/ /data/
ENV PATH=/app/bin:/nodejs/bin:/usr/local/bin:/usr/bin:/bin \
    TASKTO_ADDR=0.0.0.0:8080 \
    TASKTO_DATA_DIR=/data \
    TASKTO_AGENT=true \
    TASKTO_AGENT_NODE=/nodejs/bin/node \
    TASKTO_AGENT_ENTRY=/app/agent/dist/index.js
USER 65532:65532
VOLUME /data
EXPOSE 8080
# No shell or curl in the image: Node.js asks /healthz.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/nodejs/bin/node", "-e", "fetch('http://127.0.0.1:8080/healthz').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"]
ENTRYPOINT ["/app/bin/taskto"]
