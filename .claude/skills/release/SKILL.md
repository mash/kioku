---
name: release
description: Release a new Kioku version. Bumps `version` in plugin/.claude-plugin/plugin.json on a local release branch, commits, merges into main, pushes, and watches release.yml make the tag and GitHub Release. Use when the user says "release", "bump the version", "cut a release", "リリースして", "バージョンを上げて". Argument: patch (default), minor, major, or an explicit X.Y.Z.
---

# Release

One version names the server (`kiokud`), the CLI (`kioku`) and the plugin. Its source is `version` in `plugin/.claude-plugin/plugin.json`. A push to `main` that changes that line starts `.github/workflows/release.yml`, which tests, builds the CLI archives, and creates tag `vX.Y.Z` with a published release. Nobody pushes tags by hand.

## Steps

1. Check the checkout. Stop and tell the user if any check fails.
   - `git status --porcelain` is empty.
   - `git fetch origin` and `git rev-parse main` equals `git rev-parse origin/main`.
   - The latest `ci.yml` push run on `main` succeeded: `gh run list -R mash/kioku --workflow ci.yml --branch main --event push --limit 1 --json conclusion,headSha`. Its `headSha` is `origin/main`.
2. Read the current version: `sed -n 's/.*"version"[ ]*:[ ]*"\([0-9.]*\)".*/\1/p' plugin/.claude-plugin/plugin.json`. Read the latest release: `gh release view -R mash/kioku --json tagName --jq .tagName` (none is fine for the first release).
3. Compute the new version `X.Y.Z` from the argument: `patch` (default), `minor`, `major`, or an explicit `X.Y.Z`. It must match `^[0-9]+\.[0-9]+\.[0-9]+$` and be greater than the latest release (`sort -V`). Tag `vX.Y.Z` must not exist: `git ls-remote --tags origin vX.Y.Z` prints nothing.
4. Show the user the current version, the latest release, the new version, and `git log --oneline <latest tag>..origin/main` (all commits if no release). Ask for confirmation, because the push in step 6 publishes a release.
5. Make the commit on a release branch:
   ```bash
   git switch -c release/vX.Y.Z main
   # edit only the "version" value in plugin/.claude-plugin/plugin.json
   git commit -am "Release vX.Y.Z"
   git switch main
   git merge --ff-only release/vX.Y.Z
   git branch -d release/vX.Y.Z
   ```
   `git diff HEAD~1` must show exactly one changed line.
6. Push: `git push origin main`.
7. Watch the release run and report the result:
   ```bash
   gh run list -R mash/kioku --workflow release.yml --limit 1 --json databaseId,headSha
   gh run watch <id> -R mash/kioku --exit-status
   gh release view vX.Y.Z -R mash/kioku --json url,assets --jq '.url, (.assets[].name)'
   ```
   Expect four `kioku_vX.Y.Z_<os>_<arch>.tar.gz` archives and `SHA256SUMS.txt`.
8. Tell the user the release URL. Deploying is separate: `gh workflow run deploy.yml -R mash/kioku-deploy` deploys the latest release.

## When the release run fails

`main` then names a version that has no release. The plugin hook keeps each user's current CLI, so nothing breaks, but new users get remote mode. Read the failed log (`gh run view <id> -R mash/kioku --log-failed`), fix the cause in a normal commit on `main`, then retry with `gh workflow run release.yml -R mash/kioku`. The retry tags the same release commit. Do not bump the version again to retry.
