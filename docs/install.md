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

The owner then invites everyone else ([API](api.md#invites)). With
[single sign-on](#single-sign-on), an invited person can sign in with Google or Authentik
instead of choosing a password.

Everyone changes their own password in **Settings → Password**, with their current one;
that signs them out everywhere else. Someone who lost theirs asks an admin for a reset link
(**Settings → Members → Reset link**), which works once, for 24 hours; the admin sends the
link. An admin can make one only for a member whose every workspace they administer
([API](api.md#passwords)). Once [email](#email) is set up, the login page also offers
**Forgot your password?**, which emails the person the same kind of link.

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

### Web Push notifications

Notifications always appear in each person's **inbox** (in the sidebar, with its unread count). **Web Push**
is an optional extra on top: a person turns it on, per device, in **Settings → Notifications**,
and only then does that device get a prompt when something new arrives while TaskTo.AI is closed.
Nothing about it is required: with push off, unavailable or blocked, every workflow works, and a
push is never the way anything is synced. The server makes its VAPID key pair by itself the first
time it starts (the private key is sealed in the database with `server.key`, so a backup needs both,
as for the other sealed secrets); nothing needs configuring, except, if you like, who the push
services may contact:

- **Contact.** Every push carries a contact (the VAPID subject, RFC 8292) that a push service may
  use to reach whoever runs the server. It is `https://taskto.ai` unless you set `-push-contact`,
  `TASKTO_PUSH_CONTACT` or `push_contact` in the config file (in that order of precedence) to a
  `mailto:` address or an `https:` URL of your own, such as `mailto:ops@example.org`. Anything else
  is refused at start.

What the server needs to send a push:

- **Outbound HTTPS to the browsers' push services**, directly: Google's for Chrome and Android,
  Mozilla's for Firefox, Apple's for Safari and the iPhone's Home Screen app. Which one a device
  uses is its browser's choice; the address is in the subscription the browser gives. Port 443
  out, to public addresses. The server connects under the **egress policy for public addresses
  only**, and that isn't configurable: an endpoint is an address a person's browser reported, so
  loopback, private, link-local, CGNAT and metadata addresses are refused on every connection
  (checked after DNS resolution, as for [provider accounts](#where-provider-accounts-may-connect)),
  and redirects aren't followed. A subscription's address is checked against the same policy when a
  browser registers it (the person is told it isn't allowed), as well as on every push. It doesn't use the `HTTP_PROXY` and `HTTPS_PROXY` variables: a
  network that only lets servers out through a proxy needs a transparent one, or an allowance
  for the push services' addresses.
- **DNS** for those names, as for any outbound call.
- **A secure connection for the web app itself** (step 3): browsers offer push only to a page
  on HTTPS, and to a service worker.

When the server can't reach a push service, people see no difference except the missing prompt:
notifications are still made, still shown in the inbox, and the badge still counts them. A push
that fails with a busy or unreachable service is tried again up to five times over about forty
minutes, then dropped, and the log says `a push service didn't answer` with the service's host
(never the subscription's address, which is a capability). A push the service refuses (`400`,
`401`, `403`, `413`), or to an address the egress policy refuses, is dropped at once, and the log
says so. A service that answers `404` or `410` for a subscription has ended it: the server removes
it. A subscription is also removed when its person signs out of that browser, when its session
ends (expired sessions are swept every hour, and no push is sent to one in the meantime), and when
they leave their last workspace. Finished pushes and old notifications are purged on a schedule of
their own, which a long queue of pushes doesn't hold up.

On an iPhone or iPad, push works only for TaskTo.AI **installed on the Home Screen** (iOS 16.4 or
later), as in the install steps above: the settings page says so, and in a Safari tab the inbox
is all there is. The page tells the person when their browser has blocked notifications, and how to
allow them.

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

### Email

The server can send email: the settings page's test message, the password reset a person asks for
on the login page, the invitations an admin chooses to email, and the notifications that later
features add. It is **off until the operator names an SMTP
server**, and works the same for one person or a company: there is one SMTP server for the whole
server, set here, never per workspace, and nothing in the database. Without it nothing changes:
reset and invite links are still passed on by hand.

**The links in a message are built on `public_url`** (`-public-url`, `TASKTO_PUBLIC_URL`: the
server's external address, `https://taskto.example.com`), never on the address a request came
to, which someone asking for a reset email could choose. Without `public_url` the server
doesn't email links: it says so in its log at startup, the login page doesn't offer **Forgot
your password?**, and the invite dialog doesn't offer to email.

| Setting | Flag / environment | |
|---|---|---|
| `smtp_host` | `-smtp-host`, `TASKTO_SMTP_HOST` | The SMTP server, a name or address without a port (an IPv6 address is written `2001:db8::25` or `[2001:db8::25]`). Empty means no email. |
| `smtp_port` | `-smtp-port`, `TASKTO_SMTP_PORT` | `587` with `starttls`, `465` with `tls`, `25` with `none`, unless set. |
| `smtp_security` | `-smtp-security`, `TASKTO_SMTP_SECURITY` | `starttls` (the default: the connection is upgraded to TLS before anything is sent, and a server that doesn't offer it is refused), `tls` (implicit TLS) or `none` (a relay on the same host or network you trust, **with no sign-in**: a user name and password are never sent without TLS, to any host, `localhost` too, so `none` with `smtp_username` stops the server at startup). The server's certificate is always checked. |
| `smtp_from` | `-smtp-from`, `TASKTO_SMTP_FROM` | The sender of every message: `taskto@example.com` or `TaskTo <taskto@example.com>`. Required with `smtp_host`; use an address your SMTP server may send as. |
| `smtp_username` | `-smtp-username`, `TASKTO_SMTP_USERNAME` | The account to sign in as (`PLAIN` or `LOGIN`). Leave out for a relay that lets the server send without signing in. |
| `smtp_password_file` | `-smtp-password-file`, `TASKTO_SMTP_PASSWORD_FILE` | The absolute path of a file holding the password, which goes with `smtp_username`. |
| `smtp_egress` | `-smtp-egress`, `TASKTO_SMTP_EGRESS` | Where the connection may go: `any` (the default), `public`, or a comma-separated list of `public`, hosts, addresses and CIDR prefixes, as for [provider accounts](#where-provider-accounts-may-connect). Every connection is checked after name resolution. An internal relay on a private address needs it listed here, or `any`. |
| `integration_egress` | `-integration-egress`, `TASKTO_INTEGRATION_EGRESS` | Where calls to [integrations](integrations.md)' providers may go: `public` (the default, and what an empty value means: every connection's address is checked after name resolution, so loopback, private, link-local and metadata addresses are refused), `any`, or a list as for [provider accounts](#where-provider-accounts-may-connect). Don't list a provider's name: a listed name is trusted wherever it resolves, which switches the address check off, and each adapter already pins its own hosts. A restricted policy ignores the environment's proxy, so a server that reaches the internet only through a mandatory proxy needs `any`. |
| `update_status_dir` | `-update-status-dir`, `TASKTO_UPDATE_STATUS_DIR` | The directory where the host's updater writes the status admins see in Settings → About, an absolute path. The server only reads it. The Compose install sets it to the read-only mount of the updater's `public` directory ([automatic updates](#automatic-updates)); empty, the page says no updater is shown. |
| `update_request_dir` | `-update-request-dir`, `TASKTO_UPDATE_REQUEST_DIR` | The directory where Update now (Settings → About) puts its request for the host's updater to take, an absolute path. The one place of the updater's that the server writes: the Compose install mounts the updater's `request` directory there, read-write ([Update now](#update-now)); empty, the button says it is unavailable. |

**The password is never a setting.** It is read from its file when the server starts, kept only in
memory, and is never logged, never returned by the API (the settings page says only whether the
server signs in) and never in an error. The two variables that name the account
(`TASKTO_SMTP_USERNAME`, `TASKTO_SMTP_PASSWORD_FILE`) are removed from the server's environment
once read, so the agent runtime and the CLI workers never inherit them. A file that is missing,
unreadable or empty stops the server at startup, naming the file. Restart the server after
changing any of these.

With Compose, put the password in a file next to the project, mount it read-only, and set the
variables in `.env`. The container runs as `65532`, so the file must be readable by that user:

```sh
mkdir -p smtp && printf '%s' 'the-smtp-password' > smtp/password
sudo chown -R 65532:65532 smtp && sudo chmod 700 smtp && sudo chmod 600 smtp/password
```

```yaml
# compose.override.yaml
services:
  taskto:
    volumes:
      - ./smtp:/etc/taskto/smtp:ro
```

```sh
# .env
TASKTO_SMTP_HOST=smtp.example.com
TASKTO_SMTP_FROM=TaskTo <taskto@example.com>
TASKTO_SMTP_USERNAME=mailer-account
TASKTO_SMTP_PASSWORD_FILE=/etc/taskto/smtp/password   # as the container sees it
```

Then `docker compose up -d taskto` (a change to `.env` needs the container recreated). The server's
log says `outgoing email is on`, with the host, port and security, never the account.

**Check it:** an admin opens Settings → General → Email, which shows the server, the port, how it
is secured and the sender (the sender is shown to the operators, the admins of the first workspace; the user name and the
password never are, so give the account a name that isn't also the sender's address if you
would rather not show it), and presses **Send a test email**. The message goes to the admin's own
address, in their language, and nowhere else. If it fails, the page says at which step (the
connection, the TLS upgrade, the sign-in, or the server refusing the message); the server's own
reply is not shown, since it can name internal hosts or repeat what was sent: the log
(`mail: the test email failed`) has the step and the reply's number only, and never the server's
words. The test is limited to one a person every 30 seconds, and three at once on the server.

**Notification email.** With an SMTP server **and** `public_url` (the server's external origin, as
for [single sign-on](sso.md); the links in a mail go there), people can choose in Settings →
Notifications to be emailed about a kind of notification as it happens, or in one summary a day. It
is off for everyone until they choose, and a mail says only what happened and links back, never what
a card or an app holds ([pwa.md](pwa.md#email)). Before the first notification mail to an address,
its owner opens a confirmation link sent to it. Without the SMTP server or the public URL the
choices say so and nothing is sent.

Messages are plain text with a simple HTML alternative, translated into the recipient's language
(the one they picked, else their workspace's default, else English), with their texts in the
binary (`internal/mail/locales`, one file per language). A language without a message uses
English for it.

### Single sign-on

People can sign in with Google, Authentik or any other OpenID Connect provider, once the
operator lists it in a providers file. Only people with an account or a pending invite get in;
nobody signs up by signing in. [sso.md](sso.md) has the provider-by-provider setup, how to
restrict who may sign in, and the security notes.

With Compose, put the providers file and the secrets it names in a directory next to the
project, mount it read-only, and set the three variables in `.env`. The container runs as
`65532`, so the files must be readable by that user: owned by
it with mode `600`, in a directory it can traverse:

```sh
sudo chmod 600 sso/* && sudo chown -R 65532:65532 sso && sudo chmod 700 sso
```

The files' modes are set first: once `sso` is `65532`'s alone with mode `700`, your own shell
can no longer expand `sso/*`.

The host directory above it (the project directory) must let `65532` reach `sso`: at least
execute permission for others. Keep the mount `:ro`. The public address must be
the `https` one people use, since it makes the redirect URIs you register at the provider:

```yaml
# compose.override.yaml
services:
  taskto:
    volumes:
      - ./sso:/etc/taskto/sso:ro
```

```sh
# .env
TASKTO_SSO_PROVIDERS=/etc/taskto/sso/providers.json   # as the container sees it
TASKTO_PUBLIC_URL=https://tasks.example.com
# TASKTO_DISABLE_PASSWORD_LOGIN=true                  # once a provider works: no passwords
```

The redirect URI for a provider with the id `google` is
`https://tasks.example.com/api/sso/google/callback`. A client secret named by
`{"env": "VAR"}` has to be set in the server's environment too (`environment:` in the
override), which is why `{"file": …}`, read from the mounted directory, is preferred. Restart the server
after changing the file (`docker compose up -d taskto` after a change to `.env` or the override, which recreates the container; `restart` keeps the old environment). Turning passwords off leaves the owner signing in through the provider
with the email they set up with ([sso.md](sso.md#turning-passwords-off)).

### Who may create a workspace

Setup makes the first workspace. People make more with "New workspace" in the workspace
switcher, as far as the operator lets them (`-workspace-creation`, `TASKTO_WORKSPACE_CREATION`,
`workspace_creation` in the config file):

| Value | Who may |
|---|---|
| `admins` (the default) | anyone who is an admin of at least one workspace, so the owner, and whoever they make an admin |
| `anyone` | every signed-in person, members of other workspaces included |
| `off` | nobody: the server keeps the workspace setup made |

Whoever creates a workspace is its first admin and the only person in it; they invite the
others. A workspace shares nothing with the others: its members, spaces, boards, apps,
documents and tokens belong to it, and the person who made it is no admin of any other
because of it. `anyone` suits a server that is a small community of people each with
workspaces of their own; on a company's server, leave `admins` or turn it `off`, so no
one outside the admins adds workspaces to your storage. A person who is a member of
workspaces only, or of none (they were removed from theirs), can't create one under `admins`.
Changing the setting needs a restart (`docker compose up -d taskto` after editing `.env`),
and workspaces already made stay as they are. Each creation is in the audit log
(`workspace_created`), on the new workspace's own log, not the first's. Whatever the setting, one
account can create at most 10 workspaces and 5 an hour, so a single session can't fill the server.

**Operators.** The people who run the server are the admins of the workspace setup made (the
first one; a stored marker, not an id). Only they see what belongs to the whole server in
Settings: the backups' status (which can name the backup host) and the outgoing email. The admins
of a workspace someone else created, with `anyone`, don't, and their requests for those get `403`
(`operator_only`). Everything else an admin does is inside their own workspace.

```sh
# .env
TASKTO_WORKSPACE_CREATION=anyone   # admins (default), anyone or off
```

### Two-factor login

Anyone can turn on a second step for their password login in **Settings → Password →
Two-factor login**: a six-digit code from an authenticator app (any TOTP app: Aegis, 1Password, Google
Authenticator, Authy, a hardware key's OATH), and ten single-use recovery codes to print or keep
in a password manager. There is nothing to configure: it needs `server.key`, which the server
makes on first run. What to know as the operator:

- **Back up `server.key` with the database.** The secrets are sealed with it. A restore with
  a different key leaves every user's secret unreadable, so their logins fail at the code until
  an admin clears it (Settings → Members → **Clear two-factor**) and they turn it on again.
  Restore the `server.key` that goes with the database. If it is lost and no admin who may clear
  the member can sign in (an owner who is the only admin, say), use the stopped-server recovery
  below.
- **A lost phone and lost recovery codes** is an admin's job: Settings → Members → *Clear
  two-factor* for that member. It works under the rule for a password reset link: only
  an admin of every workspace the member belongs to may do it, and the member's sessions end.
  The member then signs in with their password and turns it on again. An owner who is the
  only admin and lost both has to restore from a backup, or edit the database: delete their
  rows in `user_totp`, `user_recovery_codes` and `login_challenges`, with the server stopped.
- **Signing in with a provider has no second step**: the provider's own sign-in is trusted
  as it is. Ask the provider for two-factor login if you want it there
  ([sso.md](sso.md#two-factor-login)). Access tokens and the CLI turns have none either.
- The authenticator's clock must agree with the server's: codes are accepted for the current
  30-second step and the one either side, so a difference of up to 30 seconds always works, and
  a larger one only depending on where in a step the two clocks are. Keep the server's clock
  right (NTP).

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

## Privacy and account deletion

A person can delete their own account, and only they can ([api.md](api.md#deleting-an-account)):
that is the one way personal data leaves the server. An admin who removes a member from a workspace
leaves the account, and what it owns, as it is. This is what happens, and what doesn't.

**Grace period.** The account is switched off at once and kept for `-account-deletion-days`
(`TASKTO_ACCOUNT_DELETION_DAYS`, `account_deletion_days`; default 14, at most 365). During it an
operator can cancel the deletion in Settings → Members ("Accounts waiting to be deleted"); the
person can't sign in, but nothing is lost. It ends with the purge, which a server that was off at
the time runs when it starts again. Telling people the period, and that an operator can cancel, is
part of your own privacy notice: the web app says it where a person deletes their account.

**What the purge deletes** (what the person owned alone): their private spaces in every workspace
(documents and unversioned files, personal boards, private apps with their records and photos), their
conversations (with the agent runtime's records of them), their CLI logins and shared-account
mappings and everything the CLI worker kept for them (including the agent browser's profile), the
agent runtime's providers file and key files (which hold their provider accounts), their unfinished
photo uploads in shared apps, their access tokens, sessions, push subscriptions, notification
settings, email ledger, two-factor data and single sign-on identities, the invitations to their
address, the search index's rows for all of it, and the account.

**What stays: what they made for others.** Cards, comments, documents and app records in shared
spaces, and the audit log, stay, shown as "Deleted user", and so do the photos of shared apps' records
(even when an unfinished move of the app had left their files in the person's private space: the purge
finishes it first) and what the apps' effects did, without who. A comment's mention of them stops
naming anyone, and no account is ever given their user number again. The person's name and address are replaced
in the shared spaces' git history (a history rewrite, in every shared space of the server, so a space
of a workspace they had left is covered too), and in the audit log. A person who wants their
contributions to shared spaces gone too asks the space's admins to delete them as content; that
is a decision about the workspace's records, not an account's.

**What a purge can't reach.**

- **Backups.** A backup made before the purge still holds the data. They age out by your
  [retention](#backups) (restic's `forget`/`prune`, or deleting old archives): say in your privacy
  notice how long that is. And a **restore** brings back the accounts the backup had: re-run the
  deletions of the accounts that were deleted after it (the log, if kept, says which), or a
  restored account that was purged is a live one again.
- **Steps that wait for the agent runtime or the CLI workers.** Pi Durable, the runtime, has no way
  to delete a conversation, so the purge **retires** each one (its work stops and the runtime refuses
  it from then on) and has the runtime **purge** its records, in `<data>/agent/agent.db`, with the
  deleted content zeroed in the file. It also rewrites the runtime's providers file, and removes the
  CLI worker's directory of the person (`<data>/cli/u<id>`, or the worker container's own copy). Each
  step is recorded before it is done (the `purge_cleanups` table) and removed from the record when it
  is: with the agent runtime or the CLI workers **off**, the account is deleted and the steps that
  need them stay recorded, and are done by the next start with them on and every ten minutes. The log
  says "purged an account, with steps still to do" while any is left, and the audit event's
  `cleanups_pending` says how many (the providers file's rewriting, which is done after the account
  is gone, is one). If the runtime is on and fails, the purge fails and is tried again; the runtime
  answers `503` while another connection holds its database, so a purge isn't acknowledged until
  the write-ahead log, which can still hold the transcript, is empty. The cause of a failed step is
  in `last_error` of `purge_cleanups`, not in the log.
- **Model providers and CLI vendors.** What a conversation sent to a provider is theirs to delete.
- **SQLite and the filesystem.** Deleted rows are overwritten as the database is reused, and
  deleted files are only unlinked; use disk encryption if remnants matter to you.
- **Authorship you didn't give to the server.** Text a person typed into another person's
  document or comment (a quotation of their name) is content, not authorship.

## Deleted records of apps

Deleting a record of an [app](api.md#apps) is soft: the record stays, with its history, and
whoever may change it can [restore it](api.md#post-appsapprecordsrecordrestore). To keep that from
growing the database for ever (a loop of inserts and deletes, by a person, an access token or the
agent, would), the server removes a deleted record, with its history and its photos, once it
has been deleted longer than the retention window: **90 days** unless you set it.

```sh
taskto -app-deleted-retention 30     # or TASKTO_APP_DELETED_RETENTION=30, or app_deleted_retention = "30"
taskto -app-deleted-retention off    # keep deleted records for ever
```

The setting is the server's, for every app of every workspace. A sweep runs when the server
starts and every ten minutes, in batches of 200 records so that nobody's writes wait on it, and
logs how many records it removed. What it does and doesn't touch:

- Only records deleted before the window are removed. A live record is never touched, and
  restoring a record inside the window starts its time again if it is deleted later.
- A record with an effect (a card, a document) still waiting to run is kept until it has run.
- The id of a record the sweep removes is never given to another, so a change still queued on a
  phone for it can't land on a different record of the app.
- The receipts of the operations that made a record, which let a phone that was offline send a
  change again safely, are kept for their own 30 days whatever the window, so a late replay is
  answered as it would be (its record may be gone: [docs/offline.md](offline.md)).
- A removed record's photos are removed with it: their rows at once, their files by the next
  sweep of files no row knows, within a day.
- Backups made before a record was removed still hold it until they are pruned.
- Independently of the window, a record's history keeps its newest 500 versions, so a record
  edited in a loop is bounded as well; the oldest versions, the first, are dropped as new ones are
  made. The record's own data is never affected.

Deleted records are not counted against [storage quotas](quotas.md), which count files, photos
and the knowledge base: the window is what bounds them.

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

## Stopping the server

On SIGTERM or SIGINT (what `docker compose stop`, `down` and a restart send) the server stops
taking requests and shuts down in bounded steps, so that nothing writes the data directory
after it lets go of it:

| Step | At most |
|---|---|
| Requests finish on their own, then are ended | 10 s |
| The handlers that ignored that are waited for | 5 s |
| The emails the server accepted | 10 s |
| The CLI workers' proxy | 10 s |
| The scheduler's running jobs | 10 s |
| The agent runtime: SIGTERM, then SIGKILL after 15 s, and its tools calls | 20 s |

The bounded steps add up to 65 s at most, and a quiet server stops in well under a second. The
server also waits, with no time limit, for the audit cleanup and the attachment sweep to return
after they are cancelled; both return as soon as they are, but if either one did not, the stop
would take longer than 65 s. Docker kills a
container that is still running when its stop grace period ends, and its default is 10 s, so
`compose.yaml` sets `stop_grace_period: 90s` for the server. Keep it at least that long if you
override it in `compose.override.yaml`; the update script takes the setting as it is.

If something still runs when its time is up (a handler that doesn't stop), the server logs it,
leaves the database open for it and exits with an error. The data directory's lock stays held
until the process is gone, so a second server or `taskto restore` is refused until then.
SQLite recovers its write-ahead log on the next start. The same shutdown follows when the
listener fails: the server drains, then exits with the listener's error.

## Upgrade and rollback

The database schema changes only through migrations, which the server applies when it
starts, each in a transaction. They are append-only: a released migration is never
edited, so every server's database is the same as a fresh one at the same version. A server
refuses to start on a database that has migrations it doesn't know, that is, one a newer
version has written: an older binary never runs on a schema it doesn't understand.

So an upgrade can't be undone in place; the way back is the backup taken before it. A server
installed with [deploy/INSTALL.md](../deploy/INSTALL.md) upgrades itself every night
([Automatic updates](#automatic-updates)); when an upgrade fails after the new server has
started, it stops and tells you, and the way back is the by-hand one that follows.

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

## Automatic updates

A server from [deploy/INSTALL.md](../deploy/INSTALL.md) keeps itself on the latest release.
`taskto-update` (`deploy/taskto-update.sh`) is a program of the host, not of the server: a
systemd timer runs it every night (at `TASKTO_UPDATE_AT`, UTC, spread by up to half an hour,
and when the VM was off at that time, at the next start). It uses what the install already
has (bash, curl, jq, `docker compose`, `flock`), and nothing gives the server container the
Docker socket. It asks GitHub's public API what is newest, so it needs no token.

It does the safe common case, and nothing else: when it can't be certain, it refuses, says
so in its status and the journal, and leaves the server as it was.

**What is "latest".** `TASKTO_UPDATE_CHANNEL` in `/etc/taskto/install.env`:

- `release` (the default): the newest GitHub Release named `vMAJOR.MINOR.PATCH`, not a draft
  or a pre-release, whose three images are published. A release whose images aren't there yet
  isn't available: the next run looks again. How to cut one: [releasing.md](releasing.md).
- `main`: the newest commit on `main` whose CI passed, that descends from the one installed,
  and whose images are published. **`main` ignores releases and the `manual-upgrade: true` marker**:
  it takes every merge that passed CI, with no gate and nobody deciding, so a change that
  needs a person (a new required setting, a step on the host) reaches the server as it is merged. It is for a server you use to try what is about to be released, not for
  one that people depend on; the checks and the way back below are the same.
- `off`: nothing runs by itself.

`TASKTO_UPDATE_REPO` (default `taskto-ai/taskto`) is the repository it reads, for a fork.
A version is exactly `vMAJOR.MINOR.PATCH`, with no leading zeros, no pre-release suffix and no
build metadata; anything else isn't a release to it, and `--to` refuses it. Versions compare
as decimal strings, by length and then by digits, so `v1.10.0` is newer than `v1.9.0`. It
never downgrades and never "updates" to the version it is on.

**What it leaves to a person.** Each of these ends the run as `skipped` or `blocked` with the
reason in the status; none changes anything.

- *A hold.* While `/etc/taskto/update.hold` exists, no automatic run updates. Make it with
  `touch`, remove it to go on.
- *A release that needs a person.* A release whose notes have the line `manual-upgrade: true`
  isn't installed, nor is any later release while this install is older than it. The updater
  reads every page of the releases down to the installed version, and if a page can't be
  read it does nothing (it fails closed). Follow the notes, then `taskto-update --to vX.Y.Z`.
- *A version that changes the worker's firewall.* If the new version's
  `deploy/worker-firewall.sh` differs from the installed one, the updater skips it ("the
  worker firewall changed: update by hand") and never restarts the firewall's unit itself.
  Install the new script and apply it as step 8 of the runbook does, then `taskto-update --to`.
  The firewall's rules must also be in place, in every run before the update and after it;
  if they aren't, the update isn't made.
- *An update that was interrupted.* A run that is stopped half way (a crash, a reboot, a kill)
  leaves `/var/lib/taskto-update/transaction`, written before the first change. While it
  exists, nothing runs (not even `--to`; the status says `interrupted`, with the phase, the
  backup and where the old files are): the files may be on either version. Look at it, put it
  right with [Upgrade and rollback](#upgrade-and-rollback) and the `previous/` files and the
  backup it names, then run `taskto-update --clear-block`.
- *A server that isn't the version on record.* If `/healthz` answers as another version than
  `/srv/taskto/.env` says, or doesn't answer, the run reports it and does nothing (for another
  version it also writes the `blocked` file, which stays until `taskto-update --clear-block`
  removes it).
- *An update that failed after the new server started, or whose way back failed.* See
  [When an update fails](#when-an-update-fails): the stack is stopped (or left as it is),
  and nothing runs again until a person has restored the backup and cleared it.

`taskto-update --dry-run` chooses and checks and changes nothing on the install (it does pull
the images, to check them, and logs in to a private registry for that, with a Docker config of
its own that is removed after, so the operator's isn't changed; it makes no backup directory);
`taskto-update --to VERSION` (a release tag or a full commit SHA) installs that version,
whatever the hold, the channel and the manual-upgrade marker say, but never an older one, and
never over an interrupted or stopped update.

**One run**, with a lock so two never overlap:

1. *Resolve and check, before touching anything.* The tag becomes one commit SHA, through the
   API, and every file of the new version (its `compose.yaml`, `deploy/Caddyfile`, the
   firewall script and `deploy/safety-check.sh`) is fetched at that SHA, never by a tag or a
   branch. It then checks, and refuses with nothing changed if any of this fails:
   - the Compose inputs are the plain ones: `compose.yaml`, and `compose.override.yaml` when
     it exists, and no other Compose file in `/srv/taskto` (`compose.yml`, `compose.override.yml`,
     `docker-compose.yml`, `docker-compose.yaml`, `docker-compose.override.yml`,
     `docker-compose.override.yaml`) and no `COMPOSE_FILE` in `.env` or the environment,
     since Compose would read those as well. An install with any is refused, and updated by
     hand. Every Compose call of the updater (the check of the new settings, `create`, `start`,
     `stop` and the way back) is given the same explicit `--project-name`, `--env-file` and one
     `-f` list, so what starts is what was checked;
   - the images the running containers use are on this machine, by their ids (what the
     running server, worker and, if it runs, the nightly backup have, not what a tag points
     at now); they are kept under `taskto-update-rollback:<image>` tags before the update
     changes anything, so that a moved version tag or a plain `docker image prune` (dangling
     images) can't take them. They do **not** protect the images from `docker image prune -a`
     or `docker system prune -a`: an image no container uses is removed whatever its tags. So
     don't prune images while an update is blocked (the way back, and a person's rollback,
     need them), and not before you have decided not to roll back;
   - the data volume is mounted at `/data` as the running server has it (never a name
     assumed): the same Docker volume, the same `volume.subpath` and the same read-only
     setting, in this install's Compose settings and in the new ones, for the server and for
     every service of TaskTo's, the backup services too, which are rendered with their
     profiles on even when they are off. A candidate that mounts the data differently is
     refused. So is one that would make a volume: every named volume the new settings use must
     exist already, and an anonymous volume is refused, so `docker compose` never creates one;
   - the new settings, rendered with this install's `.env` and `compose.override.yaml`, run
     only the images it checks (an override that swaps in another image, or an older one of
     TaskTo's, is refused); all three images are pulled by tag and refused unless their
     `org.opencontainers.image.revision` label is that SHA, and their ids are recorded (the
     switch checks, right before it starts anything, that the tags still have them); every
     other image of the enabled services (Caddy's, pinned by digest) is pulled too, since the
     switch pulls nothing;
   - the safety rules hold in those settings, **before anything is started**:
     `deploy/safety-check.sh --static` checks the rendered new settings with the same rules as
     runbook step 12 does on the running containers (the server's user is 65532, no
     privileged container, `cap_drop: ALL`, `no-new-privileges`, no capability added to the
     server and only the worker's four to the worker, no development switch or secret in
     either environment, CLI workers in container mode, the worker with none of the server's
     data, no host path or Docker socket, and on its own network, the firewall's bridge; and
     the two ways to get mounts without naming them: no service with `volumes_from`, and no
     config or secret mounted into the worker; the user rule is the running check's, exactly
     `65532:65532`). The rule about the updater's own files (the server may see only its
     public directory, read-only, and can't write that) is about where a path leads, not how it
     is spelled: every host path the server would mount, whether a bind mount, a named volume's
     `device` or a file a config or a secret is made from, is resolved first (symbolic links
     followed, `.` and `..` worked out), and the options of a volume that exists already, or is
     external, are read from Docker (`docker volume inspect`), since a volume keeps the options
     it was made with whatever the Compose file says. The running check does the same, so what
     is refused before the new server starts is what would be refused after;
   - the public address answers as the current version before anything changes (Caddy's HTTPS
     address from this machine, or in `traefik` mode the LAN port and `https://<domain>/healthz`
     as a visitor reaches it): a broken proxy isn't the update's to fix;
   - the disk, the worker's firewall (its unit and rules), and `docker compose config` with the
     new file; a setting the new file needs that isn't there stops it here, and the status
     says which.
2. *Back up* into `/var/backups/taskto/before-update-from-<old>-to-<new>-<time>.tar.gz`
   (root only, mode 600; it holds `server.key`). Then it writes the record,
   `/var/lib/taskto-update/transaction` (old and new version and commit, phase, backup,
   volume, where the old files are kept), which stays until the run ends in a success or a
   revert, and after a stop until a person clears it.
3. *Switch*: the old files are kept in `/var/lib/taskto-update/previous/`, the new ones and
   `TASKTO_VERSION` (in `/srv/taskto/.env` and `/etc/taskto/install.env`) put in, the tags
   checked once more, and then, since a tag can move at any moment before the containers
   exist: `docker compose create` (the containers are made, nothing runs), a check that
   **each container was made from the image id that was verified** for its service (a
   container made from another image is removed, and the old version is put back), and only
   then `docker compose start`. When `deploy/Caddyfile` changed, with the HTTPS profile on,
   Caddy is recreated too, since it reads the file once. The way back is made and checked the
   same way, against the ids the old containers had.
4. *Check the server*: `/healthz` is `ok` at the new version with its agent runtime ready
   (and so is the proxy: `https://<domain>/healthz`, from this host), the services run, the
   firewall's rules are in, and `deploy/safety-check.sh`, the very script runbook step 12
   runs, holds in the running containers (what the static check can't see: a setuid program
   in the worker's image, the network Docker made).
5. *After a success*, and only then, the updater's own older backups are pruned: the newest
   three by the time in their names (regular files with the updater's name only, never this
   run's), and nothing else in that directory, nor the nightly backups.

**What operators see.** Settings → About shows the operators (the admins of the workspace the
server's setup made, who run it; an admin of a workspace someone made later doesn't see it)
the version the server runs, the version on record in `.env`, the updater's channel and hold,
its last run and last change (the result, when, the versions, a reason, whether it made a
backup), and, while the install waits for a person, what to do: a stopped or interrupted
update (`taskto-update --clear-block`, once the install is put right) or a release that needs
steps by hand (`taskto-update --to VERSION`). The same is at
[`GET /api/workspaces/{workspace}/updates`](api.md). The server gets it without gaining any
power over the host (the one thing it can do is [ask for a run](#update-now)): on every run (and each night a stop is still waiting) the updater writes
a copy of its status, made for this, to `/var/lib/taskto-update/public/status.json`
(`needs_person` in it is what the page's alert is made of), and `compose.yaml` mounts that one
directory into the server, **read-only**, at `/update-status` (`TASKTO_UPDATE_STATUS_DIR`).

The copy is **structured, never prose**: results and reasons are words of a closed list
(`reason_code` in the updater), versions are checked to be versions, and a backup is only
"made". It has no backup path, registry host, volume name or path of the updater's record, no
image digests, and nothing from the updater's own state (the transaction, the old files, the
address a notification goes to); those stay in the host's journal (`journalctl -u
taskto-update`) and in `/var/lib/taskto-update/status.json`, which the server can't see. The
server checks every word again when it reads. The server can't write there, run the updater,
or reach Docker, and `deploy/safety-check.sh` fails an install whose server can see anything
else of `/var/lib/taskto-update`, or can write there, by whatever path or link.

The directory is the updater's alone. If you move it (`TASKTO_UPDATE_PUBLIC` in `/etc/taskto/install.env`, which every run of the
updater reads, by hand or by the timer, with `TASKTO_UPDATE_STATUS_SOURCE` in `.env` set to
the same path), it may be anywhere, but the
updater writes there only if the directory is **owned by root, with every directory above it
owned by root and not writable by its group or others** (a symbolic link on the way must be
root's too, and where it leads is checked the same way); otherwise it logs why and publishes
nothing, and the update itself goes on. The file is made by a new temporary file the updater
creates in that directory (`mktemp`, never a name it could be guessed) and moved into place, so
nothing the server could plant is ever written through, and the safety check refuses a server
that mounts that directory (or one above or inside it) writable, however it is reached. Docker
makes the directory on its own if it isn't there yet (root's, readable by the server). On a
machine that isn't a host install (a try-out with `docker compose up`), point
`TASKTO_UPDATE_STATUS_SOURCE` at any directory you can mount; with no status in it the page
shows only the server's version, as it does when the server wasn't told a directory at all. A
status older than three days is called stale: the updater writes it on every run, so its timer
isn't running.

**The updater of an existing host must be reinstalled once.** The updater is a script of the
host (`/usr/local/sbin/taskto-update`), and the timer runs the copy that is there: updating the
server, by hand or by an older updater, doesn't update it. A host whose updater predates the
status never writes one, so the page has nothing to show, and says "the updater on this host is
too old to report" (a server that is a release, with the status directory set up, and no status
in it). So the first release that has this status needs one more step on every host that was
set up before it: **runbook step 16, run again** (its Run and its Check, from a checkout of that
release), which puts the release's `taskto-update`, its service and timer on the host, makes
the public directory and ends with `taskto-update --publish-status` on the host, which writes
the first copy at once, without checking for an update, instead of at the next night's run. On
the `main` channel, whose servers run a commit and not a tag, do the same from a checkout of
the commit the host runs (`TASKTO_VERSION` in `/srv/taskto/.env`) or a newer one, with
`TASKTO_VERSION` in `~/.config/taskto-install.env` set to it for the step: a `main` host that
isn't reinstalled keeps its old updater, however far it updates the server.

### Update now

Settings → About has an **Update now** button for the operators. It asks the host's updater to
do what its nightly run would do, now: install the newest version of its configured channel,
with the same checks, the backup and the way back. It is a request, and **the request carries
nothing**: no version, no repository, no flag. A compromised server can ask for an update to
what the channel already offers, and nothing else, and every gate of the nightly run holds for it:

- a hold (`/etc/taskto/update.hold`), a release whose notes say `manual-upgrade: true` (and every
  later one), a worker firewall that changed, an update that was stopped or interrupted, a server
  that isn't the version on record, and the channel off, each end the run as `skipped`, `blocked`
  or `interrupted`, with the reason in the status, and the page says why and that
  `taskto-update --to VERSION` on the host is how to install anyway, once it is dealt with;
- with `TASKTO_UPDATE_CHANNEL=off` the button is unavailable, and the path unit below is off.

How it goes, and what keeps the server from gaining power over the host:

1. The server writes **one small file**, `update-now`, into a directory of its own,
   `/var/lib/taskto-update/request` (`TASKTO_UPDATE_REQUEST_DIR` in the server; `compose.yaml`
   mounts `${TASKTO_UPDATE_REQUEST_SOURCE}` at `/update-request`, read-write). It is made by a
   temporary file in that directory and a hard link, so it appears whole or not at all, and a
   second one can't replace it: one request waits at a time, and none is taken while a run is
   changing the install. Its content is a time and a random nonce, no secret. The request is an
   [audit event](audit.md) (`server_update_requested`), committed before the file is made visible
   to the host (it is written under another name first), so no update happens without its record.
   This event records the operator's intent, not delivery or installation. If publication fails
   after the commit, the API reports an error and the intent remains in the audit log; a crash
   in that window can also leave only the intent. Hidden temporary files are never replayed on
   restart. The operator can retry; the host's status reports whether a request actually ran.
2. `taskto-update-request.path` (a systemd path unit) starts `taskto-update-request.service`
   when that file exists, which runs `taskto-update --requested`. Under the updater's lock it
   **renames the file out of the directory first** (so nothing the server can change there is
   read or followed), looks at what it is, and removes it, so it can't start again by itself. A
   request that isn't a regular file of at most 512 bytes (a link, a directory, a fifo, a large
   file) is removed and ignored, and so is one whose whole content isn't the one fixed line the
   server writes (a time and a nonce). The content is only ever matched against that pattern; it is
   never used as an argument. Then it is the nightly run.
3. The host ignores a request within ten minutes of the last requested run, and the same request
   twice (the page says so); a request that comes while the nightly run is going waits for it.
4. The status copy the server reads has what the page needs: whether this host can take a
   request (`requests.supported`: the directory is in place and the path unit is on), whether a
   run is going on (`running`), which runs were requested, and what became of the last request
   (`requests.last`, which also covers one turned away before it ran). While it runs the server
   restarts, and the page keeps asking and shows the result once it answers again.

The request directory is **the one place of the updater's the server may write.**
`deploy/safety-check.sh` fails an install where the server mounts it in any other way, any other
path of the updater's, or where any service but the server (the CLI worker above all) mounts it or
a directory that holds it or is inside it, however the path is spelled. The updater reads it only
if it is a real directory (never a link), owned by the server's uid (65532), on the same
filesystem as the updater's state, and under directories only root can write. A host that doesn't
have it (installed before the button) keeps working: the button says the host can't take a
request yet, and **runbook step 16, run again**, installs the path unit and makes the directory.
Docker makes a missing source directory itself, as root's, which the server can't write, so
the host reports it as not in place.

## When an update fails

What happens depends on one fact: did the **new server start**? A new server may have run
migrations, and an older one can't read what a newer one wrote.

- **Before it started** (a failed pull or check, the static safety check, `docker compose up`
  failing before the server's container ran): nothing of the data was touched. The updater puts
  the old files, `.env` and images back (the images by the ids the running containers had),
  starts the old version, checks it with its own safety script, records the release as failed
  (so it isn't tried again every night), and reports `rolled-back`. If putting the old version
  back fails too, it stops as below, with `rollback-failed`.
- **After it started** (the server isn't healthy, a safety rule or the proxy check fails on
  the running containers, or `start` fails once the server has run): the updater **turns off
  the restart policies** of the project's containers (`docker update --restart=no`, so that a
  reboot or a restart of Docker can't start the new version on the data it may have changed),
  **stops the stack** (`docker compose stop`, never `down`, never removing a volume) and
  checks that nothing is running, keeps the pre-update backup, writes the `blocked` marker and
  keeps the transaction record, and reports `stopped` loudly: the status, the journal and the
  notification say which backup to restore, and in which data volume (the one recorded in the
  transaction, never a name assumed). If it can't confirm that nothing runs, the result is
  `stop-failed`, not `stopped`, and says to stop the stack by hand now; the record keeps that
  phase, so each blocked night repeats the instruction until you clear the stop. If the
  restart policies couldn't be turned off (or the containers couldn't be listed to do it), the
  report says so, and never that they are off. The containers' restart
  policies stay off until they are made again: the Rollback's steps remove the containers and
  step 9 makes them again, with the Compose file's policies. It does
  **not** restore data, empty or copy a volume, or put anything back: it can't know what the
  new version did to the data. A person does, by hand: restore the backup named in the report
  with [Upgrade and rollback](#upgrade-and-rollback) (and
  [Rollback](../deploy/INSTALL.md#rollback) in the runbook), start the old version, check it
  and run `taskto-update --clear-block`.

A release that was put back (`rolled-back`) is recorded as failed, durably (a file of its own,
flushed and moved into place), **before** the transaction record is removed. If it can't be
recorded (`/var/lib/taskto-update` isn't writable), the record stays, the result is
`rolled-back` with exit status 2, and the next runs are blocked, so the release can't be tried
again unseen: make the directory writable and run `taskto-update --clear-block`, which records
the release first and refuses while it can't.

While `/var/lib/taskto-update/transaction` exists nothing runs again, not even `--to`; every
run says it is blocked (and notifies, below). It is the record that blocks: if the `blocked`
marker can't be written, the record alone still does. `taskto-update --clear-block` removes
both, and only if the server is healthy and answers as the version in `/srv/taskto/.env`
(which is also the version in `/etc/taskto/install.env`), the worker's firewall rules are in,
and the safety rules hold in the running containers (the updater fetches the rules of the
version that runs, at its commit, and runs them as it does after an update; it refuses, and
says which, when it can't). The release that was being installed is then recorded as not to be
tried again by itself (unless it is the version now running, because a person finished the
update by hand). Or remove `/var/lib/taskto-update/transaction` and
`/var/lib/taskto-update/blocked`, and the checks are yours.

**Alerts.** The unit fails (`systemctl status taskto-update`), and the status and the journal
say why. For a message somewhere you will see, put one URL in
`/etc/taskto/update-notify-url` (optional; root only, mode 600, since a webhook address is a
secret; `install -m 600 /dev/null /etc/taskto/update-notify-url` and write the address into it).
On every outcome but `up-to-date`, `skipped` and a dry run (so also `updated`, `failed`,
`rolled-back`, `stopped`, `stop-failed`, `rollback-failed`, `interrupted` and every `blocked`
night after a stop) the updater POSTs a small JSON body to it:

```json
{"result":"stopped","from":"v0.2.0","to":"v0.3.0","reason":"…","backup":"/var/backups/taskto/…tar.gz","text":"taskto-update: stopped, v0.2.0 to v0.3.0: … (backup: …)"}
```

`text` is the same in a sentence, which Slack-compatible webhooks (Slack, Mattermost,
Rocket.Chat, Discord's `/slack` address) show as the message. [ntfy](https://ntfy.sh) takes
the body as the message (it shows the JSON; put its topic in the address). Anything that
accepts a JSON POST works, and `result`, `from`, `to`, `reason` and `backup` are the generic
format. An `https://` address is the one to use: the address, which may hold a secret, and the
message are sent as they are, so an `http://` one (accepted, for a receiver on a network you
trust) sends them in the clear. The address is read from the file and given to `curl` on its
standard input, never in a command line (`ps` can't show it), and never logged; a notification has a short timeout
(5 s to connect, 10 s in all), is best effort, and never changes the outcome: a webhook that is
down only leaves "the notification wasn't delivered" in the journal.

**Where to look.** `/var/lib/taskto-update/status.json` has the last run: when, `result`
(`updated`, `up-to-date`, `skipped`, `failed`, `rolled-back`, `stopped`, `stop-failed`,
`rollback-failed`, `blocked`, `interrupted` or `cleared`), `from`, `to`, the commit (`sha`), the `reason`, the `backup` file, the
images' `digests`, and `last_change`, the last run that changed or tried to change anything.
Each run's one-line summary is in the journal: `journalctl -u taskto-update`. The unit fails
(and `systemctl status taskto-update` shows it) when a run failed, rolled back, stopped, or is
blocked. Admins don't see it in the web app yet; the server doesn't read the host's files.
Point monitoring at `/healthz`, which reports the version, and at the unit.

**By hand.** `taskto-update --to` is the same steps for a version you choose. To go back to an
older version yourself, follow [Upgrade and rollback](#upgrade-and-rollback) with the backup
in `/var/backups/taskto`, and `touch /etc/taskto/update.hold` first, or the next night's run
takes the newer release again. An install whose version has no updater (or no
`deploy/safety-check.sh`) can't be updated by the updater: update it by hand once
([deploy/INSTALL.md](../deploy/INSTALL.md#updates), "By hand").

The updater is a file on the host, put there by step 16 of the runbook from the version that
was installed then. A later version's fixes to it arrive when you run that step again.
