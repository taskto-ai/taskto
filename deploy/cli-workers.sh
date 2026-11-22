#!/bin/sh
# The worker image's CLIs, pinned in deploy/cli-workers.json (docs/cli-workers.md).
#
#   cli-workers.sh install <amd64|arm64> <dir>   download each CLI, check its sha256, install it in <dir>
#   cli-workers.sh bump <cli> <version>          set <cli>'s version and checksums in the manifest
#
# Needs curl, jq and sha256sum. install refuses a download whose sha256 isn't the manifest's.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
manifest="${CLI_WORKERS_MANIFEST:-$here/cli-workers.json}"

die() { echo "cli-workers: $*" >&2; exit 1; }

install_clis() {
	arch=$1 dir=$2
	case "$arch" in amd64|arm64) ;; *) die "unknown architecture $arch" ;; esac
	mkdir -p "$dir"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	for cli in $(jq -r '.clis | keys[]' "$manifest"); do
		url=$(jq -r --arg c "$cli" --arg a "$arch" '.clis[$c].platforms[$a].url' "$manifest")
		sum=$(jq -r --arg c "$cli" --arg a "$arch" '.clis[$c].platforms[$a].sha256' "$manifest")
		member=$(jq -r --arg c "$cli" --arg a "$arch" '.clis[$c].platforms[$a].member // ""' "$manifest")
		[ "$url" != null ] && [ "$sum" != null ] || die "$cli has no $arch build in the manifest"
		echo "cli-workers: $cli $(jq -r --arg c "$cli" '.clis[$c].version' "$manifest") ($arch)" >&2
		curl -fsSL --retry 3 -o "$tmp/$cli.download" "$url"
		echo "$sum  $tmp/$cli.download" | sha256sum -c - >/dev/null || die "$cli: the download's sha256 isn't the manifest's"
		if [ -n "$member" ]; then
			tar -xzf "$tmp/$cli.download" -C "$tmp" "$member"
			mv "$tmp/$member" "$tmp/$cli.bin"
		else
			mv "$tmp/$cli.download" "$tmp/$cli.bin"
		fi
		# Root-owned and read-only to everyone else: no CLI user can write over it.
		install -m 0755 "$tmp/$cli.bin" "$dir/$cli"
	done
}

sha_of() { curl -fsSL --retry 3 "$1" | sha256sum | cut -d' ' -f1; }

bump() {
	cli=$1 version=$2
	case "$cli" in
	claude)
		base="https://downloads.claude.ai/claude-code-releases/$version"
		m=$(curl -fsSL "$base/manifest.json")
		set -- \
			amd64 "$base/linux-x64/claude" "$(echo "$m" | jq -r '.platforms["linux-x64"].checksum')" "" \
			arm64 "$base/linux-arm64/claude" "$(echo "$m" | jq -r '.platforms["linux-arm64"].checksum')" ""
		;;
	codex)
		rel=$(curl -fsSL "https://api.github.com/repos/openai/codex/releases/tags/rust-v$version")
		digest() { echo "$rel" | jq -r --arg n "$1" '.assets[] | select(.name == $n) | .digest' | sed 's/^sha256://'; }
		dl="https://github.com/openai/codex/releases/download/rust-v$version"
		set -- \
			amd64 "$dl/codex-x86_64-unknown-linux-musl.tar.gz" "$(digest codex-x86_64-unknown-linux-musl.tar.gz)" codex-x86_64-unknown-linux-musl \
			arm64 "$dl/codex-aarch64-unknown-linux-musl.tar.gz" "$(digest codex-aarch64-unknown-linux-musl.tar.gz)" codex-aarch64-unknown-linux-musl
		;;
	grok)
		dl="https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-$version"
		cdn="https://x.ai/cli/grok-$version"
		# xAI publishes no checksum: this one is ours (trust on first use). It is taken from both
		# places xAI's installer downloads from, its CDN and its bucket, which must agree.
		for arch in x86_64 aarch64; do
			a=$(sha_of "$dl-linux-$arch") b=$(sha_of "$cdn-linux-$arch")
			[ "$a" = "$b" ] || die "grok $version linux-$arch: the CDN and the bucket serve different files"
		done
		set -- \
			amd64 "$dl-linux-x86_64" "$(sha_of "$dl-linux-x86_64")" "" \
			arm64 "$dl-linux-aarch64" "$(sha_of "$dl-linux-aarch64")" ""
		;;
	*) die "unknown CLI $cli" ;;
	esac
	out=$(jq --arg c "$cli" --arg v "$version" '.clis[$c].version = $v' "$manifest")
	while [ $# -gt 0 ]; do
		arch=$1 url=$2 sum=$3 member=$4
		shift 4
		echo "$sum" | grep -Eq '^[0-9a-f]{64}$' || die "$cli $version $arch: no sha256"
		out=$(echo "$out" | jq --arg c "$cli" --arg a "$arch" --arg u "$url" --arg s "$sum" --arg m "$member" \
			'.clis[$c].platforms[$a] = ({url: $u, sha256: $s} + (if $m == "" then {} else {member: $m} end))')
	done
	echo "$out" > "$manifest"
	echo "cli-workers: $cli is now $version; record the drivers' fixtures again (internal/cliworker/testdata)" >&2
}

case "${1:-}" in
install) [ $# -eq 3 ] || die "usage: install <amd64|arm64> <dir>"; install_clis "$2" "$3" ;;
bump) [ $# -eq 3 ] || die "usage: bump <cli> <version>"; bump "$2" "$3" ;;
*) die "usage: install <arch> <dir> | bump <cli> <version>" ;;
esac
