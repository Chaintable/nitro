#!/usr/bin/env bash
# Build the ApeChain nitro image from the checked-out commit. amd64 Linux only.
#
#   apechain/build.sh <version> [prior-image]
#
# <version>      e.g. v3.9.9. Image is tagged nitro-node:apechain-<version>-<short sha>.
# [prior-image]  production image whose machines are carried into the new image.
set -euo pipefail

: "${EXPECTED_ROOT:=0x2dc824fed99dcdf659f2523ad68d1ec70bd5f08e3c533996be3a2d2b19813e83}"
: "${APECHAIN_MARKER:=underflow detected in fixed}"
MACHINE=/home/user/target/machines/latest

VERSION=${1:?version required, e.g. v3.9.9}
PRIOR_IMAGE=${2:-}

cd "$(git rev-parse --show-toplevel)"

[ "$(uname -sm)" = "Linux x86_64" ] || { echo "build natively on amd64 Linux"; exit 1; }
[ -z "$(git status --porcelain --ignore-submodules=none)" ] || { echo "working tree or submodules not clean"; git status --short; exit 1; }
find . -name '._*' -print -quit | grep -q . && { echo "macOS ._ files present"; exit 1; }

REV=$(git rev-parse HEAD)
IMAGE="nitro-node:apechain-${VERSION}-${REV:0:7}"

docker build --target nitro-node -t "$IMAGE" \
  --label org.opencontainers.image.revision="$REV" \
  --label org.opencontainers.image.source="$(git remote get-url origin)" .

ROOT=$(docker run --rm --entrypoint cat "$IMAGE" "$MACHINE/module-root.txt")
[ "$ROOT" = "$EXPECTED_ROOT" ] || { echo "module root $ROOT != expected $EXPECTED_ROOT"; exit 1; }
MARKERS=$(docker run --rm --entrypoint grep "$IMAGE" -ac "$APECHAIN_MARKER" "$MACHINE/replay.wasm")
[ "$MARKERS" -ge 1 ] || { echo "replay.wasm has no ApeChain markers"; exit 1; }

if [ -n "$PRIOR_IMAGE" ]; then
  OLD_ROOT=$(docker run --rm --entrypoint cat "$PRIOR_IMAGE" "$MACHINE/module-root.txt")
  docker build -f apechain/Dockerfile.validator -t "$IMAGE" \
    --build-arg BASE_IMAGE="$IMAGE" --build-arg PRIOR_IMAGE="$PRIOR_IMAGE" --build-arg OLD_ROOT="$OLD_ROOT" \
    --label org.opencontainers.image.revision="$REV" apechain
fi

echo "built $IMAGE"
echo "  revision    $REV"
echo "  module root $ROOT"
docker run --rm --entrypoint ls "$IMAGE" /home/user/target/machines
