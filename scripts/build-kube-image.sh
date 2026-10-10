#!/usr/bin/env bash
# Builds the kube engine's E2B guest image: images/e2b's userland (with envd,
# scripts/build-envd.sh) plus a static sandpit-agent as the entrypoint
# (images/kube/Containerfile).
#
#   ./scripts/build-kube-image.sh [tag]     # default sandpit-kube-e2b:dev
#
# Load it into a kind cluster with `kind load docker-image <tag> --name <cluster>`,
# or push it somewhere the cluster pulls from.
set -euo pipefail
cd "$(dirname "$0")/.."
tag=${1:-sandpit-kube-e2b:dev}
engine=${CONTAINER_ENGINE:-docker}

[ -x images/e2b/envd ] || ./scripts/build-envd.sh
"$engine" build -t sandpit-e2b:dev -f images/e2b/Containerfile images/e2b

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/sandpit-agent" ./cmd/sandpit-agent
"$engine" build -t "$tag" --build-arg USERLAND=sandpit-e2b:dev -f images/kube/Containerfile "$work"
echo "built $tag"
