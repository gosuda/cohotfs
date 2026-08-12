#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
staging=$(mktemp -d "${TMPDIR:-/tmp}/cohotfs-image.XXXXXX")
trap 'rm -rf "$staging"' EXIT HUP INT TERM

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$staging/cohotfs-agent" "$repo_root/cmd/cohotfs-agent"
cp "$repo_root/images/workspace-base/Containerfile" "$staging/Containerfile"
agent_sha256=$(sha256sum "$staging/cohotfs-agent" | cut -d' ' -f1)
printf '%s\n' "$agent_sha256" > "$staging/agent.sha256"
printf '%s\n' "$staging"
printf 'agent sha256: %s\n' "$agent_sha256"
printf 'build: docker buildx build --platform linux/amd64 --build-arg AGENT_SHA256=%s %s\n' "$agent_sha256" "$staging"
