#!/usr/bin/env bash
# Build the stackchan-mcp firmware with this repository's patches.
# Run inside WSL (Docker is used for ESP-IDF):
#
#   wsl -d Ubuntu-22.04 -- bash /mnt/c/Users/<you>/stackchan-cli/scripts/build-firmware.sh [tag]
#
# 1. clones kisaragi-mochi/stackchan-mcp to ~/stackchan-mcp (once),
# 2. checks the upstream tag (default below) out in a separate build tree,
#    ~/stackchan-mcp-build (a git worktree; its local changes are discarded),
# 3. applies firmware-patches/*.patch there (git apply: no commits),
# 4. runs the host tests (upstream's and the patches') and builds with
#    espressif/idf in Docker,
# 5. copies the app (xiaozhi.bin), assets and merged binary to
#    firmware/<tag>-local/ in this repository (git-ignored).
#
# Patches are made in ~/stackchan-mcp on the branch "local" (the tag plus one
# commit per patch), then exported:
#   cd ~/stackchan-mcp && git format-patch --no-signature -o <repo>/firmware-patches <tag>..local
set -euo pipefail

TAG="${1:-firmware-v1.18.0}"
IDF_IMAGE="espressif/idf:v5.5.2"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${STACKCHAN_MCP_SRC:-$HOME/stackchan-mcp}"
BUILD="${STACKCHAN_MCP_BUILD:-$HOME/stackchan-mcp-build}"
OUT="$REPO/firmware/$TAG-local"

if [ ! -d "$SRC/.git" ]; then
    git clone https://github.com/kisaragi-mochi/stackchan-mcp.git "$SRC"
fi
# --force: take a tag that upstream moved instead of failing on it.
git -C "$SRC" fetch --tags --force --quiet origin
if [ ! -e "$BUILD/.git" ]; then
    git -C "$SRC" worktree add --quiet --detach "$BUILD" "$TAG"
fi
cd "$BUILD"
git checkout --quiet --force --detach "$TAG"
# Keep the downloaded components and the host test build between builds.
git clean -fdxq -e managed_components -e dependencies.lock -e .host_test_build
git submodule update --init --recursive --quiet
shopt -s nullglob
patches=("$REPO"/firmware-patches/*.patch)
if [ ${#patches[@]} -gt 0 ]; then
    git apply "${patches[@]}"
fi
echo "source: $TAG + ${#patches[@]} patch(es)"

# The container runs as root; hand the tree back even when the build fails,
# or the next git clean cannot remove its files.
docker run --rm -v "$BUILD/firmware:/project" -w /project "$IDF_IMAGE" bash -c '
    set -eo pipefail
    trap "chown -R '"$(id -u):$(id -g)"' /project" EXIT
    {
        cmake -S host_test -B .host_test_build -G Ninja -DFETCHCONTENT_UPDATES_DISCONNECTED=ON &&
        cmake --build .host_test_build
    } >/tmp/host_test.log 2>&1 || { tail -40 /tmp/host_test.log; exit 1; }
    (cd .host_test_build && ctest --output-on-failure >/tmp/ctest.log 2>&1) || { tail -60 /tmp/ctest.log; exit 1; }
    tail -3 /tmp/ctest.log
    python ./scripts/release.py stackchan >/tmp/release.log 2>&1 || { tail -40 /tmp/release.log; exit 1; }
'

mkdir -p "$OUT"
cp "$BUILD/firmware/build/xiaozhi.bin" "$BUILD/firmware/build/merged-binary.bin" "$OUT/"
# The assets partition image (written at 0x800000, per build/flasher_args.json).
cp "$BUILD/firmware/build/generated_assets.bin" "$OUT/assets.bin"
for p in "${patches[@]}"; do basename "$p"; done > "$OUT/patches.txt"
ls -l "$OUT"
