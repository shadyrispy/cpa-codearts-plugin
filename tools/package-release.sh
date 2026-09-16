#!/usr/bin/env bash
# Package release assets in the layout the CLIProxyAPI plugin store requires.
#
# Produces, in release-assets/:
#   <id>_<version>_<goos>_<goarch>.zip
#   checksums.txt
#
# Requirements enforced by the installer (see the store README):
#   - one zip per platform, named <id>_<version>_<goos>_<goarch>.zip where
#     <version> is the release tag WITHOUT the leading v
#   - the dynamic library must sit at the zip ROOT, named <id>.<ext>
#     (nested libraries, absolute paths and zip-slip entries are rejected)
#   - checksums.txt in sha256sum format
#
# Usage:
#   ./tools/package-release.sh 0.1.0
#
# Cross-compiling other platforms needs a matching C toolchain because the
# plugin ABI is a C ABI; set CC_<GOOS>_<GOARCH> (for example CC_linux_amd64) or
# skip platforms you cannot build.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: $0 <version>   e.g. $0 0.1.0" >&2
  exit 1
fi
# The store rejects a leading v in the asset name.
VERSION="${VERSION#v}"

PLUGIN_ID="codearts-provider"
SRC_DIR="$(pwd)"
OUT_DIR="$SRC_DIR/release-assets"

# goos/goarch/ext triples to build. Trim this list if you only ship one platform.
PLATFORMS="${PLATFORMS:-windows/amd64 linux/amd64 darwin/amd64 darwin/arm64 linux/arm64}"

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

# zip_tool is "zip" when the zip(1) binary is usable, or the name of a working
# Python interpreter; empty means neither was found. It is resolved once because
# the same decision drives both the archive and the checksum step.
zip_tool=""
if command -v zip >/dev/null 2>&1; then
  zip_tool="zip"
else
  for candidate in python3 python py; do
    if command -v "$candidate" >/dev/null 2>&1 &&
       "$candidate" -c 'import zipfile' >/dev/null 2>&1; then
      zip_tool="$candidate"
      break
    fi
  done
fi
if [ -z "$zip_tool" ]; then
  echo "ERROR: no zip tool available (install zip(1) or a working Python 3)" >&2
  exit 1
fi

for platform in $PLATFORMS; do
  GOOS_TARGET="${platform%%/*}"
  GOARCH_TARGET="${platform##*/}"
  case "$GOOS_TARGET" in
    windows) EXT="dll" ;;
    darwin)  EXT="dylib" ;;
    *)       EXT="so" ;;
  esac

  LIB_NAME="${PLUGIN_ID}.${EXT}"
  ZIP_NAME="${PLUGIN_ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip"

  # Per-platform C compiler override, e.g. CC_linux_amd64=aarch64-linux-gnu-gcc
  cc_var="CC_${GOOS_TARGET}_${GOARCH_TARGET}"
  if [ -n "${!cc_var:-}" ]; then
    export CC="${!cc_var}"
  fi

  echo "==> building ${GOOS_TARGET}/${GOARCH_TARGET}"
  stage="$(mktemp -d)"
  if ! CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
       go build -trimpath -buildmode=c-shared -ldflags "-s -w" \
       -o "$stage/$LIB_NAME" . ; then
    echo "    skipped ${GOOS_TARGET}/${GOARCH_TARGET} (no C toolchain for this target)" >&2
    rm -rf "$stage"
    continue
  fi

  # Zip with the library at the root and nothing else: the installer rejects
  # nested dynamic libraries and extra copies.
  #
  # `zip` is not present in every environment (notably Git Bash on Windows), so
  # fall back to Python's zipfile, which produces an equivalent archive.
  # Resolution order: zip(1), then a working Python 3. On Windows the
  # python3.exe shim from the Microsoft Store exists but does nothing, so each
  # candidate is probed for real by asking it to import zipfile.
  if [ "$zip_tool" = "zip" ]; then
    ( cd "$stage" && zip -q -X "$OUT_DIR/$ZIP_NAME" "$LIB_NAME" )
  else
    LIB_NAME="$LIB_NAME" ZIP_PATH="$OUT_DIR/$ZIP_NAME" STAGE="$stage" "$zip_tool" -c '
import os, zipfile
stage = os.environ["STAGE"]
lib = os.environ["LIB_NAME"]
out = os.environ["ZIP_PATH"]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
    # arcname keeps the library at the archive root, as the installer requires.
    zf.write(os.path.join(stage, lib), arcname=lib)
'
  fi
  rm -rf "$stage"
  echo "    $ZIP_NAME"
done

if [ -z "$(ls -A "$OUT_DIR" 2>/dev/null)" ]; then
  echo "no platform was built; nothing to package" >&2
  exit 1
fi

# checksums.txt must use sha256sum format: "<sha256>  <filename>".
#
# The installer splits each line on whitespace, lower-cases the hash, strips a
# leading "*" (GNU binary-mode marker) but NOT a "./" prefix, and then looks the
# asset up by its bare file name. So the names written here must be plain file
# names: "./x.zip" would parse but fail lookup with "checksum for x.zip not
# found". The Python path is used whenever zip(1) was unavailable, because the
# GNU tools are frequently missing alongside it.
if [ "$zip_tool" != "zip" ]; then
  OUT_DIR="$OUT_DIR" "$zip_tool" -c "$(cat <<'PYSUM'
import hashlib, os
out_dir = os.environ["OUT_DIR"]
lines = []
for name in sorted(n for n in os.listdir(out_dir) if n.endswith(".zip")):
    digest = hashlib.sha256()
    with open(os.path.join(out_dir, name), "rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            digest.update(block)
    lines.append("%s  %s" % (digest.hexdigest(), name))
with open(os.path.join(out_dir, "checksums.txt"), "w", newline="\n") as fh:
    fh.write("\n".join(lines) + "\n")
PYSUM
)"
elif command -v sha256sum >/dev/null 2>&1; then
  # Strip the "./" prefix sha256sum adds for directory arguments.
  ( cd "$OUT_DIR" && sha256sum ./*.zip | sed 's#  \./#  #' > checksums.txt )
elif command -v shasum >/dev/null 2>&1; then
  ( cd "$OUT_DIR" && shasum -a 256 ./*.zip | sed 's#  \./#  #' > checksums.txt )
else
  echo "ERROR: no checksum tool available (install zip(1)/sha256sum(1)/shasum(1), or Python 3)" >&2
  exit 1
fi

echo
echo "release assets in $OUT_DIR:"
ls -la "$OUT_DIR"
echo
echo "Attach every .zip plus checksums.txt to the GitHub release tagged v$VERSION."
