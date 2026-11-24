#!/usr/bin/env bash
# Keeps a Compose install (deploy/INSTALL.md) on the latest release, safely. Run on the host,
# as root, by taskto-update.timer; never from the server, which has no Docker socket.
#
#   taskto-update [--dry-run] [--to VERSION]
#
# It does the safe common case and leaves the rest to a person. One run: choose a version,
# resolve it to one commit and take every file and image from that commit, check it all before
# touching anything, back up, record a transaction, switch, check the new server and, on any
# failure, put the old files back (and, if the new server may have written data, restore the
# backup). It never downgrades and never updates to the version it is on. Anything it can't
# do with certainty it refuses, reports in its status, and leaves the server as it was:
# a release with `manual-upgrade: true`, a changed worker firewall, an update that was
# interrupted, a server whose version isn't the one on record. docs/install.md#automatic-updates
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
# hold or past a manual-upgrade release; --dry-run chooses and checks, and changes nothing
# (it does pull the images, and logs in to a private registry, to check them).
#
# Every outside command and place is a variable (DOCKER, CURL, FLOCK, SYSTEMCTL, the
# TASKTO_UPDATE_* paths and base URLs below), so cmd/taskto/update_test.go runs it against fakes.
#
# Exit status: 0 updated, up to date, skipped or dry run; 1 failed, rolled back (the server is
# on its old version), or refusing to run (blocked, interrupted); 2 the rollback failed too,
# and nothing runs again until an operator looks (the "blocked" file in the state directory).
set -euo pipefail
export LC_ALL=C

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
VERIFY_TIMEOUT=${TASKTO_UPDATE_VERIFY_TIMEOUT:-300}
POLL=${TASKTO_UPDATE_POLL:-2}
IMAGES_MB=${TASKTO_UPDATE_IMAGES_MB:-4000}
PER_PAGE=${TASKTO_UPDATE_PER_PAGE:-100}
MAX_PAGES=50
TX=$STATE/transaction
# Moves the data volume aside in a rollback (the runbook's step, with a pinned image).
HELPER=${TASKTO_UPDATE_HELPER:-busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec}
IMAGE_NAMES="taskto taskto-cli-worker taskto-backup"
# The one grammar of a release version, here and in .github/workflows/release.yml.
SEMVER='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

log() { printf 'taskto-update: %s\n' "$*"; }

# envget FILE KEY: KEY's value in a KEY=value file, without single quotes.
envget() { { sed -n "s/^$2=//p" "$1" 2>/dev/null || true; } | tail -n 1 | sed "s/^'//; s/'\$//"; }
# setting NAME DEFAULT: the environment's NAME, else install.env's, else DEFAULT.
setting() { local v=${!1:-}; [ -n "$v" ] || v=$(envget "$INSTALL_ENV" "$1"); printf '%s' "${v:-$2}"; }

usage() {
	echo "usage: taskto-update [--dry-run] [--to vMAJOR.MINOR.PATCH|FULL-SHA]" >&2
	exit "${1:-2}"
}

# --- versions (no arithmetic on a version's numbers: they are strings) -------------------------

is_semver() { [ ${#1} -le 100 ] && [[ $1 =~ $SEMVER ]]; }
is_sha() { [[ $1 =~ ^[0-9a-f]{40}$ ]]; }
# semver_key VERSION: a string that sorts as the version does: each component is its length
# (three digits) and then itself, so a longer number is the bigger one, and equal lengths
# compare as text.
semver_key() {
	local IFS=. c
	read -ra c <<<"${1#v}"
	printf '%03d%s.%03d%s.%03d%s' "${#c[0]}" "${c[0]}" "${#c[1]}" "${c[1]}" "${#c[2]}" "${c[2]}"
}
semver_gt() { [[ "$(semver_key "$1")" > "$(semver_key "$2")" ]]; }

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
if [ -n "$TO" ] && ! is_semver "$TO" && ! is_sha "$TO"; then
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
result="" from="" to="" sha="" reason="" backup="" digests='{}'
stage="" login_config=""
# The transaction: what the run changes, kept in $TX from before the first change until a
# terminal outcome, so a run that is stopped half way is found, not mistaken for done.
phase="" volume="" old_server_image="" old_images='{}' new_images='{}' caddy_changed=0 data_restored=0 from_sha=""

# --- the status: $STATE/status.json, and a line in the journal ---------------------------

write_status() {
	[ "$DRY" = 0 ] || return 0
	local f=$STATE/status.json last=null change=false
	case $result in updated | rolled-back | rollback-failed | interrupted) change=true ;; esac
	if [ "$change" = false ] && [ -s "$f" ]; then last=$(jq -c '.last_change // null' "$f" 2>/dev/null) || last=null; fi
	install -d -m 755 "$STATE"
	jq -n --arg at "$started" --arg end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg result "$result" --arg channel "$CHANNEL" \
		--arg from "$from" --arg to "$to" --arg sha "$sha" --arg reason "$reason" --arg backup "$backup" \
		--argjson digests "$digests" --argjson last "$last" --argjson change "$change" '
		{at: $at, finished: $end, result: $result, channel: $channel, from: $from, to: $to, sha: $sha,
		 reason: $reason, backup: $backup, digests: $digests} as $s
		| $s + {last_change: (if $change then $s else $last end)}' >"$f.new"
	mv -f "$f.new" "$f"
}

# finish RESULT REASON [EXIT]: records the outcome and ends the run. A status that can't be
# written (a full disk) never changes the outcome.
finish() {
	result=$1 reason=$2
	write_status || log "can't write $STATE/status.json"
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
	[ -z "$login_config" ] || rm -rf "$login_config"
}
trap cleanup EXIT

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
# resolve_sha REF: the full SHA of the commit a tag or commit is, once; everything after
# is taken from that commit and no other.
resolve_sha() {
	local s
	if is_sha "$1"; then
		printf '%s' "$1"
		return 0
	fi
	s=$(api "repos/$REPO/commits/$1" | jq -r '.sha // empty') || return 1
	is_sha "$s" || return 1
	printf '%s' "$s"
}
# fetch SHA PATH DEST: a file of the repository at that commit.
fetch() { "$CURL" -fsSL --max-time 60 -o "$3" "$RAW/$REPO/$1/$2"; }

# docker for the registry only (login, manifest, pull), with a Docker config of this script's
# own, so the operator's isn't changed.
dockerp() {
	if [ -n "$login_config" ]; then DOCKER_CONFIG=$login_config "$DOCKER" "$@"; else "$DOCKER" "$@"; fi
}
registry_login() {
	[ -z "$login_config" ] || return 0
	local user
	user=$(envget "$INSTALL_ENV" TASKTO_REGISTRY_USER)
	[ -n "$user" ] && [ -s "$TOKEN_FILE" ] || return 0
	login_config=$(mktemp -d) || return 1
	dockerp login "${prefix%%/*}" -u "$user" --password-stdin <"$TOKEN_FILE" >/dev/null || {
		rm -rf "$login_config"
		login_config=""
		return 1
	}
}
# images_published TAG: all three images are in the registry.
images_published() {
	local n
	registry_login
	for n in $IMAGE_NAMES; do
		dockerp manifest inspect "$prefix$n:$1" >/dev/null 2>&1 || return 1
	done
}
failed_before() { grep -qxF "$1" "$STATE/failed" 2>/dev/null; }

# --- choosing --------------------------------------------------------------------------------

# fetch_releases: every page of the releases until one at or below this install's version,
# so a manual-upgrade release between is never missed; a page that can't be read, or a list
# that doesn't reach this install's version, skips the update (it fails closed).
fetch_releases() {
	local page=1 p n t
	rel='[]'
	while :; do
		p=$(api "repos/$REPO/releases?per_page=$PER_PAGE&page=$page") ||
			finish skipped "GitHub's API didn't answer (page $page of the releases): nothing done"
		jq -e 'type == "array"' <<<"$p" >/dev/null 2>&1 || finish skipped "GitHub's release list is unreadable: nothing done"
		rel=$(printf '%s\n%s\n' "$rel" "$p" | jq -c -s 'add') || finish skipped "GitHub's release list is unreadable: nothing done"
		n=$(jq 'length' <<<"$p")
		[ "$n" -ge "$PER_PAGE" ] || return 0 # the last page
		if is_semver "$from"; then
			while IFS= read -r t; do
				if is_semver "$t" && ! semver_gt "$t" "$from"; then return 0; fi
			done < <(jq -r '.[] | select(.draft == false and .prerelease == false) | .tag_name' <<<"$p")
		fi
		page=$((page + 1))
		[ "$page" -le "$MAX_PAGES" ] || finish skipped "the release list is longer than $MAX_PAGES pages: nothing done"
	done
}

choose_release() {
	local rel rows sorted tag man i pick=-1
	local -a tags=() manual=()
	fetch_releases
	rows=$(jq -r '
		.[] | select(.draft == false and .prerelease == false and (.tag_name | test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")))
		| [.tag_name, ((.body // "") | test("(^|\n)manual-upgrade:[ \t]*true[ \t\r]*(\n|$)"; "i"))] | @tsv' <<<"$rel") ||
		finish skipped "GitHub's release list is unreadable: nothing done"
	# Newest first, by version.
	sorted=$(while IFS=$'\t' read -r tag man; do
		is_semver "$tag" || continue
		printf '%s\t%s\t%s\n' "$(semver_key "$tag")" "$tag" "$man"
	done <<<"$rows" | sort -r | cut -f2,3)
	while IFS=$'\t' read -r tag man; do
		[ -n "$tag" ] || continue
		case $(relation "$tag" "$from") in
		newer) tags+=("$tag") manual+=("$man") ;;
		unknown) finish skipped "can't tell whether $tag is newer than $from" ;;
		*) break ;;
		esac
	done <<<"$sorted"
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
	local runs shas c waiting=0
	runs=$(api "repos/$REPO/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=30") ||
		finish skipped "GitHub's API didn't answer: nothing done"
	shas=$(jq -r '.workflow_runs[] | select(.conclusion == "success" and .head_branch == "main") | .head_sha' <<<"$runs") ||
		finish skipped "GitHub's CI runs are unreadable: nothing done"
	while read -r c; do
		[ -n "$c" ] || continue
		is_sha "$c" || continue
		case $(relation "$c" "$from") in
		newer)
			if failed_before "$c"; then
				log "$c was rolled back on this server before: not tried again"
			elif images_published "$c"; then
				to=$c
				return 0
			else
				log "$c: its images aren't published yet"
				waiting=1
			fi
			;;
		same | older) break ;;
		diverged) log "$c isn't a descendant of $from: passed over" ;;
		*) finish skipped "can't tell whether $c is newer than $from" ;;
		esac
	done <<<"$shas"
	if [ "$waiting" = 1 ]; then finish skipped "the newest green commits' images aren't published yet"; fi
	finish up-to-date "no green commit on main is newer than $from"
}

# --- checking, before anything is touched ----------------------------------------------------

compose() { "$DOCKER" compose "$@"; }
free_kb() { df -Pk "$1" | awk 'NR == 2 {print $4}'; }
check_disk() {
	local vol data_kb droot need_backup need_docker
	vol=$("$DOCKER" volume inspect "$volume" --format '{{.Mountpoint}}') || return 1
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

# The worker's firewall, as runbook step 8 checks it: the unit is on and every rule is in.
firewall_ok() {
	"$SYSTEMCTL" is-active -q taskto-worker-firewall && "$SYSTEMCTL" is-enabled -q taskto-worker-firewall &&
		"$FIREWALL" check >/dev/null 2>&1
}
https_on() { case ",$(envget "$ROOT/.env" COMPOSE_PROFILES)," in *,https,*) return 0 ;; *) return 1 ;; esac; }

# The running server: its data volume (from what it has mounted at /data, never a name
# assumed) and its image, and the images the old version's tags are now, for a rollback.
resolve_running() {
	local s mount n id
	s=$(compose ps -q taskto) && [ -n "$s" ] || { reason="the server isn't running"; return 1; }
	mount=$("$DOCKER" inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}} {{.Name}}{{end}}{{end}}' "$s") ||
		{ reason="can't inspect the server's container"; return 1; }
	[[ $mount =~ ^volume\ ([A-Za-z0-9][A-Za-z0-9_.-]*)$ ]] || { reason="can't tell the server's data volume ('$mount' is mounted at /data)"; return 1; }
	volume=${BASH_REMATCH[1]}
	old_server_image=$("$DOCKER" inspect --format '{{.Image}}' "$s") || { reason="can't tell the server's image"; return 1; }
	for n in $IMAGE_NAMES; do
		if id=$("$DOCKER" image inspect --format '{{.Id}}' "$prefix$n:$from" 2>/dev/null); then
			old_images=$(jq -c --arg k "$n" --arg v "$id" '. + {($k): $v}' <<<"$old_images")
		fi
	done
}

# volume_of_config DIR...: the name Compose gives the data volume, for each (a project
# directory, or none for this one), must be the volume the running server has.
check_volume_name() {
	local cfg name
	cfg=$(compose "$@" config --format json 2>/dev/null) || { reason="docker compose config failed"; return 1; }
	name=$(jq -r '.volumes.data.name // empty' <<<"$cfg")
	[ "$name" = "$volume" ] || { reason="the data volume of the Compose project is '$name', not '$volume', the one the running server has"; return 1; }
}

preflight() {
	local n d img rev ref id out
	stage=$(mktemp -d) || return 1
	mkdir "$stage/deploy" || return 1
	sha=$(resolve_sha "$to") || { reason="can't resolve $to to a commit"; return 1; }
	from_sha=$(resolve_sha "$from") || { reason="can't resolve $from to a commit"; return 1; }
	# The new version's files, from that one commit, as the runbook's steps 6, 8 and 12 install them.
	for n in compose.yaml deploy/Caddyfile deploy/worker-firewall.sh deploy/safety-check.sh; do
		fetch "$sha" "$n" "$stage/$n" || { reason="can't fetch $n of $to"; return 1; }
	done
	# The safety rules the old version is held to after a rollback.
	fetch "$from_sha" deploy/safety-check.sh "$stage/safety-from.sh" || { reason="can't fetch deploy/safety-check.sh of $from, the version to go back to"; return 1; }
	chmod 644 "$stage/compose.yaml" "$stage/deploy/Caddyfile" || return 1
	grep -q TASKTO_IMAGE_PREFIX "$stage/compose.yaml" || { reason="$to's compose.yaml predates published images"; return 1; }
	# The worker's firewall is never changed here: a version that changes it is for a person.
	[ -e "$FIREWALL" ] || { reason="$FIREWALL isn't installed (runbook step 8)"; return 1; }
	if ! cmp -s "$stage/deploy/worker-firewall.sh" "$FIREWALL"; then
		finish skipped "the worker firewall changed in $to: update by hand (runbook step 8), then run taskto-update --to $to"
	fi
	firewall_ok || { reason="the worker's firewall isn't in place (its unit, or its rules): runbook step 8's check says what"; return 1; }
	resolve_running || return 1
	check_volume_name || return 1
	check_disk || { [ -n "$reason" ] || reason="can't read the disk's free space"; return 1; }
	registry_login || { reason="can't log in to the registry"; return 1; }
	"$DOCKER" image inspect "$HELPER" >/dev/null 2>&1 || dockerp pull --quiet "$HELPER" >/dev/null ||
		{ reason="can't pull $HELPER"; return 1; }
	# The new Compose file with this install's own settings.
	set_version "$ROOT/.env" "$to" "" >"$stage/.env" || return 1
	[ ! -e "$ROOT/compose.override.yaml" ] || cat "$ROOT/compose.override.yaml" >"$stage/compose.override.yaml" || return 1
	if ! out=$(compose --project-directory "$stage" config --quiet 2>&1) || [[ $out == *"is not set"* ]]; then
		reason="$to's compose.yaml doesn't work with this install's settings: $(printf '%s' "$out" | head -n 3 | tr '\n' ' ')"
		return 1
	fi
	check_volume_name --project-directory "$stage" || return 1
	# Every image of the enabled services, before the backup: the switch pulls nothing. The three
	# of TaskTo's are the commit's own (their revision label says so) and are known by their digest.
	for n in $IMAGE_NAMES; do
		ref=$prefix$n:$to
		dockerp pull --quiet "$ref" >/dev/null || { reason="can't pull $ref"; return 1; }
		rev=$("$DOCKER" image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$ref") || { reason="can't inspect $ref"; return 1; }
		[ "$rev" = "$sha" ] || { reason="$ref isn't built from $sha (its revision label is '${rev:-none}')"; return 1; }
		d=$("$DOCKER" image inspect --format '{{index .RepoDigests 0}}' "$ref") || { reason="no digest for $ref"; return 1; }
		id=$("$DOCKER" image inspect --format '{{.Id}}' "$ref") || { reason="no image id for $ref"; return 1; }
		digests=$(jq -c --arg k "$n" --arg v "$d" '. + {($k): $v}' <<<"$digests")
		new_images=$(jq -c --arg k "$n" --arg v "$id" '. + {($k): $v}' <<<"$new_images")
	done
	out=$(compose --project-directory "$stage" config --images) || { reason="can't list $to's images"; return 1; }
	while IFS= read -r img; do
		[ -n "$img" ] || continue
		for n in $IMAGE_NAMES; do [ "$img" != "$prefix$n:$to" ] || continue 2; done
		dockerp pull --quiet "$img" >/dev/null || { reason="can't pull $img"; return 1; }
	done <<<"$out"
	cmp -s "$stage/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" || { ! https_on || caddy_changed=1; }
}

# --- the switch, and the way back ------------------------------------------------------------

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
# Only this script's own archives (by their name), the newest KEEP of them by the time in the
# name, and never the one of this run: after a success only.
prune_backups() {
	local f b n=1 re='^before-update-from-[A-Za-z0-9.]+-to-[A-Za-z0-9.]+-([0-9]{8}-[0-9]{6})\.tar\.gz$'
	while IFS=$'\t' read -r _ f; do
		[ "$f" != "$backup" ] || continue
		n=$((n + 1))
		[ "$n" -le "$KEEP" ] || rm -f -- "$f"
	done < <(
		for f in "$BACKUPS"/before-update-from-*-to-*.tar.gz; do
			b=${f##*/}
			[ -f "$f" ] && [ ! -L "$f" ] && [[ $b =~ $re ]] || continue
			printf '%s\t%s\n' "${BASH_REMATCH[1]}" "$f"
		done | sort -r
	)
}

# tx_write PHASE: the record, before the step it names; durable before anything is changed.
tx_write() {
	phase=$1
	install -d -m 755 "$STATE" || return 1
	jq -n --arg from "$from" --arg to "$to" --arg sha "$sha" --arg phase "$phase" --arg backup "$backup" --arg volume "$volume" \
		--arg previous "$STATE/previous" --arg at "$started" --argjson old_images "$old_images" --arg old_server_image "$old_server_image" \
		'{from: $from, to: $to, sha: $sha, phase: $phase, backup: $backup, volume: $volume, previous: $previous,
		  old_server_image: $old_server_image, old_images: $old_images, started: $at}' >"$TX.new" &&
		mv -f "$TX.new" "$TX" && sync
}

put() { # put SRC DEST: replaced in one move
	cp -p "$1" "$2.new" && mv -f "$2.new" "$2"
}
save_previous() {
	local p=$STATE/previous
	install -d -m 755 "$STATE" && rm -rf "$p" && mkdir -p "$p/deploy" &&
		cp -p "$ROOT/compose.yaml" "$ROOT/.env" "$p/" &&
		cp -p "$ROOT/deploy/Caddyfile" "$p/deploy/" &&
		cp -p "$INSTALL_ENV" "$p/install.env"
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
# The images the switch will start are the ones that were checked: the tags still point at them.
images_unchanged() {
	local n id
	for n in $IMAGE_NAMES; do
		id=$("$DOCKER" image inspect --format '{{.Id}}' "$prefix$n:$to") || return 1
		[ "$id" = "$(jq -r --arg k "$n" '.[$k]' <<<"$new_images")" ] || return 1
	done
}

# The proxy in front of the server answers as VERSION too: Caddy over HTTPS (this machine's
# own address for the domain), or in traefik mode the LAN port, as the runbook's step 9 does.
proxy_answers() {
	local mode lan domain h
	mode=$(envget "$INSTALL_ENV" TASKTO_MODE)
	if https_on && [ "$mode" != traefik ]; then
		domain=$(envget "$ROOT/.env" TASKTO_DOMAIN)
		[ -n "$domain" ] || return 0
		h=$("$CURL" -fsS --max-time 5 --resolve "$domain:443:127.0.0.1" "https://$domain/healthz" 2>/dev/null) || return 1
	elif [ "$mode" = traefik ]; then
		lan=$(envget "$INSTALL_ENV" TASKTO_LAN_IP)
		[ -n "$lan" ] || return 0
		h=$("$CURL" -fsS --max-time 5 "http://$lan/healthz" 2>/dev/null) || return 1
	else
		return 0
	fi
	jq -e --arg v "$1" '.version == $v' <<<"$h" >/dev/null 2>&1
}
answers() { # VERSION: the server, and the proxy before it, are up as that version
	last=$("$CURL" -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null) || return 1
	jq -e --arg v "$1" '.status == "ok" and .agent == "ready" and .version == $v' <<<"$last" >/dev/null 2>&1 || return 1
	proxy_answers "$1"
}
# verify VERSION SAFETY-SCRIPT: it answers as that version, with its agent runtime, the
# services run, the worker's firewall is in, and the runbook's step 12 (the same script) holds.
verify() {
	local deadline=$((SECONDS + VERIFY_TIMEOUT)) svc svcs="taskto cli-worker" out last=""
	until answers "$1"; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			reason="the server isn't healthy as $1 after ${VERIFY_TIMEOUT}s (${last:-no answer})"
			return 1
		fi
		sleep "$POLL"
	done
	if https_on; then svcs="$svcs caddy"; fi
	for svc in $svcs; do
		[ "$(compose ps --format '{{.State}}' "$svc")" = running ] || { reason="$svc isn't running"; return 1; }
	done
	firewall_ok || { reason="the worker's firewall isn't in place"; return 1; }
	out=$(TASKTO_ROOT=$ROOT DOCKER=$DOCKER bash "$2" 2>&1) || { reason="the safety rules: $(printf '%s' "$out" | tail -n 1)"; return 1; }
}

up() {
	compose up -d --no-build --pull never --remove-orphans --wait --wait-timeout "$VERIFY_TIMEOUT" || return 1
	# Caddy reads its Caddyfile once, and a file that was replaced is a new file to its mount.
	[ "$caddy_changed" = 0 ] ||
		compose up -d --no-build --pull never --no-deps --force-recreate --wait --wait-timeout "$VERIFY_TIMEOUT" caddy
}

apply_update() {
	step "installing the new files" tx_write switching || return 1
	step "installing the new files" install_new || return 1
	step "checking the images" images_unchanged || return 1
	# From here the new server may run, and write data.
	step "recording the update" tx_write starting || return 1
	step "docker compose up" up || return 1
	verify "$to" "$stage/deploy/safety-check.sh"
}

# Did the new server run (so it may have written data the old one doesn't have)? Only an
# `up` can have started it; if what's running isn't the old image and has started, or we can't
# tell, yes.
new_server_started() {
	[ "$phase" = starting ] || return 1
	local c info img st
	c=$(compose ps -aq taskto 2>/dev/null | head -n 1) || return 0
	[ -n "$c" ] || return 1
	info=$("$DOCKER" inspect --format '{{.Image}} {{.State.StartedAt}}' "$c" 2>/dev/null) || return 0
	read -r img st <<<"$info"
	[ -n "$img" ] || return 0
	[ "$img" != "$old_server_image" ] && [ "$st" != 0001-01-01T00:00:00Z ]
}
retag_old_images() {
	local n id
	for n in $IMAGE_NAMES; do
		id=$(jq -r --arg k "$n" '.[$k] // empty' <<<"$old_images")
		[ -z "$id" ] || "$DOCKER" tag "$id" "$prefix$n:$from" || return 1
	done
}
restore_data() { compose run --rm --no-deps -T taskto restore - <"$backup"; }
# The data the new server wrote is kept, in a volume beside the original, and only then is the
# original emptied for the restore (a volume is never removed: the one the server has stays).
move_data_aside() {
	local aside
	aside=${volume}_rolled_back_$(date +%Y%m%d%H%M%S)
	"$DOCKER" run --rm -v "$volume:/from" -v "$aside:/to" "$HELPER" sh -c \
		'cp -a /from/. /to/ && [ "$(find /from | wc -l)" = "$(find /to | wc -l)" ] && find /from -mindepth 1 -maxdepth 1 -exec rm -rf {} +' </dev/null
}
# The old files, and the old images; the data restored only if the new server may have written
# any (moved aside, never deleted); the old version started and checked.
rollback() {
	local p=$STATE/previous restore=0
	if new_server_started; then restore=1 data_restored=1; fi
	step "putting the old files back" put "$p/compose.yaml" "$ROOT/compose.yaml" || return 1
	step "putting the old files back" put "$p/.env" "$ROOT/.env" || return 1
	step "putting the old files back" put "$p/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" || return 1
	step "putting the old files back" put "$p/install.env" "$INSTALL_ENV" || return 1
	step "putting the old images back" retag_old_images || return 1
	if [ "$restore" = 1 ]; then
		step "docker compose down" compose down || return 1
		step "moving the data aside" move_data_aside || return 1
		step "restoring the backup" restore_data || return 1
	fi
	step "docker compose up" up || return 1
	verify "$from" "$stage/safety-from.sh"
}

# --- one run -----------------------------------------------------------------------------------

[ -r "$ROOT/.env" ] || finish failed "no $ROOT/.env: not an install from deploy/INSTALL.md" 1
cd "$ROOT" # where Compose finds the project
# An update that was stopped half way (a crash, a reboot, a kill) left its record: nothing
# runs, not even --to, until a person has looked, since the files may be on either version.
if [ -e "$TX" ]; then
	from=$(jq -r '.from // "?"' "$TX" 2>/dev/null) || from="?"
	to=$(jq -r '.to // "?"' "$TX" 2>/dev/null) || to="?"
	backup=$(jq -r '.backup // ""' "$TX" 2>/dev/null) || backup=""
	finish interrupted "an update from $from to $to was interrupted in phase $(jq -r '.phase // "?"' "$TX" 2>/dev/null || echo ?) and neither finished nor rolled back: the files may be on either version. The backup is ${backup:-not made}, the old files are in $STATE/previous. Look at it, put it right by hand (docs/install.md#rollback), then remove $TX" 1
fi
from=$(envget "$ROOT/.env" TASKTO_VERSION)
prefix=$(envget "$ROOT/.env" TASKTO_IMAGE_PREFIX)
port=$(envget "$ROOT/.env" TASKTO_PORT)
HEALTH_URL=${TASKTO_UPDATE_HEALTH:-http://127.0.0.1:${port:-8080}/healthz}
case $CHANNEL in release | main | off) ;; *) finish failed "TASKTO_UPDATE_CHANNEL is $CHANNEL, not release, main or off" 1 ;; esac
[[ $REPO =~ ^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$ ]] || finish failed "TASKTO_UPDATE_REPO is $REPO, not OWNER/NAME" 1
[ -n "$prefix" ] || finish failed "TASKTO_IMAGE_PREFIX isn't set: this install doesn't use the published images" 1
[[ $KEEP =~ ^[1-9][0-9]*$ ]] || finish failed "TASKTO_UPDATE_KEEP is $KEEP, not a number of backups" 1
[[ $PER_PAGE =~ ^([1-9][0-9]?|100)$ ]] || finish failed "TASKTO_UPDATE_PER_PAGE is $PER_PAGE, not 1 to 100" 1
if [ -z "$TO" ] && [ -e "$STATE/blocked" ] && [ "$DRY" = 0 ]; then
	# A rollback that failed left the install as it was: nobody should change it again unseen.
	# The status stays what the failed run said.
	result=blocked
	log "blocked: $(cat "$STATE/blocked"); look at it, then remove $STATE/blocked (or run taskto-update --to VERSION)"
	exit 1
fi
if [ -z "$TO" ]; then
	if [ "$CHANNEL" = off ]; then finish skipped "automatic updates are off (TASKTO_UPDATE_CHANNEL=off)"; fi
	if [ -e "$HOLD" ]; then finish skipped "updates are on hold ($HOLD exists)"; fi
fi
# What is on record must be what runs: a server that answers as another version than the .env's
# was changed outside the updater (or by one that didn't finish).
health=$("$CURL" -fsS --max-time 10 "$HEALTH_URL" 2>/dev/null) || finish failed "the server doesn't answer at $HEALTH_URL: not updating a server that isn't up" 1
running=$(jq -r '.version // empty' <<<"$health" 2>/dev/null) || running=""
if [ "$running" != "$from" ]; then
	reason="the server runs '${running:-no version}' but $ROOT/.env says $from: look at it, then remove $STATE/blocked"
	if [ "$DRY" = 0 ]; then
		install -d -m 755 "$STATE" && printf '%s\n' "$reason" >"$STATE/blocked" || true
	fi
	finish blocked "$reason" 1
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
	if ! is_semver "$from" && ! is_sha "$from"; then
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
log "backed up to $backup"
# The record, and the old files, before the first change: if either can't be written, nothing
# has been changed and nothing is rolled back.
if ! tx_write prepared || ! save_previous; then
	rm -f "$TX"
	finish failed "can't write $STATE (the update's record, the old files): nothing was changed" 1
fi

if apply_update; then
	rm -f "$TX" "$STATE/blocked"
	prune_backups || true
	finish updated "" 0
fi
cause=$reason
log "the update failed ($cause): rolling back to $from"
# The way back first; what is written about the failure comes after, and never stops it.
if rollback; then
	rm -f "$TX" "$STATE/blocked"
	printf '%s\n' "$to" >>"$STATE/failed" || true
	if [ "$data_restored" = 1 ]; then
		finish rolled-back "$cause; the server is on $from again, with the data from before the update (the data it had is in a volume beside it)" 1
	fi
	finish rolled-back "$cause; the server is on $from again, its data not touched" 1
fi
printf '%s: %s; the rollback failed: %s\n' "$to" "$cause" "$reason" >"$STATE/blocked" || true
printf '%s\n' "$to" >>"$STATE/failed" || true
rm -f "$TX"
finish rollback-failed "$cause; the rollback failed too ($reason): the install is left as it is, backup $backup" 2
