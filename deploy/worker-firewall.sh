#!/bin/sh
# The CLI worker's network reaches the internet and nothing else
# (docs/cli-workers.md#the-workers-network). Run as root on the Docker host.
#
#   worker-firewall.sh add     put the rules in, replacing any earlier ones
#   worker-firewall.sh del     take them out
#   worker-firewall.sh check   exit 0 only if every rule is in, once
#
# compose.yaml puts the worker alone on the bridge `taskto-worker`, so Docker already keeps it
# from the other Compose services. These rules close the rest to it: this machine and every
# address that isn't public (the LAN, other containers, cloud metadata services).
#
# - raw PREROUTING drops what comes in from the bridge for a non-public address. The raw table
#   runs before Docker's NAT, so it sees the address the worker asked for, and Docker never
#   rewrites it, so the rules hold across Docker restarts and can go in before Docker starts.
# - filter INPUT drops what comes in from the bridge for this machine itself, its public
#   address included. (A port published on that address is NATed to its container, so the
#   worker reaches it as anyone on the internet would.)
# - IPv6 from the bridge is dropped entirely: the network has none.
#
# The worker's own traffic to its proxy goes over a Unix socket, so it needs no exception.
#
# Every iptables call waits for the xtables lock (-w), which Docker holds while it changes its
# own rules, a container starting say, rather than failing at once.
set -eu

bridge=taskto-worker
tag=taskto-worker-firewall
# Every IPv4 range that isn't public: IANA's special-purpose registry, as in
# internal/egress/special.go (keep them in step).
ranges="0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 168.63.129.16/32 172.16.0.0/12
192.0.0.0/24 192.0.2.0/24 192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24
203.0.113.0/24 224.0.0.0/4 240.0.0.0/4"

die() { echo "worker-firewall: $*" >&2; exit 1; }

# The rules, one per line: command, table, chain, match.
rules() {
	for r in $ranges; do echo "iptables raw PREROUTING -i $bridge -d $r"; done
	echo "iptables filter INPUT -i $bridge"
	if ipv6; then
		echo "ip6tables raw PREROUTING -i $bridge"
		echo "ip6tables filter INPUT -i $bridge"
	fi
}

# A kernel without IPv6 has no ip6tables tables, and nothing to close.
ipv6() { [ -e /proc/net/if_inet6 ] && ip6tables -w -t raw -S PREROUTING >/dev/null 2>&1; }

remove() {
	for cmd in iptables ip6tables; do
		[ "$cmd" = iptables ] || ipv6 || continue
		for table in raw filter; do
			$cmd -w -t "$table" -S | grep -E -e "--comment \"?$tag\"?( |$)" | sed 's/^-A /-D /' | while read -r rule; do
				# shellcheck disable=SC2086 # a rule as iptables printed it, word by word
				eval "$cmd -w -t $table $rule"
			done
		done
	done
	# The deletions run in a pipeline, whose failures set -e doesn't see: count what is left.
	[ "$(tagged)" -eq 0 ] || die "$(tagged) rules tagged $tag are left after removing them"
}

# How many rules carry the tag, in every table this script uses.
tagged() {
	n=0
	for cmd in iptables ip6tables; do
		[ "$cmd" = iptables ] || ipv6 || continue
		for table in raw filter; do
			n=$((n + $($cmd -w -t "$table" -S | grep -E -c -e "--comment \"?$tag\"?( |$)" || true)))
		done
	done
	echo "$n"
}

add() {
	remove
	# Inserted at the top, in reverse, so they end up in this order and before any ACCEPT.
	rules | sed '1!G;h;$!d' | while read -r cmd table chain match; do
		# shellcheck disable=SC2086
		$cmd -w -t "$table" -I "$chain" 1 $match -m comment --comment "$tag" -j DROP
	done
	# As in remove: the insertions ran in a pipeline. Every rule must be in, once.
	check
}

check() {
	want=$(rules | wc -l)
	missing=$(rules | while read -r cmd table chain match; do
		# shellcheck disable=SC2086
		$cmd -w -t "$table" -C "$chain" $match -m comment --comment "$tag" -j DROP 2>/dev/null || echo "$cmd -t $table $chain $match"
	done)
	[ -z "$missing" ] || die "missing: $missing"
	have=$(tagged)
	[ "$have" -eq "$want" ] || die "$have rules tagged $tag, not $want: run add again"
}

[ "$(id -u)" = 0 ] || die "run as root"
command -v iptables >/dev/null || die "no iptables"
case "${1:-}" in
add) add ;;
del) remove ;;
check) check && echo "worker-firewall: $(rules | wc -l | tr -d ' ') rules in" ;;
*) die "usage: add | del | check" ;;
esac
