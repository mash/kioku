#!/bin/sh
# SessionStart accepts plain stdout as developer context. No server calls here.
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) || exit 0
printf 'Kioku plugin paths (literal filesystem paths, not shell commands):\n'
printf 'Skill: %s/skills/kioku/SKILL.md\nCLI: %s/bin/kioku\n' "$ROOT" "$ROOT"
if [ ! -x "$ROOT/bin/kioku" ] || [ ! -f "$ROOT/skills/kioku/SKILL.md" ]; then
    printf 'Kioku plugin is incomplete. Report this and reinstall before Kioku operations.\n'
    exit 0
fi
cat "$ROOT/prompts/session-start.txt"
exit 0
