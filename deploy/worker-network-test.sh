#!/bin/sh
# Proves the CLI worker's network (docs/cli-workers.md#the-workers-network): from the worker,
# as a CLI's Unix user, the internet and the per-turn proxy answer, and nothing else does:
# not the server, not Ollama, not this machine, not a machine on its LAN.
#
#   deploy/worker-network-test.sh <taskto built for linux>
#
# It needs Docker that runs privileged containers, and the internet. Everything happens in a
# throwaway Docker-in-Docker container, the "host", whose firewall the test sets with
# deploy/worker-firewall.sh, the script the install runs. Beside it, on a network of their
# own, a busybox stands for a machine on the host's LAN. The real compose.yaml runs inside,
# with the real server and worker binaries in a Debian image (the server's distroless one and
# the worker's with the CLIs take minutes to build, and the network doesn't depend on them),
# and busybox standing in for Ollama. Every "can't reach" has a control that reaches the same
# listener from elsewhere, so a refusal means the network, not a listener that isn't up.
#
# Leaves nothing behind. The containers and images are pinned, like the rest of the install.
set -eu

DIND=docker:29.5.2-dind@sha256:6b9cd914eb9c6b342c040a49a27a5eb3804453bae6ecc90f7ff96133595a95e8
BUSYBOX=busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
# The worker image's base (Dockerfile).
DEBIAN=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

die() { echo "worker-network-test: $*" >&2; exit 1; }

# retry <what> <command...>: the command, up to three times, for what the test needs from
# the internet (an image, a public address), which can fail for a moment on its own.
retry() {
	what=$1
	shift
	for attempt in 1 2 3; do
		"$@" && return 0
		[ "$attempt" = 3 ] || { echo "worker-network-test: $what failed, trying again" >&2; sleep 5; }
	done
	return 1
}

# On a failure, the Docker-in-Docker host's own log (its dockerd's warnings and errors)
# before it goes.
outer_exit() {
	status=$?
	set +e # the cleanup and the diagnosis run to the end
	if [ "$status" != 0 ]; then
		echo "--- the Docker-in-Docker host's warnings and errors (the last 40)"
		docker logs "$id-host" 2>&1 | grep -E 'level=(warning|error|fatal)' | tail -40
	fi
	docker rm -f "$id-host" "$id-lan" >/dev/null 2>&1
	docker network rm "$id" >/dev/null 2>&1
	exit "$status"
}

outer() {
	bin=$1
	[ -x "$bin" ] || die "usage: $0 <taskto built for linux>"
	here=$(cd "$(dirname "$0")/.." && pwd)
	id="taskto-wn-$$"
	trap outer_exit EXIT
	docker network create "$id" >/dev/null
	docker run -d --name "$id-lan" --network "$id" "$BUSYBOX" httpd -f -p 8000 -h /tmp >/dev/null
	lan=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$id\").IPAddress}}" "$id-lan")
	docker run -d --privileged --name "$id-host" --network "$id" -e DOCKER_TLS_CERTDIR= "$DIND" >/dev/null
	i=0
	until docker exec "$id-host" docker info >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || { docker logs "$id-host" 2>&1 | tail -20; die "the Docker-in-Docker host didn't start"; }
		sleep 1
	done
	docker exec "$id-host" mkdir -p /w/deploy
	docker cp "$here/compose.yaml" "$id-host:/w/compose.yaml"
	docker cp "$here/deploy/worker-firewall.sh" "$id-host:/w/deploy/worker-firewall.sh"
	docker cp "$here/deploy/worker-network-test.sh" "$id-host:/w/deploy/worker-network-test.sh"
	docker cp "$bin" "$id-host:/w/taskto"
	docker exec "$id-host" sh /w/deploy/worker-network-test.sh inner "$lan"
}

# --- Inside the Docker-in-Docker host, as root. ---

failed=0
pass() { echo "ok    $*"; }
fail() { echo "FAIL  $*"; failed=1; }

# reach <container> <user> <address> <port>: a TCP connection opens within 4 seconds.
reach() {
	docker exec -u "$2" "$1" timeout 4 bash -c "exec 3<>/dev/tcp/$3/$4" </dev/null >/dev/null 2>&1
}
# A connection that must open gets three tries: the internet, or a listener just started,
# may miss one. The firewall refuses every time, so this can't hide a hole. One that must
# not open gets one: any connection fails it.
must_reach() {
	if retry "$5" reach "$@"; then pass "$5"; else fail "$5"; fi
}
must_not_reach() {
	if reach "$@"; then fail "$5"; else pass "$5"; fi
}

# proxy_status <container> <user>: the status line of a request without a token to the
# proxy, through the worker's relay.
proxy_status() {
	docker exec -u "$2" "$1" timeout 5 bash -c \
		'exec 3<>/dev/tcp/127.0.0.1/8081 && printf "GET /cli-proxy/v1/models HTTP/1.0\r\n\r\n" >&3 && head -1 <&3' </dev/null 2>/dev/null | tr -d '\r'
}

# diagnose prints what a failure needs to be understood from the job's log alone: the
# services and their logs, the containers, the networks and the firewall's rules.
diagnose() {
	echo "--- the services"
	$compose ps -a
	echo "--- their logs (last 100 lines, setup token left out)"
	$compose logs --no-color --timestamps 2>&1 | grep -v setup_token | tail -100
	echo "--- every container"
	docker ps -a --format '{{.Names}} {{.Image}} {{.Status}} {{.Networks}}'
	echo "--- the worker's network"
	docker network inspect -f '{{.Name}} {{.IPAM.Config}} {{.Options}}' taskto_worker 2>&1
	echo "--- the firewall's rules"
	iptables -w -t raw -S PREROUTING 2>&1
	iptables -w -t filter -S INPUT 2>&1
}

inner_exit() {
	status=$?
	set +e # the cleanup and the diagnosis run to the end
	[ "$status" = 0 ] || diagnose
	exit "$status"
}

inner() {
	lan=$1
	cd /w
	compose="docker compose -f compose.yaml -f test.override.yaml --profile cli --profile ollama"
	# Any failure, a check's or a command's under set -e, ends with the diagnosis.
	trap inner_exit EXIT
	retry "pulling $DEBIAN" docker pull -q "$DEBIAN" >/dev/null
	retry "pulling $BUSYBOX" docker pull -q "$BUSYBOX" >/dev/null
	# One image for the server and the worker: the real binary, the server's data directory
	# its user's, and no setuid program (the worker refuses to start with one).
	docker build -q -t taskto-wn -f - /w >/dev/null <<EOF
FROM $DEBIAN
COPY taskto /usr/local/bin/taskto
RUN find / -xdev -perm /6000 -type f -exec chmod a-s {} + \
 && mkdir -m 700 /data && chown 65532:65532 /data \
 && mkdir -p /homes /run/taskto-cli && chmod 711 /homes /run/taskto-cli
EOF
	cat > test.override.yaml <<EOF
services:
  taskto:
    image: taskto-wn
    entrypoint: [/usr/local/bin/taskto]
    user: "65532:65532"
    environment:
      TASKTO_ADDR: 0.0.0.0:8080
      TASKTO_DATA_DIR: /data
  cli-worker:
    image: taskto-wn
    entrypoint: [/usr/local/bin/taskto, cli-worker]
  ollama:
    image: $BUSYBOX
    entrypoint: [httpd, -f, -p, "11434", -h, /tmp]
EOF

	sh deploy/worker-firewall.sh add
	sh deploy/worker-firewall.sh add # twice: it replaces, never duplicates
	sh deploy/worker-firewall.sh check

	export TASKTO_CLI_WORKERS=container
	$compose up -d --no-build --pull never --quiet-pull
	server=$($compose ps -q taskto)
	worker=$($compose ps -q cli-worker)
	ollama=$($compose ps -q ollama)
	[ -n "$server" ] && [ -n "$worker" ] && [ -n "$ollama" ] || die "a service didn't start"
	# A CLI's user: uid 100000 + its TaskTo user's ID (docs/cli-workers.md).
	cli=100001:100001

	# The server's proxy listens once the worker has made its directory.
	i=0
	until [ "$(proxy_status "$worker" "$cli")" = "HTTP/1.0 401 Unauthorized" ]; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || die "the proxy never answered through the worker's relay"
		sleep 1
	done

	ip_on() { docker inspect -f "{{(index .NetworkSettings.Networks \"$2\").IPAddress}}" "$1"; }
	server_ip=$(ip_on "$server" taskto_default)
	ollama_ip=$(ip_on "$ollama" taskto_default)
	gateway=$(docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' taskto_worker)
	host_ip=$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1)
	[ -n "$server_ip" ] && [ -n "$ollama_ip" ] && [ -n "$gateway" ] && [ -n "$host_ip" ] || die "addresses: '$server_ip' '$ollama_ip' '$gateway' '$host_ip'"
	# A service of the host's own (sshd, say), on all its addresses.
	docker run -d --name host-service --network host "$BUSYBOX" httpd -f -p 7000 -h /tmp >/dev/null
	# Controls: a container on the Compose network, and one on a network like any other.
	docker run -d --name on-compose --network taskto_default "$DEBIAN" sleep infinity >/dev/null
	docker network create plain >/dev/null
	docker run -d --name on-plain --network plain "$DEBIAN" sleep infinity >/dev/null

	echo "-- the worker's network"
	nets=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$worker")
	if [ "$nets" = "taskto_worker " ]; then pass "the worker is on taskto_worker only"; else fail "the worker's networks: $nets"; fi
	if [ "$(docker network inspect -f '{{index .Options "com.docker.network.bridge.name"}}' taskto_worker)" = taskto-worker ]; then
		pass "its bridge is taskto-worker, the firewall's"
	else fail "the bridge isn't taskto-worker"; fi
	if docker exec "$worker" sh -c 'grep -qv " lo$" /proc/net/if_inet6' 2>/dev/null; then
		fail "the worker has an IPv6 address: $(docker exec "$worker" cat /proc/net/if_inet6)"
	else pass "the worker has no IPv6 address"; fi

	echo "-- what the worker reaches, as a CLI's user"
	if [ "$(proxy_status "$worker" "$cli")" = "HTTP/1.0 401 Unauthorized" ]; then
		pass "the per-turn proxy, through the relay (401 without a token)"
	else fail "the per-turn proxy"; fi
	if retry "DNS" docker exec -u "$cli" "$worker" getent hosts example.com >/dev/null 2>&1; then pass "DNS for a public name"; else fail "DNS for a public name"; fi
	must_reach "$worker" "$cli" example.com 443 "the internet: example.com:443"
	must_reach "$worker" "$cli" 1.1.1.1 443 "the internet: 1.1.1.1:443"

	echo "-- what the worker doesn't reach"
	if docker exec -u "$cli" "$worker" getent hosts taskto >/dev/null; then fail "the name taskto resolves"; else pass "the name taskto doesn't resolve"; fi
	must_not_reach "$worker" "$cli" "$server_ip" 8080 "the server ($server_ip:8080)"
	must_not_reach "$worker" "$cli" "$ollama_ip" 11434 "Ollama ($ollama_ip:11434)"
	must_not_reach "$worker" "$cli" "$gateway" 7000 "the host through its gateway ($gateway:7000)"
	must_not_reach "$worker" "$cli" "$host_ip" 7000 "the host's own address ($host_ip:7000)"
	must_not_reach "$worker" "$cli" "$lan" 8000 "a machine on the host's LAN ($lan:8000)"
	# Root in the worker has no NET_ADMIN to undo anything, and is held the same.
	must_not_reach "$worker" 0 "$lan" 8000 "the LAN, as the worker's root"
	must_not_reach "$worker" 0 "$server_ip" 8080 "the server, as the worker's root"

	echo "-- controls: the same listeners, from elsewhere"
	must_reach on-compose 0 "$server_ip" 8080 "the server, from the Compose network"
	must_reach on-compose 0 "$ollama_ip" 11434 "Ollama, from the Compose network"
	must_reach on-plain 0 "$host_ip" 7000 "the host, from another container"
	must_reach on-plain 0 "$lan" 8000 "the LAN machine, from another container"
	sh deploy/worker-firewall.sh del
	must_reach "$worker" "$cli" "$lan" 8000 "the LAN machine, from the worker without the firewall"
	must_reach "$worker" "$cli" "$host_ip" 7000 "the host, from the worker without the firewall"
	sh deploy/worker-firewall.sh add
	sh deploy/worker-firewall.sh check
	must_not_reach "$worker" "$cli" "$lan" 8000 "the LAN machine, with the firewall back"

	[ "$failed" = 0 ] || die "the worker's network isn't what it should be"
	echo "worker-network-test: OK"
}

case "${1:-}" in
inner) shift; inner "$@" ;;
*) outer "$@" ;;
esac
