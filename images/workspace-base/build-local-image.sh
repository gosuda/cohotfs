#!/bin/sh
set -eu

if [ "$#" -gt 1 ]; then
	printf 'usage: %s [image-tag]\n' "$0" >&2
	exit 2
fi

image=${1:-ghcr.io/gosuda/cohotfs/workspace-base:dev}
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
staging=$(mktemp -d "${TMPDIR:-/tmp}/cohotfs-image.XXXXXX")
trap 'rm -rf "$staging"' EXIT HUP INT TERM

if ! command -v docker >/dev/null 2>&1; then
	printf 'docker is required to build %s\n' "$image" >&2
	exit 3
fi

(
	cd "$repo_root"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags='-s -w' \
		-o "$staging/cohotfs-agent" ./cmd/cohotfs-agent
)
cp "$repo_root/images/workspace-base/Containerfile" "$staging/Containerfile"
agent_sha256=$(sha256sum "$staging/cohotfs-agent" | cut -d' ' -f1)

docker build \
	--platform linux/amd64 \
	--tag "$image" \
	--build-arg "AGENT_SHA256=$agent_sha256" \
	--build-arg "COHOTFS_VERSION=dev" \
	--file "$staging/Containerfile" \
	"$staging"

printf 'built %s (agent sha256: %s)\n' "$image" "$agent_sha256"
