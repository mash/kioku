#!/bin/sh
exec 2>/dev/null
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cat >/dev/null
DATA=${CLAUDE_PLUGIN_DATA:-"$HOME/.local/share/kioku"}
TARGET="$DATA/bin/kioku"
T=
trap '[ -n "$T" ] && rm -rf "$T"' 0

# Print the version field of the CLI at TARGET. Fail when that CLI does not work.
# The pattern needs a quote before "version", so it does not match go_version.
current_version() {
    [ -f "$TARGET" ] && [ -x "$TARGET" ] || return 1
    OUT=$("$TARGET" version </dev/null) || return 1
    printf '%s\n' "$OUT" | sed -n 's/.*"version"[ ]*:[ ]*"\([^"]*\)".*/\1/p'
}

new_temp() {
    mkdir -p "$DATA/bin" "$DATA/tmp" || return 1
    T=$(mktemp -d "$DATA/tmp/dl.XXXXXX")
}

copy_override() {
    [ -f "$KIOKU_CLI" ] || return 1
    new_temp || return 1
    cp "$KIOKU_CLI" "$T/kioku" || return 1
    chmod 0755 "$T/kioku" || return 1
    mv -f "$T/kioku" "$TARGET"
}

download() {
    [ -r "$ROOT/cli-pin/version" ] || return 1
    VERSION=$(cat "$ROOT/cli-pin/version")
    [ -n "$VERSION" ] || return 1
    if CURRENT=$(current_version) && [ "$CURRENT" = "$VERSION" ]; then
        return 0
    fi
    case $(uname -s) in
        Darwin) OS=darwin ;;
        Linux) OS=linux ;;
        *) return 1 ;;
    esac
    case $(uname -m) in
        x86_64|amd64) ARCH=amd64 ;;
        arm64|aarch64) ARCH=arm64 ;;
        *) return 1 ;;
    esac
    command -v curl >/dev/null || return 1
    command -v tar >/dev/null || return 1
    if command -v sha256sum >/dev/null; then
        HASH=sha256sum
    elif command -v shasum >/dev/null; then
        HASH=shasum
    else
        return 1
    fi
    ARCHIVE="kioku_${VERSION}_${OS}_${ARCH}.tar.gz"
    # tools/pin-cli.sh writes one lower-case "<hash>  <name>" line per archive.
    LINES=$(grep -c "  $ARCHIVE\$" "$ROOT/cli-pin/SHA256SUMS.txt") || return 1
    [ "$LINES" = 1 ] || return 1
    EXPECTED=$(grep "  $ARCHIVE\$" "$ROOT/cli-pin/SHA256SUMS.txt") || return 1
    EXPECTED=${EXPECTED%% *}
    [ ${#EXPECTED} = 64 ] || return 1
    new_temp || return 1
    BASE=${KIOKU_CLI_DOWNLOAD_BASE:-https://github.com/mash/kioku/releases/download}
    curl -fsSL --connect-timeout 10 --max-time 90 -o "$T/a.tar.gz" "$BASE/$VERSION/$ARCHIVE" >/dev/null || return 1
    if [ "$HASH" = sha256sum ]; then
        ACTUAL=$(sha256sum < "$T/a.tar.gz") || return 1
    else
        ACTUAL=$(shasum -a 256 < "$T/a.tar.gz") || return 1
    fi
    ACTUAL=${ACTUAL%% *}
    [ "$ACTUAL" = "$EXPECTED" ] || return 1
    mkdir "$T/x" || return 1
    tar -xzf "$T/a.tar.gz" -C "$T/x" kioku || return 1
    [ -f "$T/x/kioku" ] && [ ! -L "$T/x/kioku" ] || return 1
    chmod 0755 "$T/x/kioku" || return 1
    mv -f "$T/x/kioku" "$TARGET"
}

if [ "${1:-}" = compact ]; then
    # Compact never installs, so an older session cannot replace a newer CLI here.
    cat "$ROOT/prompts/hook-compact.txt"
    current_version >/dev/null || cat "$ROOT/prompts/hook-no-cli.txt"
    exit 0
fi
if [ -n "${KIOKU_CLI:-}" ]; then
    copy_override
else
    download
fi
if current_version >/dev/null; then
    cat "$ROOT/prompts/hook-start.txt"
else
    cat "$ROOT/prompts/hook-no-cli.txt"
fi
exit 0
