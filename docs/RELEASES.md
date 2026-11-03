# CLI builds and releases

This repository builds the CLI for macOS and Linux, each on amd64 and arm64. The release archives contain one executable named `kioku`; they do not include the server, account configuration or infrastructure files. `kioku version` reports the version, source revision and build time without reading login configuration.

## Local verification

From a clean reviewed checkout, choose a version and its source commit, then run:

```bash
make release VERSION=v0.1.0 COMMIT=FULL_40_CHARACTER_COMMIT_SHA
```

`COMMIT` defaults to `HEAD`. The `dist` directory must not already exist. The target cross-compiles with CGO disabled, strips local build paths/VCS metadata, embeds the supplied version and commit, and creates four `.tar.gz` archives, `SHA256SUMS.txt` and release notes. It executes the native-platform client to verify the embedded metadata. For a real release, the supplied commit must identify the checkout being built; the workflow resolves that automatically from an existing source tag.

Archive checksums detect corruption; binaries are not currently Developer ID signed or Apple-notarized. Install the extracted executable on your PATH and verify `kioku version` before authenticating.

## GitHub Actions

`.github/workflows/release.yml` runs on `v*` tag pushes or manual dispatch. The source must be an existing `vX.Y.Z` tag (optional prerelease suffix), reachable from `main`. The workflow checks out the resolved commit, runs the full race suite and vet, builds all archives, and retains the artifacts for 14 days.

Manual dispatch defaults to **artifacts only**. Tag pushes, or dispatch with `publish_draft=true`, create a **draft release**. A maintainer reviews the assets and explicitly publishes the draft. Existing releases are never overwritten by the workflow; use a new version for changed binaries.

The workflow publishes to this repository with `github.token`; it needs no other token or variable. The release tag is the source tag itself, `vX.Y.Z`, with no prefix. Each release has four archives named `kioku_<version>_<os>_<arch>.tar.gz` (`darwin` or `linux`, `amd64` or `arm64`) and `SHA256SUMS.txt`, so the download URL is `https://github.com/mash/kioku/releases/download/vX.Y.Z/kioku_vX.Y.Z_<os>_<arch>.tar.gz`.

The publish job downloads only the release artifacts and does not check out the repository. It uploads the four client archives and the checksums. `gh release create --verify-tag` fails when the tag does not exist, so the job never creates a new tag.

After a maintainer publishes the draft, pin the plugin to it: run `plugin/tools/pin-cli.sh vX.Y.Z` and bump `plugin/.claude-plugin/plugin.json` in the same commit. The plugin's SessionStart hook then downloads that version and checks it against the pinned checksums. See [plugin/README.md](../plugin/README.md).

Preparing these files locally runs no pipeline and pushes no tag or release.
