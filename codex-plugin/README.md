# Kioku for Codex

The plugin includes the Kioku skill, a SessionStart hook, and the matching CLI for
macOS/Linux on arm64/amd64. No global Kioku installation or PATH changes are needed.
Windows native is not supported. The runtime needs POSIX sh and `sha256sum` or
`shasum`; Python and Go are needed only to build the package.

## Install a release archive

Verify the release archive against SHA256SUMS.txt, then extract
`kioku-codex_vX.Y.Z.tar.gz` to a directory you will keep:

```sh
codex plugin marketplace add /absolute/path/to/kioku-codex
codex plugin add kioku@kioku
```

In Codex desktop, use the local marketplace to install Kioku. Review and trust
its SessionStart hook using Codex's hook settings, then start a new session.
Installation does not automatically trust the hook. Hook definitions changed by
an update may require review again. The hook only supplies context; it never
logs in, pulls a wiki, sends sources, or downloads executable code.

The hook runs at startup, resume, clear, fork and after compaction. It asks Codex
to read the skill, select a relevant project, and pull/read its wiki before work.
It preserves the conversation's selected project and completed pull state.
Model compliance is not guaranteed by successful hook execution.

If the hook is not trusted, explicitly invoke the kioku skill. It resolves the
bundled CLI relative to its own SKILL.md. A missing/corrupt binary requires
reinstallation, not switching to a different CLI from PATH. The launcher checks
the bundled binary's SHA-256 on every invocation; that detects corruption, not
malicious replacement of both the binary and its checksum.

Human login still requires browser authentication. Follow the skill's terminal
command using the actual bundled CLI path. This package does not bundle an MCP
connection or grant server permissions. Never blanket-allow all CLI commands.

## Update and remove

Finish/close sessions using Kioku before updating: Codex deletes the old version's
cache during installation, so its absolute CLI path is no longer usable. Extract
the new release separately, keep the old release archive for rollback, then run:

```sh
codex plugin marketplace remove kioku
codex plugin marketplace add /absolute/path/to/new/kioku-codex
codex plugin add kioku@kioku
```

Removing the marketplace registration is necessary when its path changes. Do not
continue if one of these commands fails; otherwise reinstall can silently keep
the old source. Review any changed hook definition, start a new session, and
check its bundled CLI `version` against the intended release. Use the same steps
with the old extracted archive to roll back. The server's minimum CLI version may
prevent rollback from working. Do not roll back credentials or project data.
`codex plugin remove kioku@kioku` removes the installation; authentication,
project `.kioku/config`, and wiki files outside the plugin remain separate.

After verifying this plugin, remove any manually copied `~/.agents/skills/kioku`
from skill discovery to avoid duplicate or stale skills. Compare/back up local
changes first. The plugin does not edit AGENTS.md or user settings for you.

## Build and distribution

From the Kioku repository, run `make release VERSION=vX.Y.Z COMMIT=<full SHA>`.
The version must match `plugin/.claude-plugin/plugin.json`. This also generates
`dist/codex/marketplace`, a marketplace archive and an npm tarball. Only the
current canonical skill is used; Claude-specific startup/login text is adapted
at build time. The existing Claude package remains unchanged.

The package uses the documented `.codex-plugin/plugin.json` compatibility
manifest. On the tested Codex CLI `0.167.0-alpha.3.2`, emitting the portable
schema manifest as well caused hook discovery to return an empty list. The
compatibility manifest alone discovers both the skill and hook.

`make test-codex-plugin` tests packaging and runtime failure behavior.
Set `CODEX_TEST_BINARY` to the absolute Codex CLI path to also test actual
marketplace installation and app-server skill/hook discovery in an isolated
CODEX_HOME. This does not authenticate or trust hooks. The release
workflow uploads the two plugin archives alongside CLI archives. Packaging
never publishes npm packages or registers a marketplace on the developer's host.
The npm tarball includes all four binaries and needs no lifecycle scripts.
Its default name `kioku-codex-plugin` is a placeholder, not a reserved npm name.
Before publishing, verify ownership and pass `--npm-name` to the packaging tool.
An npm marketplace entry must pin the published version; installation requires
npm on the host. Release-archive installation above does not require npm.

Hook-containing plugins are for manually installed/local marketplaces, not the
public OpenAI plugin directory. See the official specifications:

- https://developers.openai.com/plugins/build/plugins
- https://learn.chatgpt.com/docs/hooks

Clean desktop hook trust and authenticated server operations must be validated
on the target host; fixture tests alone do not establish that integration.
