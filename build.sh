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
#   OUT_NAME    library file name (default: codearts-provider.<ext>)
#   LDFLAGS     override the linker flags (default: -s -w -extldflags "-Wl,-s")
#
# Linux targets must match the libc of the container CLIProxyAPI runs in, because
# the plugin is dlopen()ed into that process:
#
#   glibc (debian/ubuntu images, and the official CLIProxyAPI Dockerfile):
#     CC="zig cc -target x86_64-linux-gnu.2.17" ./build.sh linux amd64
#   musl (alpine images):
#     CC="zig cc -target x86_64-linux-musl" ./build.sh linux amd64
#
# The glibc build links against libc.so.6/libpthread.so.0/libresolv.so.2 and needs
# no symbol newer than GLIBC_2.3.2. Verify the result with:
#   readelf -d plugins/linux/amd64/codearts-provider.so | grep NEEDED
#   objdump -T plugins/linux/amd64/codearts-provider.so | grep cliproxy
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
OUT_NAME="${OUT_NAME:-codearts-provider.${EXT}}"
LDFLAGS="${LDFLAGS:--s -w -extldflags \"-Wl,-s\"}"
mkdir -p "$OUT_DIR"

echo "building plugin for ${GOOS_TARGET}/${GOARCH_TARGET} -> ${OUT_DIR}/${OUT_NAME}"

# -s -w strips the Go symbol table and DWARF; the extldflags strip removes the
# same tables from the C/linker side, which cgo's external linking adds back.
# shellcheck disable=SC2086
CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -trimpath -buildmode=c-shared \
  -ldflags "$LDFLAGS" \
  -o "${OUT_DIR}/${OUT_NAME}" .

echo
echo "done. Verify the ABI exports with:"
case "$GOOS_TARGET" in
  windows) echo "  objdump -p ${OUT_DIR}/${OUT_NAME} | grep -A6 'Ordinal/Name Pointer'" ;;
  darwin)  echo "  nm -gU ${OUT_DIR}/${OUT_NAME} | grep cliproxy" ;;
  *)       echo "  objdump -T ${OUT_DIR}/${OUT_NAME} | grep cliproxy" ;;
esac
