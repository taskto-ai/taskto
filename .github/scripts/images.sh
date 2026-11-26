#!/bin/sh
# Builds taskto.ai's three images from the Dockerfile and pushes them to a container
# registry with immutable tags. Run by .github/workflows/images.yml (to ghcr.io); see it for
# when, and docs/install.md#prebuilt-images for how the images are used.
#
#   <registry>/<owner>/taskto             the server (the Dockerfile's last stage)
#   <registry>/<owner>/taskto-cli-worker  --target cli-worker
#   <registry>/<owner>/taskto-backup      --target backup
#
# The tag is the commit's full SHA on main, or the version tag (v*) on a tag. It is also the
# version the image reports (/healthz). A tag that already exists in the registry is never
# pushed again: tags are immutable, so a re-run leaves the published image as it was.
#
# Environment (the runner sets the GITHUB_* ones):
#   REGISTRY_TOKEN  a token that may write the owner's packages; without it, build only
#   REGISTRY_USER   the token's user (default: the repository's owner)
#   REGISTRY        registry host (default: the forge's own, from GITHUB_SERVER_URL; GitHub
#                   Actions sets ghcr.io, since github.com isn't a registry)
#   IMAGE_OWNER     the images' owner (default: the repository's owner), lowercased
#   PLATFORM        default linux/amd64
#   REGISTRY_INSECURE  set for a test registry on plain HTTP
set -eu

registry="${REGISTRY:-${GITHUB_SERVER_URL#*://}}"
registry="${registry%/}"
# Image names are lowercase; an organisation's name may not be.
owner="$(printf '%s' "${IMAGE_OWNER:-${GITHUB_REPOSITORY_OWNER:?}}" | tr '[:upper:]' '[:lower:]')"
sha="${GITHUB_SHA:?}"
# Links each image to its repository (on ghcr.io, the package page links back to it).
source="${GITHUB_SERVER_URL:?}/${GITHUB_REPOSITORY:?}"
platform="${PLATFORM:-linux/amd64}"

case "${GITHUB_REF:-}" in
refs/tags/v*) tag="${GITHUB_REF#refs/tags/}" ;;
*) tag="$sha" ;;
esac
# A Docker tag: letters, digits, '_', '.', '-', at most 128 characters.
if ! printf '%s' "$tag" | grep -Eq '^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'; then
	echo "::error::'$tag' can't be an image tag"
	exit 1
fi

# Push only from main or a version tag, on a push (or a manual run of one), with a token.
push=false
case "${GITHUB_EVENT_NAME:-}:${GITHUB_REF:-}" in
push:refs/heads/main | push:refs/tags/v* | workflow_dispatch:refs/heads/main | workflow_dispatch:refs/tags/v*)
	if [ -n "${REGISTRY_TOKEN:-}" ]; then
		push=true
	else
		echo "::warning::REGISTRY_TOKEN isn't set: the images are built but not pushed. Give the workflow a token that may write packages to publish them."
	fi
	;;
esac

docker buildx version >/dev/null 2>&1 || {
	echo "::error::docker buildx (BuildKit) is needed: the Dockerfile uses COPY --chmod and --platform"
	exit 1
}
echo "Building $tag for $platform on $(uname -m), images under $registry/$owner"

images="taskto: taskto-cli-worker:cli-worker taskto-backup:backup"
for entry in $images; do
	name="${entry%%:*}" target="${entry#*:}"
	ref="$registry/$owner/$name:$tag"
	echo "::group::build $ref"
	# shellcheck disable=SC2046 # the target flag is two words, or none
	docker buildx build --load --platform "$platform" \
		$([ -n "$target" ] && printf -- '--target %s' "$target") \
		--build-arg VERSION="$tag" \
		--label org.opencontainers.image.revision="$sha" \
		--label org.opencontainers.image.source="$source" \
		--label org.opencontainers.image.version="$tag" \
		-t "$ref" .
	echo "::endgroup::"
done

# The local tags go at the end, so the runner's disk doesn't fill up with images; BuildKit's
# cache stays, for the next build. The login lives in a config of its own, removed with it.
login_config=""
cleanup() {
	if [ -n "$login_config" ]; then
		DOCKER_CONFIG="$login_config" docker logout "$registry" >/dev/null 2>&1 || true
		rm -rf "$login_config"
	fi
	for entry in $images; do
		docker image rm "$registry/$owner/${entry%%:*}:$tag" >/dev/null 2>&1 || true
	done
}
trap cleanup EXIT

if [ "$push" != true ]; then
	echo "Built, not pushed."
	exit 0
fi

# The token goes in a throwaway config, never the runner's own. That config has no
# contexts, so name the daemon the current context uses.
DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
export DOCKER_HOST
login_config="$(mktemp -d)"
export DOCKER_CONFIG="$login_config"
printf '%s' "$REGISTRY_TOKEN" | docker login "$registry" -u "${REGISTRY_USER:-$owner}" --password-stdin >/dev/null

for entry in $images; do
	ref="$registry/$owner/${entry%%:*}:$tag"
	# Present, absent, or an error we must not take for absent (that would overwrite a tag).
	if out="$(docker manifest inspect ${REGISTRY_INSECURE:+--insecure} "$ref" 2>&1)"; then
		echo "$ref is already in the registry: tags are immutable, so it isn't pushed again"
		continue
	fi
	denied=""
	case "$out" in
	*"denied"*) denied=yes ;;
	esac
	case "$out" in
	# A tag the registry doesn't have, or the first image of a name. ghcr.io answers
	# "denied" for a package that doesn't exist yet. Taking that for absent can't overwrite
	# a tag: a token that can't read the package can't push to it either.
	*"manifest unknown"* | *"no such manifest"* | *"not found"* | *"name unknown"* | *"denied"*) ;;
	*)
		echo "::error::can't tell whether $ref exists: $out"
		exit 1
		;;
	esac
	if ! docker push "$ref"; then
		echo "::error::pushing $ref failed${denied:+; the registry had also denied reading it, so check that the token may write this package}"
		exit 1
	fi
	digest="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | grep -m1 "^$registry/$owner/${entry%%:*}@" || true)"
	echo "pushed $ref ${digest:+($digest)}"
done
