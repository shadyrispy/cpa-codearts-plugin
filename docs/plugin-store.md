## Publishing to the CLIProxyAPI plugin store

The official store (`router-for-me/CLIProxyAPI-Plugins-Store`) hosts only
`registry.json`; your binaries stay in your own repository. There are two ways a
gateway can consume a plugin: the official store, or your own registry URL added
through `plugins.store-sources`.

### 1. Prepare a release

Publish this code to your own GitHub repository, then:

```bash
# The tag MUST be v<dotted numeric version>.
git tag v0.1.1
git push origin v0.1.1

# Build the per-platform zips and checksums.txt into ./release-assets/
bash tools/package-release.sh 0.1.1
```

Asset names use the version **without** the leading `v`:

```text
codearts-provider_0.1.1_windows_amd64.zip
codearts-provider_0.1.1_linux_amd64.zip
checksums.txt
```

Each zip must contain the dynamic library **at the archive root**, named
`<id>.<ext>` (`codearts-provider.dll`, `codearts-provider.so`, `codearts-provider.dylib`). Nested libraries,
extra files, absolute paths and zip-slip entries are rejected by the installer.

`checksums.txt` uses `<sha256>  <filename>` with **bare file names**. The host's
parser splits on whitespace and strips a leading `*` (GNU binary-mode marker) but
*not* a `./` prefix, so a line like `./codearts-provider_0.1.1_windows_amd64.zip` would parse
yet fail lookup with "checksum not found". `tools/package-release.sh` emits the
correct form and works whether or not `zip(1)`/`sha256sum(1)` are installed.

Attach the current version's zips plus `checksums.txt` to the release.

### 2. Get into the official store

Fork
[`CLIProxyAPI-Plugins-Store`](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store),
add an entry to `registry.json`, and open a pull request. A ready-to-edit entry is
in [`registry-entry.json`](registry-entry.json).

Required fields: `id`, `name`, `description`, `author`, `repository`. Optional:
`version`, `logo`, `homepage`, `license`, `tags`. Validation rules the host
enforces:

- `schema_version` must be `1` (or `2` for a `direct` install plan).
- `id` must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}` and be unique.
- `repository` must be exactly `https://github.com/{owner}/{repo}`.
- `version`, when present, is only a **display fallback**; the installed version
  comes from the GitHub latest release tag. It must not start with `v`.

The store README asks the pull request to include the repository URL, the latest
release tag in `v<version>` form, evidence that the zip and `checksums.txt` exist
in that release, and a short description of the capability added.

Under `schema_version: 1` every entry installs from a GitHub release. Schema
version 2 additionally allows a `direct` install plan with explicit per-platform
artifact URLs and sha256 digests — undocumented in the development guide but
supported by the host.

### 3. Or skip the store entirely

Store approval is not required to use your own build. Either place the library in
the plugin directory yourself (see *Install* above), or publish your own
`registry.json` and point the gateway at it:

This repository already contains a complete [`registry.json`](../registry.json).
Its default branch is `master`, so after committing and pushing that file the
third-party source URL is:

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-codearts-plugin/master/registry.json
```

The general GitHub raw URL format is
`https://raw.githubusercontent.com/<owner>/<repo>/<branch>/registry.json`.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/zyxzjyzjj/cpa-codearts-plugin/master/registry.json"
```

The official store is always consulted first and cannot be removed. A source ID is
derived from a hash of the URL, so the same file served from two URLs counts as
two distinct sources, and an ID collision between two URLs is a hard error.

### 4. Install from the store

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" \
  http://localhost:8317/v0/management/plugin-store

curl -X POST -H "Authorization: Bearer $ADMIN_KEY" \
  "http://localhost:8317/v0/management/plugin-store/codearts-provider/install?source=<source_id>"
```

The store listing supplies `<source_id>`. It is required only when the same
plugin ID is present in multiple sources.

Installing writes the dynamic library and sets that plugin's `enabled: true`, but
does **not** switch on the global `plugins.enabled`. Use `?source=<sourceID>` when
two registries define the same plugin ID. Because a loaded DLL cannot be
overwritten on Windows, updating a running plugin reports a restart conflict.
