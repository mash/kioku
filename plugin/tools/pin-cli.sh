#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fail() {
    printf '%s\n' "$*" >&2
    exit 1
}
[ "$#" -ge 1 ] && [ "$#" -le 2 ] || fail 'Usage: tools/pin-cli.sh VERSION [SUMS_FILE]'
VERSION=$1
printf '%s\n' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || fail 'Invalid version'
T=$(mktemp -d "$ROOT/.pin-cli.XXXXXX")
trap 'rm -rf "$T"' 0
if [ "$#" -eq 2 ]; then
    cp "$2" "$T/SHA256SUMS.txt" || fail 'Cannot read checksum file'
else
    BASE=${KIOKU_CLI_DOWNLOAD_BASE:-https://github.com/mash/kioku/releases/download}
    curl -fsSL -o "$T/SHA256SUMS.txt" "$BASE/$VERSION/SHA256SUMS.txt" || fail 'Cannot download checksum file'
fi
awk -v version="$VERSION" '
    BEGIN {
        names["kioku_" version "_darwin_amd64.tar.gz"] = 0
        names["kioku_" version "_darwin_arm64.tar.gz"] = 0
        names["kioku_" version "_linux_amd64.tar.gz"] = 0
        names["kioku_" version "_linux_arm64.tar.gz"] = 0
    }
    $2 in names {
        names[$2]++
        if (NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/) bad = 1
    }
    END {
        for (name in names) if (names[name] != 1) bad = 1
        exit bad
    }
' "$T/SHA256SUMS.txt" || fail 'Expected one lower-case SHA-256 checksum for each of the four CLI archives'
printf '%s\n' "$VERSION" > "$T/version"
mkdir -p "$ROOT/cli-pin"
mv "$T/version" "$ROOT/cli-pin/version"
mv "$T/SHA256SUMS.txt" "$ROOT/cli-pin/SHA256SUMS.txt"
printf 'Pinned %s. Bump .claude-plugin/plugin.json version in the same commit.\n' "$VERSION"
