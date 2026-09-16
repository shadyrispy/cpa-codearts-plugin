# Creating a release

Step-by-step for this repository (`github.com/zyxzjyzjj/cpa-codearts-plugin`).
Work through it top to bottom; every command is copy-pasteable from Git Bash.

## TL;DR

On Windows, `release.bat` validates the repository and performs the commit,
`master` push, annotated tag and tag push. Double-click it for the version in
`main.go`, or preview without changing anything using:

```bat
release.bat 0.1.1 --dry-run
```

Use `release.bat 0.1.1 --yes` to skip its confirmation prompt.

```bash
# 1. Build the assets (tag and version must agree)
bash tools/package-release.sh 0.1.1

# 2. Commit the source, tag it, push both
git add -A && git commit -m "release: v0.1.1"
git tag v0.1.1
git push origin master
git push origin v0.1.1

# 3. Create the GitHub release and attach the assets
gh release create v0.1.1 --title "v0.1.1" --notes "Initial release." \
  release-assets/codearts-provider_0.1.1_*.zip release-assets/checksums.txt
```

Without `gh`, use the web UI: **Releases → Draft a new release → choose the tag
`v0.1.1` → attach every `.zip` plus `checksums.txt` → Publish**.

## Why the naming matters

The CLIProxyAPI plugin store installs from your **latest GitHub release**, and it
is strict about the artifacts. Getting these wrong produces a plugin that looks
published but fails to install.

| Requirement | Value here | What breaks otherwise |
| --- | --- | --- |
| Release tag | `v0.1.1` (leading `v`, dotted numeric) | The store derives the version from the tag; a non-numeric tag invalidates the entry. |
| Asset name | `codearts-provider_0.1.1_windows_amd64.zip` — version **without** `v` | The installer builds the expected name from the tag and finds nothing. |
| Zip contents | `codearts-provider.dll` at the **root**, nothing else | Nested libraries, extra files, absolute paths and zip-slip entries are rejected. |
| Checksums | `checksums.txt`, `<sha256>␣␣<bare-filename>` | A `./` prefix parses but then fails lookup with "checksum not found". |

`tools/package-release.sh` produces all of this correctly. Do not hand-build the
zips.

## 1. Build the release assets

The plugin ABI is a C ABI, so **CGO and a C toolchain are required**.

```bash
cd /c/Users/zjj/Downloads/huaweicloud.vscode-codebot-26.3.6/cpa-codearts-plugin

# Windows only (MinGW-w64 gcc must be on PATH, or set CC):
PLATFORMS="windows/amd64" bash tools/package-release.sh 0.1.1

# All platforms you have toolchains for:
bash tools/package-release.sh 0.1.1
```

The version argument must match the tag you are about to create, minus the `v`.

Platforms without a usable toolchain are **skipped with a warning** rather than
producing a broken archive, so a partial build is safe but check the output:

```
release assets in .../release-assets:
  codearts-provider_0.1.1_windows_amd64.zip
  checksums.txt
```

Cross-compiling needs a matching C compiler. Set a per-platform override:

```bash
CC_linux_amd64=x86_64-linux-gnu-gcc PLATFORMS="linux/amd64" bash tools/package-release.sh 0.1.1
```

**The Linux artifact must match the libc of the target container**, because
CLIProxyAPI `dlopen()`s the library into its own process. The official
CLIProxyAPI `Dockerfile` runs on Debian, so the release asset is a glibc build;
an Alpine deployment needs the musl build instead.

```bash
# glibc (default; zig cc is only needed when cross-compiling from another OS)
CC_linux_amd64="zig cc -target x86_64-linux-gnu.2.17" \
  PLATFORMS="linux/amd64" bash tools/package-release.sh 0.1.1

# musl / Alpine — ship it out of band or under its own tag: the store derives one
# asset name per GOOS/GOARCH from the release tag.
CC_linux_amd64="zig cc -target x86_64-linux-musl" \
  OUT_NAME=codearts-provider-musl.so bash build.sh linux amd64
```

Check the result before publishing:

```bash
readelf -d codearts-provider.so | grep NEEDED   # libc.so.6 => glibc, libc.so => musl
objdump -T codearts-provider.so | grep cliproxy # the four ABI exports must be present
```

Packaging is additive: `tools/package-release.sh` only writes this version's
archives and regenerates `checksums.txt` for this version. Earlier archives stay
in `release-assets/` but are not uploaded with the new release. Set
`PRUNE_ASSETS=1` if you do want older archives removed.

On Windows the practical answer is to build `windows/amd64` locally and let CI
build the rest (see below).

## 2. Verify before publishing

```bash
cd release-assets
sha256sum -c checksums.txt        # or: shasum -a 256 -c checksums.txt
unzip -l codearts-provider_0.1.1_windows_amd64.zip
```

The zip listing must show exactly one entry, at the root:

```
  codearts-provider.dll
```

## 3. Tag and push

```bash
git add -A
git commit -m "release: v0.1.1"
git tag v0.1.1
git push origin master
git push origin v0.1.1
```

If you already pushed the tag and need to move it:

```bash
git tag -f v0.1.1 && git push -f origin v0.1.1
```

Force-moving a tag on a published release is disruptive for anyone who already
installed it; prefer a new version.

## 4. Create the release

### With the GitHub CLI

```bash
gh release create v0.1.1 \
  --title "v0.1.1" \
  --notes "Initial release." \
  release-assets/codearts-provider_0.1.1_*.zip release-assets/checksums.txt
```

Use `--notes-file release-notes.md` instead of `--notes` to pull the description
from a file. Add `--draft` to stage it privately, then publish from the UI.

### With the web UI

1. Open `https://github.com/zyxzjyzjj/cpa-codearts-plugin/releases/new`
2. **Choose a tag** → `v0.1.1` (create it if you did not push it in step 3)
3. Title `v0.1.1`, add description
4. Drag in `release-assets/codearts-provider_0.1.1_*.zip` plus `release-assets/checksums.txt`
5. **Publish release**

A release without `checksums.txt` or without a zip for the user's platform cannot
be installed by the store.

## 5. Confirm the store can see it

```bash
curl -s https://api.github.com/repos/zyxzjyzjj/cpa-codearts-plugin/releases/latest \
  | grep -E '"(tag_name|name)"'
```

`tag_name` must be `v0.1.1`. Confirm the asset names in the same response:

```bash
curl -s https://api.github.com/repos/zyxzjyzjj/cpa-codearts-plugin/releases/latest \
  | grep '"name":' | grep -E '\.zip|checksums'
```

## 6. Publish the next version

Release assets come from the **latest** release, so the registry entry does not
change when you ship an update — only when metadata like the description does.

```bash
# bump the version in code, then:
bash tools/package-release.sh 0.1.1
git add -A && git commit -m "release: v0.1.1"
git tag v0.1.1 && git push origin master && git push origin v0.1.1
gh release create v0.1.1 --title "v0.1.1" --notes "Fixes ..." \
  release-assets/codearts-provider_0.1.1_*.zip release-assets/checksums.txt
```

Installing the update through the gateway:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" \
  http://localhost:8317/v0/management/plugin-store/codearts-provider/install
```

On Windows a loaded DLL cannot be overwritten: the install reports a conflict and
you restart the gateway to finish.

## Building the other platforms in CI

The repository includes
[`/.github/workflows/release.yml`](../.github/workflows/release.yml). Pushing a
tag such as `v0.1.1` runs tests, builds Linux/glibc amd64 and Windows amd64,
checks both ABIs and archives, then creates the GitHub Release with the two zips
and `checksums.txt`. The workflow can also be rerun manually with an existing
tag; existing assets are replaced with the rebuilt copies.

The job requests only `contents: write` and uses GitHub's built-in token.
Protected environments and personal access tokens are not required. If an
organization policy blocks write-capable workflow tokens, allow this repository
to create releases under Settings → Actions → General.

macOS targets (`darwin/amd64`, `darwin/arm64`) require a macOS runner because
cross-compiling cgo for Darwin from Linux needs osxcross and the Apple SDK.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `go build` fails with "cgo: C compiler not found" | No C toolchain on PATH. Install MinGW-w64 and set `CC` to `gcc.exe`. |
| `skipped <platform> (no C toolchain for this target)` | Expected for platforms you cannot cross-compile; set `CC_<goos>_<goarch>` to enable one. |
| Store reports "checksum for x not found" | `checksums.txt` has a `./` prefix. Regenerate with `tools/package-release.sh`. |
| Store install finds no asset | Tag/asset version mismatch, or a missing `checksums.txt`. |
| `bash: ./build.sh: /bin/bash^M` on Linux | CRLF line endings. `.gitattributes` prevents this; run `dos2unix build.sh` if a file slipped through. |
| Install returns a conflict on Windows | The DLL is loaded. Restart the gateway, then retry. |
