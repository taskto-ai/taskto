# Installing TaskTo.AI

TaskTo.AI runs as one container: the Go server, with the web app inside it, and the agent
runtime's Node.js sidecar, which the server starts and supervises. Its data lives in one
volume. This guide takes you from nothing to a server behind HTTPS, with backups, in a few
minutes. It is the same for one person or a company: a company invites its people after
setup.

You need Docker with Compose (v2), and for HTTPS a domain name pointing at the machine with
ports 80 and 443 open.

> **One server process per data directory.** Exactly one `taskto` server uses a data
> directory (the `/data` volume) at a time. Agents, scripts and people may read the spaces'
> files under `workspaces/`, but never write their git repositories: every change goes
> through the server, which lets one writer at a time change a space, checks each change
> against the version it was based on, and keeps the search index in step. A second
> process writing the same repositories races the server and can lose or corrupt changes.
>
> The server enforces it. Before it opens anything in the data directory it locks
> `taskto.lock` there, and holds the lock until it exits, however it exits. A second
> server on the same directory (two containers on one volume, with `--scale` or a second
> Compose project pointed at `taskto_data`) exits at once with `data directory /data is
> in use by another taskto process (pid N)`. The lock is the kernel's (`flock`), not the
> file: the file stays after the server stops or crashes, and is no obstacle to the next
> one. Don't count on it across machines that share a network filesystem: run one server
> per data directory, on one host.

## 1. Start it

```sh
git clone https://github.com/taskto-ai/taskto.git taskto && cd taskto
docker compose up -d
```

This builds the image (`Dockerfile`) and starts it. The server listens on
`127.0.0.1:8080` only, so nothing outside this machine can reach it yet. Check it:

```sh
curl http://127.0.0.1:8080/healthz
# {"agent":"ready","database":"ok","status":"ok","version":"dev"}
```

`agent` is `starting` for a moment, then `ready`. The first build takes a few minutes.

Compose reads settings for itself from a `.env` file next to `compose.yaml`. Name the
version you run there; it tags the image and is what `/healthz` reports:

```sh
# .env
TASKTO_VERSION=v0.1.0      # after: git checkout v0.1.0
```

What the image does:

- runs as a non-root user (65532), on a read-only root filesystem, with no Linux
  capabilities (`compose.yaml`);
- keeps everything in the `/data` volume (`taskto_data`): the database, the agent's
  database, `server.key` (which encrypts the provider API keys stored in the database), and
  the spaces' files under `workspaces/`;
- runs the agent runtime (`TASKTO_AGENT=true`); without a providers file it only has the
  scripted offline model (see [Models](#models));
- is based on a distroless Node.js image: no shell or package manager inside.

### Prebuilt images

Instead of building, you can run the public images the project's CI publishes
(`.github/workflows/images.yml`) to the GitHub Container Registry, under the `taskto-ai`
organisation:

| Image | What |
|---|---|
| `ghcr.io/taskto-ai/taskto` | the server |
| `ghcr.io/taskto-ai/taskto-cli-worker` | the [CLI worker](#cli-workers) |
| `ghcr.io/taskto-ai/taskto-backup` | the [backup job](#backups) |

- Each commit on `main` is tagged with its full SHA, and each release with its version tag
  (`v0.2.0`). Tags are immutable: CI never pushes a tag that exists, so a tag always means
  the same image. There is no `latest`; pin the tag you run.
- They are built for linux/amd64.
- `/healthz` reports the tag as the version.

Point Compose at them with the registry path, its trailing `/` included, and the tag, then
pull instead of building:

```sh
# .env
TASKTO_IMAGE_PREFIX=ghcr.io/taskto-ai/
TASKTO_VERSION=v0.2.0
```

```sh
docker compose pull taskto            # add --profile cli (and backup) for the others
docker compose up -d --no-build
```

The images are public, so pulling needs no login. If you publish them to a private
registry of your own instead (`.github/scripts/images.sh` builds and pushes them to any
registry), set `TASKTO_IMAGE_PREFIX` to it and `docker login` with a **read-only** token
before you pull.

The Compose file still needs to match the image: use the `compose.yaml` of the same
commit or tag. [deploy/INSTALL.md](../deploy/INSTALL.md) is a step-by-step install of
published images on a fresh Ubuntu VM, written for an agent to run over SSH, behind the
bundled Caddy or another machine's reverse proxy.

Settings are `TASKTO_*` environment variables (see the [README](../README.md#running)). Put
them in `compose.override.yaml`, which Compose reads next to `compose.yaml`:

```yaml
# compose.override.yaml
services:
  taskto:
    environment:
      TASKTO_AGENT_PROVIDERS: /config/providers.json
```

## 2. Set up the owner

A fresh server has no accounts. The first account is the owner, admin of the first
workspace. So that nobody else who reaches a fresh server can claim it, setup needs a
**setup token**: a random token the server prints to its log at startup while no account
exists, and only someone who can read the log has it.

```sh
docker compose logs taskto | grep setup_token
# ... INFO first-run setup: ... setup_token=Zm9vYmFy...
```

Then open the web app (`http://localhost:8080` on the machine itself, or your HTTPS
address once [step 3](#3-https) is done): a fresh server shows its setup screen, which asks
for the token, your name, email and password, and the first workspace's name. Or create
the owner with the API:

```sh
curl -sS http://127.0.0.1:8080/api/setup \
  -H 'Content-Type: application/json' \
  -d '{"email": "you@example.com", "name": "Your Name", "password": "at least 12 characters",
       "workspace_name": "Home", "setup_token": "<the token from the log>"}'
```

- A missing or wrong token gets `403`; once setup is done, every setup gets `409`.
- The token works once. It changes every time the server restarts, and a server that has
  accounts prints none.
- The log keeps the line, but the token is useless once setup is done.

The owner then invites everyone else ([API](api.md#invites)).

Everyone changes their own password in **Settings → Password**, with their current one;
that signs them out everywhere else. Someone who lost theirs asks an admin for a reset link
(**Settings → Members → Reset link**), which works once, for 24 hours. There is no email:
the admin sends the link. An admin can make one only for a member whose every workspace
they administer ([API](api.md#passwords)).

## 3. HTTPS

Browsers only send the session cookie over HTTPS: it is `Secure` and named
`__Host-taskto_session`. So serve TaskTo.AI over HTTPS, through a reverse proxy. (On the
machine itself, `http://localhost:8080` works in browsers that treat localhost as secure,
such as Chrome and Firefox.)

The `insecure_cookies` setting turns this off for plain-http development on localhost. It
is for development only: never set it on a server others use, since the session cookie
would then travel in clear text.

### With the bundled Caddy

`compose.yaml` has Caddy in the `https` profile. It gets and renews a certificate for your
domain by itself (`deploy/Caddyfile`):

```sh
# .env
TASKTO_DOMAIN=tasks.example.com
```

```sh
docker compose --profile https up -d
```

Open `https://tasks.example.com`. Caddy passes the `Host` header through, sets
`X-Forwarded-For` to the client's address, and doesn't buffer the agent's streamed replies.

### The client's address behind a proxy

Failed logins are limited per client address ([API](api.md#post-apilogin)). Behind a proxy,
every request comes from the proxy, so the server must be told which proxies it may believe
about the real address: `TASKTO_TRUSTED_PROXIES`, a comma-separated list of IP addresses or
CIDR prefixes (`trusted_proxies` in a config file, `-trusted-proxies` as a flag).

- Only when the direct peer is a trusted proxy does the server read `X-Forwarded-For`. It
  takes the right-most address that isn't a trusted proxy: each proxy appends the address it
  received the request from, so everything left of that can be made up by the client.
- From any other peer, `X-Forwarded-For` is ignored.
- Without trusted proxies (the default), the client is whoever connects.

`compose.yaml` gives Caddy a fixed address, `172.29.80.10`, on the project's network, and
sets `TASKTO_TRUSTED_PROXIES` to it; no other container can get that address. If the
`172.29.80.0/24` subnet collides with one of your networks, change the subnet, Caddy's
address and `TASKTO_TRUSTED_PROXIES` together.

### Another reverse proxy

nginx, Traefik, HAProxy or a load balancer work the same way. The proxy must:

- terminate HTTPS and forward to the server's port;
- pass the original `Host` header (the server compares it with the browser's `Origin` to
  refuse cross-site requests);
- append the client's address to `X-Forwarded-For`;
- not buffer responses, since agent replies stream.

Then set `TASKTO_TRUSTED_PROXIES` to the proxy's address as the server sees it, and nothing
broader. For nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;
}
```

A proxy on the host reaching a published port shows up with the address of Docker's bridge
gateway, which can be shared with other local traffic; prefer running the proxy in the
Compose project, as the bundled Caddy does.

### The app on phones and computers

TaskTo.AI is an installable web app (a PWA): from the browser, a person can add it to a phone's
Home Screen or a computer's apps, where it opens in a window of its own
([docs/pwa.md](pwa.md) has the design). It works in a plain browser tab too; nothing here is
required for that.

Installing, and the offline shell, need a **secure context**: browsers run service workers
only on pages served over HTTPS (or from `localhost`). So a server people install from needs
what step 3 already asks for:

- **A certificate the devices trust.** A public one (Caddy's, or Let's Encrypt) just works. A
  self-signed or private-CA certificate must be installed on each phone and computer first;
  when the browser shows a certificate warning, it won't register the worker or offer to
  install, and iOS won't keep the Home Screen app's login.
- **The app at the root of its host** (`https://tasks.example.com/`, not under a path): the
  manifest's start URL and scope are `/`.
- **A proxy that leaves `/sw.js` and `/manifest.webmanifest` alone.** The server sends them
  with `Cache-Control: no-cache`; a proxy or CDN must not cache them (or the app won't learn of
  a new version), nor answer for the server's routes (`/api`, `/mcp`, `/healthz`).

Each person installs from **Settings → About → Install the app**, which says what to do on
their device:

- **iPhone and iPad (Safari):** the Share button, then **Add to Home Screen**. iOS has no
  install prompt a page can start.
- **Android (Chrome and others):** the browser's menu, then **Install app** or **Add to Home
  screen**; Chrome may also offer an **Install TaskTo.AI** button there.
- **Computers (Chrome, Edge):** the install icon at the end of the address bar. Safari: **File →
  Add to Dock**.

Only the app's own files (the screens, scripts and icons) are kept for offline use. Nothing
from `/api` is cached by the worker: a person's conversations, documents, boards and apps
always come from the server, so offline the app opens and says it can't reach it.

An upgrade of the server ([Upgrade and rollback](#upgrade-and-rollback)) reaches open apps
as a prompt, **A new version of TaskTo.AI is ready**, with **Reload to update** and **Later**.
The page is never reloaded for anyone: they update when they have saved their work.

### Where provider accounts may connect

Workspace admins add provider accounts for the workspace, and every member can add
personal ones ([Models](#models)), with any base URL. The server then calls that URL with
the account's key: to test the account, for the search index's embeddings, and, through
the agent runtime, for conversations. That is what lets people use an Ollama on their own
machine or a model on the LAN; but it also lets anyone who can add an account make the
server reach addresses that only the server can: a cloud's metadata service
(`169.254.169.254`), an admin interface, anything on the company network.

The **egress policy** limits that, separately for workspaces' accounts and personal
ones:

| Setting | Config file | Flag |
|---|---|---|
| `TASKTO_PROVIDER_EGRESS_WORKSPACE` | `provider_egress_workspace` | `-provider-egress-workspace` |
| `TASKTO_PROVIDER_EGRESS_PERSONAL` | `provider_egress_personal` | `-provider-egress-personal` |

Each is one of:

- `any` (the default): no limit. Right for a single-user install, or a team that trusts
  everyone who can add an account.
- `public`: public addresses only. Loopback, private (`10/8`, `172.16/12`,
  `192.168/16`), link-local (with the metadata services), unique-local (`fc00::/7`),
  CGNAT (`100.64/10`), multicast, documentation and other reserved ranges are refused,
  IPv4 and IPv6, including IPv6 addresses that carry one of those IPv4 addresses.
- a comma-separated allowlist of host names, IP addresses and CIDR prefixes, which may
  include `public`: `public, ollama.internal, 10.20.0.0/16`. A host name allows every
  address it resolves to, so list only names whose DNS you control; an address or prefix
  allows exactly that.

Tighten it when the server runs where it can reach things its users shouldn't: in a cloud
(the metadata service hands out the machine's credentials), or on a company network.
Typically: `public` for personal accounts, and `public` plus the hosts of your own model
servers for workspaces' accounts, which only admins add:

```yaml
# compose.override.yaml
services:
  taskto:
    environment:
      TASKTO_PROVIDER_EGRESS_PERSONAL: public
      TASKTO_PROVIDER_EGRESS_WORKSPACE: public, ollama
```

(`ollama` is the bundled [Ollama](#ollama-in-the-same-project), on the project's network.)

How it holds:

- Addresses are checked after DNS resolution, on every connection, by the server and the
  agent runtime alike. A name that resolves to a public address first and a private one
  later (DNS rebinding) is refused the second time, before anything is sent.
- Redirects aren't followed, and the policy's calls don't use `HTTP_PROXY`/`HTTPS_PROXY`,
  since a proxy would make the connection the server can't check.
- Saving an account, or changing its base URL, that the policy forbids is refused with a
  message that says so. After a policy is tightened, accounts saved before still exist:
  testing one says the policy refuses it, a conversation using it fails with that message,
  and so does semantic search, which falls back to full text and says why.
- The operator's own providers file (`TASKTO_AGENT_PROVIDERS`) isn't limited: the operator
  chose those endpoints. An entry may set its own `egress` list
  ([agent-providers.md](agent-providers.md#schema)).

The policy is read at startup: restart the server to change it.

## Models

Users and workspaces add their own model providers through the API, with their keys
stored encrypted ([provider accounts](agent-providers.md#provider-accounts)); they take
effect without a restart. As the operator you can also give everyone providers with a
providers file ([agent-providers.md](agent-providers.md)), mounted into the container.
Without either, the agent runs only the scripted offline model.

### Ollama in the same project

The `ollama` profile runs [Ollama](https://ollama.com) next to the server, reachable as
`http://ollama:11434` on the project's network only. Create `providers.json`:

```json
{
  "providers": [
    {
      "id": "ollama",
      "kind": "openai-compatible",
      "name": "Ollama",
      "baseUrl": "http://ollama:11434/v1",
      "models": [{ "id": "llama3.2:3b", "name": "Llama 3.2 3B", "contextWindow": 131072 }]
    }
  ]
}
```

and mount it:

```yaml
# compose.override.yaml
services:
  taskto:
    environment:
      TASKTO_AGENT_PROVIDERS: /config/providers.json
    volumes:
      - ./providers.json:/config/providers.json:ro
```

```sh
docker compose --profile ollama up -d
docker compose exec ollama ollama pull llama3.2:3b
```

The server reads this file when it starts: after changing it, restart the server
(`docker compose restart taskto`). An API key goes in a file or variable the file points
to, never in the file itself.

## CLI workers

Conversations can run on the official Claude Code, Codex and Grok Build CLIs, signed in
with each user's own subscription ([cli-workers.md](cli-workers.md)). It is off by default.

- `-cli-workers local` (`TASKTO_CLI_WORKERS=local`) runs the CLIs found on the server's
  `PATH`, as the server's own OS user, each TaskTo user in their own directory under
  `<data>/cli/`. There is **no OS boundary between users** in this mode: a user's CLI, or a
  command it runs, can read other users' CLI logins and the server's data, `server.key`
  included. So it works only while the server has a single user (a personal install), and
  is refused once a second user joins.
- `-cli-workers local-unsafe` drops that check, for development only.
- `-cli-workers container` is the mode for teams. The CLIs run in a worker container of
  their own, each TaskTo user's as a Unix user of their own (uid 100000 + the user's ID),
  with no capabilities and no setuid program to escalate with. The worker container never
  sees the server's data. The server reaches it on a socket only the server's user can open
  (`-cli-worker-socket`).

With Compose:

```sh
echo TASKTO_CLI_WORKERS=container >> .env
docker compose --profile cli up -d --build
```

- The `cli-worker` service is built from the same Dockerfile (`--target cli-worker`). It
  installs the Claude Code, Codex and Grok Build versions pinned in
  `deploy/cli-workers.json`, each checked against its sha256. They never update
  themselves: a TaskTo.AI release moves them (`deploy/cli-workers.sh bump`).
- It runs as root with only `SETUID`, `SETGID`, `CHOWN` and `KILL` (to make the users and
  run their CLIs as them) and `no-new-privileges`. Its memory and CPUs are capped by
  `TASKTO_CLI_WORKER_MEMORY` (default `4g`) and `TASKTO_CLI_WORKER_CPUS` (default `2`).
- Each user's CLI logins and sessions are on the `cli-homes` volume, which `taskto backup`
  doesn't cover. If it is lost, users sign in again, and their conversations start new CLI
  sessions with the conversation so far.
- The worker needs the internet to reach Anthropic, OpenAI and xAI. The server's egress
  policies don't apply to it. It reaches the internet and nothing else: it is alone on a
  network of its own (`worker`, the bridge `taskto-worker`), so the other services don't
  answer it, and `deploy/worker-firewall.sh`, run as root on the Docker host, keeps it from
  the host and every non-public address, your LAN included
  ([cli-workers.md](cli-workers.md#the-workers-network)). Run `sudo
  deploy/worker-firewall.sh add` before `docker compose --profile cli up`, and again after
  each boot (before Docker starts: [deploy/INSTALL.md](../deploy/INSTALL.md), step 8, has a
  systemd unit for it); `check` says whether the rules are in. It asks public DNS resolvers,
  since yours may be on your LAN.
- With `-browser` (`TASKTO_BROWSER=true`) the agent gets a browser per user, with a live view
  and hand-over for logins ([agent-browser.md](agent-browser.md)). It is off by default, and
  needs CLI workers. The worker image has its programs: Debian's Chromium, Xvfb and x11vnc.
  In local mode they are found on the `PATH`; `-browser-chromium` names another Chromium. Each user's browser runs as their Unix user, with
  its profile in their directory on `cli-homes`. `taskto cli-worker -browser-idle 5m` sets how
  long a browser runs unused, and `-browser-max 4` how many run at once. Chromium takes
  about 300–500 MB each, so raise `TASKTO_CLI_WORKER_MEMORY` with `-browser-max`.

### Agents never run as root

Agents read text other people wrote, so the server keeps them from ever acting as root
([agent-safety.md](agent-safety.md)):

- With the agent runtime or `local` CLI workers on, the server refuses to start as root, or
  as a user for whom `sudo -n true` (or `doas -n true`) works without a password. A sudo
  password cached in your terminal counts: run `sudo -k` first. The image runs as `65532`,
  which has no sudo.
- The `cli-worker` refuses to start if its image has a setuid or setgid program, and every
  CLI runs with `no_new_privs`.
- `-unsafe-privileged-agents` (`TASKTO_UNSAFE_PRIVILEGED_AGENTS=true`) turns both refusals
  into warnings, for development only (a throwaway VM where you are root, say). Never set it
  on a server people use.

### Shared accounts and their proxy

A workspace's admins can add **shared CLI accounts**: an API key per CLI that members' turns
use ([account-switching.md](account-switching.md)). They are never a subscription login,
which the providers' terms forbid sharing. The key stays in the server. A turn on a shared
account points its CLI at the server's **proxy** with a token for that turn, and the proxy
adds the key to its requests.

- The proxy has a listener of its own, never the public one: `-cli-proxy-listen`
  (`TASKTO_CLI_PROXY_LISTEN`). `-cli-proxy-url` (`TASKTO_CLI_PROXY_URL`) is its address as the
  CLIs reach it.
- In `local` mode it listens on `127.0.0.1`, on a free port, unless you set it otherwise.
- In `container` mode the Compose file sets `unix:/run/taskto-cli/proxy/proxy.sock`, a socket
  in the volume the server and the worker share, and `http://127.0.0.1:8081`, where the
  worker relays it to its CLIs. The worker's network has no route to the server, so the
  proxy has no network address at all.
- Without a proxy, shared accounts can be added, but turns on them are refused and say why.
- The same listener serves TaskTo's own tools (the knowledge base, boards, apps and the agent
  browser) to every CLI turn, at `/cli-mcp`, with a token for that turn
  ([cli-tools.md](cli-tools.md)). Without a proxy, CLI turns get no TaskTo tools, and the
  server says so in its log at start.
- The proxy connects to the accounts' addresses under the workspace accounts' egress
  policy (`-provider-egress-workspace`).

## Dictation

The chat box's microphone sends speech to the server, which turns it into text
([dictation.md](dictation.md)). Every workspace uses the **local model** unless a
workspace admin picks the hosted API (with a speech-to-text API key) in Settings → Dictation;
a key alone doesn't switch to it. The local model runs on this server, so speech never
leaves it.

**The local model** is `-dictation-local` (`TASKTO_DICTATION_LOCAL`):

| Value | Model | Languages | Download | Memory while dictating |
|---|---|---|---|---|
| `parakeet-v3` (default) | Parakeet TDT 0.6B v3 (NVIDIA, CC-BY-4.0), int8, on sherpa-onnx 2.0.2 (Apache-2.0) | 25 European languages, Portuguese included | about 700 MB | about 1.2 GB |
| `whistle` | Whistle (Cactus Compute, Apache-2.0) on the Cactus runtime 4.0 (Apache-2.0) | en, de, fr, es, it, nl, pl | about 18 MB | small |
| `off` | none: dictation needs an admin to pick the hosted API, with a key | | | |

- The files are **not in the image**. The first dictation that needs them downloads them
  from GitHub and Hugging Face. Each file is pinned to a fixed release or commit and checked
  against its sha256. A file that doesn't match is discarded. Until the files are ready,
  dictation answers "downloading" with its progress.
- The files are kept in `-dictation-cache` (default `<data>/cache/dictation`), outside the
  knowledge base. They can be fetched again at any time, so a backup can leave that
  folder out.
- **Without internet**, copy the cache folder from a machine where the download ran. The
  files are checked by their hashes, so the copy is verified as well.
- Each dictation runs the model in a child process: in a process group of its own, with a
  clean environment (no keys, `CACTUS_TELEMETRY=0`, `DO_NOT_TRACK=1`) and a 5-minute
  limit. The model is in memory only while it transcribes, and one dictation runs at a
  time, so plan for the memory above once, not per user. On an M-series Mac, Parakeet
  transcribed 48 seconds of speech in 2.4 seconds on 2 threads, loading the model included.
- It runs on Linux x86-64 and arm64 (the image included) and on macOS arm64. Elsewhere,
  dictation needs an admin to pick the hosted API, with a key.
- The download doesn't go through the provider egress policies. Those cover endpoints that
  people type in; this download only reaches the fixed URLs in the server's code.

**An API key** is set per workspace (Settings → Dictation). Any endpoint that speaks
OpenAI's `/audio/transcriptions` works, such as OpenAI, Groq or a self-hosted server. Calls
to it follow the egress policy of workspaces' provider accounts
([above](#where-provider-accounts-may-connect)).

### The Desert Ant Voz add-on

Voz (Desert Ant Labs) is a speech model under a source-available licence, so TaskTo.AI
doesn't ship it. You can install it yourself:

- **Before you install it, read its licence** (<https://license.desertant.com/>):
  - its licence check contacts Desert Ant Labs every time the model loads, and it can't be
    turned off;
  - each server counts as an install;
  - attribution is required. TaskTo.AI shows it on the About page while a workspace uses Voz.
- Install Voz's SDK and model, then put an executable at `<addons_dir>/desertant-voz/transcribe`
  (`-addons-dir`, default `<data>/addons`). The server looks for it when it starts.
- The program's contract:
  - it reads a WAV file (16-bit PCM, mono, 16 kHz) on standard input;
  - it may get `--language <code>`, which it may ignore;
  - it writes `{"text": "…"}` on standard output and exits 0.
- It runs like the local model: a child process, a clean environment, one dictation at a
  time. A clean environment isn't a sandbox: the program runs as the server's OS user and
  can read whatever the server can, `server.key` and the database included. Install only a
  program you trust, and keep `addons_dir` writable only by you.
- Then a workspace admin chooses it in Settings → Dictation, after confirming the licence
  notice.

A sketch of the program with Voz's Node SDK and `onnxruntime-node`, both installed next to
it, not tested here (the image has Node at `/nodejs/bin/node`):

```js
#!/nodejs/bin/node
// <addons_dir>/desertant-voz/transcribe
import * as ort from "onnxruntime-node";
import { Voz } from "@desert-ant-labs/voz";

const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const wav = Buffer.concat(chunks);
const voz = await Voz.load({ ort });
const result = await voz.transcribe(wav.buffer.slice(wav.byteOffset, wav.byteOffset + wav.length));
process.stdout.write(JSON.stringify({ text: result.text }));
```

## Backups

`taskto backup` writes a snapshot of the data directory. It runs while the server does:

- every SQLite database (the server's `taskto.db`, the agent's `agent/agent.db`) is copied
  with SQLite's `VACUUM INTO`, a consistent copy of one moment, rather than the live files;
- every other file under the data directory is copied as is: `server.key`, the spaces'
  files with the knowledge base's unversioned files (large files, audio and video, and heavy binaries, whose
  bytes are in each space's `workspaces/<id>/spaces/<id>/.git/lfs/objects/`,
  [api.md](api.md#unversioned-files)), and the CLI homes of `-cli-workers local` under
  `cli/`;
- the agent's socket and the server's lock file (`taskto.lock`) are left out, and so are
  symbolic links (it says which);
- so are `agent/providers.json` and `agent/keys/`, which the server writes afresh from the
  database at every start and which hold the provider accounts' keys in clear;
- so is `cache/`, which holds only what can be downloaded again (the dictation models);
- the archive is a tar, gzipped unless you pass `-uncompressed`. Its first entry is a
  manifest (`taskto-backup.json`, the version and the time) and its last counts the
  entries, so a restore refuses an archive that was cut short.

Only the data volume is backed up. Other volumes are left out: Caddy's certificates and
Ollama's models can be made again, and the CLI worker's `cli-homes` (container mode) holds
the users' CLI logins, which they sign in to again (see [CLI workers](#cli-workers)).

```sh
docker compose exec -T taskto taskto backup - > taskto-$(date +%Y%m%d-%H%M%S).tar.gz
```

`-T` matters: without it the archive goes through a terminal and is ruined (the command
refuses to write to a terminal). Outside Docker, `taskto backup FILE` writes FILE, owner-only,
and refuses a file that exists or one inside the data directory.

The archive holds password hashes, sessions, and `server.key` next to the provider keys it
encrypts, so whoever has the archive can read those keys: keep it as private as the server,
and encrypt it before it leaves the machine.

A backup is a copy of one moment, and later changes don't reach it. When a file's old
versions are [taken out of a space's history](api.md#taking-a-files-old-versions-out-of-history),
the archives and restic snapshots taken before still hold them, until they expire
(`TASKTO_BACKUP_KEEP`, [below](#3-removing-old-snapshots-on-the-backup-host-only)) or you
remove them on the backup host.

### Backups the server can't delete

A backup on the server's own disk goes with the server. A copy sent elsewhere with keys
that can also delete it (an `rclone` or `aws s3` script with a full-access key) goes with
it too, if someone takes over the server: whoever has root there, or an agent that
escapes, has those keys and can erase the history before anyone notices. So the setup
here splits the keys:

- **The server can only add.** It sends each snapshot to a [restic](https://restic.net)
  repository through [rest-server](https://github.com/restic/rest-server) in
  `--append-only` mode, which refuses to delete or overwrite anything (except restic's own
  lock files), whoever asks. The server's user can add snapshots and read them; nothing it
  holds removes one.
- **Deleting happens elsewhere.** Old snapshots are removed on the backup host, by a job
  that opens the repository's files directly. That access never exists on the server.
- **restic encrypts** everything before it leaves the server, with the repository's
  password, and stores unchanged data once: a daily snapshot costs about what changed that
  day. (That's why the server sends a plain tar, `taskto backup -uncompressed`: a gzipped
  one would be new data every time.)

Two machines, then: the TaskTo.AI server, and a **backup host** that it can reach and that
nothing on the server can log in to. The files for each are in this repository:
`compose.yaml`'s `backup` service on the server, `deploy/backup-host/compose.yaml` on the
backup host.

#### 1. The backup host

Any machine with Docker and room for the backups: a NAS, a small server, a VPS from
another provider than the server's. Copy `deploy/backup-host/compose.yaml` there and
start it:

```sh
cd deploy/backup-host
echo 'TASKTO_BACKUP_ADDR=<private-address>' >> .env  # where to listen (see below); required
docker compose up -d                                # rest-server on port 8000, append-only
docker compose exec rest-server create_user taskto  # the server's user; asks for its password
docker compose restart rest-server                  # so it reads the new user
```

`--append-only` and `--private-repos` are fixed in that file: each user has its own
repository (`/taskto/` for the user `taskto`) and none can delete. Settings go in a `.env`
next to it:

| Variable | Default | What |
|---|---|---|
| `TASKTO_BACKUP_REPOS` | `repos` (a volume) | where the repositories live: a volume name, or a path such as `/srv/restic` |
| `TASKTO_BACKUP_ADDR` | none: required | the address rest-server's port is published on: the backup host's address on the private network, or `0.0.0.0` (every interface) with TLS and a firewall |
| `TASKTO_BACKUP_PORT` | `8000` | the port rest-server listens on |
| `TASKTO_BACKUP_TLS` | none | rest-server's TLS flags, for example `--tls --tls-cert /data/tls/cert.pem --tls-key /data/tls/key.pem` (paths inside the repositories' directory) |
| `TASKTO_BACKUP_USER` | `taskto` | the repository the prune job works on |
| `TASKTO_BACKUP_KEEP` | `90d` | how long the prune job keeps snapshots |

**Let only the TaskTo.AI server reach the port, and use TLS unless the network between
the two is private.** Either a private network (WireGuard, Tailscale) with `TASKTO_BACKUP_ADDR` set to the
backup host's address on it, or `0.0.0.0` with TLS and a firewall rule for the server's
address. There is no default, so the choice is made on purpose.
Without TLS, the append-only user's password crosses the network in clear. Whoever catches
it can add snapshots and download the repository's files, but not read them: restic
encrypts everything on the server, and the repository's password never crosses the
network. They can't delete anything either.

#### 2. The server

On the TaskTo.AI server, next to `compose.yaml`, put the repository's address (with the
append-only user) and a password for the repository in `backup/`. Both are placeholders
here; `backup/` is in `.gitignore`, and nothing else in the checkout holds them:

```sh
install -d -m 700 backup
printf '%s\n' 'rest:https://taskto:<append-only-password>@backup.example.com:8000/taskto/' > backup/repository
openssl rand -base64 32 > backup/password   # the repository's password
chmod 600 backup/*
sudo chown -R 65532:65532 backup            # the backup job runs as the server's user
docker compose run --rm -T backup init      # creates the repository
docker compose run --rm -T backup           # the first backup
```

**Save the repository's password and address somewhere other than the server** (a
password manager): without the password nobody can read the backups, you included, and if
the server is lost, so is its copy.

`docker compose run --rm -T backup` runs `restic backup --stdin-from-command -- taskto
backup -uncompressed -`: restic runs `taskto backup` itself, and saves a snapshot only if
it finishes cleanly. It reads the data volume beside the running server. To run it every
night, turn on the schedule (next section).

Other restic commands work the same way: `docker compose run --rm backup snapshots`
lists the snapshots, and `docker compose run --rm backup check` checks the repository.
`forget` and `prune` fail here with `403 Forbidden`: that's the point.

Outside Docker, install restic 0.17 or later and run the same with the binary:

```sh
export RESTIC_REPOSITORY_FILE=/etc/taskto/backup/repository RESTIC_PASSWORD_FILE=/etc/taskto/backup/password
restic backup --host=taskto --tag=taskto --stdin-filename=taskto.tar \
  --stdin-from-command -- taskto backup -uncompressed -data-dir /var/lib/taskto -
```

#### Every night, retried, and watched

The `backup-schedule` service runs `taskto backup-schedule`: the same image, files and
backup as above, every night. Turn it on with its profile, next to the ones you use:

```sh
echo 'COMPOSE_PROFILES=backup-schedule' >> .env   # or add it to a COMPOSE_PROFILES already there
docker compose up -d
docker compose run --rm -T backup-schedule -now   # one backup now, recorded like the nightly ones
```

- **When.** Every night at `TASKTO_BACKUP_AT` (in `.env`, `HH:MM` in UTC; `03:15` by
  default). A backup is stopped after `TASKTO_BACKUP_TIMEOUT` (`4h`).
- **Retries.** A failed night is tried again every `TASKTO_BACKUP_RETRY_EVERY` (`1h`), up to
  `TASKTO_BACKUP_RETRIES` (`6`) times; then it waits for the next night. A night the
  schedule didn't get to (the machine was off, the container stopped) is backed up as soon
  as it runs again.
- **The marker.** The schedule keeps how it went in the data volume, in
  `backup-schedule/status.json`: the last success, the last error, the night's failures, the
  next backup and when a success is due. Retries go by it, so a restart neither forgets a
  failed night nor retries it sooner. A backup leaves it out.
- **When the backup host is down**, restic would retry for many minutes. The schedule first
  checks that the repository answers (`restic cat config`, two minutes at most), and only
  then starts the backup, so an outage writes nothing to the server's disk. What a backup
  that was killed left in the data volume (its databases' copies, in `.backup-*`) is
  removed after the failure, and by the next backup.
- **Seeing it.** Admins see the last successful backup, the last error and the next backup
  in **Settings → About**. Monitoring asks `https://<domain>/healthz/backup`: `200` while
  backups are on time, `503` once a night is missed (no success by an hour after its last
  retry was due) or when no schedule runs ([api.md](api.md#backups)). It says when, never
  why: the error stays with the admins. On the server, the file itself:
  `docker compose run --rm --no-deps -T --entrypoint cat backup-schedule /data/backup-schedule/status.json`.

Outside Docker, run `taskto backup-schedule -data-dir /var/lib/taskto` as a service of its
own, as the server's user, with restic on its `PATH` and the two `RESTIC_*_FILE` variables
above (a systemd unit with `Restart=always`). `taskto backup-schedule -h` lists its flags,
which the `TASKTO_BACKUP_*` variables set too.

#### 3. Removing old snapshots, on the backup host only

```sh
cd deploy/backup-host
install -d -m 700 prune
printf '%s\n' '<repository-password>' > prune/password && chmod 600 prune/password
docker compose run --rm prune snapshots    # what is there
docker compose run --rm prune              # forget snapshots older than 90 days, and prune
docker compose run --rm prune check
```

The prune job keeps every snapshot from the last `TASKTO_BACKUP_KEEP` (`--keep-within`),
and only that. Don't switch it to `--keep-last`, `--keep-daily` and the like: whoever
takes over the server can't delete snapshots, but can add fake ones with made-up times,
and those rules would then pick the fakes and forget the real ones. With `--keep-within`
the real ones stay for the whole window, so the window is how long you have to notice a
takeover. If you suspect one, stop pruning until you have looked.

(To keep the server's password off the backup host as well, give the backup host a key of
its own: `docker compose run --rm prune key add`, then put the new password in
`prune/password`.)

#### For one person

- The backup host can be anything you own that isn't the server: a NAS or a mini PC at
  home, or a cheap VPS at another provider. A private network between the two (Tailscale,
  WireGuard) saves setting up TLS and opening a port.
- Back up daily (the schedule above). Prune by hand a few times a year, after a look at
  `snapshots`; a full disk is the only reason to hurry.
- Keep the repository's password in your password manager, and try a restore once (below),
  so you know it works before you need it.

#### For a company

- Run the backup host under different people and credentials than the server: no SSH key,
  password or automation on the server opens a shell on the backup host, and its admins
  aren't the server's. A takeover of one then doesn't reach the other.
- Turn on TLS (`TASKTO_BACKUP_TLS`, or a reverse proxy in front of rest-server) and allow
  only the servers' addresses through the firewall.
- One rest-server can hold several servers' backups: a user per server (`create_user
  taskto-eu`), each with its own repository. Run the prune job per repository
  (`TASKTO_BACKUP_USER=taskto-eu docker compose run --rm prune`).
- If a day's work is too much to lose, back up more often as well: `docker compose run
  --rm -T backup-schedule -now` from cron, hourly. Snapshots of unchanged data are small.
- Point your monitoring at `/healthz/backup`, so a missed night pages someone.
- Prune from cron on the backup host, weekly, and keep the window long enough to notice a
  takeover (the 90-day default, or longer).
- Watch that backups arrive, from the backup host, where the server can't silence it. With
  `TASKTO_BACKUP_REPOS=/srv/restic`:

  ```sh
  # /etc/cron.daily/taskto-backup-check, on the backup host
  find /srv/restic/taskto/snapshots -type f -mmin -1560 | grep -q . || echo "No TaskTo.AI backup in 26 hours"
  ```

- Practise a restore every quarter, on a machine that isn't production (below). A
  snapshot is only known good once it has been restored.
- For a second copy, sync the backup host's repositories to object storage with object
  lock (next section), from the backup host.

#### Or: S3-compatible storage with object lock

Without a second machine, a bucket with object lock does the same job: S3, R2, B2 and
MinIO support it. The server's key can write, but the storage keeps every version until
its retention ends, and the server's key can't remove versions. restic writes its own
files only once, so this works with restic as is, but restic doesn't manage the lock: the
bucket does. Set it up with the provider's own tools:

- a bucket with **versioning** and **object lock**, with a default retention (compliance
  mode, so not even an admin key shortens it), say 30 days;
- a key for the server that may only `ListBucket`, `GetObject`, `PutObject` and
  `DeleteObject`, the last for restic's lock files. On a versioned bucket, deleting only
  hides an object behind a delete marker; its versions stay. Not `DeleteObjectVersion`,
  `BypassGovernanceRetention`, or any `PutBucket*`, `PutObjectRetention` or
  `PutLifecycleConfiguration`;
- a lifecycle rule that removes versions once they are hidden and older than the
  retention: that, not the server, is what frees space;
- pruning (`restic forget --keep-within … --prune`) from another machine, with a key that
  stays there.

On the server, point `backup/repository` at the bucket (`s3:https://s3.example.com/my-bucket/taskto`),
and give restic the key in a file, through `compose.override.yaml`:

```yaml
# compose.override.yaml
services:
  backup:
    environment:
      AWS_SHARED_CREDENTIALS_FILE: /run/taskto-backup/aws-credentials
```

What this guards less well than rest-server: whoever takes over the server can still hide
every object behind delete markers, and the data then lasts only until the lifecycle rule
removes the hidden versions. Recovering means removing the markers with an admin key. Set
the rule long enough to notice, and watch the bucket as you would the backup host. This
setup isn't covered by the project's tests; rest-server is.

### Restore

`taskto restore` writes a backup, gzipped or plain, into an **empty** data directory, and
checks every database it restores (`PRAGMA integrity_check`). It refuses a directory with
anything in it (a `taskto.lock` left on its own doesn't count), an archive that isn't a
TaskTo.AI backup or was cut short, and paths that would leave the directory. Stop the
server first: restore takes the data directory's lock, so it refuses a directory a server
runs on, and a server started on the directory before the restore is done is refused. A
restore that fails removes what it wrote, so the directory is empty for the next try.

From an archive file:

`docker compose down` stops the backup schedule too when its profile is in `.env`'s
`COMPOSE_PROFILES`; it uses the data volume, which can't be removed while it runs.

```sh
docker compose down                       # stops the server; the volume stays
docker volume rm taskto_data              # the data you are replacing: be sure, or keep it (below)
docker compose run --rm -T taskto restore - < taskto-20260102-030405.tar.gz
docker compose up -d
```

From the restic repository: take the snapshot out first, then replace the data with it. On
a new machine, put `backup/` back first (from your password manager); the server's user is
enough, since it can read.

```sh
umask 077
docker compose run --rm backup snapshots                                   # pick one, or use latest
docker compose run --rm -T backup dump latest /taskto.tar > taskto-restore.tar
docker compose down
docker volume rm taskto_data
docker compose run --rm -T taskto restore - < taskto-restore.tar
docker compose up -d
rm taskto-restore.tar                     # it holds everything, unencrypted
```

To keep the current data aside instead of removing it, copy the volume first
(`docker run --rm -v taskto_data:/from -v taskto_old:/to alpine cp -a /from/. /to/`).

Outside Docker: `taskto restore -data-dir /var/lib/taskto-new FILE`, or
`restic dump latest /taskto.tar | taskto restore -data-dir /var/lib/taskto-new -`, then
point the server at the new directory.

**Practise it.** On any machine with the binary (not production), restore into a scratch
directory and start a server on it: `taskto -data-dir /tmp/drill -addr 127.0.0.1:8099`.
Sign in, look at a few spaces, stop it and delete the directory. The project's own test of
this setup (`TASKTO_BACKUP_IT=1 go test -run TestAppendOnlyBackupAndRestore ./cmd/taskto/`,
with restic and rest-server installed) does the same with real restic and rest-server:
the server's user is refused when it deletes, the backup host's prune works, and the
latest snapshot restores into a server that starts.

## Conversations out of step with the agent runtime

The server records who owns each conversation; the agent runtime (`<data>/agent/`) keeps
their content. A backup holds both, so a restore keeps them in step. They can drift apart
two ways:

- **Lost:** the server has a conversation the agent runtime doesn't, because its data was
  wiped or restored on its own. The conversation's routes answer `410` and the server logs
  `the agent runtime doesn't have a recorded conversation`, with both IDs. Its content is
  gone.
- **Orphaned:** the agent runtime has a conversation the server never recorded, because
  recording it failed right after the agent runtime created it (logged as `recording a new
  conversation failed`). Nobody can reach it.

`taskto conversations check` lists both, by ID only (titles are private to their owners), and
`taskto conversations clean` repairs them: `-lost` deletes the server's rows of lost
conversations, in one transaction, and `-orphans` retires orphaned ones in the agent
runtime. If the agent runtime has none of the server's conversations, `-lost` refuses
until you add `-all`: that is what a wiped agent runtime looks like, but also a listing
gone wrong, so check which before deleting every conversation. Retiring stops the
conversation's work, and from then on the agent runtime refuses the conversation. Its records
stay in the agent runtime's database, which never deletes. A conversation the agent runtime
created less than 10 minutes ago isn't taken for an orphan, because the server may still be
recording it. Both commands talk to the agent runtime of the running server, so run them
while it runs with the agent runtime on:

```sh
docker compose exec taskto taskto conversations check
docker compose exec taskto taskto conversations -lost -orphans clean
```

Outside Docker, add `-data-dir` (or `-config`) as for `taskto backup`.

## Upgrade and rollback

The database schema changes only through migrations, which the server applies when it
starts, each in a transaction. They are append-only: a released migration is never
edited, so every server's database is the same as a fresh one at the same version. A server
refuses to start on a database that has migrations it doesn't know, that is, one a newer
version has written: an older binary never runs on a schema it doesn't understand.

So an upgrade can't be undone in place; the way back is the backup taken before it.

**Upgrade**, say from v0.1.0 to v0.2.0:

```sh
docker compose exec -T taskto taskto backup - > before-v0.2.0.tar.gz
git fetch --tags && git checkout v0.2.0
# in .env: TASKTO_VERSION=v0.2.0
docker compose up -d --build
docker compose logs taskto | grep 'applied migration'
```

With [prebuilt images](#prebuilt-images) (`TASKTO_IMAGE_PREFIX` set in `.env`, and logged
in if the registry is private), check out the same tag for its `compose.yaml`, then `docker compose
pull` and `docker compose up -d --no-build` instead of `--build`. Without the prefix,
`pull` would look for the images on Docker Hub, where they aren't published.

The old image stays as `taskto:v0.1.0` until you remove it.

**Roll back** to v0.1.0 with the data from before the upgrade (anything written since is
lost):

```sh
git checkout v0.1.0
# in .env: TASKTO_VERSION=v0.1.0
docker compose down
docker volume rm taskto_data
docker compose build
docker compose run --rm -T taskto restore - < before-v0.2.0.tar.gz
docker compose up -d
```

The base images are pinned by digest in the `Dockerfile`, so rebuilding a version uses the
same bases; their security updates come with new versions.

The agent runtime's database follows the same rule: it is in the same backup, and restored
with it.
