#!/usr/bin/env bash
# Build the CodeArts Doer CLIProxyAPI plugin as a native dynamic library.
#
# The plugin ABI is a C ABI, so CGO and a C toolchain are mandatory.
#
# Usage:
#   ./build.sh                 # build for the host platform into ./plugins/<os>/<arch>/
#   ./build.sh linux amd64     # cross-compile with a matching C cross toolchain
#
# Environment:
#   CC          C compiler to use (default: gcc)
#   OUT_DIR     output directory (default: plugins/<GOOS>/<GOARCH>)
set -euo pipefail

cd "$(dirname "$0")"

GOOS_TARGET="${1:-$(go env GOOS)}"
GOARCH_TARGET="${2:-$(go env GOARCH)}"

case "$GOOS_TARGET" in
  windows) EXT="dll" ;;
  darwin)  EXT="dylib" ;;
  *)       EXT="so" ;;
esac

OUT_DIR="${OUT_DIR:-plugins/${GOOS_TARGET}/${GOARCH_TARGET}}"
mkdir -p "$OUT_DIR"

echo "building plugin for ${GOOS_TARGET}/${GOARCH_TARGET} -> ${OUT_DIR}/codearts-provider.${EXT}"

CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w" \
  -o "${OUT_DIR}/codearts-provider.${EXT}" .

echo
echo "done. Verify the ABI exports with:"
case "$GOOS_TARGET" in
  windows) echo "  objdump -p ${OUT_DIR}/codearts-provider.${EXT} | grep -A6 'Ordinal/Name Pointer'" ;;
  darwin)  echo "  nm -gU ${OUT_DIR}/codearts-provider.${EXT} | grep cliproxy" ;;
  *)       echo "  nm -D --defined-only ${OUT_DIR}/codearts-provider.${EXT} | grep cliproxy" ;;
esac
