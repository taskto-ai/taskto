#!/usr/bin/env bash
# Keeps a Compose install (deploy/INSTALL.md) on the latest release, safely. Run on the host,
# as root, by taskto-update.timer; never from the server, which has no Docker socket.
#
#   taskto-update [--dry-run] [--to VERSION]
#   taskto-update --clear-block
#   taskto-update --publish-status
#   taskto-update --requested
#
# It does the safe common case and leaves the rest to a person. One run: choose a version,
# resolve it to one commit and take every file and image from that commit, check it all before
# touching anything (the new Compose settings against the safety rules too, without starting
# anything), back up, record a transaction, switch and check the new server. It never downgrades
# and never updates to the version it is on. Anything it can't do with certainty it refuses,
# reports in its status, and leaves the server as it was: a release with `manual-upgrade: true`,
# a changed worker firewall, an update that was interrupted, a server whose version isn't the
# one on record. docs/install.md#automatic-updates says what operators set; docs/releasing.md
# says what a release is.
#
# When a run fails, what it does depends on whether the new server has started (a new server
# may have run migrations, which an older one can't read):
#   before it started   the old files, .env and images are put back, the old version is started
#                       and checked, the release is recorded as failed, and it is reported;
#   after it started    the stack's restart policies are turned off (`docker update
#                       --restart=no`, so a reboot can't start it again) and it is stopped
#                       (`docker compose stop`), and that is checked; the backup is kept, the
#                       `blocked` marker and the transaction record are written, and it is
#                       reported loudly with the backup to restore (`stopped`; `stop-failed` when
#                       the stop couldn't be confirmed). The updater never restores data,
#                       empties or copies a volume, or runs `taskto restore`: a person does, with
#                       deploy/INSTALL.md's Rollback section, and then `taskto-update
#                       --clear-block`.
#
# Compose is always given its inputs: --project-name, --env-file and one -f list (compose.yaml
# and compose.override.yaml when it exists), the same for the check of the new settings, `create`,
# `start`, `stop` and the way back. An install with any other Compose file in its directory, or
# COMPOSE_FILE set, is refused. The containers are made first (`create`), each is checked to have
# the image that was verified, and only then started.
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
# /etc/taskto/update-notify-url (a file, root only, one URL: it may hold a secret) is where
# every outcome but up-to-date, skipped and a dry run is POSTed as JSON, best effort.
# --to VERSION installs that version, a vMAJOR.MINOR.PATCH tag or a full commit SHA, even on
# hold or past a manual-upgrade release; --dry-run chooses and checks, and changes nothing
# (it does pull the images, and logs in to a private registry, to check them);
# --clear-block, once a person has put an install right, removes the stop record, if the
# server is healthy and is the version in .env, the worker's firewall rules are in and the safety
# rules hold in the running containers.
#
# What admins see (Settings → About): after every run, and on each night a stop is still waiting,
# it writes a copy of its status, made for the server, to $PUBLIC/status.json ($STATE/public by
# default), which compose.yaml mounts read-only into the server (write_public below): codes and
# versions only, no path, host or volume name (those stay in the journal and $STATE/status.json).
# The server reads nothing else of $STATE and runs nothing: deploy/safety-check.sh fails an
# install where it could. --publish-status writes that copy from what is there, and nothing
# else: it is what the first run of a newer updater needs to show admins at once, instead of at
# the next timer (INSTALL.md step 16).
#
# --requested is what an operator's "Update now" (Settings → About) runs, started by
# taskto-update-request.path when the server has put a request in $REQUEST. The request is no
# more than its existence: it names no version, repository or flag, and the server is never
# trusted with more (the directory is the one place of the updater's it can write, so what is in
# it is hostile input: a link, a directory or a large file is removed and ignored, and its
# content is only ever matched against a fixed pattern). The run is the nightly run, on the
# configured channel, and every gate of it holds (a hold, manual-upgrade, a firewall change, a
# stopped or interrupted update, the channel off); the request is taken and removed first, under
# the lock, so it can't loop, and a second one within $REQUEST_MIN seconds of the last is ignored.
# The status records that the run was requested, and its outcome (Settings → About, INSTALL.md
# step 16).
#
# Every outside command and place is a variable (DOCKER, CURL, FLOCK, SYSTEMCTL, the
# TASKTO_UPDATE_* paths and base URLs below), so cmd/taskto/update_test.go runs it against fakes.
#
# Exit status: 0 updated, up to date, skipped, dry run or cleared; 1 failed, rolled back (the
# server is on its old version), or refusing to run (blocked, interrupted); 2 stopped (the
# new server started and failed; or stop-failed, when it couldn't be confirmed stopped), the way
# back failed too, or a release that was rolled back couldn't be recorded as failed: nothing runs
# again until an operator looks (the "blocked" file and the transaction record in the state
# directory).
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
# The one directory the server may read (compose.yaml mounts it, read-only): the status for admins.
PUBLIC=""  # set below, once `setting` is defined: the environment's, else install.env's, else $STATE/public
# The one directory the server may write: an operator's "Update now" puts a request in it (the file
# $REQUEST_FILE). The server's uid owns it; it is on the same filesystem as $STATE, so taking the
# request is a rename, never a read of what the server controls.
REQUEST=""  # set below, with PUBLIC
REQUEST_FILE=update-now
REQUEST_OWNER=${TASKTO_UPDATE_REQUEST_OWNER:-65532}
REQUEST_MIN=${TASKTO_UPDATE_REQUEST_MIN_INTERVAL:-600}
REQUEST_MAX_BYTES=512
REQUEST_UNIT=${TASKTO_UPDATE_REQUEST_UNIT:-taskto-update-request.path}
# How long a requested run waits for another run (the nightly one) to end, before it gives up.
LOCK_WAIT=${TASKTO_UPDATE_LOCK_WAIT:-7200}
# Who must own it and every directory above it, and where the check of those stops (the
# directory above which nothing is checked): root and / on a host, other values for the tests.
OWNER=${TASKTO_UPDATE_OWNER:-0}
TRUSTED_ROOT=${TASKTO_UPDATE_TRUSTED_ROOT:-/}
BACKUPS=${TASKTO_UPDATE_BACKUPS:-/var/backups/taskto}
LOCK=${TASKTO_UPDATE_LOCK:-/run/taskto-update.lock}
FIREWALL=${TASKTO_UPDATE_FIREWALL:-/usr/local/sbin/taskto-worker-firewall}
TOKEN_FILE=${TASKTO_UPDATE_TOKEN_FILE:-/etc/taskto/registry-token}
NOTIFY_FILE=${TASKTO_UPDATE_NOTIFY_FILE:-/etc/taskto/update-notify-url}
API=${TASKTO_UPDATE_API:-https://api.github.com}
RAW=${TASKTO_UPDATE_RAW:-https://raw.githubusercontent.com}
VERIFY_TIMEOUT=${TASKTO_UPDATE_VERIFY_TIMEOUT:-300}
POLL=${TASKTO_UPDATE_POLL:-2}
IMAGES_MB=${TASKTO_UPDATE_IMAGES_MB:-4000}
PER_PAGE=${TASKTO_UPDATE_PER_PAGE:-100}
MAX_PAGES=50
TX=$STATE/transaction
# The old version's images, kept under these tags (one per image) from the moment an update
# starts, so a moved version tag, or a plain `docker image prune` (dangling images), can't take
# them. A prune of all unused images (with -a) still can: an image no container uses goes
# whatever its tags.
KEEP_TAG=taskto-update-rollback
IMAGE_NAMES="taskto taskto-cli-worker taskto-backup"
# The one grammar of a release version, and its longest tag, here and in .github/workflows/release.yml.
SEMVER='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
MAX_TAG=100
# Compose's project name, the one `name:` of compose.yaml.
PROJECT=taskto

log() { printf 'taskto-update: %s\n' "$*"; }

# envget FILE KEY: KEY's value in a KEY=value file, without single quotes.
envget() { { sed -n "s/^$2=//p" "$1" 2>/dev/null || true; } | tail -n 1 | sed "s/^'//; s/'\$//"; }
# setting NAME DEFAULT: the environment's NAME, else install.env's, else DEFAULT.
setting() { local v=${!1:-}; [ -n "$v" ] || v=$(envget "$INSTALL_ENV" "$1"); printf '%s' "${v:-$2}"; }
# Where the status for the server goes: in install.env, so a run by hand, the timer's and the
# runbook's all agree, with TASKTO_UPDATE_STATUS_SOURCE in .env saying the same to Compose.
PUBLIC=$(setting TASKTO_UPDATE_PUBLIC "$STATE/public")
REQUEST=$(setting TASKTO_UPDATE_REQUEST "$STATE/request")

usage() {
	echo "usage: taskto-update [--dry-run] [--to vMAJOR.MINOR.PATCH|FULL-SHA] | --clear-block | --publish-status | --requested" >&2
	exit "${1:-2}"
}

# --- versions (no arithmetic on a version's numbers: they are strings) -------------------------

is_semver() { [ ${#1} -le "$MAX_TAG" ] && [[ $1 =~ $SEMVER ]]; }
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

DRY=0 TO="" CLEAR=0 PUBLISH=0 REQUESTED=0 RUN_ON="" QUIET=""
while [ $# -gt 0 ]; do
	case $1 in
	--dry-run) DRY=1 ;;
	--clear-block) CLEAR=1 ;;
	--publish-status) PUBLISH=1 ;;
	--requested) REQUESTED=1 ;;
	--to)
		[ $# -ge 2 ] && [ -n "$2" ] || usage
		TO=$2
		shift
		;;
	-h | --help) usage 0 ;;
	*) usage ;;
	esac
	shift
done
if [ "$CLEAR" = 1 ] && { [ "$DRY" = 1 ] || [ -n "$TO" ]; }; then usage; fi
if [ "$PUBLISH" = 1 ] && { [ "$CLEAR" = 1 ] || [ "$DRY" = 1 ] || [ -n "$TO" ]; }; then usage; fi
if [ "$REQUESTED" = 1 ] && { [ "$CLEAR" = 1 ] || [ "$DRY" = 1 ] || [ -n "$TO" ] || [ "$PUBLISH" = 1 ]; }; then usage; fi
if [ -n "$TO" ] && ! is_semver "$TO" && ! is_sha "$TO"; then
	echo "taskto-update: --to takes a vMAJOR.MINOR.PATCH tag or a full commit SHA" >&2
	exit 2
fi

# Two runs never overlap: the second leaves the first's status alone.
exec 9>"$LOCK"
if [ "$REQUESTED" = 1 ]; then
	# A request that comes while the nightly run is going waits for it (the path unit would start
	# this again at once, and again, while the request is there), then is looked at.
	"$FLOCK" -w "$LOCK_WAIT" 9 || {
		log "another update was still running after ${LOCK_WAIT}s: the request stays for the next run"
		exit 0
	}
else
	"$FLOCK" -n 9 || {
		log "another update is already running: nothing to do"
		exit 0
	}
fi

CHANNEL=$(setting TASKTO_UPDATE_CHANNEL release)
REPO=$(setting TASKTO_UPDATE_REPO taskto-ai/taskto)
KEEP=$(setting TASKTO_UPDATE_KEEP 3)
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
result="" from="" to="" sha="" reason="" backup="" digests='{}' needs=""
stage="" login_config=""
# The transaction: what the run changes, kept in $TX from before the first change until a
# terminal outcome, so a run that is stopped half way is found, not mistaken for done. After a
# failure that a person has to look at, it stays, in the phase `stopped`, `stop-failed` or
# `rollback-failed`, until `taskto-update --clear-block`.
phase="" volume="" subpath="" data_mount="" data_key="" old_server_image="" old_images='{}' old_service_images='{}' new_images='{}' new_ids='{}' caddy_changed=0 from_sha="" cause=""

# --- the status: $STATE/status.json, and a line in the journal ---------------------------

write_status() {
	[ "$DRY" = 0 ] || return 0
	local f=$STATE/status.json last=null change=false
	case $result in updated | rolled-back | rollback-failed | stopped | stop-failed | interrupted) change=true ;; esac
	if [ "$change" = false ] && [ -s "$f" ]; then last=$(jq -c '.last_change // null' "$f" 2>/dev/null) || last=null; fi
	install -d -m 755 "$STATE"
	jq -n --arg at "$started" --arg end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg result "$result" --arg channel "$CHANNEL" \
		--arg from "$from" --arg to "$to" --arg sha "$sha" --arg reason "$reason" --arg backup "$backup" \
		--arg code "$(reason_code "$result" "$reason")" \
		--argjson digests "$digests" --argjson last "$last" --argjson change "$change" --argjson requested "$([ "$REQUESTED" = 1 ] && echo true || echo false)" '
		({at: $at, finished: $end, result: $result, channel: $channel, from: $from, to: $to, sha: $sha,
		  reason: $reason, code: $code, backup: $backup, digests: $digests}
		 + (if $requested then {requested: true} else {} end)) as $s
		| $s + {last_change: (if $change then $s else $last end)}' >"$f.new" || {
		# A write that failed half way (a full disk) leaves the last status as it was.
		rm -f "$f.new"
		return 1
	}
	mv -f "$f.new" "$f"
	write_public || log "can't write $PUBLIC/status.json"
}

# reason_code RESULT REASON: the code that stands for a run's reason, one of a closed list the
# server and the web app know (internal/updatestatus, `settings.updates.reasons`), or nothing for a
# run that needs none. The reason's own words name paths, hosts and volumes: they stay in the
# journal and $STATE/status.json, and never reach the server. A reason that matches nothing
# here is `other`.
reason_code() {
	case $1 in
	updated | up-to-date | cleared | dry-run | published | "") return 0 ;;
	stopped | stop-failed | rollback-failed | interrupted | rolled-back)
		printf '%s' "$1"
		return 0
		;;
	esac
	case $2 in
	"a requested update ran"*) echo rate-limited ;;
	"the request wasn't"*) echo bad-request ;;
	"automatic updates are off"*) echo off ;;
	"updates are on hold"*) echo hold ;;
	*"needs manual steps"* | *"worker firewall changed"*) echo manual-upgrade ;;
	*"GitHub's"* | *"release list is longer"*) echo github ;;
	*"images aren't published"*) echo not-published ;;
	*"is older than"* | *"isn't a descendant"* | *"already on"* | *"can't tell whether"* | *"neither a release nor a commit"*) echo not-newer ;;
	*"the server runs"*) echo version-mismatch ;;
	*"doesn't answer at"*) echo server-down ;;
	*"break a safety rule"*) echo unsafe-settings ;;
	*"not enough free disk"*) echo disk ;;
	*"can't pull"* | *"log in to the registry"*) echo registry ;;
	*"the backup failed"*) echo backup ;;
	TASKTO_*) echo bad-setting ;;
	*"firewall"*) echo firewall ;;
	*"Compose"*) echo settings ;;
	*) if [ "$1" = blocked ]; then echo blocked; else echo other; fi ;;
	esac
}

# blocked_code: why the install waits for a person, as a code (reason_code's list): from the
# transaction's phase while there is one, else from this run's refusal, else from the run that
# left the marker.
blocked_code() {
	local ph=""
	if [ -e "$TX" ]; then ph=$(jq -r '.phase // ""' "$TX" 2>/dev/null) || ph=""; fi
	case $ph in
	stopped | stop-failed | rollback-failed)
		printf '%s' "$ph"
		return 0
		;;
	rolled-back)
		echo unrecorded-failure
		return 0
		;;
	"") ;;
	*)
		echo interrupted
		return 0
		;;
	esac
	case $result:$reason in
	blocked:*"the server runs"*)
		echo version-mismatch
		return 0
		;;
	esac
	case $(jq -r '.result // ""' "$STATE/status.json" 2>/dev/null) in
	stopped) echo stopped ;;
	stop-failed) echo stop-failed ;;
	rollback-failed) echo rollback-failed ;;
	interrupted) echo interrupted ;;
	*) echo blocked ;;
	esac
}

# trusted_path PATH: PATH, and every directory above it up to $TRUSTED_ROOT, is a directory
# owned by $OWNER that its group and others can't write; a symbolic link on the way belongs to
# $OWNER too, and the place it leads to is checked the same way. So nobody but the updater can
# put, replace or swap anything in it, however the server mounts it.
trusted_path() {
	local p=$1 q=/ part info t u m r
	local -a parts prefixes=(/)
	case /$p/ in */../* | */./*) return 1 ;; esac
	IFS=/ read -ra parts <<<"${p#/}"
	for part in "${parts[@]}"; do
		[ -n "$part" ] || continue
		q=${q%/}/$part
		prefixes+=("$q")
	done
	for q in "${prefixes[@]}"; do
		[[ $q == "$TRUSTED_ROOT" || $q == "${TRUSTED_ROOT%/}"/* ]] || continue
		info=$(stat -c '%F|%u|%a' -- "$q") || return 1
		IFS='|' read -r t u m <<<"$info"
		[ "$u" = "$OWNER" ] || return 1
		if [ "$t" = "symbolic link" ]; then
			r=$(realpath -e -- "$q") || return 1
			trusted_path "$r" || return 1
		else
			[ "$t" = directory ] && [ $((8#$m & 8#022)) -eq 0 ] || return 1
		fi
	done
}

# request_dir_ok: the request directory is in place as the updater needs it: an absolute path of
# plain characters, a real directory (never a link) of the server's uid (a directory Docker made
# itself, as root's, is one the server can't write: not in place), on the same
# filesystem as $STATE (so taking a request is a rename), and in a directory above it that is root's
# and nobody else's to write (trusted_path), so the server can't swap it for another place.
# The server can write inside it, and that is all it can do.
request_dir_ok() {
	local info t u
	[[ $REQUEST =~ ^/[A-Za-z0-9_./-]+$ ]] || return 1
	case $REQUEST/ in */../* | */./* | *//*) return 1 ;; esac
	{ [ ! -L "$REQUEST" ] && [ -d "$REQUEST" ]; } || return 1
	info=$(stat -c '%F|%u' -- "$REQUEST") || return 1
	IFS='|' read -r t u <<<"$info"
	[ "$t" = directory ] && [ "$u" = "$REQUEST_OWNER" ] || return 1
	[ "$(stat -c %d -- "$REQUEST")" = "$(stat -c %d -- "$STATE")" ] || return 1
	trusted_path "$(dirname "$REQUEST")"
}
# request_supported: this host can take an operator's request now: the directory is in place and
# the path unit that watches it is on. What the page says, in the status.
request_supported() {
	request_dir_ok && "$SYSTEMCTL" is-active -q "$REQUEST_UNIT" 2>/dev/null
}
# record_request_outcome: what became of this run's request, in $STATE/request-outcome.json, for
# the page to find: a run that ended, or a request turned away before one began.
record_request_outcome() {
	local f=$STATE/request-outcome.json tmp
	install -d -m 755 "$STATE" || return 1
	tmp=$(mktemp "$STATE/.request-outcome.XXXXXXXX") || return 1
	if ! jq -n --arg at "$started" --arg end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg result "$result" --arg code "$(reason_code "$result" "$reason")" \
		'{at: $at, finished: $end, result: $result, code: $code}' >"$tmp" || ! chmod 644 "$tmp" || ! mv -f -T "$tmp" "$f"; then
		rm -f "$tmp"
		return 1
	fi
}

# take_request: for --requested, under the lock: look at the request, remove it, and decide whether
# this run goes on. It returns when the run goes on; it exits when there is nothing to do or the
# request is turned away (neither changes the install or the last run's status). The request is
# renamed out of the server's directory into $STATE first and looked at there, so what the server
# can change in the directory (a link, a fifo, a huge file, a swap after a check) is never read or
# followed; only a regular file of at most $REQUEST_MAX_BYTES is a request, and its whole content
# must be one fixed line (a time and a nonce, which only tell requests apart), else it is ignored. The
# request is gone before anything else is done, so a failed or refused run can't be started again
# by it.
take_request() {
	local f=$REQUEST/$REQUEST_FILE taken=$STATE/request.taken t size content line nonce="" last_at=0 last_nonce="" now
	if ! request_dir_ok; then
		log "the request directory $REQUEST isn't in place as the updater needs it (a real directory of uid $REQUEST_OWNER, the server's, on the same filesystem as $STATE, in a directory only root writes): nothing is read or removed there (deploy/INSTALL.md step 16)"
		QUIET=1
		exit 0
	fi
	if [ ! -e "$f" ] && [ ! -L "$f" ]; then
		log "no request waits: nothing to do"
		QUIET=1
		exit 0
	fi
	install -d -m 755 "$STATE"
	rm -rf -- "$taken"
	if ! mv -T -- "$f" "$taken" 2>/dev/null; then
		rm -rf -- "$f"
		if [ -e "$f" ] || [ -L "$f" ]; then
			log "the request can't be removed: it would start this again and again; look at $REQUEST"
			exit 1
		fi
	fi
	t=$(stat -c %F -- "$taken" 2>/dev/null) || t=""
	size=$(stat -c %s -- "$taken" 2>/dev/null) || size=0
	case $t in
	"regular file" | "regular empty file") ;;
	*)
		rm -rf -- "$taken"
		result=ignored reason="the request wasn't a regular file (${t:-gone}): removed, nothing done"
		write_public || true
		log "ignored: $reason"
		exit 0
		;;
	esac
	if [ "$size" -gt "$REQUEST_MAX_BYTES" ]; then
		rm -f -- "$taken"
		result=ignored reason="the request wasn't small ($size bytes, at most $REQUEST_MAX_BYTES): removed, nothing done"
		write_public || true
		log "ignored: $reason"
		exit 0
	fi
	# At most one byte more than the limit is read, whatever the file has grown to meanwhile; and
	# the whole of it must be the one fixed line the server writes, a time and a nonce. Anything
	# else is not from the server's code: it is removed and ignored, never used.
	# Bash strips NUL bytes from command substitutions. Replace each with a byte the
	# grammar rejects before capture, so malformed raw input cannot become a valid line.
	content=$(head -c $((REQUEST_MAX_BYTES + 1)) -- "$taken" | tr '\000' '?'; echo x) || content=x
	content=${content%x}
	rm -f -- "$taken"
	line=${content%$'\n'}
	if [ "${#content}" -gt "$REQUEST_MAX_BYTES" ] || [ "$line" = "$content" ] ||
		! [[ $line =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z\ ([0-9a-f]{32})$ ]]; then
		result=ignored reason="the request wasn't in the fixed format (a time and a nonce, one line): removed, nothing done"
		write_public || true
		log "ignored: $reason"
		exit 0
	fi
	nonce=${BASH_REMATCH[1]}
	# One requested run per $REQUEST_MIN seconds, and never the same request twice.
	now=$(date +%s)
	if [ -s "$STATE/request-last-run" ]; then
		read -r last_at last_nonce <"$STATE/request-last-run" || true
		[[ $last_at =~ ^[0-9]+$ ]] || last_at=0
	fi
	if { [ "$REQUEST_MIN" -gt 0 ] && [ $((now - last_at)) -lt "$REQUEST_MIN" ]; } || { [ -n "$nonce" ] && [ "$nonce" = "$last_nonce" ]; }; then
		result=ignored reason="a requested update ran less than ${REQUEST_MIN}s ago (or this request was handled): nothing done"
		write_public || true
		log "ignored: $reason"
		exit 0
	fi
	# A request must not run without its rate-limit record: a full disk or a failed rename
	# must never turn repeated requests into unrestricted update attempts.
	if ! printf '%s %s\n' "$now" "$nonce" >"$STATE/request-last-run.new" ||
		! mv -f -T "$STATE/request-last-run.new" "$STATE/request-last-run"; then
		rm -f -- "$STATE/request-last-run.new" 2>/dev/null || true
		result=failed reason="can't record the requested run: nothing done"
		write_public || true
		log "failed: $reason"
		exit 1
	fi
	log "an operator requested an update: taken, and removed"
	# From here the page shows the run as going on, until it ends.
	RUN_ON=1
	write_public || log "can't write $PUBLIC/status.json"
}

# write_public: the part of the status the server shows its admins (Settings → About), in $PUBLIC,
# the one directory compose.yaml mounts, read-only, into the server. It is a copy made for that,
# of codes and versions only: no digests, no path, host or volume name, nothing from the
# transaction, the old files or the backups, and never a setting that may hold a secret (the
# notification address). Every text of it is from a closed list (reason_code), every version
# checked to be one, and the backup only a yes or no. It says what the server can't know: the
# last run and the last change, the version on record, whether a hold is on, and whether the
# install waits for a person (`needs_person`: kind `blocked`, cleared with `taskto-update
# --clear-block`, or `manual-upgrade`, done by hand, then `taskto-update --to`).
#
# The directory is the updater's alone: it must belong to $OWNER, with every directory above it,
# and no one else may write any of them (trusted_path), so the server can't plant a link or a
# file there even if something mounts it writable. The file is written to a temporary file made
# in it for this (mktemp: a name that is new, created exclusively) and moved into place, so the
# server sees the old file or the new one, never part of one, and no name is one to guess;
# `checked` is when, so a status that stops being written shows as stale. A failure to write it
# never changes the outcome.
write_public() {
	[ "$DRY" = 0 ] || return 0
	local f=$PUBLIC/status.json tmp priv=null kind="" code="" hold=false reqsup=false reqlast=null running=false
	# A requested run's outcome is kept apart from the last run's, so one that was turned away
	# before it ran is answered too.
	if [ "$REQUESTED" = 1 ] && [ -n "$result" ]; then record_request_outcome || log "can't write $STATE/request-outcome.json"; fi
	if [ -s "$STATE/request-outcome.json" ]; then reqlast=$(jq -c '{at, finished, result, code: (.code // "")}' "$STATE/request-outcome.json" 2>/dev/null) || reqlast=null; fi
	if request_supported; then reqsup=true; fi
	if [ "$RUN_ON" = 1 ] && [ -z "$result" ]; then running=true; fi
	[[ $PUBLIC == /* ]] || { log "$PUBLIC isn't an absolute path"; return 1; }
	install -d -m 755 "$PUBLIC" || return 1
	trusted_path "$PUBLIC" || {
		log "$PUBLIC, or a directory above it, isn't owned by uid $OWNER or can be written by others: nothing is published there"
		return 1
	}
	if [ -s "$STATE/status.json" ]; then priv=$(jq -c '.' "$STATE/status.json" 2>/dev/null) || priv=null; fi
	if [ -e "$STATE/blocked" ] || [ -e "$TX" ]; then
		kind=blocked code=$(blocked_code)
	elif [ -n "$needs" ]; then
		kind=$needs code=manual-upgrade
	else
		# A run that couldn't tell (a hold, the channel off, a failed check, an unreadable release
		# list) can't say the steps were done: what was pending stays, until a run looks and finds
		# nothing newer, or an update is made.
		case $result in updated | cleared | up-to-date) ;; *)
			if [ -s "$f" ] && [ "$(jq -r '.needs_person.kind // ""' "$f" 2>/dev/null)" = manual-upgrade ]; then
				kind=manual-upgrade code=manual-upgrade
			fi
			;;
		esac
	fi
	[ ! -e "$HOLD" ] || hold=true
	tmp=$(mktemp "$PUBLIC/.status.XXXXXXXX") || return 1
	jq -n --argjson priv "$priv" --arg checked "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg channel "$CHANNEL" --argjson hold "$hold" \
		--arg recorded "$(envget "$ROOT/.env" TASKTO_VERSION)" --arg kind "$kind" --arg code "$code" \
		--argjson reqsup "$reqsup" --argjson reqlast "$reqlast" --argjson running "$running" --arg started "$started" --arg to "$to" \
		--argjson requested "$([ "$REQUESTED" = 1 ] && echo true || echo false)" '
		def ver: if type == "string" and length <= 100 and test("^(v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)|[0-9a-f]{40})$") then . else null end;
		def run: if . == null then null else {at, finished, result, from: (.from | ver), to: (.to | ver), code: (.code // null), backed_up: ((.backup // "") != "")}
			+ (if .requested == true then {requested: true} else {} end) end;
		{version: 1, checked: $checked, channel: $channel, hold: $hold, recorded_version: ($recorded | ver),
		 last_run: ($priv | run), last_change: ($priv.last_change // null | run),
		 needs_person: (if $kind == "" then null else {kind: $kind, code: $code} end),
		 running: (if $running then {since: $started, to: ($to | ver), requested: $requested} else null end),
		 requests: {supported: $reqsup, last: $reqlast}}' >"$tmp" || {
		rm -f "$tmp"
		return 1
	}
	if ! chmod 644 "$tmp" || ! mv -f -T "$tmp" "$f"; then
		rm -f "$tmp"
		return 1
	fi
}

# notify: the outcome, as JSON, POSTed to the URL in $NOTIFY_FILE, if there is one: any outcome
# but up-to-date, skipped, a dry run and a clearing. Best effort, and it never changes the
# outcome. The URL may hold a secret: it is read from the file, handed to curl in a config on
# its standard input (never in its command line, where `ps` shows it), and never logged, and
# curl's own messages (which name the host) are dropped. The body has `result`, `from`, `to`,
# `reason` and `backup`, and `text` (those in a sentence, for chat webhooks that want one).
notify() {
	[ "$DRY" = 0 ] && [ "$CLEAR" = 0 ] || return 0
	case $result in updated | failed | rolled-back | rollback-failed | stopped | stop-failed | blocked | interrupted) ;; *) return 0 ;; esac
	[ -s "$NOTIFY_FILE" ] || return 0
	local url body
	url=$(head -n 1 "$NOTIFY_FILE" | tr -d '\r') || return 0
	[[ $url =~ ^https?://[^[:space:]\"\\]+$ ]] || {
		log "$NOTIFY_FILE doesn't hold one http(s) address: no notification"
		return 0
	}
	body=$(mktemp) || return 0
	if jq -cn --arg result "$result" --arg from "$from" --arg to "$to" --arg reason "$reason" --arg backup "$backup" '
		{result: $result, from: $from, to: $to, reason: $reason, backup: $backup,
		 text: ("taskto-update: " + $result + (if $from != "" then ", " + $from else "" end) + (if $to != "" then " to " + $to else "" end)
		        + (if $reason != "" then ": " + $reason else "" end) + (if $backup != "" then " (backup: " + $backup + ")" else "" end))}' >"$body" &&
		printf 'url = "%s"\nrequest = "POST"\nheader = "Content-Type: application/json"\ndata-binary = "@%s"\nconnect-timeout = 5\nmax-time = 10\nfail\nsilent\noutput = "/dev/null"\n' "$url" "$body" |
		"$CURL" -q -K - >/dev/null 2>&1; then
		log "notified"
	else
		log "the notification wasn't delivered (it never changes the outcome)"
	fi
	rm -f "$body"
	return 0
}

# finish RESULT REASON [EXIT]: records the outcome and ends the run. A status that can't be
# written (a full disk), or a notification that can't be sent, never changes the outcome.
finish() {
	result=$1 reason=$2
	write_status || log "can't write $STATE/status.json"
	log "result=$result from=${from:-?} to=${to:-?}${reason:+ reason=\"$reason\"}"
	notify || true
	# An explicit --to that wasn't done is a failure to whoever ran it.
	local code=${3:-0}
	if [ "$code" = 0 ] && [ -n "$TO" ] && [ "$DRY" = 0 ]; then
		case $result in skipped | up-to-date) code=1 ;; esac
	fi
	exit "$code"
}

cleanup() {
	local rc=$?
	# A quiet exit (a request that isn't there, a directory not in place) is nothing to report.
	if [ -z "$result" ] && [ -z "$QUIET" ]; then
		result=failed reason="unexpected error (exit $rc)"
		write_status || true
		log "result=failed reason=\"$reason\""
		notify || true
	fi
	[ -z "$stage" ] || rm -rf "$stage"
	[ -z "$login_config" ] || rm -rf "$login_config"
}
trap cleanup EXIT

api() { "$CURL" -q -fsS --max-time 30 -H 'Accept: application/vnd.github+json' "$API/$1"; }
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
fetch() { "$CURL" -q -fsSL --max-time 60 -o "$3" "$RAW/$REPO/$1/$2"; }

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

# fetch_releases: every page of the releases (GitHub lists them by date, not by version, so no
# page can be left unread: a manual-upgrade release can be on any); a page that can't be read
# skips the update (it fails closed).
fetch_releases() {
	local page=1 p n
	rel='[]'
	while :; do
		p=$(api "repos/$REPO/releases?per_page=$PER_PAGE&page=$page") ||
			finish skipped "GitHub's API didn't answer (page $page of the releases): nothing done"
		jq -e 'type == "array"' <<<"$p" >/dev/null 2>&1 || finish skipped "GitHub's release list is unreadable: nothing done"
		rel=$(printf '%s\n%s\n' "$rel" "$p" | jq -c -s 'add') || finish skipped "GitHub's release list is unreadable: nothing done"
		n=$(jq 'length' <<<"$p")
		[ "$n" -ge "$PER_PAGE" ] || return 0 # the last page
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
			log "${tags[i]} failed on this server before: not tried again"
		elif images_published "${tags[i]}"; then
			pick=$i
			break
		else
			log "${tags[i]}: its images aren't published yet"
		fi
	done
	[ "$pick" -ge 0 ] || finish skipped "no newer release is available: ${tags[0]}'s images aren't published (or it failed here before)"
	# The release itself and every one between it and this install.
	for ((i = pick; i < ${#tags[@]}; i++)); do
		if [ "${manual[i]}" = true ]; then
			needs="manual-upgrade"
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
				log "$c failed on this server before: not tried again"
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

# inputs DIR: A is the explicit Compose inputs of the project in DIR: its name, its .env, and its
# compose.yaml with compose.override.yaml when there is one, no other file.
inputs() {
	A=(--project-directory "$1" --project-name "$PROJECT" --env-file "$1/.env" -f "$1/compose.yaml")
	[ ! -e "$1/compose.override.yaml" ] || A+=(-f "$1/compose.override.yaml")
}
LIVE=() STAGE=() A=()
compose() { "$DOCKER" compose "${LIVE[@]}" "$@"; }  # the install's project
scompose() { "$DOCKER" compose "${STAGE[@]}" "$@"; } # the staged new settings
# profile_args DIR: --profile for each one DIR/.env enables and for the backup services' (which
# the nightly backup, or a by-hand backup, runs), one word per line, so a service of any of them
# is rendered and checked.
profile_args() {
	local p
	{ envget "$1/.env" COMPOSE_PROFILES | tr ',' '\n'; printf '%s\n' backup backup-schedule; } | sort -u | while IFS= read -r p; do
		[ -z "$p" ] || printf '%s\n%s\n' --profile "$p"
	done
}
# layout_ok: nothing in the install's directory can change which Compose files Compose reads.
layout_ok() {
	local f
	for f in compose.yml compose.override.yml docker-compose.yml docker-compose.yaml docker-compose.override.yml docker-compose.override.yaml; do
		if [ -e "$ROOT/$f" ] || [ -L "$ROOT/$f" ]; then
			reason="$ROOT/$f is a Compose file the updater doesn't render (it uses compose.yaml and compose.override.yaml only): update by hand (deploy/INSTALL.md#updates)"
			return 1
		fi
	done
	if [ -n "${COMPOSE_FILE:-}" ] || [ -n "$(envget "$ROOT/.env" COMPOSE_FILE)" ]; then
		reason="COMPOSE_FILE is set: the updater renders compose.yaml and compose.override.yaml only; update by hand (deploy/INSTALL.md#updates)"
		return 1
	fi
	[ -f "$ROOT/compose.yaml" ] || { reason="no $ROOT/compose.yaml"; return 1; }
}
free_kb() { df -Pk "$1" | awk 'NR == 2 {print $4}'; }
# nearest_dir PATH: PATH, or the closest directory above it that exists (a dry run makes nothing).
nearest_dir() {
	local p=$1
	while [ ! -d "$p" ] && [ "$p" != / ] && [ "$p" != . ]; do p=$(dirname "$p"); done
	printf '%s' "$p"
}
check_disk() {
	local vol data_kb droot need_backup need_docker dir
	vol=$("$DOCKER" volume inspect "$volume" --format '{{.Mountpoint}}') || return 1
	data_kb=$(du -sk "$vol" | cut -f1) || return 1
	droot=$("$DOCKER" info --format '{{.DockerRootDir}}') || return 1
	dir=$(nearest_dir "$BACKUPS")
	# A backup is at most the data; the new images come beside the old ones.
	need_backup=$((data_kb + 102400))
	need_docker=$((IMAGES_MB * 1024))
	if [ "$(stat -c %d "$dir")" = "$(stat -c %d "$droot")" ]; then
		need_docker=$((need_docker + need_backup))
	fi
	if [ "$(free_kb "$dir")" -lt "$need_backup" ] || [ "$(free_kb "$droot")" -lt "$need_docker" ]; then
		reason="not enough free disk for a backup (${need_backup} KB) and the images"
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

# What a /data mount is, the same for the running server (from `docker inspect`) and for
# Compose's rendered settings: the volume's Docker name, its subpath and whether it is read-only.
# Two mounts are the same when their keys are (those three values as a JSON array, in base64 so
# nothing in a name can mix with the rest, and no value can pass for another); the description is
# for the messages only.
DESC_DEF='def key($n; $s; $ro): [$n, $s, $ro] | tojson | @base64;
	def desc($n; $s; $ro): $n + (if $s != "" then " (subpath " + $s + ")" else "" end) + (if $ro then " (read-only)" else "" end);'

# The running server: its data volume and how it is mounted (from what it has mounted at /data,
# never a name assumed), and the images the running containers use: by id, as the containers
# have them, never what a tag points at now. The three of TaskTo's must still be on this machine,
# or nothing could put them back.
resolve_running() {
	local s insp pair svc n cid id c info mount
	s=$(compose ps -q taskto) && [ -n "$s" ] || { reason="the server isn't running"; return 1; }
	insp=$("$DOCKER" inspect "$s") || { reason="can't inspect the server's container"; return 1; }
	volume=$(jq -r '[.[0].Mounts[]? | select(.Destination == "/data" and .Type == "volume") | .Name][0] // empty' <<<"$insp") || volume=""
	[[ $volume =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || { reason="can't tell the server's data volume"; return 1; }
	mount=$(jq -c "$DESC_DEF"' .[0] as $c | [$c.Mounts[]? | select(.Destination == "/data")] as $m
		| ([$c.HostConfig.Mounts[]? | select(.Target == "/data")][0].VolumeOptions.Subpath // "") as $s
		| if ($m | length) != 1 then empty else
		  {key: key($m[0].Name; $s; ($m[0].RW == false)), desc: desc($m[0].Name; $s; ($m[0].RW == false)), subpath: $s} end' <<<"$insp") || mount=""
	[ -n "$mount" ] || { reason="can't tell how the server's data volume is mounted"; return 1; }
	data_mount=$(jq -r '.desc' <<<"$mount") data_key=$(jq -r '.key' <<<"$mount") subpath=$(jq -r '.subpath' <<<"$mount")
	old_images='{}'
	for pair in taskto:taskto cli-worker:taskto-cli-worker backup-schedule:taskto-backup; do
		svc=${pair%%:*} n=${pair#*:}
		cid=$(compose ps -q "$svc") || { reason="can't list the $svc container"; return 1; }
		if [ -z "$cid" ]; then
			[ "$svc" = backup-schedule ] && continue # the nightly backup is optional
			reason="the $svc service isn't running"
			return 1
		fi
		id=$("$DOCKER" inspect --format '{{.Image}}' "$cid") && [ -n "$id" ] || { reason="can't tell the image the running $svc has"; return 1; }
		"$DOCKER" image inspect --format '{{.Id}}' "$id" >/dev/null 2>&1 ||
			{ reason="the image the running $svc has ($id) isn't on this machine any more: nothing could put it back"; return 1; }
		old_images=$(jq -c --arg k "$n" --arg v "$id" '. + {($k): $v}' <<<"$old_images")
	done
	old_server_image=$(jq -r '.taskto' <<<"$old_images")
	# Every running service's image id, by service: what the way back checks its containers against.
	old_service_images='{}'
	cid=$(compose ps -q) || { reason="can't list the running containers"; return 1; }
	while IFS= read -r c; do
		[ -n "$c" ] || continue
		info=$("$DOCKER" inspect --format '{{index .Config.Labels "com.docker.compose.service"}} {{.Image}}' "$c") || { reason="can't inspect a running container"; return 1; }
		read -r svc id <<<"$info"
		[ -n "$svc" ] && [ -n "$id" ] || { reason="can't tell a running container's service and image"; return 1; }
		old_service_images=$(jq -c --arg k "$svc" --arg v "$id" '. + {($k): $v}' <<<"$old_service_images")
	done <<<"$cid"
}

# data_mounts CONFIG: "service<TAB>what its /data is" for every service of TaskTO's own (the
# server, the backup jobs, and any service running one of TaskTo's images; not Caddy's, whose
# /data is its own volume): "service, key, description" for each: the volume's effective Docker
# name with its subpath and whether it is read-only (DESC_DEF), or what else it is.
data_mounts() {
	jq -r --arg p "$prefix" "$DESC_DEF"'
		def repo: sub("@.*$"; "") | sub(":[^/:]*$"; "");
		. as $c
		| .services | to_entries[]
		| select((.key | IN("taskto", "backup", "backup-schedule")) or ((.value.image // "") | repo | IN($p + "taskto", $p + "taskto-cli-worker", $p + "taskto-backup")))
		| .key as $k | (.value.volumes // [])[] | select(.target == "/data")
		| (if .type == "volume" then [key($c.volumes[.source // ""].name // "?"; (.volume.subpath // ""); (.read_only // false)), desc($c.volumes[.source // ""].name // "?"; (.volume.subpath // ""); (.read_only // false))]
		   else [(["mount", .type] | tojson | @base64), "a " + .type + " mount"] end) as $m
		| [$k, $m[0], $m[1]] | @tsv' <<<"$1"
}
# check_data_volume CONFIG WHAT: every one of those is mounted at /data exactly as the running
# server has it (volume, subpath and read-only: compared as values, never as the text of the
# message), the backup services included; and a backup service the settings have mounts it.
check_data_volume() {
	local out svc key what seen=0 mounted=" " s
	out=$(data_mounts "$1") || { reason="can't read the mounts of $2"; return 1; }
	while IFS=$'\t' read -r svc key what; do
		[ -n "$svc" ] || continue
		[ "$svc" != taskto ] || seen=1
		mounted="$mounted$svc "
		[ "$key" = "$data_key" ] || { reason="in $2, the /data of $svc is '$what', not '$data_mount', the mount the running server has"; return 1; }
	done <<<"$out"
	[ "$seen" = 1 ] || { reason="$2 has no /data volume for the server"; return 1; }
	# A backup job with no /data would back up whatever its image has there.
	for s in $(jq -r '.services | keys[] | select(IN("backup", "backup-schedule"))' <<<"$1"); do
		case $mounted in *" $s "*) ;; *) reason="in $2, $s has no /data mount: the backup would not be of the server's data"; return 1 ;; esac
	done
}
# volumes_exist CONFIG: every volume the enabled services mount exists already, and none is
# anonymous, so that starting the new settings never creates one.
volumes_exist() {
	local v names
	jq -e '[.services[].volumes[]? | select(.type == "volume" and ((.source // "") == ""))] | length == 0' <<<"$1" >/dev/null ||
		{ reason="$to's Compose settings have an anonymous volume, which starting them would create"; return 1; }
	names=$(jq -r '. as $c | [.services[].volumes[]? | select(.type == "volume") | ($c.volumes[.source].name // .source)] | unique[]' <<<"$1") ||
		{ reason="can't read the volumes of $to's Compose settings"; return 1; }
	while IFS= read -r v; do
		[ -n "$v" ] || continue
		"$DOCKER" volume inspect "$v" >/dev/null 2>&1 ||
			{ reason="$to's Compose settings use the volume '$v', which doesn't exist: starting them would create it"; return 1; }
	done <<<"$names"
}

# image_problems CONFIG: every service of TaskTo's (by name, or by running one of its images)
# runs the image of the new version, as `prefix + name:version`, which was checked: no override
# swaps in another, or an older one.
image_problems() {
	jq -r --arg p "$prefix" --arg v "$to" '
		def repo: sub("@.*$"; "") | sub(":[^/:]*$"; "");
		{"taskto": "taskto", "cli-worker": "taskto-cli-worker", "backup": "taskto-backup", "backup-schedule": "taskto-backup"} as $own
		| ["taskto", "taskto-cli-worker", "taskto-backup"] as $names
		| .services | to_entries[]
		| .key as $k | (.value.image // "") as $i
		| (if $own[$k] != null then $own[$k] else ($names[] | select($p + . == ($i | repo))) end) as $n
		| select($i != $p + $n + ":" + $v)
		| "\($k) uses \($i), not \($p + $n + ":" + $v)"' <<<"$1"
}

# The proxy in front of the server answers as VERSION too: Caddy over HTTPS (this machine's
# own address for the domain); in traefik mode the LAN port, and the public address
# https://<domain>/healthz, from this host, as a visitor reaches it.
proxy_needs() {
	local mode
	mode=$(envget "$INSTALL_ENV" TASKTO_MODE)
	if [ "$mode" = traefik ]; then
		[ -n "$(envget "$INSTALL_ENV" TASKTO_LAN_IP)" ] && [ -n "$(envget "$ROOT/.env" TASKTO_DOMAIN)" ]
	elif https_on; then
		[ -n "$(envget "$ROOT/.env" TASKTO_DOMAIN)" ]
	fi
}
proxy_answers() {
	local mode lan domain h
	mode=$(envget "$INSTALL_ENV" TASKTO_MODE)
	domain=$(envget "$ROOT/.env" TASKTO_DOMAIN)
	if [ "$mode" = traefik ]; then
		lan=$(envget "$INSTALL_ENV" TASKTO_LAN_IP)
		[ -n "$lan" ] && [ -n "$domain" ] || return 1
		h=$("$CURL" -q -fsS --max-time 5 "http://$lan/healthz" 2>/dev/null) || return 1
		jq -e --arg v "$1" '.version == $v' <<<"$h" >/dev/null 2>&1 || return 1
		h=$("$CURL" -q -fsS --max-time 10 "https://$domain/healthz" 2>/dev/null) || return 1
	elif https_on; then
		[ -n "$domain" ] || return 1
		h=$("$CURL" -q -fsS --max-time 5 --resolve "$domain:443:127.0.0.1" "https://$domain/healthz" 2>/dev/null) || return 1
	else
		return 0
	fi
	jq -e --arg v "$1" '.version == $v' <<<"$h" >/dev/null 2>&1
}

preflight() {
	local n d img rev ref id out cfg bad info
	local -a pargs
	layout_ok || return 1
	stage=$(mktemp -d) || return 1
	mkdir "$stage/deploy" || return 1
	sha=$(resolve_sha "$to") || { reason="can't resolve $to to a commit"; return 1; }
	from_sha=$(resolve_sha "$from") || { reason="can't resolve $from to a commit"; return 1; }
	# The new version's files, from that one commit, as the runbook's steps 6, 8 and 12 install them.
	for n in compose.yaml deploy/Caddyfile deploy/worker-firewall.sh deploy/safety-check.sh; do
		fetch "$sha" "$n" "$stage/$n" || { reason="can't fetch $n of $to"; return 1; }
	done
	# The safety rules the old version is held to once it is started again.
	fetch "$from_sha" deploy/safety-check.sh "$stage/safety-from.sh" ||
		{ reason="can't fetch deploy/safety-check.sh of $from: it predates the updater, which can't check it. Update by hand (deploy/INSTALL.md#updates, \"By hand\")"; return 1; }
	chmod 644 "$stage/compose.yaml" "$stage/deploy/Caddyfile" || return 1
	grep -q TASKTO_IMAGE_PREFIX "$stage/compose.yaml" || { reason="$to's compose.yaml predates published images"; return 1; }
	# The worker's firewall is never changed here: a version that changes it is for a person.
	[ -e "$FIREWALL" ] || { reason="$FIREWALL isn't installed (runbook step 8)"; return 1; }
	if ! cmp -s "$stage/deploy/worker-firewall.sh" "$FIREWALL"; then
		needs="manual-upgrade"
		finish skipped "the worker firewall changed in $to: update by hand (runbook step 8), then run taskto-update --to $to"
	fi
	firewall_ok || { reason="the worker's firewall isn't in place (its unit, or its rules): runbook step 8's check says what"; return 1; }
	resolve_running || return 1
	mapfile -t pargs < <(profile_args "$ROOT")
	# This install's settings, with the backup services' profiles on too: the data is mounted the
	# same way in each of TaskTo's services, those that aren't running included.
	cfg=$(compose "${pargs[@]}" config --format json 2>/dev/null) || { reason="docker compose config failed"; return 1; }
	check_data_volume "$cfg" "this install's Compose project" || return 1
	# A broken proxy isn't the update's fault: nothing is changed on top of one.
	proxy_needs || { reason="the proxy's settings (TASKTO_DOMAIN, TASKTO_LAN_IP) aren't in $ROOT/.env and $INSTALL_ENV: can't check the public address"; return 1; }
	proxy_answers "$from" || { reason="the public address doesn't answer as $from before the update (the proxy isn't the update's to fix)"; return 1; }
	check_disk || { [ -n "$reason" ] || reason="can't read the disk's free space"; return 1; }
	registry_login || { reason="can't log in to the registry"; return 1; }
	# The new Compose file with this install's own settings: the same two files, in the stage.
	set_version "$ROOT/.env" "$to" "" >"$stage/.env" || return 1
	[ ! -e "$ROOT/compose.override.yaml" ] || cat "$ROOT/compose.override.yaml" >"$stage/compose.override.yaml" || return 1
	inputs "$stage"
	STAGE=("${A[@]}")
	if ! out=$(scompose config --quiet 2>&1) || [[ $out == *"is not set"* ]]; then
		reason="$to's compose.yaml doesn't work with this install's settings: $(printf '%s' "$out" | head -n 3 | tr '\n' ' ')"
		return 1
	fi
	cfg=$(scompose config --format json 2>/dev/null) || { reason="can't render $to's Compose settings"; return 1; }
	# Where the data is, which images run and which volumes are used, in the settings that will be started.
	out=$(scompose "${pargs[@]}" config --format json 2>/dev/null) || { reason="can't render $to's Compose settings with the backup services"; return 1; }
	check_data_volume "$out" "$to's Compose settings" || return 1
	volumes_exist "$cfg" || return 1
	bad=$(image_problems "$out") || { reason="can't read the images of $to's Compose settings"; return 1; }
	[ -z "$bad" ] || { reason="$to's Compose settings run an image that wasn't checked: $(head -n 1 <<<"$bad")"; return 1; }
	# The three of TaskTo's are the commit's own (their revision label says so), and are known by their id:
	# the label and the id come from one inspect of one reference.
	for n in $IMAGE_NAMES; do
		ref=$prefix$n:$to
		dockerp pull --quiet "$ref" >/dev/null || { reason="can't pull $ref"; return 1; }
		info=$("$DOCKER" image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}|{{.Id}}|{{index .RepoDigests 0}}' "$ref") || { reason="can't inspect $ref"; return 1; }
		IFS='|' read -r rev id d <<<"$info"
		[ "$rev" = "$sha" ] || { reason="$ref isn't built from $sha (its revision label is '${rev:-none}')"; return 1; }
		[ -n "$id" ] && [ -n "$d" ] || { reason="no image id or digest for $ref"; return 1; }
		digests=$(jq -c --arg k "$n" --arg v "$d" '. + {($k): $v}' <<<"$digests")
		new_images=$(jq -c --arg k "$ref" --arg v "$id" '. + {($k): $v}' <<<"$new_images")
	done
	# The safety rules, on the new settings, before anything of them starts.
	if ! out=$(TASKTO_ROOT=$ROOT DOCKER=$DOCKER TASKTO_UPDATE_STATE=$STATE TASKTO_UPDATE_PUBLIC=$PUBLIC TASKTO_UPDATE_REQUEST=$REQUEST bash "$stage/deploy/safety-check.sh" --static "$stage" 2>&1); then
		reason="$to's Compose settings break a safety rule: $(printf '%s' "$out" | tail -n 1)"
		return 1
	fi
	# Every other image of the enabled services, before the backup: the switch pulls nothing.
	while IFS= read -r img; do
		[ -n "$img" ] || continue
		jq -e --arg k "$img" 'has($k)' <<<"$new_images" >/dev/null && continue
		dockerp pull --quiet "$img" >/dev/null || { reason="can't pull $img"; return 1; }
		id=$("$DOCKER" image inspect --format '{{.Id}}' "$img") || { reason="no image id for $img"; return 1; }
		new_images=$(jq -c --arg k "$img" --arg v "$id" '. + {($k): $v}' <<<"$new_images")
	done < <(jq -r '.services[].image // empty' <<<"$cfg")
	# The image each service's container must be made from: the id that was checked for its image.
	new_ids=$(jq -c --argjson i "$new_images" '[.services | to_entries[] | {(.key): ($i[.value.image // ""] // "?")}] | add // {}' <<<"$cfg") ||
		{ reason="can't tell the images of $to's services"; return 1; }
	cmp -s "$stage/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" || { ! https_on || caddy_changed=1; }
}

# --- the switch, and the way back ------------------------------------------------------------

# step WHAT COMMAND...: the command, or the reason it failed.
step() {
	local what=$1
	shift
	"$@" || { reason="$what failed${reason:+ ($reason)}"; return 1; }
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

# The images the running containers have, kept under tags of their own (by id): the old
# version's tags can move, and a prune can take what no tag names.
protect_old_images() {
	local n id
	for n in $IMAGE_NAMES; do
		id=$(jq -r --arg k "$n" '.[$k] // empty' <<<"$old_images")
		[ -z "$id" ] || "$DOCKER" tag "$id" "$KEEP_TAG:$n" || return 1
	done
}

# tx_write PHASE: the record, before the step it names; durable before anything is changed.
tx_write() {
	phase=$1
	install -d -m 755 "$STATE" || return 1
	jq -n --arg from "$from" --arg to "$to" --arg sha "$sha" --arg phase "$phase" --arg backup "$backup" --arg volume "$volume" --arg subpath "$subpath" --arg data_mount "$data_mount" \
		--arg previous "$STATE/previous" --arg at "$started" --argjson old_images "$old_images" --arg old_server_image "$old_server_image" \
		--arg why "$cause" \
		'{from: $from, to: $to, sha: $sha, phase: $phase, why: $why, backup: $backup, volume: $volume, subpath: $subpath, data_mount: $data_mount, previous: $previous,
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
# The images the switch will start are the ones that were checked: every tag still has the id
# it had when it was.
images_unchanged() {
	local ref id
	while IFS=$'\t' read -r ref id; do
		[ "$("$DOCKER" image inspect --format '{{.Id}}' "$ref" 2>/dev/null)" = "$id" ] || return 1
	done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$new_images")
}

answers() { # VERSION: the server, and the proxy before it, are up as that version
	last=$("$CURL" -q -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null) || return 1
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
	out=$(TASKTO_ROOT=$ROOT DOCKER=$DOCKER TASKTO_UPDATE_STATE=$STATE TASKTO_UPDATE_PUBLIC=$PUBLIC TASKTO_UPDATE_REQUEST=$REQUEST bash "$2" 2>&1) || { reason="the safety rules: $(printf '%s' "$out" | tail -n 1)"; return 1; }
}

# created_match EXPECTED: every container of the project was made from the image that was
# verified for its service (EXPECTED is {service: image id}), not from what a tag points at now.
# A container of a service that has no entry doesn't match.
created_match() {
	local ids c info svc id want
	ids=$(compose ps -aq) || { reason="can't list the containers just made"; return 1; }
	[ -n "$ids" ] || { reason="no container was made"; return 1; }
	while IFS= read -r c; do
		[ -n "$c" ] || continue
		info=$("$DOCKER" inspect --format '{{index .Config.Labels "com.docker.compose.service"}} {{.Image}}' "$c") || { reason="can't inspect a container just made"; return 1; }
		read -r svc id <<<"$info"
		want=$(jq -r --arg s "$svc" '.[$s] // empty' <<<"$1")
		[ -n "$want" ] && [ "$want" = "$id" ] ||
			{ reason="the container of ${svc:-a service} was made from the image '${id:-?}', not the checked '${want:-none}': a tag moved"; return 1; }
	done <<<"$ids"
}
# remove_created: the containers that were made and never started (only those), for the way back.
remove_created() {
	local ids
	ids=$("$DOCKER" ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter status=created) || return 1
	[ -z "$ids" ] || xargs "$DOCKER" rm <<<"$ids" >/dev/null
}
# bring_up EXPECTED: make the containers, check them, start them. The images are checked on the
# containers, after they are made and before anything of them runs, since a tag can move between
# any earlier check and the start.
bring_up() {
	# Caddy reads its Caddyfile once, and a file that was replaced is a new file to its mount.
	# `create` takes no --no-deps: naming Caddy makes it, and what it depends on (the server),
	# new, and not started, as the create below does for all of them.
	[ "$caddy_changed" = 0 ] || compose create --no-build --pull never --force-recreate caddy || return 1
	compose create --no-build --pull never --remove-orphans || return 1
	created_match "$1" || { remove_created || true; return 1; }
	compose start --wait --wait-timeout "$VERIFY_TIMEOUT"
}

apply_update() {
	step "installing the new files" tx_write switching || return 1
	step "installing the new files" install_new || return 1
	# From here the new server may start, and write data.
	step "recording the update" tx_write starting || return 1
	step "checking the images" images_unchanged || return 1
	step "docker compose create and start" bring_up "$new_ids" || return 1
	verify "$to" "$stage/deploy/safety-check.sh"
}

# Did the new server run (so it may have written data, or migrated it, which the old one can't
# read)? Only an `up` can have started it; if any container of the server isn't the old image
# and has started, or we can't tell, yes.
new_server_started() {
	[ "$phase" = starting ] || return 1
	local ids c info img st
	ids=$(compose ps -aq taskto 2>/dev/null) || return 0
	[ -n "$ids" ] || return 1
	while IFS= read -r c; do
		[ -n "$c" ] || continue
		info=$("$DOCKER" inspect --format '{{.Image}} {{.State.StartedAt}}' "$c" 2>/dev/null) || return 0
		read -r img st <<<"$info"
		[ -n "$img" ] || return 0
		if [ "$img" != "$old_server_image" ] && [ "$st" != 0001-01-01T00:00:00Z ]; then return 0; fi
	done <<<"$ids"
	return 1
}
# The old images, by the ids the running containers had, under the old version's tags again.
retag_old_images() {
	local n id
	for n in $IMAGE_NAMES; do
		id=$(jq -r --arg k "$n" '.[$k] // empty' <<<"$old_images")
		[ -z "$id" ] || { "$DOCKER" image inspect --format '{{.Id}}' "$id" >/dev/null && "$DOCKER" tag "$id" "$prefix$n:$from"; } || return 1
	done
}
# Only before the new server has started: the old files, and the old images, put back, and
# the old version started and checked. The data was never touched, so there is none to restore.
revert() {
	local p=$STATE/previous
	reason=""
	step "putting the old files back" put "$p/compose.yaml" "$ROOT/compose.yaml" || return 1
	step "putting the old files back" put "$p/.env" "$ROOT/.env" || return 1
	step "putting the old files back" put "$p/deploy/Caddyfile" "$ROOT/deploy/Caddyfile" || return 1
	step "putting the old files back" put "$p/install.env" "$INSTALL_ENV" || return 1
	step "putting the old images back" retag_old_images || return 1
	step "docker compose create and start" bring_up "$old_service_images" || return 1
	verify "$from" "$stage/safety-from.sh"
}

# record_failed VERSION: the release is on the list of those not tried again, durably: the list
# is written to a file of its own, flushed, and moved into place in one step. Fails when it
# can't, and then nothing may be assumed recorded.
record_failed() {
	local f=$STATE/failed
	install -d -m 755 "$STATE" || return 1
	if [ -e "$f" ] || [ -L "$f" ]; then
		[ -f "$f" ] || return 1
		! grep -qxF "$1" "$f" || return 0
	fi
	{ if [ -f "$f" ]; then cat "$f"; fi && printf '%s\n' "$1"; } >"$f.new" && sync && mv -fT "$f.new" "$f" && sync
}

# stop_stack: the project's containers can't come back by themselves, and are stopped, and none
# of them is running after. Their restart policies are turned off first, so that a reboot or a
# restart of Docker can't start what is stopped. Fails when the stop can't be confirmed; the
# policies' own failure (or that they couldn't be looked up) only goes in $policy_note, and
# when they are off, $policy_off says so; the stop is tried, and confirmed, either way.
policy_note="" policy_off=""
stop_stack() {
	local ids running listed=1
	local -a all=()
	ids=$("$DOCKER" ps -aq --filter "label=com.docker.compose.project=$PROJECT") || listed=0
	if [ "$listed" = 0 ]; then
		policy_note=" The containers couldn't be listed, so their restart policies couldn't be turned off: they may start again after a reboot."
	elif [ -z "$ids" ]; then
		policy_off=1
	else
		mapfile -t all <<<"$ids"
		if "$DOCKER" update --restart=no "${all[@]}" >/dev/null; then
			policy_off=1
		else
			policy_note=" The restart policies of its containers couldn't be turned off: they may start again after a reboot."
		fi
	fi
	compose stop || true
	running=$("$DOCKER" ps -q --filter "label=com.docker.compose.project=$PROJECT") || return 1
	[ -z "$running" ]
}

# halt RESULT PHASE MESSAGE: the install is left as it is, for a person: the record of the
# transaction stays (in PHASE), the release isn't tried again, the blocked marker is written,
# and the run ends loudly. Each of these writes is best effort and the first alone is enough:
# a record that can't be rewritten is still there, and blocks the next run, and a marker that
# can't be written changes nothing, since the record is there.
halt() {
	tx_write "$2" || log "can't rewrite $TX: its old content still blocks the next run"
	record_failed "$to" || true
	printf '%s\n' "$3" >"$STATE/blocked" || true
	finish "$1" "$3" 2
}

# clear_block: for a person who has put the install right (the backup restored by hand with
# the old version, or the update finished by hand): the record and the marker go, if the server
# answers as the version in .env, the worker's firewall is in, and the safety rules hold in the
# running containers (checked with the rules of the version that runs, at its commit), so the
# next run starts from a known state. The release that was being installed isn't tried again,
# unless that is the version now; that is recorded durably before the record goes, and the record
# stays when it can't be.
clear_block() {
	[ -e "$TX" ] || [ -L "$TX" ] || [ -e "$STATE/blocked" ] || [ -L "$STATE/blocked" ] || {
		result=cleared
		log "nothing to clear: no update is stopped or interrupted"
		exit 0
	}
	local t_to iv sf out rc
	refuse() {
		result=refused
		log "refusing: $*"
		exit 1
	}
	iv=$(envget "$INSTALL_ENV" TASKTO_VERSION)
	if [ -n "$iv" ] && [ "$iv" != "$from" ]; then
		refuse "$INSTALL_ENV says $iv, $ROOT/.env says $from: put them right first"
	fi
	answers "$from" || refuse "the server isn't healthy as $from, the version in $ROOT/.env (${last:-no answer}): start it, and check the data, first"
	firewall_ok || refuse "the worker's firewall isn't in place (its unit, or its rules): runbook step 8's check says what"
	stage=$(mktemp -d) || refuse "no temporary directory"
	sf=$(resolve_sha "$from") && fetch "$sf" deploy/safety-check.sh "$stage/safety.sh" || refuse "can't fetch deploy/safety-check.sh of $from to check the running containers with"
	out=$(TASKTO_ROOT=$ROOT DOCKER=$DOCKER TASKTO_UPDATE_STATE=$STATE TASKTO_UPDATE_PUBLIC=$PUBLIC TASKTO_UPDATE_REQUEST=$REQUEST bash "$stage/safety.sh" 2>&1) || refuse "the safety rules don't hold in the running containers: $(printf '%s' "$out" | tail -n 1)"
	if [ -e "$TX" ]; then
		t_to=$(jq -r '.to // empty' "$TX" 2>/dev/null) || t_to=""
		# The release the stop was for is recorded as failed already, unless it is what runs now.
		if [ -n "$t_to" ] && [ "$t_to" = "$from" ] && [ -f "$STATE/failed" ]; then
			# grep says 1 when nothing is left (an empty list is right), and 2 when it couldn't
			# finish (a full disk): then the list stays as it is, and so does the stop.
			rc=0
			grep -vxF -e "$t_to" "$STATE/failed" >"$STATE/failed.new" || rc=$?
			if [ "$rc" -gt 1 ] || ! mv -fT "$STATE/failed.new" "$STATE/failed"; then
				rm -f "$STATE/failed.new"
				refuse "can't take $t_to off $STATE/failed, so the record stays: put $STATE right first"
			fi
		elif [ -n "$t_to" ]; then
			record_failed "$t_to" || refuse "can't record $t_to as failed in $STATE/failed, so the record stays: put $STATE right first"
		fi
	fi
	rm -f "$TX" "$STATE/blocked"
	result=cleared reason="a person cleared the stop; the server is $from"
	write_status || log "can't write $STATE/status.json"
	log "result=cleared from=$from"
	exit 0
}

# --- one run -----------------------------------------------------------------------------------

if [ "$REQUESTED" = 1 ]; then take_request; fi

[ -r "$ROOT/.env" ] || finish failed "no $ROOT/.env: not an install from deploy/INSTALL.md" 1
cd "$ROOT"
inputs "$ROOT" # Compose reads these files, this name and this .env, and no others
LIVE=("${A[@]}")
from=$(envget "$ROOT/.env" TASKTO_VERSION)
prefix=$(envget "$ROOT/.env" TASKTO_IMAGE_PREFIX)
port=$(envget "$ROOT/.env" TASKTO_PORT)
HEALTH_URL=${TASKTO_UPDATE_HEALTH:-http://127.0.0.1:${port:-8080}/healthz}
last=""
if [ "$CLEAR" = 1 ]; then clear_block; fi
# --publish-status: the server's copy of the status, from what is there, and nothing else.
if [ "$PUBLISH" = 1 ]; then
	write_public || {
		result=failed
		log "can't write $PUBLIC/status.json"
		exit 1
	}
	result=published
	log "wrote $PUBLIC/status.json"
	exit 0
fi
# An update that was stopped half way (a crash, a reboot, a kill) left its record, and one that
# failed after the new server started, or whose way back failed, left it too: nothing runs,
# not even --to, until a person has looked, since the files may be on either version and the
# data may be the new version's.
if [ -e "$TX" ]; then
	from=$(jq -r '.from // "?"' "$TX" 2>/dev/null) || from="?"
	to=$(jq -r '.to // "?"' "$TX" 2>/dev/null) || to="?"
	backup=$(jq -r '.backup // ""' "$TX" 2>/dev/null) || backup=""
	tphase=$(jq -r '.phase // "?"' "$TX" 2>/dev/null) || tphase="?"
	case $tphase in
	stopped | stop-failed | rollback-failed)
		# What the failed run said stays the status.
		tvol=$(jq -r '.volume // "?"' "$TX" 2>/dev/null) || tvol="?"
		tsub=$(jq -r '.subpath // ""' "$TX" 2>/dev/null) || tsub=""
		result=blocked reason="the update from $from to $to ended in '$tphase' and was left for a person: restore the backup ${backup:-(none was made)} by hand, in the data volume '$tvol'${tsub:+ (its subpath '$tsub')} (deploy/INSTALL.md, Rollback), then run taskto-update --clear-block"
		# The stack couldn't be confirmed stopped: that comes first, since a restart would run the new version.
		[ "$tphase" != stop-failed ] || reason="the stack of the failed update from $from to $to could NOT be confirmed stopped: stop it by hand now (docker compose stop; docker ps), since a restart would run the new version on the data it may have changed. Then, $reason"
		log "blocked: $reason"
		write_public || log "can't write $PUBLIC/status.json"
		notify || true
		exit 1
		;;
	rolled-back)
		result=blocked reason="the update from $from to $to was rolled back (the server is on $from), but $to couldn't be recorded as failed in $STATE/failed: make $STATE writable, then run taskto-update --clear-block, which records it"
		log "blocked: $reason"
		write_public || log "can't write $PUBLIC/status.json"
		notify || true
		exit 1
		;;
	esac
	finish interrupted "an update from $from to $to was interrupted in phase $tphase and neither finished nor was undone: the files may be on either version. The backup is ${backup:-not made}, the old files are in $STATE/previous. Look at it, put it right by hand (docs/install.md#upgrade-and-rollback, deploy/INSTALL.md Rollback), then run taskto-update --clear-block" 1
fi
case $CHANNEL in release | main | off) ;; *) finish failed "TASKTO_UPDATE_CHANNEL is $CHANNEL, not release, main or off" 1 ;; esac
[[ $REPO =~ ^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$ ]] || finish failed "TASKTO_UPDATE_REPO is $REPO, not OWNER/NAME" 1
[ -n "$prefix" ] || finish failed "TASKTO_IMAGE_PREFIX isn't set: this install doesn't use the published images" 1
[[ $KEEP =~ ^[1-9][0-9]*$ ]] || finish failed "TASKTO_UPDATE_KEEP is $KEEP, not a number of backups" 1
[[ $PER_PAGE =~ ^([1-9][0-9]?|100)$ ]] || finish failed "TASKTO_UPDATE_PER_PAGE is $PER_PAGE, not 1 to 100" 1
if [ -z "$TO" ] && [ -e "$STATE/blocked" ] && [ "$DRY" = 0 ]; then
	# The install is for a person to look at: nobody should change it again unseen.
	# The status stays what the failed run said.
	result=blocked reason="$(cat "$STATE/blocked"); look at it, then run taskto-update --clear-block (or taskto-update --to VERSION)"
	log "blocked: $reason"
	# Its own status stays, but the server's copy is made again: it says the install is still
	# waiting, and when this was last looked at.
	write_public || log "can't write $PUBLIC/status.json"
	notify || true
	exit 1
fi
if [ -z "$TO" ]; then
	if [ "$CHANNEL" = off ]; then finish skipped "automatic updates are off (TASKTO_UPDATE_CHANNEL=off)"; fi
	if [ -e "$HOLD" ]; then finish skipped "updates are on hold ($HOLD exists)"; fi
fi
# What is on record must be what runs: a server that answers as another version than the .env's
# was changed outside the updater (or by one that didn't finish).
health=$("$CURL" -q -fsS --max-time 10 "$HEALTH_URL" 2>/dev/null) || finish failed "the server doesn't answer at $HEALTH_URL: not updating a server that isn't up" 1
running=$(jq -r '.version // empty' <<<"$health" 2>/dev/null) || running=""
if [ "$running" != "$from" ]; then
	reason="the server runs '${running:-no version}' but $ROOT/.env says $from: look at it, then run taskto-update --clear-block"
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
	finish dry-run "would update from $from to $to: its files, images, safety rules, disk and Compose settings are fine"
fi

# The run is about to change the install: the page shows it as going on (the server restarts).
RUN_ON=1
write_public || log "can't write $PUBLIC/status.json"
protect_old_images || finish failed "can't keep the old images under $KEEP_TAG: nothing was changed" 1
make_backup || finish failed "the backup failed: nothing was changed" 1
log "backed up to $backup"
# The record, and the old files, before the first change: if either can't be written, nothing
# has been changed and nothing is undone.
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
if new_server_started; then
	# The new server ran: it may have changed the data, in ways the old version can't read. No
	# guess about it is safe, so nothing is undone: the stack is stopped (and can't restart by
	# itself), and a person restores.
	log "the update failed after the new server started ($cause): stopping"
	restore="restore the backup $backup by hand, in the data volume '$volume'${subpath:+ (its subpath '$subpath')} (deploy/INSTALL.md, Rollback; docs/install.md#upgrade-and-rollback), then run taskto-update --clear-block"
	if stop_stack; then
		off=""
		[ -z "$policy_off" ] || off=", its containers' restart policies are turned off (docker update --restart=no; the Rollback's steps remove the containers and step 9 makes them again, with the Compose file's restart policies)"
		halt stopped stopped "$to started and then failed ($cause). The stack is stopped$off, and nothing was rolled back, since the new version may have changed the data: $restore.$policy_note"
	fi
	halt stop-failed stop-failed "$to started and then failed ($cause), and the stack could NOT be confirmed stopped: stop it by hand now (docker compose stop; docker ps), since a restart would run the new version on the data it may have changed. Nothing was rolled back: $restore.$policy_note"
fi
log "the update failed before the new server started ($cause): putting $from back"
# The way back first; what is written about the failure comes after, and never stops it. The
# record of the release that failed is written, durably, before the transaction is removed, and
# when it can't be the transaction stays and blocks the next run.
if revert; then
	if record_failed "$to"; then
		rm -f "$TX" "$STATE/blocked"
		finish rolled-back "$cause; the server is on $from again, its data not touched" 1
	fi
	cause="$cause; the server is on $from again, its data not touched, but $to couldn't be recorded as failed in $STATE/failed"
	tx_write rolled-back || log "can't rewrite $TX: its old content still blocks the next run"
	printf '%s\n' "$cause: the update's record stays and blocks the next run until a person looks and runs taskto-update --clear-block" >"$STATE/blocked" || true
	finish rolled-back "$cause: the update's record stays and blocks the next run until a person looks and runs taskto-update --clear-block" 2
fi
halt rollback-failed rollback-failed "$cause; putting $from back failed too ($reason): the install is left as it is, backup $backup (deploy/INSTALL.md, Rollback), then run taskto-update --clear-block"
