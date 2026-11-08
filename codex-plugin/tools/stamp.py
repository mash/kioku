#!/usr/bin/env python3
"""Write a checksum-bound receipt immediately after building a CLI binary.

-trimpath binaries omit ldflags from Go build metadata; release packaging uses
this build-time receipt for cross-platform binaries and executes the host binary
in Makefile as an independent version/commit check.
"""
import hashlib
import json
from pathlib import Path
import sys

if __name__ == '__main__':
    binary, version, commit, target = sys.argv[1:]
    path = Path(binary)
    path.with_name('build.json').write_text(json.dumps({
        'version': version, 'commit': commit, 'target': target,
        'sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
    }, indent=2) + '\n')
