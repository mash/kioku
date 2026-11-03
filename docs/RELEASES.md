# Releases

One tag `vX.Y.Z` names one release of this repository. The server (`kiokud`), the CLI (`kioku`) and the plugin report the same version.

## Make a release

1. Set `version` in `plugin/.claude-plugin/plugin.json` to a new `X.Y.Z`. The new version must be greater than the latest release. Claude Code updates the plugin on any change of `version`, so every plugin change that users must get needs a bump. Commit only this change; the `release` skill (`.claude/skills/release/SKILL.md`) does this on a local `release/vX.Y.Z` branch and merges it into `main`.
2. Push `main`. `.github/workflows/release.yml` then makes the tag `vX.Y.Z` on the bump commit and publishes the release, marked latest.

Nobody pushes tags by hand. The bump commit is the latest commit that changed the `"version"` line of `plugin.json`.

The workflow:

- fails when the version is not `X.Y.Z`, when the tag exists without a published release, or when the version is not greater than the latest release;
- succeeds without changes when the release is already published;
- in the `build` job (`contents: read`), checks out the bump commit, runs the race tests and vet, and runs `make release`;
- in the `publish` job (`contents: write`), deletes a draft left by a failed run and runs `gh release create vX.Y.Z --target <bump commit> --latest`.

After a failed run, fix the cause and run `gh workflow run release.yml -R mash/kioku`. The retry names the same bump commit. Until the release is published, the plugin hook keeps the CLI it has.

## Release assets

Each release has four archives `kioku_vX.Y.Z_<os>_<arch>.tar.gz` (`darwin` or `linux`, `amd64` or `arm64`) and `SHA256SUMS.txt`. Each archive holds one executable, `kioku`. The archives contain no server, account configuration or infrastructure files. The binaries are not Developer ID signed or Apple-notarized.

The plugin's SessionStart hook reads `version` from `plugin.json`, downloads `SHA256SUMS.txt` and the archive from `https://github.com/mash/kioku/releases/download/vX.Y.Z/`, and checks the archive against its line in `SHA256SUMS.txt`. Immutable releases stop changes to the assets after publishing. The checksum detects transfer damage. See [plugin/README.md](../plugin/README.md).

## Versions in builds

`kioku version` reports the version and the commit without reading login configuration.

| Build | Version | Commit |
|---|---|---|
| `make release VERSION=vX.Y.Z COMMIT=<sha>` | `VERSION` (form `vX.Y.Z`) | `COMMIT` (full SHA, default `HEAD`) |
| `make build` | `git describe --tags --always`, for example `v0.2.0` or `v0.2.0-3-gabc1234` | `HEAD` |
| `Dockerfile` | build arg `VERSION` (default `dev`) | build arg `COMMIT` (default `unknown`) |

`make release` writes the four archives, `SHA256SUMS.txt` and `RELEASE_NOTES.md` into `dist/`. The `dist` directory must not exist. To check a release build locally, run:

```bash
make release VERSION=v0.1.0 COMMIT=FULL_40_CHARACTER_COMMIT_SHA
```

A local run pushes no tag and creates no release.
