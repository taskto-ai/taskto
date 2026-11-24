#!/usr/bin/env bash
# The agent safety rules (docs/agent-safety.md, AGENTS.md), checked in the RUNNING containers of
# a Compose install. One script, two callers, so they can't drift: step 12 of deploy/INSTALL.md
# runs it over SSH (`vm < deploy/safety-check.sh`), and deploy/taskto-update.sh runs it after
# every update, and after a rollback. It changes nothing; it prints OK, or FAIL: and the
# first rule that doesn't hold, and exits 1. It never prints a value from a container's
# environment, only the names.
#
#   safety-check.sh                the running containers
#   safety-check.sh --static DIR   the same rules, on the Compose settings of the project
#                                  directory DIR as `docker compose config` renders them, with
#                                  nothing started: the updater runs it on a new version's
#                                  settings (with this install's .env and override) before
#                                  it changes anything. What only a running container shows
#                                  (a setuid program in the image, the network Docker made)
#                                  is left to the live check, which still runs after the start.
#
#   TASKTO_ROOT          the Compose project (default /srv/taskto)
#   DOCKER               the docker command (default docker)
#   TASKTO_UPDATE_STATE  the updater's state directory (default /var/lib/taskto-update), which the
#                        server may not see, except its `public` directory, read-only
#
# Compose is always given its inputs: the project name, DIR's .env, and DIR's compose.yaml and,
# when it exists, compose.override.yaml, and no other file (the updater refuses an install that
# has one). The same list the updater starts the stack with.
set -euo pipefail

DOCKER=${DOCKER:-docker}
state=${TASKTO_UPDATE_STATE:-/var/lib/taskto-update}
state=${state%/}
mode=live dir=""
case ${1:-} in
"") ;;
--static)
	mode=static dir=${2:-}
	[ $# -eq 2 ] && [ -n "$dir" ] || {
		echo "usage: safety-check.sh [--static PROJECT-DIRECTORY]" >&2
		exit 2
	}
	dir=$(cd "$dir" && pwd)
	;;
*)
	echo "usage: safety-check.sh [--static PROJECT-DIRECTORY]" >&2
	exit 2
	;;
esac
cd "${TASKTO_ROOT:-/srv/taskto}"
base=${dir:-$PWD}
cargs=(--project-directory "$base" --project-name taskto --env-file "$base/.env" -f "$base/compose.yaml")
[ ! -e "$base/compose.override.yaml" ] || cargs+=(-f "$base/compose.override.yaml")
fail() {
	echo "FAIL: $*"
	exit 1
}
# docker, away from this script's own standard input (it may be the script itself).
d() { "$DOCKER" "$@" </dev/null; }
# docker compose, on the project's explicit inputs.
c() { d compose "${cargs[@]}" "$@"; }

if [ "$mode" = static ]; then
	cfg=$(c config --format json) || fail "can't render the Compose settings"
	# A jq test on the settings, for one service (S): `t S 'filter'`.
	t() { jq -e --arg s "$1" "$2" >/dev/null <<<"$cfg"; }
	t taskto '.services[$s] != null' && t cli-worker '.services[$s] != null' || fail "the server or the CLI worker isn't in the Compose settings (is the cli profile on?)"

	# The server runs as 65532:65532, exactly as the running container check wants it: the
	# Compose file's user if it sets one, else the image's.
	user=$(jq -r '.services.taskto.user // empty' <<<"$cfg")
	if [ -z "$user" ]; then
		img=$(jq -r '.services.taskto.image // empty' <<<"$cfg")
		[ -n "$img" ] || fail "the server has no image"
		user=$(d image inspect --format '{{.Config.User}}' "$img") || fail "can't tell the server's user"
	fi
	[ "$user" = 65532:65532 ] || fail "the server wouldn't run as 65532:65532 (its user is '${user:-root}')"

	# No capabilities, no new privileges, not privileged, in both; and beyond that nothing added:
	# none for the server, exactly SETUID, SETGID, CHOWN and KILL for the worker. (The worker's own
	# user is root, which makes the CLIs' Unix users; the CLIs never run as root.)
	for s in taskto cli-worker; do
		t "$s" '.services[$s] | ((.privileged // false) == false) and ((.cap_drop // []) == ["ALL"]) and ((.security_opt // []) == ["no-new-privileges:true"])' ||
			fail "$s isn't capability-free with no-new-privileges, or is privileged"
	done
	t taskto '(.services[$s].cap_add // []) | length == 0' || fail "the server has capabilities added"
	t cli-worker '[(.services[$s].cap_add // [])[] | ltrimstr("CAP_")] | sort == ["CHOWN","KILL","SETGID","SETUID"]' ||
		fail "the worker has capabilities beyond SETUID, SETGID, CHOWN and KILL"
	t taskto '[.services.taskto, .services["cli-worker"] | ((.command // []), (.entrypoint // [])) | if type == "array" then join(" ") else tostring end] | join(" ") | test("unsafe"; "i") | not' ||
		fail "a container's command has a development switch"

	# No development switch and no secret in either environment (names only are shown); the
	# server's own environment has the CLI workers in container mode. The one name that says
	# PASSWORD and is allowed is the server's TASKTO_SMTP_PASSWORD_FILE, whose value is the path of
	# a file (empty when email is off, as Compose renders an unset variable): never the worker's,
	# and never a value that isn't a path.
	bad=$(jq -r 'def risky: (.key | test("INSECURE|UNSAFE|TOKEN|SECRET|PASSWORD|API_KEY"; "i")) and .key != "TASKTO_DISABLE_PASSWORD_LOGIN";
		def smtpfile: .key == "TASKTO_SMTP_PASSWORD_FILE" and ((.value // "" | tostring) | test("^(/[^\\s]*)?$"));
		((.services.taskto.environment // {}) | to_entries[] | select(risky and (smtpfile | not)) | .key),
		((.services["cli-worker"].environment // {}) | to_entries[] | select(risky) | .key)' <<<"$cfg")
	[ -z "$bad" ] || fail "a development switch or a secret is in a container's environment: $(tr '\n' ' ' <<<"$bad")"
	t taskto '.services.taskto.environment.TASKTO_CLI_WORKERS == "container"' || fail "CLI workers aren't in container mode"

	# The worker can't see the server's data, has no host path and no Docker socket. A volume is
	# its effective Docker name, which is what the same name in two services shares. No service
	# takes another's mounts (volumes_from), and no file is mounted into the worker as a config or
	# a secret (the running check sees both as host paths).
	t taskto '[.services[] | select((.volumes_from // []) | length > 0)] | length == 0' || fail "a service takes another's mounts (volumes_from)"
	t cli-worker '((.services[$s].configs // []) | length == 0) and ((.services[$s].secrets // []) | length == 0)' ||
		fail "the worker has a config or a secret mounted"
	dv=$(jq -r '. as $c | [.services.taskto.volumes[]? | select(.target == "/data" and .type == "volume") | ($c.volumes[.source // ""].name // .source)][0] // empty' <<<"$cfg")
	[ -n "$dv" ] || fail "can't tell the server's data volume"
	jq -e --arg v "$dv" '. as $c | [.services["cli-worker"].volumes[]? | select(.type == "bind" or (.type == "volume" and (($c.volumes[.source // ""].name // .source) == $v)))] | length == 0' <<<"$cfg" >/dev/null ||
		fail "the worker can see the server's data, or has a host path"
	for s in taskto cli-worker; do
		t "$s" '[.services[$s].volumes[]? | select(((.source // "") + (.target // "")) | contains("docker.sock"))] | length == 0' ||
			fail "a container has the Docker socket"
	done
	# The server reads one thing of the host's updater, its status for admins: the directory
	# $state/public, read-only. Nothing that holds the updater's own state (the transaction, the
	# old files and .env, the backups), and no directory above it, which would hold it.
	jq -e --arg state "$state" 'def bad: (.source // "" | rtrimstr("/")) as $p | ($state == $p or $p == "" or ($state | startswith($p + "/")) or
			(($p | startswith($state + "/")) and ($p != $state + "/public" or .read_only != true)));
		[.services.taskto.volumes[]? | select(.type == "bind" and bad)] | length == 0' <<<"$cfg" >/dev/null ||
		fail "the server has a host path that holds the updater's state (only $state/public, read-only, may be mounted)"

	# The worker is on one network, its own, which the server isn't on, and the firewall's bridge.
	t cli-worker '(.services[$s].networks // {default: null}) | length == 1' || fail "the worker isn't on one network only"
	net=$(jq -r '(.services["cli-worker"].networks // {default: null}) | keys[0]' <<<"$cfg")
	jq -e --arg n "$net" '(.services.taskto.networks // {default: null}) | has($n) | not' <<<"$cfg" >/dev/null || fail "the server is on the worker's network"
	jq -e --arg n "$net" '.networks[$n].driver_opts["com.docker.network.bridge.name"] == "taskto-worker"' <<<"$cfg" >/dev/null ||
		fail "the worker's bridge isn't taskto-worker, the firewall's"
	echo OK
	exit 0
fi

s=$(c ps -q taskto) || fail "can't list the server's container"
w=$(c ps -q cli-worker) || fail "can't list the CLI worker's container"
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

c exec -T cli-worker true || fail "can't run a command in the worker"
# Without DAC capabilities a few root-only directories are unreadable, to the CLIs too.
suid=$(c exec -T cli-worker find / -xdev -perm /6000 -type f 2>/dev/null || true)
[ -z "$suid" ] || fail "the worker image has setuid or setgid programs: $suid"

# No development switch and no secret in either environment (names only are shown).
senv=$(jq -r '.[0].Config.Env[]?' <<<"$si")
wenv=$(jq -r '.[0].Config.Env[]?' <<<"$wi")
# The rule is about names: a value that is a file's path (TASKTO_SSO_PROVIDERS=/run/secrets/sso.json)
# is not a secret in the environment. The one name that says PASSWORD and is allowed is the
# server's TASKTO_SMTP_PASSWORD_FILE, set to a path or to nothing (Compose's unset variable):
# never the worker's, and never a value that isn't a path.
senv_checked=$(grep -vE '^TASKTO_SMTP_PASSWORD_FILE=(/[^[:space:]]*)?$' <<<"$senv" || true)
bad=$(printf '%s\n%s\n' "$senv_checked" "$wenv" | grep -v '^TASKTO_DISABLE_PASSWORD_LOGIN=' | cut -d= -f1 | grep -iE 'INSECURE|UNSAFE|TOKEN|SECRET|PASSWORD|API_KEY' || true)
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
# The same for the updater's state as in the static check: the server may mount $state/public, read-only.
jq -e --arg state "$state" 'def bad: (.Source // "" | rtrimstr("/")) as $p | ($state == $p or $p == "" or ($state | startswith($p + "/")) or
		(($p | startswith($state + "/")) and ($p != $state + "/public" or .RW != false)));
	[.[0].Mounts[]? | select(.Type == "bind" and bad)] | length == 0' <<<"$si" >/dev/null ||
	fail "the server has a host path that holds the updater's state (only $state/public, read-only, may be mounted)"

# The worker is on one network, its own, which the server isn't on, and the firewall's bridge.
q '.[0].NetworkSettings.Networks | length == 1' <<<"$wi" || fail "the worker isn't on one network only"
net=$(jq -r '.[0].NetworkSettings.Networks | keys[0]' <<<"$wi")
jq -e --arg n "$net" '.[0].NetworkSettings.Networks | has($n) | not' <<<"$si" >/dev/null || fail "the server is on the worker's network"
bridge=$(d network inspect "$net" --format '{{index .Options "com.docker.network.bridge.name"}}') || fail "can't inspect the worker's network"
[ "$bridge" = taskto-worker ] || fail "the worker's bridge isn't taskto-worker, the firewall's"
echo OK
