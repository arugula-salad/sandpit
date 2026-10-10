#!/usr/bin/env bash
# Builds the substrate engine's E2B guest image (images/substrate/Containerfile)
# and pushes it to a registry the cluster's workers pull from, printing the
# reference by digest an ActorTemplate needs.
#
#   ./scripts/build-substrate-image.sh [repo]   # default localhost:5001/sandpit-e2b-substrate
set -euo pipefail
cd "$(dirname "$0")/.."
repo=${1:-localhost:5001/sandpit-e2b-substrate}
engine=${CONTAINER_ENGINE:-docker}

[ -x images/e2b/envd ] || ./scripts/build-envd.sh
"$engine" build -q -t sandpit-e2b:dev -f images/e2b/Containerfile images/e2b >/dev/null
"$engine" build -q -t "$repo:dev" --build-arg USERLAND=sandpit-e2b:dev -f images/substrate/Containerfile images/substrate >/dev/null
"$engine" push -q "$repo:dev" >/dev/null
"$engine" inspect --format '{{index .RepoDigests 0}}' "$repo:dev"
