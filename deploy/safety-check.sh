#!/usr/bin/env bash
# The agent safety rules (docs/agent-safety.md, AGENTS.md), checked in the RUNNING containers of
# a Compose install. One script, two callers, so they can't drift: step 12 of deploy/INSTALL.md
# runs it over SSH (`vm < deploy/safety-check.sh`), and deploy/taskto-update.sh runs it after
# every update, and after a rollback. It changes nothing; it prints OK, or FAIL: and the
# first rule that doesn't hold, and exits 1. It never prints a value from a container's
# environment, only the names.
#
#   TASKTO_ROOT  the Compose project (default /srv/taskto)
#   DOCKER       the docker command (default docker)
set -euo pipefail

DOCKER=${DOCKER:-docker}
cd "${TASKTO_ROOT:-/srv/taskto}"
fail() {
	echo "FAIL: $*"
	exit 1
}
# docker, away from this script's own standard input (it may be the script itself).
d() { "$DOCKER" "$@" </dev/null; }

s=$(d compose ps -q taskto) || fail "can't list the server's container"
w=$(d compose ps -q cli-worker) || fail "can't list the CLI worker's container"
[ -n "$s" ] && [ -n "$w" ] || fail "the server or the CLI worker isn't running"
# Whole `docker inspect` output of each, captured so a failure is a failure, not an empty pipe.
si=$(d inspect "$s") || fail "can't inspect the server's container"
wi=$(d inspect "$w") || fail "can't inspect the CLI worker's container"
q() { jq -e "$1" >/dev/null; } # a jq test on standard input

[ "$(jq -r '.[0].Config.User' <<<"$si")" = 65532:65532 ] || fail "the server doesn't run as 65532"

# No capabilities, no new privileges, not privileged, in both; and beyond that nothing added:
# none for the server, exactly SETUID, SETGID, CHOWN and KILL for the worker.
for i in "$si" "$wi"; do
	q '.[0].HostConfig | (.CapDrop == ["ALL"]) and (.SecurityOpt == ["no-new-privileges:true"]) and (.Privileged == false)' <<<"$i" ||
		fail "a container isn't capability-free with no-new-privileges"
done
q '(.[0].HostConfig.CapAdd // []) | length == 0' <<<"$si" || fail "the server has capabilities added"
q '[(.[0].HostConfig.CapAdd // [])[] | ltrimstr("CAP_")] | sort == ["CHOWN","KILL","SETGID","SETUID"]' <<<"$wi" ||
	fail "the worker has capabilities beyond SETUID, SETGID, CHOWN and KILL"

d compose exec -T cli-worker true || fail "can't run a command in the worker"
# Without DAC capabilities a few root-only directories are unreadable, to the CLIs too.
suid=$(d compose exec -T cli-worker find / -xdev -perm /6000 -type f 2>/dev/null || true)
[ -z "$suid" ] || fail "the worker image has setuid or setgid programs: $suid"

# No development switch and no secret in either environment (names only are shown).
senv=$(jq -r '.[0].Config.Env[]?' <<<"$si")
wenv=$(jq -r '.[0].Config.Env[]?' <<<"$wi")
bad=$(printf '%s\n%s\n' "$senv" "$wenv" | grep -v '^TASKTO_DISABLE_PASSWORD_LOGIN=' | grep -iE 'INSECURE|UNSAFE|TOKEN|SECRET|PASSWORD|API_KEY' | cut -d= -f1 || true)
[ -z "$bad" ] || fail "a development switch or a secret is in a container's environment: $(tr '\n' ' ' <<<"$bad")"
# The server's own environment, not the worker's.
grep -qx 'TASKTO_CLI_WORKERS=container' <<<"$senv" || fail "CLI workers aren't in container mode"

# The worker can't see the server's data, has no host path and no Docker socket.
dv=$(jq -r '[.[0].Mounts[]? | select(.Destination == "/data") | .Name][0] // empty' <<<"$si")
[ -n "$dv" ] || fail "can't tell the server's data volume"
jq -e --arg v "$dv" '[.[0].Mounts[]? | select(.Name == $v or .Type == "bind")] | length == 0' <<<"$wi" >/dev/null ||
	fail "the worker can see the server's data, or has a host path"
for i in "$si" "$wi"; do
	q '[.[0].Mounts[]? | select(((.Source // "") + (.Destination // "")) | contains("docker.sock"))] | length == 0' <<<"$i" ||
		fail "a container has the Docker socket"
done

# The worker is on one network, its own, which the server isn't on, and the firewall's bridge.
q '.[0].NetworkSettings.Networks | length == 1' <<<"$wi" || fail "the worker isn't on one network only"
net=$(jq -r '.[0].NetworkSettings.Networks | keys[0]' <<<"$wi")
jq -e --arg n "$net" '.[0].NetworkSettings.Networks | has($n) | not' <<<"$si" >/dev/null || fail "the server is on the worker's network"
bridge=$(d network inspect "$net" --format '{{index .Options "com.docker.network.bridge.name"}}') || fail "can't inspect the worker's network"
[ "$bridge" = taskto-worker ] || fail "the worker's bridge isn't taskto-worker, the firewall's"
echo OK
