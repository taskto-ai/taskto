#!/usr/bin/env bash
# Keeps a Compose install (deploy/INSTALL.md) on the latest release, safely. Run on the host,
# as root, by taskto-update.timer; never from the server, which has no Docker socket.
#
#   taskto-update [--dry-run] [--to VERSION]
#
# One run: choose a version, check it before touching anything (its files, its three images,
# the disk, `docker compose config` with this install's settings), back up, switch, check the
# new server, and on any failure put the old files back and restore the backup. It never
# downgrades and never updates to the version it is on. docs/install.md#automatic-updates
# says what operators set; docs/releasing.md says what a release is.
#
# Settings: /etc/taskto/install.env (or the environment, which wins):
#   TASKTO_UPDATE_CHANNEL  release (default): the newest GitHub Release named vMAJOR.MINOR.PATCH
#                          that isn't a draft or a pre-release, whose images are published;
#                          main: the newest commit on main whose CI passed, a descendant of
#                          this one, whose images are published; off: nothing.
#   TASKTO_UPDATE_REPO     the GitHub repository (default taskto-ai/taskto)
#   TASKTO_UPDATE_KEEP     how many of this script's own backups to keep (default 3)
# Also: /etc/taskto/update.hold (a file) stops automatic runs; a release whose notes have the
# line `manual-upgrade: true` is not installed automatically, nor is one past such a release.
# --to VERSION installs that version, a vMAJOR.MINOR.PATCH tag or a full commit SHA, even on
# hold or past a manual-upgrade release; --dry-run chooses and checks, and changes nothing.
#
# Every outside command and place is a variable (DOCKER, CURL, FLOCK, SYSTEMCTL, the
# TASKTO_UPDATE_* paths and base URLs below), so cmd/taskto/update_test.go runs it against fakes.
#
# Exit status: 0 updated, up to date, skipped or dry run; 1 failed or rolled back (the server
# is on its old version); 2 the rollback failed too, and nothing runs again until an operator
# looks (the "blocked" file in the state directory).
set -euo pipefail

DOCKER=${DOCKER:-docker}
CURL=${CURL:-curl}
FLOCK=${FLOCK:-flock}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
ROOT=${TASKTO_UPDATE_ROOT:-/srv/taskto}
INSTALL_ENV=${TASKTO_UPDATE_INSTALL_ENV:-/etc/taskto/install.env}
HOLD=${TASKTO_UPDATE_HOLD:-/etc/taskto/update.hold}
STATE=${TASKTO_UPDATE_STATE:-/var/lib/taskto-update}
BACKUPS=${TASKTO_UPDATE_BACKUPS:-/var/backups/taskto}
LOCK=${TASKTO_UPDATE_LOCK:-/run/taskto-update.lock}
FIREWALL=${TASKTO_UPDATE_FIREWALL:-/usr/local/sbin/taskto-worker-firewall}
TOKEN_FILE=${TASKTO_UPDATE_TOKEN_FILE:-/etc/taskto/registry-token}
API=${TASKTO_UPDATE_API:-https://api.github.com}
RAW=${TASKTO_UPDATE_RAW:-https://raw.githubusercontent.com}
VOLUME=${TASKTO_UPDATE_VOLUME:-taskto_data}
VERIFY_TIMEOUT=${TASKTO_UPDATE_VERIFY_TIMEOUT:-300}
POLL=${TASKTO_UPDATE_POLL:-2}
IMAGES_MB=${TASKTO_UPDATE_IMAGES_MB:-4000}
# Moves the data volume aside in a rollback (the runbook's step, with a pinned image).
HELPER=${TASKTO_UPDATE_HELPER:-busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec}
IMAGE_NAMES="taskto taskto-cli-worker taskto-backup"

log() { printf 'taskto-update: %s\n' "$*"; }

# envget FILE KEY: KEY's value in a KEY=value file, without single quotes.
envget() { { sed -n "s/^$2=//p" "$1" 2>/dev/null || true; } | tail -n 1 | sed "s/^'//; s/'\$//"; }
# setting NAME DEFAULT: the environment's NAME, else install.env's, else DEFAULT.
setting() { local v=${!1:-}; [ -n "$v" ] || v=$(envget "$INSTALL_ENV" "$1"); printf '%s' "${v:-$2}"; }

usage() {
	echo "usage: taskto-update [--dry-run] [--to vMAJOR.MINOR.PATCH|FULL-SHA]" >&2
	exit "${1:-2}"
}
DRY=0 TO=""
while [ $# -gt 0 ]; do
	case $1 in
	--dry-run) DRY=1 ;;
	--to)
		[ $# -ge 2 ] || usage
		TO=$2
		shift
		;;
	-h | --help) usage 0 ;;
	*) usage ;;
	esac
	shift
done
if [ -n "$TO" ] && ! [[ $TO =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || $TO =~ ^[0-9a-f]{40}$ ]]; then
	echo "taskto-update: --to takes a vMAJOR.MINOR.PATCH tag or a full commit SHA" >&2
	exit 2
fi

# Two runs never overlap: the second leaves the first's status alone.
exec 9>"$LOCK"
"$FLOCK" -n 9 || {
	log "another update is already running: nothing to do"
	exit 0
}

CHANNEL=$(setting TASKTO_UPDATE_CHANNEL release)
REPO=$(setting TASKTO_UPDATE_REPO taskto-ai/taskto)
KEEP=$(setting TASKTO_UPDATE_KEEP 3)
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
result="" from="" to="" reason="" backup="" digests='{}'
stage="" login_host=""

# --- the status: $STATE/status.json, and a line in the journal ---------------------------

write_status() {
	[ "$DRY" = 0 ] || return 0
	local f=$STATE/status.json last=null change=false
	case $result in updated | rolled-back | rollback-failed) change=true ;; esac
	if [ "$change" = false ] && [ -s "$f" ]; then last=$(jq -c '.last_change // null' "$f" 2>/dev/null) || last=null; fi
	install -d -m 755 "$STATE"
	jq -n --arg at "$started" --arg end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg result "$result" --arg channel "$CHANNEL" \
		--arg from "$from" --arg to "$to" --arg reason "$reason" --arg backup "$backup" \
		--argjson digests "$digests" --argjson last "$last" --argjson change "$change" '
		{at: $at, finished: $end, result: $result, channel: $channel, from: $from, to: $to,
		 reason: $reason, backup: $backup, digests: $digests} as $s
		| $s + {last_change: (if $change then $s else $last end)}' >"$f.new"
	mv -f "$f.new" "$f"
}

# finish RESULT REASON [EXIT]: records the outcome and ends the run.
finish() {
	result=$1 reason=$2
	write_status
	log "result=$result from=${from:-?} to=${to:-?}${reason:+ reason=\"$reason\"}"
	# An explicit --to that wasn't done is a failure to whoever ran it.
	local code=${3:-0}
	if [ "$code" = 0 ] && [ -n "$TO" ] && [ "$DRY" = 0 ]; then
		case $result in skipped | up-to-date) code=1 ;; esac
	fi
	exit "$code"
}

cleanup() {
	local rc=$?
	if [ -z "$result" ]; then
		result=failed reason="unexpected error (exit $rc)"
		write_status || true
		log "result=failed reason=\"$reason\""
	fi
	[ -z "$stage" ] || rm -rf "$stage"
	[ -z "$login_host" ] || "$DOCKER" logout "$login_host" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- versions ------------------------------------------------------------------------------

is_semver() { [[ $1 =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; }
semver_gt() {
	local IFS=. a b i
	read -ra a <<<"${1#v}"
	read -ra b <<<"${2#v}"
	for i in 0 1 2; do
		if ((10#${a[i]} > 10#${b[i]})); then return 0; fi
		if ((10#${a[i]} < 10#${b[i]})); then return 1; fi
	done
	return 1
}
api() { "$CURL" -fsS --max-time 30 -H 'Accept: application/vnd.github+json' "$API/$1"; }
# relation CANDIDATE CURRENT: newer, same, older, diverged (not a descendant) or unknown.
# Two release tags compare as semantic versions; anything else, through GitHub's compare.
relation() {
	if is_semver "$1" && is_semver "$2"; then
		if [ "$1" = "$2" ]; then echo same; elif semver_gt "$1" "$2"; then echo newer; else echo older; fi
		return 0
	fi
	local st
	st=$(api "repos/$REPO/compare/$2...$1" | jq -r '.status // empty') || st=""
	case $st in
	ahead) echo newer ;;
	identical) echo same ;;
	behind) echo older ;;
	diverged) echo diverged ;;
	*) echo unknown ;;
	esac
}

registry_login() {
	[ -z "$login_host" ] || return 0
	[ -n "$(envget "$INSTALL_ENV" TASKTO_REGISTRY_USER)" ] && [ -s "$TOKEN_FILE" ] || return 0
	login_host=${prefix%%/*}
	"$DOCKER" login "$login_host" -u "$(envget "$INSTALL_ENV" TASKTO_REGISTRY_USER)" --password-stdin <"$TOKEN_FILE" >/dev/null
}
# images_published TAG: all three images are in the registry.
images_published() {
	local n
	registry_login
	for n in $IMAGE_NAMES; do
		"$DOCKER" manifest inspect "$prefix$n:$1" >/dev/null 2>&1 || return 1
	done
}
failed_before() { grep -qxF "$1" "$STATE/failed" 2>/dev/null; }

# --- choosing --------------------------------------------------------------------------------

choose_release() {
	local rel rows tag man i pick=-1
	local -a tags=() manual=()
	rel=$(api "repos/$REPO/releases?per_page=30") || finish skipped "GitHub's API didn't answer: nothing done"
	rows=$(jq -r '
		[.[] | select(.draft == false and .prerelease == false and (.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")))]
		| sort_by(.tag_name | ltrimstr("v") | split(".") | map(tonumber)) | reverse | .[]
		| [.tag_name, ((.body // "") | test("(^|\n)manual-upgrade:[ \t]*true[ \t\r]*(\n|$)"; "i"))] | @tsv' <<<"$rel") ||
		finish skipped "GitHub's release list is unreadable: nothing done"
	while IFS=$'\t' read -r tag man; do
		[ -n "$tag" ] || continue
		case $(relation "$tag" "$from") in
		newer) tags+=("$tag") manual+=("$man") ;;
		unknown) finish skipped "can't tell whether $tag is newer than $from" ;;
		*) break ;;
		esac
	done <<<"$rows"
	[ ${#tags[@]} -gt 0 ] || finish up-to-date "no release is newer than $from"
	for i in "${!tags[@]}"; do
		if failed_before "${tags[i]}"; then
			log "${tags[i]} was rolled back on this server before: not tried again"
		elif images_published "${tags[i]}"; then
			pick=$i
			break
		else
			log "${tags[i]}: its images aren't published yet"
		fi
	done
	[ "$pick" -ge 0 ] || finish skipped "no newer release is available: ${tags[0]}'s images aren't published (or it was rolled back before)"
	# The release itself and every one between it and this install.
	for ((i = pick; i < ${#tags[@]}; i++)); do
		if [ "${manual[i]}" = true ]; then
			finish skipped "${tags[i]} needs manual steps (its notes say manual-upgrade: true): follow its notes, then run taskto-update --to ${tags[pick]}"
		fi
	done
	to=${tags[pick]}
}

choose_main() {
	local runs shas sha waiting=0
	runs=$(api "repos/$REPO/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=30") ||
		finish skipped "GitHub's API didn't answer: nothing done"
	shas=$(jq -r '.workflow_runs[] | select(.conclusion == "success" and .head_branch == "main") | .head_sha' <<<"$runs") ||
		finish skipped "GitHub's CI runs are unreadable: nothing done"
	while read -r sha; do
		[ -n "$sha" ] || continue
		case $(relation "$sha" "$from") in
		newer)
			if failed_before "$sha"; then
				log "$sha was rolled back on this server before: not tried again"
			elif images_published "$sha"; then
				to=$sha
				return 0
			else
				log "$sha: its images aren't published yet"
				waiting=1
			fi
			;;
		same | older) break ;;
		diverged) log "$sha isn't a descendant of $from: passed over" ;;
		*) finish skipped "can't tell whether $sha is newer than $from" ;;
		esac
	done <<<"$shas"
	if [ "$waiting" = 1 ]; then finish skipped "the newest green commits' images aren't published yet"; fi
	finish up-to-date "no green commit on main is newer than $from"
}

# --- checking, before anything is touched ----------------------------------------------------

free_kb() { df -Pk "$1" | awk 'NR == 2 {print $4}'; }
check_disk() {
	local vol data_kb droot need_backup need_docker
	vol=$("$DOCKER" volume inspect "$VOLUME" --format '{{.Mountpoint}}') || return 1
	data_kb=$(du -sk "$vol" | cut -f1) || return 1
	droot=$("$DOCKER" info --format '{{.DockerRootDir}}') || return 1
	install -d -m 700 "$BACKUPS" || return 1
	# A backup is at most the data; a rollback copies the data again, beside the images.
	need_backup=$((data_kb + 102400))
	need_docker=$((data_kb + IMAGES_MB * 1024))
	if [ "$(stat -c %d "$BACKUPS")" = "$(stat -c %d "$droot")" ]; then
		need_docker=$((need_docker + need_backup))
	fi
	if [ "$(free_kb "$BACKUPS")" -lt "$need_backup" ] || [ "$(free_kb "$droot")" -lt "$need_docker" ]; then
		reason="not enough free disk for a backup (${need_backup} KB), a copy of the data and the images"
		return 1
	fi
}

# set_version FILE VERSION QUOTE: FILE with its TASKTO_VERSION line set, on standard output.
set_version() {
	if grep -q '^TASKTO_VERSION=' "$1"; then
		sed "s/^TASKTO_VERSION=.*/TASKTO_VERSION=$3$2$3/" "$1"
	else
		cat "$1"
		echo "TASKTO_VERSION=$3$2$3"
	fi
}

preflight() {
	local n d
	stage=$(mktemp -d) || return 1
	mkdir "$stage/deploy" || return 1
	# The new version's files, as the runbook's steps 6 and 8 install them.
	for n in compose.yaml deploy/Caddyfile; do
		"$CURL" -fsSL --max-time 60 -o "$stage/$n" "$RAW/$REPO/$to/$n" || { reason="can't fetch $n of $to"; return 1; }
	done
	chmod 644 "$stage/compose.yaml" "$stage/deploy/Caddyfile" || return 1
	grep -q TASKTO_IMAGE_PREFIX "$stage/compose.yaml" || { reason="$to's compose.yaml predates published images"; return 1; }
	if [ -e "$FIREWALL" ]; then
		"$CURL" -fsSL --max-time 60 -o "$stage/worker-firewall.sh" "$RAW/$REPO/$to/deploy/worker-firewall.sh" ||
			{ reason="can't fetch deploy/worker-firewall.sh of $to"; return 1; }
	fi
	check_disk || { [ -n "$reason" ] || reason="can't read the disk's free space"; return 1; }
	registry_login || { reason="can't log in to the registry"; return 1; }
	"$DOCKER" image inspect "$HELPER" >/dev/null 2>&1 || "$DOCKER" pull --quiet "$HELPER" >/dev/null ||
		{ reason="can't pull $HELPER"; return 1; }
	for n in $IMAGE_NAMES; do
		"$DOCKER" pull --quiet "$prefix$n:$to" >/dev/null || { reason="can't pull $prefix$n:$to"; return 1; }
		d=$("$DOCKER" image inspect --format '{{index .RepoDigests 0}}' "$prefix$n:$to") || { reason="no digest for $prefix$n:$to"; return 1; }
		digests=$(jq -c --arg k "$n" --arg v "$d" '. + {($k): $v}' <<<"$digests")
	done
	# The new Compose file with this install's own settings.
	set_version "$ROOT/.env" "$to" "" >"$stage/.env" || return 1
	[ ! -e "$ROOT/compose.override.yaml" ] || cat "$ROOT/compose.override.yaml" >"$stage/compose.override.yaml" || return 1
	if ! out=$("$DOCKER" compose --project-directory "$stage" config --quiet 2>&1) || [[ $out == *"is not set"* ]]; then
		reason="$to's compose.yaml doesn't work with this install's settings: $(printf '%s' "$out" | head -n 3 | tr '\n' ' ')"
		return 1
	fi
}

# --- the switch, and the way back ------------------------------------------------------------

compose() { "$DOCKER" compose "$@"; }
# step WHAT COMMAND...: the command, or the reason it failed.
step() {
	local what=$1
	shift
	"$@" || { reason="$what failed"; return 1; }
}

make_backup() {
	install -d -m 700 "$BACKUPS" || return 1
	local short_from=${from:0:12} short_to=${to:0:12}
	backup=$BACKUPS/before-update-from-$short_from-to-$short_to-$(date +%Y%m%d-%H%M%S).tar.gz
	# The archive holds everything, server.key included: root only.
	if ! (umask 077; compose exec -T taskto taskto backup - </dev/null >"$backup.part") || ! [ -s "$backup.part" ]; then
		rm -f "$backup.part"
		backup=""
		return 1
	fi
	mv "$backup.part" "$backup"
}
# Only this script's own backups, the newest KEEP of them: never restic's, never a person's.
prune_backups() {
	local f n=0
	while IFS= read -r f; do
		n=$((n + 1))
		[ "$n" -le "$KEEP" ] || rm -f -- "$f"
	done < <(ls -1t "$BACKUPS"/before-update-from-*-to-*.tar.gz 2>/dev/null || true)
}

put() { # put SRC DEST: replaced in one move
	cp -p "$1" "$2.new" && mv -f "$2.new" "$2"
}
save_previous() {
	local p=$STATE/previous
	install -d -m 755 "$STATE" && rm -rf "$p" && mkdir -p "$p/deploy" &&
		cp -p "$ROOT/compose.yaml" "$ROOT/.env" "$p/" &&
		cp -p "$ROOT/deploy/Caddyfile" "$p/deploy/" &&
		cp -p "$INSTALL_ENV" "$p/install.env" &&
		{ [ ! -e "$FIREWALL" ] || cp -p "$FIREWALL" "$p/worker-firewall.sh"; }
}
install_new() {
	put "$stage/compose.yaml" "$ROOT/compose.yaml" &&
		put "$stage/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" &&
		put "$stage/.env" "$ROOT/.env" || return 1
	# The runbook's later steps read the version from here.
	if grep -q '^TASKTO_VERSION=' "$INSTALL_ENV"; then
		set_version "$INSTALL_ENV" "$to" "'" >"$INSTALL_ENV.new" &&
			chmod --reference="$INSTALL_ENV" "$INSTALL_ENV.new" && mv -f "$INSTALL_ENV.new" "$INSTALL_ENV"
	fi
}
# firewall_from FILE: the worker's firewall script, put in and applied, when it changed (step 8).
firewall_from() {
	[ -e "$1" ] && [ -e "$FIREWALL" ] || return 0
	cmp -s "$1" "$FIREWALL" && return 0
	install -m 755 "$1" "$FIREWALL.new" && mv -f "$FIREWALL.new" "$FIREWALL" &&
		"$SYSTEMCTL" restart taskto-worker-firewall && "$FIREWALL" check
}

# verify VERSION: the server answers as that version, with its agent runtime, and the
# safety rules of the runbook's step 12 still hold.
verify() {
	local deadline=$((SECONDS + VERIFY_TIMEOUT)) h s w c e svc
	until h=$("$CURL" -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null) &&
		jq -e --arg v "$1" '.status == "ok" and .agent == "ready" and .version == $v' <<<"$h" >/dev/null 2>&1; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			reason="the server isn't healthy as $1 after ${VERIFY_TIMEOUT}s (${h:-no answer})"
			return 1
		fi
		sleep "$POLL"
	done
	for svc in taskto cli-worker; do
		[ "$(compose ps --format '{{.State}}' "$svc")" = running ] || { reason="$svc isn't running"; return 1; }
	done
	s=$(compose ps -q taskto) w=$(compose ps -q cli-worker)
	[ "$("$DOCKER" inspect --format '{{.Config.User}}' "$s")" = 65532:65532 ] || { reason="the server doesn't run as 65532"; return 1; }
	for c in "$s" "$w"; do
		"$DOCKER" inspect --format '{{json .HostConfig.CapDrop}} {{json .HostConfig.SecurityOpt}} {{.HostConfig.Privileged}}' "$c" |
			grep -q '^\["ALL"\] \["no-new-privileges:true"\] false$' || { reason="a container isn't capability-free with no-new-privileges"; return 1; }
	done
	e=$("$DOCKER" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$s" "$w")
	if grep -iE 'INSECURE|UNSAFE' <<<"$e" >/dev/null ||
		grep -v '^TASKTO_DISABLE_PASSWORD_LOGIN=' <<<"$e" | grep -iE '(TOKEN|SECRET|PASSWORD|API_KEY)[A-Z_]*=.' >/dev/null; then
		reason="a development switch or a secret is in a container's environment"
		return 1
	fi
	grep -qx 'TASKTO_CLI_WORKERS=container' <<<"$e" || { reason="CLI workers aren't in container mode"; return 1; }
}

up() { compose up -d --no-build --pull never --remove-orphans --wait --wait-timeout "$VERIFY_TIMEOUT"; }

apply_update() {
	step "saving the current files" save_previous || return 1
	step "installing the new files" install_new || return 1
	step "applying the worker's firewall" firewall_from "$stage/worker-firewall.sh" || return 1
	step "docker compose up" up || return 1
	verify "$to"
}

restore_data() { compose run --rm --no-deps -T taskto restore - <"$backup"; }
move_data_aside() {
	"$DOCKER" run --rm -v "$VOLUME:/from" -v "${VOLUME}_rolled_back_$(date +%Y%m%d%H%M%S):/to" "$HELPER" cp -a /from/. /to/ </dev/null &&
		"$DOCKER" volume rm "$VOLUME" >/dev/null
}
# The old files, the data moved aside (never deleted) and the backup restored by the OLD image.
rollback() {
	local p=$STATE/previous
	step "putting the old files back" put "$p/compose.yaml" "$ROOT/compose.yaml" || return 1
	step "putting the old files back" put "$p/.env" "$ROOT/.env" || return 1
	step "putting the old files back" put "$p/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" || return 1
	step "putting the old files back" put "$p/install.env" "$INSTALL_ENV" || return 1
	step "putting the old firewall back" firewall_from "$p/worker-firewall.sh" || return 1
	step "docker compose down" compose down || return 1
	step "moving the data aside" move_data_aside || return 1
	step "restoring the backup" restore_data || return 1
	step "docker compose up" up || return 1
	verify "$from"
}

# --- one run -----------------------------------------------------------------------------------

[ -r "$ROOT/.env" ] || finish failed "no $ROOT/.env: not an install from deploy/INSTALL.md" 1
cd "$ROOT" # where Compose finds the project
from=$(envget "$ROOT/.env" TASKTO_VERSION)
prefix=$(envget "$ROOT/.env" TASKTO_IMAGE_PREFIX)
port=$(envget "$ROOT/.env" TASKTO_PORT)
HEALTH_URL=${TASKTO_UPDATE_HEALTH:-http://127.0.0.1:${port:-8080}/healthz}
case $CHANNEL in release | main | off) ;; *) finish failed "TASKTO_UPDATE_CHANNEL is $CHANNEL, not release, main or off" 1 ;; esac
[ -n "$prefix" ] || finish failed "TASKTO_IMAGE_PREFIX isn't set: this install doesn't use the published images" 1
[[ $KEEP =~ ^[1-9][0-9]*$ ]] || finish failed "TASKTO_UPDATE_KEEP is $KEEP, not a number of backups" 1
if [ -z "$TO" ] && [ -e "$STATE/blocked" ] && [ "$DRY" = 0 ]; then
	# A rollback that failed left the install as it was: nobody should change it again unseen.
	# The status stays what the failed run said.
	result=blocked
	log "blocked: $(cat "$STATE/blocked"); look at it, then remove $STATE/blocked (or run taskto-update --to VERSION)"
	exit 1
fi

if [ -n "$TO" ]; then
	to=$TO
	case $(relation "$to" "$from") in
	newer) ;;
	same) finish skipped "already on $to" ;;
	older) finish skipped "$to is older than $from: downgrades are refused" ;;
	diverged) finish skipped "$to isn't a descendant of $from" ;;
	*) finish failed "can't tell whether $to is newer than $from" 1 ;;
	esac
	images_published "$to" || finish failed "$to's images aren't published" 1
else
	if [ "$CHANNEL" = off ]; then finish skipped "automatic updates are off (TASKTO_UPDATE_CHANNEL=off)"; fi
	if [ -e "$HOLD" ]; then finish skipped "updates are on hold ($HOLD exists)"; fi
	if ! is_semver "$from" && ! [[ $from =~ ^[0-9a-f]{40}$ ]]; then
		finish skipped "$from is neither a release nor a commit: can't tell what is newer"
	fi
	case $CHANNEL in release) choose_release ;; main) choose_main ;; esac
fi
log "from $from to $to ($CHANNEL channel${TO:+, by request})"

preflight || finish failed "$reason" 1
if [ "$DRY" = 1 ]; then
	finish dry-run "would update from $from to $to: its files, images, disk and Compose settings are fine"
fi

make_backup || finish failed "the backup failed: nothing was changed" 1
prune_backups
log "backed up to $backup"

if apply_update; then
	rm -f "$STATE/blocked"
	finish updated "" 0
fi
cause=$reason
log "the update failed ($cause): rolling back to $from"
printf '%s\n' "$to" >>"$STATE/failed"
if rollback; then
	rm -f "$STATE/blocked"
	finish rolled-back "$cause; the server is on $from again, with the data from before the update" 1
fi
printf '%s: %s; the rollback failed: %s\n' "$to" "$cause" "$reason" >"$STATE/blocked"
finish rollback-failed "$cause; the rollback failed too ($reason): the install is left as it is, backup $backup" 2
