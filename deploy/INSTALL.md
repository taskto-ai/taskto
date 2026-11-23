# Installing TaskTo.AI on a fresh Ubuntu VM, step by step

This is a runbook for an agent, or a person, installing one TaskTo.AI server on a fresh
Ubuntu VM over SSH, from the published images. The examples use **https://tasks.example.com**;
everything specific comes from the inputs below. The general self-host guide, with the
reasons behind each setting, is [docs/install.md](../docs/install.md). This file doesn't
repeat it; it links to it.

How to follow it:

- Run the steps in order. Each step has a **Run** block and a **Check** block. Go on only
  when the check prints `OK`. When a check fails, stop and report what it printed.
- Every step can be run again safely. To resume, start at the first step whose check fails.
- Blocks run **on your machine** (bash or zsh), in a checkout of this repository. The ones that act on
  the VM pipe a script through `vm`, which runs it as root over SSH (defined in step 0).
  Each block starts with `. ~/.config/taskto-install.env`, so it works in a fresh shell.
- Inside a `vm` script, give `</dev/null` to any command that could read its input
  (`docker compose exec`, `docker run`): the script itself is that input, and the command
  would swallow the rest of it.
- Never print a secret: the setup token, the admin password, the backup repository's
  address and password and, with a private registry, its pull token stay in files. Don't
  `cat` them, don't echo them, don't paste them in an issue, a chat or a log.
- Don't set `TASKTO_INSECURE_COOKIES` or `TASKTO_UNSAFE_PRIVILEGED_AGENTS`, or put the CLI
  workers in `local` or `local-unsafe` mode, anywhere. They are for development
  ([agent-safety.md](../docs/agent-safety.md)), and steps 6 and 12 fail if one is set.

There are two network layouts. Choose one with `TASKTO_MODE`:

| `TASKTO_MODE` | When | TLS | The server listens on |
|---|---|---|---|
| `caddy` | The VM has its own public IP, with ports 80 and 443 open to it | The bundled Caddy, with a Let's Encrypt certificate | `127.0.0.1:8080`, and Caddy on `:80` and `:443` |
| `traefik` | A Traefik on another machine forwards the domain to the VM | Traefik | `127.0.0.1:8080`, and `<TASKTO_LAN_IP>:80` in plain HTTP, open to Traefik's address only |

## Inputs

Get the values from whoever asked for the install and whoever brought up the VM. Nothing
here is a secret. The published images are public, so no token is needed to pull them.

| Variable | Example | What |
|---|---|---|
| `TASKTO_SSH` | `deploy@203.0.113.10` | How you SSH to the VM. That user needs passwordless `sudo`. |
| `TASKTO_MODE` | `caddy` | `caddy` or `traefik` (above). |
| `TASKTO_DOMAIN` | `tasks.example.com` | The public hostname. |
| `TASKTO_VERSION` | `v0.2.0` or a full 40-character commit SHA | The image tag to install. It must be published (see [Images](#images)), and be a commit of this repository. |
| `TASKTO_REGISTRY` | `ghcr.io/taskto-ai` | Where the images are: the registry host and the organisation that publishes them, with no trailing `/`. Change it only for a fork or a private registry. |
| `TASKTO_ADMIN_EMAIL` | `owner@example.com` | The first account, the owner and admin of the first workspace. |
| `TASKTO_ADMIN_NAME` | `Owner` | Their name. |
| `TASKTO_WORKSPACE` | `TaskTo` | The first workspace's name. |
| `TASKTO_LAN_IP` | `10.0.0.20` | `traefik` only: the VM's private address, the one Traefik forwards to. |
| `TASKTO_PROXY_IP` | `10.0.0.2` | `traefik` only: Traefik's address as the VM sees it. Only it may reach the port, and only it is trusted for `X-Forwarded-For`. |
| `TASKTO_CLI_WORKER_MEMORY` | `6g` | The CLI worker's memory cap. The CLIs take 250–400 MB each, and the agent browser 300–500 MB each. |
| `TASKTO_CLI_WORKER_CPUS` | `3` | The CLI worker's CPUs. |
| `TASKTO_BROWSER` | `true` | The agent browser ([agent-browser.md](../docs/agent-browser.md)). |
| `TASKTO_BACKUP_DIR` | `$HOME/.config/taskto-backup` | A directory **on your machine**, mode 700, for the backups (step 15): `repository`, the backup repository's address with its append-only user, `rest:https://<user>:<password>@<backup-host>:8000/<user>/`, from whoever runs the backup host ([docs/install.md](../docs/install.md#backups-the-server-cant-delete)); `password`, the repository's password, which step 15 makes if it isn't there; and, only if the backup host asks for a TLS client certificate, `client.pem`, that certificate and its key in one PEM file, from whoever runs the backup host. All mode 600. `none` only for an install that nobody outside a private network can reach. |
| `TASKTO_BACKUP_AT` | `03:15` | When the nightly backup starts, `HH:MM` in UTC. |

The sizes are for a VM of 4 cores and 12 GB, which leaves the server what it needs: its own
process, the agent runtime and, while someone dictates, the speech model (about 1.2 GB).

## Images

CI publishes three public images (`.github/workflows/images.yml`,
[docs/install.md](../docs/install.md#prebuilt-images)):
`<registry>/taskto`, `<registry>/taskto-cli-worker` and `<registry>/taskto-backup`. A
commit on `main` is tagged with its full SHA, a release with its version tag, and a tag is
never pushed twice, so `TASKTO_VERSION` always means the same image. They are amd64 only.
To see what is published, look at the organisation's **Packages** page on GitHub, or at the
Images job of the commit's CI run.

> **A private registry instead.** If you build and push the images to a registry that needs
> a login, set two more inputs in step 0: `TASKTO_REGISTRY_USER`, the user of a **read-only**
> pull token, and `TASKTO_PULL_TOKEN_FILE`, a file on your machine holding that token, mode
> 600. Step 5 then copies the token to the VM, and step 7 logs in with it for each pull.
> With the public images, leave both empty and step 5 only makes sure no token is left.

## Step 0: inputs, on your machine

**Run**: fill in the values, then write the file. It holds no secret.

```sh
mkdir -p ~/.config && umask 077 && cat > ~/.config/taskto-install.env <<'EOF'
TASKTO_SSH=deploy@203.0.113.10
TASKTO_MODE=caddy
TASKTO_DOMAIN=tasks.example.com
TASKTO_VERSION=v0.2.0
TASKTO_REGISTRY=ghcr.io/taskto-ai
# Only for a private registry (see Images); empty for the public images:
TASKTO_REGISTRY_USER=
TASKTO_PULL_TOKEN_FILE=
TASKTO_ADMIN_EMAIL=owner@example.com
TASKTO_ADMIN_NAME='The Owner'
TASKTO_WORKSPACE=TaskTo
TASKTO_LAN_IP=
TASKTO_PROXY_IP=
TASKTO_CLI_WORKER_MEMORY=6g
TASKTO_CLI_WORKER_CPUS=3
TASKTO_BROWSER=true
# Backups (step 15): the directory with the repository's address and password, or none.
TASKTO_BACKUP_DIR=$HOME/.config/taskto-backup
TASKTO_BACKUP_AT=03:15

# Runs its standard input as root on the VM, stopping at the first error.
vm() { ssh -o BatchMode=yes -o ConnectTimeout=15 "$TASKTO_SSH" sudo -n bash -euo pipefail -s; }
EOF
```

**Check**: it prints `OK` only when every input is good; otherwise the first problem.

```sh
. ~/.config/taskto-install.env
(
fail() { echo "FAIL: $*"; exit 1; }
for v in TASKTO_SSH TASKTO_MODE TASKTO_DOMAIN TASKTO_VERSION TASKTO_REGISTRY \
  TASKTO_ADMIN_EMAIL TASKTO_ADMIN_NAME TASKTO_WORKSPACE \
  TASKTO_CLI_WORKER_MEMORY TASKTO_CLI_WORKER_CPUS TASKTO_BROWSER; do
  [ -n "$(eval "printf %s \"\${$v:-}\"")" ] || fail "$v is empty"
done
case "$TASKTO_MODE" in
  caddy) ;;
  traefik) [ -n "$TASKTO_LAN_IP" ] && [ -n "$TASKTO_PROXY_IP" ] || fail "traefik needs TASKTO_LAN_IP and TASKTO_PROXY_IP" ;;
  *) fail "TASKTO_MODE is $TASKTO_MODE, not caddy or traefik" ;;
esac
printf '%s' "$TASKTO_VERSION" | grep -Eq '^(v[0-9A-Za-z._-]+|[0-9a-f]{40})$' || fail "TASKTO_VERSION isn't a v* tag or a full SHA"
case "$TASKTO_REGISTRY" in
  */) fail "TASKTO_REGISTRY ends with /" ;;
esac
if [ -n "${TASKTO_PULL_TOKEN_FILE:-}${TASKTO_REGISTRY_USER:-}" ]; then
  [ -n "${TASKTO_PULL_TOKEN_FILE:-}" ] && [ -n "${TASKTO_REGISTRY_USER:-}" ] || fail "a private registry needs both TASKTO_REGISTRY_USER and TASKTO_PULL_TOKEN_FILE"
  [ -s "$TASKTO_PULL_TOKEN_FILE" ] || fail "no pull token in $TASKTO_PULL_TOKEN_FILE"
  [ "$(stat -c %a "$TASKTO_PULL_TOKEN_FILE" 2>/dev/null || stat -f %Lp "$TASKTO_PULL_TOKEN_FILE")" = 600 ] || fail "$TASKTO_PULL_TOKEN_FILE isn't mode 600"
fi
git fetch -q --tags origin
git cat-file -e "$TASKTO_VERSION^{commit}" 2>/dev/null || fail "$TASKTO_VERSION isn't a commit of this repository"
git cat-file -e "${TASKTO_VERSION}:compose.yaml" && git cat-file -e "${TASKTO_VERSION}:deploy/Caddyfile" || fail "$TASKTO_VERSION has no compose.yaml or deploy/Caddyfile"
git show "${TASKTO_VERSION}:compose.yaml" | grep -q 'TASKTO_IMAGE_PREFIX' || fail "$TASKTO_VERSION's compose.yaml predates published images"
git cat-file -e "${TASKTO_VERSION}:deploy/worker-firewall.sh" || fail "$TASKTO_VERSION predates the CLI worker's own network"
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
case "${TASKTO_BACKUP_DIR:-}" in
  "") fail "TASKTO_BACKUP_DIR is empty: an install open to the internet has backups (see Inputs)" ;;
  none) echo "WARN: no backups: only for an install nobody outside a private network can reach" ;;
  *)
    [ -d "$TASKTO_BACKUP_DIR" ] && [ "$(mode "$TASKTO_BACKUP_DIR")" = 700 ] || fail "$TASKTO_BACKUP_DIR isn't a directory of mode 700"
    [ -s "$TASKTO_BACKUP_DIR/repository" ] || fail "no repository address in $TASKTO_BACKUP_DIR/repository"
    for f in repository password client.pem; do
      [ ! -e "$TASKTO_BACKUP_DIR/$f" ] || [ "$(mode "$TASKTO_BACKUP_DIR/$f")" = 600 ] || fail "$TASKTO_BACKUP_DIR/$f isn't mode 600"
    done
    grep -Eq '^rest:https?://[^/@]+@[^/]+/[^/]+/$' "$TASKTO_BACKUP_DIR/repository" || fail "$TASKTO_BACKUP_DIR/repository isn't rest:https://<user>:<password>@<host>:<port>/<user>/"
    if [ -e "$TASKTO_BACKUP_DIR/client.pem" ]; then
      grep -q -- '-----BEGIN CERTIFICATE-----' "$TASKTO_BACKUP_DIR/client.pem" && grep -Eq -- '-----BEGIN ([A-Z]+ )?PRIVATE KEY-----' "$TASKTO_BACKUP_DIR/client.pem" \
        || fail "$TASKTO_BACKUP_DIR/client.pem doesn't hold a certificate and its key"
      grep -q '^rest:https://' "$TASKTO_BACKUP_DIR/repository" || fail "a client certificate needs an https:// repository"
    fi
    printf '%s' "${TASKTO_BACKUP_AT:-}" | grep -Eq '^([01][0-9]|2[0-3]):[0-5][0-9]$' || fail "TASKTO_BACKUP_AT isn't HH:MM"
    git show "${TASKTO_VERSION}:compose.yaml" | grep -q '^  backup-schedule:' || fail "$TASKTO_VERSION predates the nightly backups"
    ;;
esac
echo OK
)
```

## Step 1: the inputs, on the VM

The VM keeps the inputs it needs in `/etc/taskto/install.env` (no secrets), so the later
steps and the firewall unit read them there. This is also the first time you reach the VM.

**Run**:

```sh
. ~/.config/taskto-install.env
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
TASKTO_BACKUPS=on TASKTO_BACKUP_CLIENT_CERT=off
[ "$TASKTO_BACKUP_DIR" != none ] || TASKTO_BACKUPS=off
[ "$TASKTO_BACKUPS" = off ] || [ ! -e "$TASKTO_BACKUP_DIR/client.pem" ] || TASKTO_BACKUP_CLIENT_CERT=on
{
  echo 'install -d -m 755 /etc/taskto'
  echo "cat > /etc/taskto/install.env.new <<'VARS'"
  for v in TASKTO_MODE TASKTO_DOMAIN TASKTO_VERSION TASKTO_REGISTRY TASKTO_REGISTRY_USER \
    TASKTO_ADMIN_EMAIL TASKTO_ADMIN_NAME TASKTO_WORKSPACE TASKTO_LAN_IP TASKTO_PROXY_IP \
    TASKTO_CLI_WORKER_MEMORY TASKTO_CLI_WORKER_CPUS TASKTO_BROWSER TASKTO_BACKUPS TASKTO_BACKUP_AT \
    TASKTO_BACKUP_CLIENT_CERT; do
    echo "$v=$(q "$(eval "printf %s \"\${$v:-}\"")")"
  done
  echo 'VARS'
  echo 'chmod 644 /etc/taskto/install.env.new && mv /etc/taskto/install.env.new /etc/taskto/install.env'
} | vm
```

If `vm` fails here, SSH or passwordless `sudo` doesn't work for `TASKTO_SSH`: that is for
whoever brought up the VM.

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
[ -n "$TASKTO_VERSION" ] && [ -n "$TASKTO_MODE" ] && echo OK
EOF
```

## Step 2: the VM fits

**Run**: nothing to change. This step only looks.

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
. /etc/os-release
fail() { echo "FAIL: $*"; exit 1; }
[ "$ID" = ubuntu ] && case "$VERSION_ID" in 22.04|24.04|26.04) true ;; *) false ;; esac || fail "not Ubuntu 22.04, 24.04 or 26.04: $PRETTY_NAME"
[ "$(dpkg --print-architecture)" = amd64 ] || echo "WARN: $(dpkg --print-architecture): the published images are amd64 only"
[ "$(nproc)" -ge 2 ] || fail "fewer than 2 CPUs"
[ "$(awk '/MemTotal/ {print int($2/1048576)}' /proc/meminfo)" -ge 5 ] || fail "less than 6 GB of memory"
[ "$(df -BG --output=avail / | tail -1 | tr -dc 0-9)" -ge 25 ] || fail "less than 25 GB free on /"
for p in docker.io podman-docker docker-compose; do
  if dpkg -s "$p" >/dev/null 2>&1; then fail "$p is installed; this install uses Docker's own packages"; fi
done
if [ "$TASKTO_MODE" = caddy ]; then
  addrs=$(getent ahostsv4 "$TASKTO_DOMAIN" | awk '{print $1}' | sort -u)
  [ -n "$addrs" ] || fail "$TASKTO_DOMAIN doesn't resolve"
  for a in $addrs; do ip -4 -o addr show | grep -q " $a/" || fail "$TASKTO_DOMAIN resolves to $a, which isn't on this VM"; done
  for p in 80 443; do
    if ss -Hltnp "sport = :$p" | grep -v -e docker-proxy -e '"caddy"' | grep -q .; then fail "port $p is taken: $(ss -Hltnp "sport = :$p")"; fi
  done
else
  ip -4 -o addr show | grep -q " $TASKTO_LAN_IP/" || fail "$TASKTO_LAN_IP isn't on this VM"
  if ss -Hltnp "sport = :80" | grep -v docker-proxy | grep -q .; then fail "port 80 is taken: $(ss -Hltnp 'sport = :80')"; fi
fi
echo OK
EOF
```

In `traefik` mode the DNS name points at Traefik, so it isn't checked here. For a VM with
4 cores and 12 GB the sizes in the inputs fit; on a smaller one, lower
`TASKTO_CLI_WORKER_MEMORY` and `TASKTO_CLI_WORKER_CPUS`.

## Step 3: system updates

Security updates install themselves (Ubuntu's `unattended-upgrades`, security origins only:
Docker's packages aren't among them, so the daemon doesn't restart on its own).

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg jq openssl unattended-upgrades >/dev/null
printf '%s\n' 'APT::Periodic::Update-Package-Lists "1";' 'APT::Periodic::Unattended-Upgrade "1";' > /etc/apt/apt.conf.d/20auto-upgrades
systemctl enable --now unattended-upgrades >/dev/null 2>&1
EOF
```

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
command -v jq curl openssl >/dev/null && systemctl is-active -q unattended-upgrades \
  && grep -q 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades && echo OK
EOF
```

## Step 4: Docker

Docker Engine and the Compose plugin from Docker's own repository, its signing key checked
against Docker's published fingerprint. Container logs rotate, and stay on the VM.

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
export DEBIAN_FRONTEND=noninteractive
install -m 0755 -d /etc/apt/keyrings
if [ ! -s /etc/apt/keyrings/docker.asc ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc.new
  gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc.new | grep -q '^fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:$' \
    || { echo "FAIL: Docker's key has an unexpected fingerprint"; rm -f /etc/apt/keyrings/docker.asc.new; exit 1; }
  mv /etc/apt/keyrings/docker.asc.new /etc/apt/keyrings/docker.asc
fi
chmod a+r /etc/apt/keyrings/docker.asc
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME:-$VERSION_CODENAME} stable" > /etc/apt/sources.list.d/docker.list
apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
# Log rotation, merged into any daemon.json already there.
mkdir -p /etc/docker
[ -s /etc/docker/daemon.json ] || echo '{}' > /etc/docker/daemon.json
want=$(jq -S '. + {"log-driver": "local", "log-opts": {"max-size": "20m", "max-file": "5"}}' /etc/docker/daemon.json)
if [ "$(jq -S . /etc/docker/daemon.json)" != "$want" ]; then
  printf '%s\n' "$want" > /etc/docker/daemon.json.new && mv /etc/docker/daemon.json.new /etc/docker/daemon.json
  systemctl restart docker
fi
systemctl enable --now docker containerd >/dev/null 2>&1
EOF
```

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
docker version --format 'docker {{.Server.Version}}'
docker compose version
[ "$(docker info --format '{{.LoggingDriver}}')" = local ] && systemctl is-enabled -q docker && echo OK
EOF
```

## Step 5: the pull token (private registry only)

The published images are public. With them, `TASKTO_PULL_TOKEN_FILE` is empty: this step
removes any token an earlier install left, and its check prints `OK`.

With a private registry, the VM pulls with a **read-only** token, kept in
`/etc/taskto/registry-token`, which only root can read. It is used for each pull and logged
out after, so it isn't left in Docker's config.

**Run**: the token goes from your file to the VM's without being shown.

```sh
. ~/.config/taskto-install.env
if [ -n "${TASKTO_PULL_TOKEN_FILE:-}" ]; then
  ssh -o BatchMode=yes "$TASKTO_SSH" \
    'sudo -n sh -c "umask 077; mkdir -p /etc/taskto; cat > /etc/taskto/registry-token.new && mv /etc/taskto/registry-token.new /etc/taskto/registry-token"' \
    < "$TASKTO_PULL_TOKEN_FILE"
else
  echo 'rm -f /etc/taskto/registry-token' | vm
fi
```

**Check**: with a token, the file is root's, mode 600, not empty, and the token works,
without printing it; without one, there is no token file.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
if [ -z "${TASKTO_REGISTRY_USER:-}" ]; then
  [ ! -e /etc/taskto/registry-token ] || { echo "FAIL: a token is left in /etc/taskto/registry-token"; exit 1; }
  echo OK
  exit 0
fi
[ "$(stat -c '%U %a' /etc/taskto/registry-token)" = "root 600" ] && [ -s /etc/taskto/registry-token ] || { echo "FAIL: /etc/taskto/registry-token"; exit 1; }
host=${TASKTO_REGISTRY%%/*}
docker login "$host" -u "$TASKTO_REGISTRY_USER" --password-stdin < /etc/taskto/registry-token >/dev/null
docker logout "$host" >/dev/null
echo OK
EOF
```

## Step 6: the Compose project

`/srv/taskto` holds the `compose.yaml` and `deploy/Caddyfile` of `TASKTO_VERSION`, copied
from your checkout, and two files written from the inputs:

- `.env`, Compose's settings: which images, which profiles (`cli`, `https` for Caddy, and
  `backup-schedule` for the nightly backups), the domain, CLI workers in container mode, the
  worker's size, the backups' time;
- `compose.override.yaml`: the egress policies (both `public`, so nobody's provider
  account can reach the VM's private network or a metadata service:
  [docs/install.md](../docs/install.md#where-provider-accounts-may-connect)), in
  `traefik` mode the LAN port and the trusted proxy
  ([docs/install.md](../docs/install.md#the-clients-address-behind-a-proxy)), and, when
  the backup host asks for a client certificate, restic's `RESTIC_TLS_CLIENT_CERT` for the
  two backup services (the file itself comes in step 15).

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
install -d -m 755 /srv/taskto /srv/taskto/deploy
EOF
for f in compose.yaml deploy/Caddyfile; do
  git show "${TASKTO_VERSION}:$f" | ssh -o BatchMode=yes "$TASKTO_SSH" "sudo -n sh -c 'cat > /srv/taskto/$f.new && chmod 644 /srv/taskto/$f.new && mv /srv/taskto/$f.new /srv/taskto/$f'"
done
vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
profiles=cli
[ "$TASKTO_MODE" = caddy ] && profiles=cli,https
if [ "${TASKTO_BACKUPS:-}" = on ]; then
  profiles=$profiles,backup-schedule
  # The repository's address and password go here in step 15, readable by the backups only.
  install -d -m 700 -o 65532 -g 65532 backup
fi
cat > .env.new <<ENV
# Written by deploy/INSTALL.md (step 6); change the inputs and run the step again.
COMPOSE_PROFILES=$profiles
TASKTO_IMAGE_PREFIX=$TASKTO_REGISTRY/
TASKTO_VERSION=$TASKTO_VERSION
TASKTO_DOMAIN=$TASKTO_DOMAIN
TASKTO_CLI_WORKERS=container
TASKTO_BROWSER=$TASKTO_BROWSER
TASKTO_CLI_WORKER_MEMORY=$TASKTO_CLI_WORKER_MEMORY
TASKTO_CLI_WORKER_CPUS=$TASKTO_CLI_WORKER_CPUS
TASKTO_BACKUP_AT=${TASKTO_BACKUP_AT:-03:15}
ENV
{
  echo '# Written by deploy/INSTALL.md (step 6); change the inputs and run the step again.'
  echo 'services:'
  echo '  taskto:'
  echo '    environment:'
  echo '      TASKTO_PROVIDER_EGRESS_PERSONAL: public'
  echo '      TASKTO_PROVIDER_EGRESS_WORKSPACE: public'
  if [ "$TASKTO_MODE" = traefik ]; then
    echo "      TASKTO_TRUSTED_PROXIES: \"$TASKTO_PROXY_IP\""
    echo '    ports:'
    echo "      - \"$TASKTO_LAN_IP:80:8080\""
  fi
  if [ "${TASKTO_BACKUPS:-}" = on ] && [ "${TASKTO_BACKUP_CLIENT_CERT:-}" = on ]; then
    for s in backup backup-schedule; do
      echo "  $s:"
      echo '    environment:'
      echo '      RESTIC_TLS_CLIENT_CERT: /run/taskto-backup/client.pem'
    done
  fi
} > compose.override.yaml.new
chmod 644 .env.new compose.override.yaml.new
mv .env.new .env
mv compose.override.yaml.new compose.override.yaml
EOF
```

**Check**: the files match the checkout, and Compose reads what was meant.

```sh
. ~/.config/taskto-install.env
(
for f in compose.yaml deploy/Caddyfile; do
  [ "$(git show "${TASKTO_VERSION}:$f" | shasum -a 256 | cut -c1-64)" = "$(ssh -o BatchMode=yes "$TASKTO_SSH" sha256sum /srv/taskto/$f | cut -c1-64)" ] \
    || { echo "FAIL: /srv/taskto/$f differs from $TASKTO_VERSION"; exit 1; }
done
) && vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
fail() { echo "FAIL: $*"; exit 1; }
cfg=$(docker compose config --format json)
[ "$(jq -r '.services.taskto.image' <<<"$cfg")" = "$TASKTO_REGISTRY/taskto:$TASKTO_VERSION" ] || fail "taskto image"
[ "$(jq -r '.services["cli-worker"].image' <<<"$cfg")" = "$TASKTO_REGISTRY/taskto-cli-worker:$TASKTO_VERSION" ] || fail "cli-worker image"
[ "$(jq -r '.services.taskto.environment.TASKTO_CLI_WORKERS' <<<"$cfg")" = container ] || fail "CLI workers aren't in container mode"
[ "$(jq -r '.services.taskto.environment.TASKTO_PROVIDER_EGRESS_PERSONAL' <<<"$cfg")" = public ] || fail "egress"
if [ "$TASKTO_MODE" = caddy ]; then
  jq -e '.services.caddy' <<<"$cfg" >/dev/null || fail "no caddy service in the https profile"
  caddy_ip=$(jq -r '[.services.caddy.networks[]?.ipv4_address // empty][0] // empty' <<<"$cfg")
  [ -n "$caddy_ip" ] && [ "$(jq -r '.services.taskto.environment.TASKTO_TRUSTED_PROXIES' <<<"$cfg")" = "$caddy_ip" ] || fail "trusted proxy isn't Caddy's fixed address"
else
  jq -e '.services.caddy' <<<"$cfg" >/dev/null && fail "caddy is on in traefik mode"
  [ "$(jq -r '.services.taskto.environment.TASKTO_TRUSTED_PROXIES' <<<"$cfg")" = "$TASKTO_PROXY_IP" ] || fail "trusted proxy isn't Traefik"
  jq -e --arg ip "$TASKTO_LAN_IP" '.services.taskto.ports[] | select(.host_ip == $ip and .published == "80" and .target == 8080)' <<<"$cfg" >/dev/null || fail "no $TASKTO_LAN_IP:80 port"
fi
jq -e '.services[].ports[]? | select(.target == 8081)' <<<"$cfg" >/dev/null && fail "the CLI proxy's port is published"
if [ "${TASKTO_BACKUPS:-}" = on ]; then
  [ "$(jq -r '.services["backup-schedule"].image' <<<"$cfg")" = "$TASKTO_REGISTRY/taskto-backup:$TASKTO_VERSION" ] || fail "no backup-schedule service with the backup image"
  [ "$(jq -r '.services["backup-schedule"].environment.TASKTO_BACKUP_AT' <<<"$cfg")" = "${TASKTO_BACKUP_AT:-03:15}" ] || fail "the backups' time"
  [ "$(stat -c '%u:%g %a' backup)" = "65532:65532 700" ] || fail "/srv/taskto/backup isn't 65532's, mode 700"
  want=null
  [ "${TASKTO_BACKUP_CLIENT_CERT:-}" = on ] && want=/run/taskto-backup/client.pem
  for s in backup backup-schedule; do
    [ "$(docker compose --profile backup --profile backup-schedule config --format json | jq -r --arg s "$s" '.services[$s].environment.RESTIC_TLS_CLIENT_CERT')" = "$want" ] \
      || fail "$s's RESTIC_TLS_CLIENT_CERT isn't $want"
  done
fi
grep -rqiE 'insecure|unsafe|local-unsafe' .env compose.override.yaml && fail "a development switch is set"
[ -s /etc/taskto/registry-token ] && grep -rqF -f /etc/taskto/registry-token /srv/taskto && fail "the pull token is in /srv/taskto"
echo OK
EOF
```

## Step 7: the images

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
# A private registry only: the public images need no login.
if [ -n "${TASKTO_REGISTRY_USER:-}" ]; then
  host=${TASKTO_REGISTRY%%/*}
  docker login "$host" -u "$TASKTO_REGISTRY_USER" --password-stdin < /etc/taskto/registry-token >/dev/null
  trap 'docker logout "$host" >/dev/null' EXIT
fi
docker compose pull --quiet
docker compose --profile backup pull --quiet backup
EOF
```

**Check**: each image is there, for this VM's architecture, built from `TASKTO_VERSION`.
Note the digests in your report.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
arch=$(dpkg --print-architecture)
for name in taskto taskto-cli-worker taskto-backup; do
  ref="$TASKTO_REGISTRY/${name}:${TASKTO_VERSION}"
  docker image inspect "$ref" >/dev/null 2>&1 || { echo "FAIL: $ref isn't pulled"; exit 1; }
  [ "$(docker image inspect --format '{{.Architecture}}' "$ref")" = "$arch" ] || { echo "FAIL: $ref isn't $arch"; exit 1; }
  [ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$ref")" = "$TASKTO_VERSION" ] || { echo "FAIL: $ref's version label"; exit 1; }
  docker image inspect --format '{{index .RepoDigests 0}}' "$ref"
done
echo OK
EOF
```

## Step 8: the worker's network

The CLI worker, and the agent browser in it, reach the internet and the server's per-turn
proxy, nothing else ([cli-workers.md](../docs/cli-workers.md#the-workers-network)).
`compose.yaml` puts the worker alone on a network of its own, which keeps it from the other
services. `deploy/worker-firewall.sh` closes the rest to it: this VM itself and every address
that isn't public (the LAN, a cloud's metadata service). A unit puts the rules in before Docker
starts, so the worker never runs without them; Docker doesn't touch them.

**Run**:

```sh
. ~/.config/taskto-install.env
git show "${TASKTO_VERSION}:deploy/worker-firewall.sh" | ssh -o BatchMode=yes "$TASKTO_SSH" \
  "sudo -n sh -c 'cat > /usr/local/sbin/taskto-worker-firewall.new && chmod 755 /usr/local/sbin/taskto-worker-firewall.new && mv /usr/local/sbin/taskto-worker-firewall.new /usr/local/sbin/taskto-worker-firewall'"
vm <<'EOF'
cat > /etc/systemd/system/taskto-worker-firewall.service <<'UNIT'
[Unit]
Description=TaskTo.AI: the CLI worker's network reaches the internet only
Before=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/taskto-worker-firewall add
ExecStop=/usr/local/sbin/taskto-worker-firewall del

[Install]
WantedBy=multi-user.target docker.service
UNIT
systemctl daemon-reload
systemctl enable taskto-worker-firewall >/dev/null 2>&1
systemctl restart taskto-worker-firewall
EOF
```

**Check**: the script is this version's, the unit runs, every rule is in once, and Compose
puts the worker on its own network only.

```sh
. ~/.config/taskto-install.env
(
[ "$(git show "${TASKTO_VERSION}:deploy/worker-firewall.sh" | shasum -a 256 | cut -c1-64)" = "$(ssh -o BatchMode=yes "$TASKTO_SSH" sha256sum /usr/local/sbin/taskto-worker-firewall | cut -c1-64)" ] \
  || { echo "FAIL: /usr/local/sbin/taskto-worker-firewall differs from $TASKTO_VERSION's"; exit 1; }
) && vm <<'EOF'
fail() { echo "FAIL: $*"; exit 1; }
systemctl is-active -q taskto-worker-firewall && systemctl is-enabled -q taskto-worker-firewall || fail "the unit"
/usr/local/sbin/taskto-worker-firewall check || fail "the rules"
cfg=$(cd /srv/taskto && docker compose config --format json)
[ "$(jq -c '.services["cli-worker"].networks | keys' <<<"$cfg")" = '["worker"]' ] || fail "the worker isn't on its own network only"
[ "$(jq -r '.networks.worker.driver_opts["com.docker.network.bridge.name"]' <<<"$cfg")" = taskto-worker ] || fail "the worker's bridge isn't taskto-worker, the firewall's"
jq -e '.services.taskto.networks | has("worker")' <<<"$cfg" >/dev/null && fail "the server is on the worker's network"
echo OK
EOF
```

Step 13 checks it live, from the worker. The proof in CI is `deploy/worker-network-test.sh`.

## Step 9: start

**Run**: `--wait` returns once the server's health check passes. In `traefik` mode, a Caddy
left from an earlier `caddy` install is removed first: Compose leaves the services of a
profile that is no longer on, and Caddy would keep port 80.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
[ "$TASKTO_MODE" = caddy ] || docker compose --profile https rm --stop --force caddy
docker compose up -d --no-build --pull never --remove-orphans --wait --wait-timeout 300
EOF
```

**Check**: the server is healthy at the right version, its agent runtime is ready, and the
CLI worker (and Caddy) run.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
timeout 120 bash -c 'until curl -fsS http://127.0.0.1:8080/healthz | jq -e ".agent == \"ready\"" >/dev/null; do sleep 2; done' || { echo "FAIL: the agent runtime isn't ready"; docker compose logs --tail 50 taskto | grep -v setup_token; exit 1; }
h=$(curl -fsS http://127.0.0.1:8080/healthz)
echo "$h"
[ "$(jq -r .status <<<"$h")" = ok ] && [ "$(jq -r .version <<<"$h")" = "$TASKTO_VERSION" ] || { echo "FAIL: /healthz"; exit 1; }
services="taskto cli-worker"
[ "$TASKTO_MODE" = caddy ] && services="$services caddy"
for s in $services; do
  [ "$(docker compose ps --format '{{.State}}' "$s")" = running ] || { echo "FAIL: $s isn't running"; docker compose logs --tail 30 "$s"; exit 1; }
done
if [ "$TASKTO_MODE" = traefik ]; then
  [ -z "$(docker compose --profile https ps -q caddy)" ] || { echo "FAIL: Caddy runs in traefik mode"; exit 1; }
  curl -fsS "http://$TASKTO_LAN_IP/healthz" >/dev/null || { echo "FAIL: nothing on $TASKTO_LAN_IP:80"; exit 1; }
fi
echo OK
EOF
```

Never paste the server's log into an issue or a report as is: until setup is done it holds the
setup token. Filter it out (`grep -v setup_token`), as above.

## Step 10: only Traefik reaches the LAN port (`traefik` mode)

Docker's published ports bypass `ufw`, so the rule goes in Docker's `DOCKER-USER` chain:
connections to `TASKTO_LAN_IP:80` from anywhere but `TASKTO_PROXY_IP` are dropped. A small
unit puts it back whenever Docker starts. The rule carries a comment, so running the step
again after the addresses change replaces it. In `caddy` mode the step removes the unit, if
an earlier `traefik` install left one.

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
if [ "$TASKTO_MODE" != traefik ]; then
  if [ -e /etc/systemd/system/taskto-lan-firewall.service ]; then
    systemctl disable --now taskto-lan-firewall >/dev/null 2>&1
    rm -f /etc/systemd/system/taskto-lan-firewall.service /usr/local/sbin/taskto-lan-firewall
    systemctl daemon-reload
  fi
  echo "caddy mode: no LAN port"; exit 0
fi
cat > /usr/local/sbin/taskto-lan-firewall <<'SH'
#!/bin/sh
# Written by deploy/INSTALL.md (step 10): only the reverse proxy may reach TaskTo's LAN port.
set -eu
. /etc/taskto/install.env
tag="taskto-lan-firewall"
remove() {
  iptables -S DOCKER-USER | grep -- "--comment $tag" | sed 's/^-A /-D /' | while read -r r; do
    # shellcheck disable=SC2086
    iptables $r
  done
}
case "${1:-add}" in
add)
  remove
  # The connection's original source and destination, so replies match as well as requests.
  iptables -I DOCKER-USER 1 -p tcp -m conntrack --ctorigdst "$TASKTO_LAN_IP" --ctorigdstport 80 \
    ! --ctorigsrc "$TASKTO_PROXY_IP" -m comment --comment "$tag" -j DROP
  ;;
del) remove ;;
esac
SH
chmod 755 /usr/local/sbin/taskto-lan-firewall
cat > /etc/systemd/system/taskto-lan-firewall.service <<'UNIT'
[Unit]
Description=TaskTo.AI: only the reverse proxy reaches the LAN port
After=docker.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/taskto-lan-firewall add
ExecStop=/usr/local/sbin/taskto-lan-firewall del

[Install]
WantedBy=docker.service
UNIT
systemctl daemon-reload
systemctl enable taskto-lan-firewall >/dev/null 2>&1
systemctl restart taskto-lan-firewall
EOF
```

**Check**: exactly one such rule, for the current addresses, and the unit runs.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
if [ "$TASKTO_MODE" != traefik ]; then
  iptables -S DOCKER-USER 2>/dev/null | grep -q taskto-lan-firewall && { echo "FAIL: a LAN rule is left"; exit 1; }
  echo OK; exit 0
fi
systemctl is-active -q taskto-lan-firewall && systemctl is-enabled -q taskto-lan-firewall || { echo "FAIL: the unit"; exit 1; }
rules=$(iptables -S DOCKER-USER | grep -- taskto-lan-firewall)
[ "$(printf '%s\n' "$rules" | wc -l)" = 1 ] || { echo "FAIL: not exactly one rule"; iptables -S DOCKER-USER; exit 1; }
# Each part on its own: iptables versions differ in option order and in a /32 suffix.
for part in "! --ctorigsrc $TASKTO_PROXY_IP(/32)? " "--ctorigdst $TASKTO_LAN_IP(/32)? " "--ctorigdstport 80 " "-j DROP"; do
  printf '%s\n' "$rules" | grep -Eq -- "$part" || { echo "FAIL: the rule lacks '$part'"; iptables -S DOCKER-USER; exit 1; }
done
echo OK
EOF
```

The proof that it lets Traefik through is step 14: the public address works.

## Step 11: the owner's account

A fresh server has no account, and only someone who can read its log can create the first
one ([docs/install.md](../docs/install.md#2-set-up-the-owner)). This step does it with
`TASKTO_ADMIN_EMAIL`, and a random password that it keeps in `/etc/taskto/admin-password`,
readable by root only. Neither the password nor the setup token is printed.

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
cd /srv/taskto
if [ "$(curl -fsS http://127.0.0.1:8080/api/setup | jq -r .needed)" != true ]; then
  echo "setup is already done"; exit 0
fi
[ -s /etc/taskto/admin-password ] || (umask 077; openssl rand -base64 24 | tr -d '\n' > /etc/taskto/admin-password)
setup_token() { docker compose logs --no-color --no-log-prefix taskto | sed -n 's/.*setup_token=\([^ ]*\).*/\1/p' | tail -1; }
token=$(setup_token)
if [ -z "$token" ]; then
  # The line rotated out of the log: the server prints a new token each time it starts
  # without an account.
  docker compose restart taskto >/dev/null
  timeout 120 bash -c 'until curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; do sleep 2; done'
  token=$(setup_token)
fi
[ -n "$token" ] || { echo "FAIL: no setup token in the log"; exit 1; }
code=$(jq -n --arg email "$TASKTO_ADMIN_EMAIL" --arg name "$TASKTO_ADMIN_NAME" --arg ws "$TASKTO_WORKSPACE" \
    --rawfile password /etc/taskto/admin-password --arg token "$token" \
    '{email: $email, name: $name, password: $password, workspace_name: $ws, setup_token: $token}' \
  | curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:8080/api/setup)
[ "$code" = 201 ] || { echo "FAIL: setup answered $code"; exit 1; }
echo "setup done"
EOF
```

**Check**: setup is closed, and the owner can sign in with a cookie that is `Secure`,
`HttpOnly` and `__Host-`. (Once the owner has changed the password and deleted the file,
as below, only the first part runs.)

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
[ "$(curl -fsS http://127.0.0.1:8080/api/setup | jq -r .needed)" = false ] || { echo "FAIL: setup is still open"; exit 1; }
if [ -s /etc/taskto/admin-password ]; then
  headers=$(jq -n --arg email "$TASKTO_ADMIN_EMAIL" --rawfile password /etc/taskto/admin-password '{email: $email, password: $password}' \
    | curl -sS -D - -o /dev/null -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:8080/api/login)
  grep -q '^HTTP/[0-9.]* 200' <<<"$headers" || { echo "FAIL: the owner can't sign in with /etc/taskto/admin-password (if they changed it, they delete the file): $(head -1 <<<"$headers")"; exit 1; }
  grep -i '^set-cookie: __Host-taskto_session=' <<<"$headers" | grep -i '; secure' | grep -qi '; httponly' || { echo "FAIL: the session cookie isn't __Host-, Secure and HttpOnly"; exit 1; }
fi
echo OK
EOF
```

**Hand the password to the owner** by telling them where it is, never what it is. It was
made by this install, and anyone who could read the VM as root had it, so the owner
replaces it at once. Tell them to:

1. read it themselves, on the VM (they run this, not you):

   ```sh
   ssh <TASKTO_SSH> sudo cat /etc/taskto/admin-password
   ```

2. sign in at `https://TASKTO_DOMAIN` with `TASKTO_ADMIN_EMAIL` and that password;
3. in **Settings → Password**, change it to one of their own, kept in their password
   manager. The generated one stops working, and any other session is signed out;
4. delete the file:

   ```sh
   ssh <TASKTO_SSH> sudo rm /etc/taskto/admin-password
   ```

If they lose their password later, another admin makes them a reset link (**Settings →
Members → Reset link**, [api.md](../docs/api.md#passwords)). The server has no other way
back into an account, so an owner who is the only admin should make a second one.

Everyone else joins by invitation from the owner (Settings → Members). There is no open
sign-up.

## Step 12: the agent safety rules, in the running containers

[agent-safety.md](../docs/agent-safety.md) and the rules in [AGENTS.md](../AGENTS.md) are
enforced by the code; this checks the install left them on.

**Run**: nothing to change.

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
cd /srv/taskto
fail() { echo "FAIL: $*"; exit 1; }
s=$(docker compose ps -q taskto) w=$(docker compose ps -q cli-worker)
[ "$(docker inspect --format '{{.Config.User}}' "$s")" = 65532:65532 ] || fail "the server doesn't run as 65532"
for c in "$s" "$w"; do
  docker inspect --format '{{json .HostConfig.CapDrop}} {{json .HostConfig.SecurityOpt}} {{.HostConfig.Privileged}}' "$c" \
    | grep -q '^\["ALL"\] \["no-new-privileges:true"\] false$' || fail "$c isn't capability-free with no-new-privileges"
done
[ "$(docker inspect --format '{{json .HostConfig.CapAdd}}' "$w" | jq -c 'sort')" = '["CAP_CHOWN","CAP_KILL","CAP_SETGID","CAP_SETUID"]' ] \
  || [ "$(docker inspect --format '{{json .HostConfig.CapAdd}}' "$w" | jq -c 'sort')" = '["CHOWN","KILL","SETGID","SETUID"]' ] \
  || fail "the worker has capabilities beyond SETUID, SETGID, CHOWN and KILL"
docker compose exec -T cli-worker true </dev/null || fail "can't run a command in the worker"
# Without DAC capabilities a few root-only directories are unreadable, to the CLIs too.
suid=$(docker compose exec -T cli-worker find / -xdev -perm /6000 -type f </dev/null 2>/dev/null || true)
[ -z "$suid" ] || fail "the worker image has setuid or setgid programs: $suid"
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$s" "$w" | grep -iE 'INSECURE|UNSAFE|TOKEN|SECRET|PASSWORD|API_KEY' && fail "a development switch or a secret is in a container's environment"
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$s" | grep -qx 'TASKTO_CLI_WORKERS=container' || fail "CLI workers aren't in container mode"
docker inspect --format '{{range .Mounts}}{{println .Name}}{{end}}' "$w" | grep -qx taskto_data && fail "the worker can see the server's data"
echo OK
EOF
```

## Step 13: the open-internet checklist

Before the address is given to anyone, each of these holds. Steps 6, 11 and 12 checked most
of them; this runs the rest against the live server.

| | What | Where it's checked |
|---|---|---|
| 1 | No open sign-up: the first account needs the setup token, then setup answers `409`; everyone else comes by invitation | here, and step 11 |
| 2 | Failed logins are limited: 30 per client address, 10 per email from one address, 100 per email, per 15 minutes ([api.md](../docs/api.md#post-apilogin)) | step 14, through the public address |
| 3 | The session cookie is `Secure`, `HttpOnly`, `__Host-`; no `insecure_cookies` | steps 6, 11, 12 |
| 4 | HTTPS only, with HSTS | step 14 |
| 5 | The client's address comes from the one trusted proxy: Caddy's fixed address, or `TASKTO_PROXY_IP`, which in `traefik` mode is also the only address let through | steps 6, 10, 14 |
| 6 | [Agent safety](../docs/agent-safety.md): no agent as root or with sudo, `no_new_privs`, no setuid programs, no secrets in agents' environments, permission prompts on (the default, `ask`; only a conversation's owner turns them off, for that conversation) | step 12 |
| 7 | CLI workers in container mode only; the worker never sees the server's data | steps 6, 12 |
| 8 | Provider accounts can't reach private addresses (egress `public`) | step 6 |
| 9 | No passwordless root for anyone's agents; no shared subscription logins (shared CLI accounts are API keys, behind the proxy); the images are public, so updates need no credential (with a private registry, a read-only pull token in a root-only file, never a source token); backups go to an append-only repository the server can't delete from (below) | steps 5, 12, 15 |
| 10 | Only the expected ports listen: 22, Caddy's 80/443 (or `TASKTO_LAN_IP:80`), and `127.0.0.1:8080`; the CLI proxy (8081) is never published | here |
| 11 | The CLI worker and the agent browser reach the internet and the per-turn proxy, nothing else: not the server, Ollama or another service, not this VM, its LAN or a metadata service | step 8, and here |

**Run**: nothing to change.

**Check**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
fail() { echo "FAIL: $*"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/api/setup)" = 409 ] || fail "setup isn't closed"
code=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"token":"not-an-invite","name":"x","password":"not a real password"}' http://127.0.0.1:8080/api/invites/accept)
case "$code" in 4??) ;; *) fail "a made-up invite answered $code" ;; esac
listening=$(ss -Hltn | awk '{print $4}' | sort -u)
echo "$listening" | grep -q ':8081$' && fail "the CLI proxy's port listens on the host"
echo "$listening" | grep -E ':8080$' | grep -vqE '^127\.0\.0\.1:8080$' && fail "8080 listens beyond 127.0.0.1"
echo "$listening"
# From the worker, as a CLI's user would: what it must reach, and real listeners it mustn't.
cd /srv/taskto
w=$(docker compose ps -q cli-worker)
reach() { docker exec -u 65534:65534 "$w" timeout 5 bash -c "exec 3<>/dev/tcp/$1/$2" </dev/null >/dev/null 2>&1; }
server_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "taskto_default").IPAddress}}' "$(docker compose ps -q taskto)")
gateway=$(docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' taskto_worker)
vm_ip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
[ "$(docker exec -u 65534:65534 "$w" curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/cli-proxy/v1/models </dev/null)" = 401 ] || fail "the worker doesn't reach the per-turn proxy"
reach api.anthropic.com 443 || fail "the worker doesn't reach the internet"
reach "$server_ip" 8080 && fail "the worker reaches the server"
reach "$gateway" 22 && fail "the worker reaches this VM's SSH through its gateway"
reach "$vm_ip" 22 && fail "the worker reaches this VM's SSH on $vm_ip"
[ "$TASKTO_MODE" = traefik ] && reach "$TASKTO_LAN_IP" 80 && fail "the worker reaches the LAN port"
reach 169.254.169.254 80 && fail "the worker reaches the metadata service's address"
echo OK
EOF
```

Read the listening addresses it prints: anything you don't expect is a finding.

## Step 14: the public address

**Run**: nothing to change. In `traefik` mode, Traefik must already route the domain to
`http://TASKTO_LAN_IP:80` (that is part of bringing up the VM). It must pass the `Host` header
through (the default), not buffer responses, add HSTS
(`Strict-Transport-Security: max-age=31536000`) and redirect HTTP to HTTPS.

**Check**, on your machine:

```sh
. ~/.config/taskto-install.env
(
fail() { echo "FAIL: $*"; exit 1; }
h=$(curl -fsS --max-time 20 "https://$TASKTO_DOMAIN/healthz") || fail "https://$TASKTO_DOMAIN/healthz doesn't answer, or its certificate isn't valid"
echo "$h"
printf '%s' "$h" | grep -q "\"version\":\"$TASKTO_VERSION\"" || fail "the public address serves another version"
curl -sSI --max-time 20 "https://$TASKTO_DOMAIN/" | grep -qi '^strict-transport-security: max-age=' || fail "no HSTS header"
curl -sSI --max-time 20 "http://$TASKTO_DOMAIN/" | grep -qE '^HTTP/[0-9.]+ 30[18]' || fail "http:// doesn't redirect to https://"
# The login limit, through the proxy: 10 failures for a made-up address from one client
# answer 401, the 11th 429. It costs your address 11 of its 30 failed attempts for 15
# minutes; the owner's own address isn't touched (the limit per email is per email).
email="limit-check-$(date +%s)@example.invalid"
codes=""
for i in $(seq 1 11); do
  codes="$codes $(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -H 'Content-Type: application/json' \
    -d "{\"email\":\"$email\",\"password\":\"not the password\"}" "https://$TASKTO_DOMAIN/api/login")"
done
[ "$codes" = " 401 401 401 401 401 401 401 401 401 401 429" ] || fail "failed logins answered$codes, not ten 401 then 429"
echo OK
)
```

Then open `https://TASKTO_DOMAIN` in a browser: the sign-in page loads, and the certificate
is valid. If `curl` fails on the certificate in `caddy` mode, Caddy may still be getting
it: `docker compose logs caddy` on the VM shows why (DNS, port 80 closed, or Let's
Encrypt's rate limit).

## Step 15: backups

An install open to the internet backs up every night to a repository the server can only
add to: restic, through rest-server in append-only mode on **a backup host**, another
machine under other credentials ([docs/install.md](../docs/install.md#backups-the-server-cant-delete)).
Whoever runs the backup host gives you the repository's address with its append-only user,
which you keep in `$TASKTO_BACKUP_DIR/repository`. This step copies it and the repository's
password to the VM (`/srv/taskto/backup/`, readable by the backups only) without showing
them, creates the repository the first time, and runs the first backup. From then on the
`backup-schedule` service, started in step 9, backs up every night at `TASKTO_BACKUP_AT`
UTC and retries a failed night hourly, six times.

**The repository's password and address are kept off the VM.** They are in
`$TASKTO_BACKUP_DIR` on your machine; the owner puts both in their password manager. Without
the password nobody can read the backups, and if the VM is lost, so is its copy.

**Run**:

```sh
. ~/.config/taskto-install.env
if [ "$TASKTO_BACKUP_DIR" = none ]; then
  echo "backups are off: say so in the report"
else
  (umask 077; [ -s "$TASKTO_BACKUP_DIR/password" ] || openssl rand -base64 32 > "$TASKTO_BACKUP_DIR/password")
  files="repository password" cert=off ok=yes
  [ ! -e "$TASKTO_BACKUP_DIR/client.pem" ] || { files="$files client.pem"; cert=on; }
  # Compose passes restic a client certificate as step 1 recorded it: one added or removed
  # since then needs steps 1, 6 and 9 first. Nothing is copied or removed until they agree.
  recorded=$(echo '. /etc/taskto/install.env; echo "${TASKTO_BACKUP_CLIENT_CERT:-off}"' | vm) || recorded=unknown
  if [ "$recorded" = unknown ]; then
    echo "FAIL: can't read /etc/taskto/install.env on the VM"; ok=no
  elif [ "$recorded" != "$cert" ]; then
    echo "FAIL: client.pem was added or removed since step 1: run steps 1, 6 and 9 again, then this step"; ok=no
  fi
  for f in $files; do
    [ "$ok" = yes ] || break
    ssh -o BatchMode=yes "$TASKTO_SSH" \
      "sudo -n sh -c 'umask 077; cat > /srv/taskto/backup/$f.new && chown 65532:65532 /srv/taskto/backup/$f.new && mv /srv/taskto/backup/$f.new /srv/taskto/backup/$f'" \
      < "$TASKTO_BACKUP_DIR/$f" || { echo "FAIL: copying $f to the VM"; ok=no; }
  done
  # No client certificate here, and none recorded: none is left on the VM either.
  [ "$ok" = no ] || [ "$cert" = on ] || echo 'rm -f /srv/taskto/backup/client.pem' | vm || ok=no
  [ "$ok" = yes ] && vm <<'EOF'
cd /srv/taskto
# The repository is created the first time. One that is there already (a reinstall) is
# kept, and opens only with its own password: init then fails, and the step stops.
timeout 300 docker compose run --rm -T backup cat config </dev/null >/dev/null 2>&1 \
  || docker compose run --rm -T backup init </dev/null
docker compose run --rm -T backup-schedule -now </dev/null
EOF
fi
```

If the first backup fails, `docker compose logs backup-schedule` and the error it printed
say why: most often the backup host's firewall, its TLS certificate, a missing or wrong
client certificate (`client.pem`, for a backup host that asks for one), or a wrong user or
password in `repository`. A `client.pem` added or removed after step 1 reaches restic only once steps
1, 6 and 9 have run again: this step first compares it with what step 1 recorded, and
stops, changing nothing, until they agree. Once they do, a removed one is removed from the
VM too.

If the backup host's prune job opens the repository with a key of its own
([docs/install.md](../docs/install.md#3-removing-old-snapshots-on-the-backup-host-only)),
and its admins would rather add it from this side than hold the repository's password,
they send the new key on standard input, never as an argument, to
`docker compose run --rm -T backup key add --new-password-file /dev/stdin` in `/srv/taskto`
on the VM. The append-only server accepts a new key; it can't remove one.

**Check**: the files are the backups' only, the schedule runs unprivileged, the last backup
succeeded, and the backup host refuses a delete from this VM. The delete is of a made-up
snapshot: an append-only server answers `403`; one that isn't answers `404`, so nothing is
deleted either way.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
fail() { echo "FAIL: $*"; exit 1; }
if [ "${TASKTO_BACKUPS:-}" != on ]; then echo "backups are off"; echo OK; exit 0; fi
cd /srv/taskto
[ "$(stat -c '%u:%g %a' backup)" = "65532:65532 700" ] || fail "backup/ isn't 65532's, mode 700"
files="repository password"
cert=""
if [ "${TASKTO_BACKUP_CLIENT_CERT:-}" = on ]; then
  files="$files client.pem"
  cert="--cert backup/client.pem"
fi
for f in $files; do
  [ -s "backup/$f" ] && [ "$(stat -c '%u:%g %a' "backup/$f")" = "65532:65532 600" ] || fail "backup/$f isn't 65532's, mode 600"
done
grep -qrF -f backup/password .env compose.yaml compose.override.yaml /etc/taskto/install.env && fail "the repository's password is in a Compose file"
s=$(docker compose ps -q backup-schedule)
[ -n "$s" ] && [ "$(docker inspect -f '{{.State.Running}}' "$s")" = true ] || fail "the backup schedule isn't running"
docker inspect --format '{{.Config.User}} {{json .HostConfig.CapDrop}} {{.HostConfig.ReadonlyRootfs}}' "$s" \
  | grep -q '^65532:65532 \["ALL"\] true$' || fail "the backup schedule isn't unprivileged"
h=$(curl -sS http://127.0.0.1:8080/healthz/backup)
echo "$h"
[ "$(jq -r .status <<<"$h")" = ok ] || fail "the last backup didn't succeed: docker compose logs backup-schedule"
url=$(sed -n 's|^rest:||p' backup/repository | head -1)
zeros=$(printf '0%.0s' $(seq 64))
# shellcheck disable=SC2086 # $cert is empty, or two words.
code=$(printf 'url = "%s/snapshots/%s"\n' "${url%/}" "$zeros" | curl -sS -o /dev/null -w '%{http_code}' $cert -X DELETE -K -)
[ "$code" = 403 ] || fail "the backup host answered $code to a delete from this VM, not 403: it isn't append-only"
echo OK
EOF
```

Admins also see the last backup, the last error and the next backup in **Settings →
About**. Point the owner's monitoring at `https://TASKTO_DOMAIN/healthz/backup`: it answers
`503` once a night is missed ([docs/install.md](../docs/install.md#every-night-retried-and-watched)).

## Step 16: the restore drill

A backup is known good only once it has been restored. This takes the latest snapshot out
of the repository into a scratch volume, starts a server on it with no network, checks it
holds the owner's account, and removes it all. It needs free disk for one more copy of the
data. Do it again every few months, and after any change to the backup host.

**Run**:

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
[ "${TASKTO_BACKUPS:-}" = on ] || { echo "backups are off: no drill"; exit 0; }
cd /srv/taskto
fail() { echo "FAIL: $*"; exit 1; }
drill=taskto-restore-drill
image="$TASKTO_REGISTRY/taskto:$TASKTO_VERSION"
cleanup() { docker rm -f "$drill" >/dev/null 2>&1 || true; docker volume rm "$drill" >/dev/null 2>&1 || true; }
cleanup
trap cleanup EXIT
snap=$(docker compose run --rm -T backup snapshots --latest 1 --json </dev/null | jq -r '.[-1] | "\(.short_id) of \(.time)"')
# From the repository straight into the scratch volume: the archive never lands on disk.
docker compose run --rm -T backup dump latest /taskto.tar </dev/null \
  | docker run --rm -i --network none -v "$drill":/data "$image" restore -
# A server on it, cut off from everything: no network, no agent runtime.
docker run -d --name "$drill" --network none -e TASKTO_AGENT=false -v "$drill":/data "$image" >/dev/null
timeout 180 bash -c "until [ \"\$(docker inspect -f '{{.State.Health.Status}}' $drill)\" = healthy ]; do sleep 3; done" \
  || fail "the restored server isn't healthy: $(docker logs --tail 20 "$drill" 2>&1)"
ask() { docker exec "$drill" /nodejs/bin/node -e "fetch('http://127.0.0.1:8080$1').then(r => r.text()).then(console.log, () => process.exit(1))"; }
ask /healthz
[ "$(ask /api/setup | jq -r .needed)" = false ] || fail "the restored server has no owner: the snapshot is empty"
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) restored snapshot $snap into a server that started" > /etc/taskto/restore-drill
cat /etc/taskto/restore-drill
EOF
```

**Check**: the drill passed today, and left nothing behind.

```sh
. ~/.config/taskto-install.env
vm <<'EOF'
. /etc/taskto/install.env
fail() { echo "FAIL: $*"; exit 1; }
if [ "${TASKTO_BACKUPS:-}" != on ]; then echo OK; exit 0; fi
[ -n "$(find /etc/taskto/restore-drill -mmin -1440 2>/dev/null)" ] || fail "no restore drill in the last day"
docker ps -a --format '{{.Names}}' | grep -qx taskto-restore-drill && fail "the drill's server is left"
docker volume ls -q | grep -qx taskto-restore-drill && fail "the drill's volume is left"
echo OK
EOF
```

## Step 17: report

To whoever asked for the install, report: the mode, `TASKTO_VERSION`, the image digests from step 7,
the `/healthz` line, that every check printed `OK` (or which didn't, and why), and where
the owner's password is (`/etc/taskto/admin-password`, root-only). For the backups: the
`/healthz/backup` line from step 15, the drill's line from step 16, the time they run, and
that the repository's address and password are in `TASKTO_BACKUP_DIR` on your machine, for
the owner's password manager; or that backups are off, and why. No tokens, passwords,
repository addresses, private addresses or log excerpts with the setup token.

## Updates

Migrations only go forward: once a newer version has started on the data, an older one
won't ([docs/install.md](../docs/install.md#upgrade-and-rollback)). So every update starts
with a backup on the VM, and a rollback means restoring it.

1. Back up, on the VM, into a directory only root reads. The archive holds everything,
   `server.key` included:

   ```sh
   . ~/.config/taskto-install.env
   vm <<'EOF'
   . /etc/taskto/install.env
   install -d -m 700 /var/backups/taskto
   cd /srv/taskto
   f="/var/backups/taskto/before-update-from-$TASKTO_VERSION-$(date +%Y%m%d-%H%M%S).tar.gz"
   (umask 077; docker compose exec -T taskto taskto backup - </dev/null > "$f")
   ls -l "$f"
   EOF
   ```

2. Set `TASKTO_VERSION` in `~/.config/taskto-install.env` to the new tag.
3. Run steps 0, 1, 6, 7, 8 and 9, then 12, 13, 14 and 15, each with its check.
4. The previous images stay on the VM until you remove them (`docker image prune -a`, once
   you won't roll back).

## Rollback

To the version before, with the data from the backup taken before the update. Anything
written since is lost.

1. Set `TASKTO_VERSION` back in `~/.config/taskto-install.env`, and run steps 0, 1, 6, 7 and 8.
2. Replace the data with the backup, on the VM. `BACKUP` is the file from update step 1:

   ```sh
   . ~/.config/taskto-install.env
   vm <<'EOF'
   BACKUP=/var/backups/taskto/before-update-from-v0.1.0-20260101-030405.tar.gz
   cd /srv/taskto
   [ -s "$BACKUP" ] || { echo "FAIL: no $BACKUP"; exit 1; }
   docker compose down
   docker run --rm </dev/null -v taskto_data:/from -v "taskto_data_rolled_back_$(date +%Y%m%d%H%M%S)":/to alpine cp -a /from/. /to/
   docker volume rm taskto_data
   docker compose run --rm --no-deps -T taskto restore - < "$BACKUP"
   EOF
   ```

   The data you replaced is kept in a `taskto_data_rolled_back_*` volume; remove it once
   you are sure.
3. Run step 9, then 12, 13, 14 and 15.

## Backups

Steps 15 and 16 set them up and prove a restore. After that:

- The schedule runs by itself; admins see how it does in **Settings → About**, and
  `/healthz/backup` answers `503` once a night is missed. Updates pull the new backup image
  with the others (step 7), and step 9 restarts the schedule on it.
- Run the drill (step 16) every few months.
- Only the backup host's prune job (`deploy/backup-host/`) deletes snapshots; nothing on
  the VM can ([docs/install.md](../docs/install.md#3-removing-old-snapshots-on-the-backup-host-only)).
- To move the backups' time, change `TASKTO_BACKUP_AT` and run steps 0, 1, 6 and 9.
- To restore after losing the VM: install a new one with the same `TASKTO_BACKUP_DIR` (the
  repository's address and password, kept off the VM for this) up to step 9. Then, on the
  VM, `docker compose stop backup-schedule`, so it doesn't back up the new, empty server;
  copy the files with the `files=` lines and the `for` loop of step 15's Run (not the rest of it, whose backup
  would be of the empty server); and follow [docs/install.md](../docs/install.md#restore),
  "From the restic repository", in `/srv/taskto`, naming the snapshot by its ID from
  `snapshots` rather than `latest`. Then run steps 9 to 17.
