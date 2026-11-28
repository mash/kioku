import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]


def build_test_binary(cls):
    binary = os.environ.get('KIOKU_BIN') or os.environ.get('KIOKU_TEST_BINARY')
    if binary:
        return Path(binary).resolve()
    tmp = tempfile.TemporaryDirectory(prefix='kioku-hook-cli-')
    cls.addClassCleanup(tmp.cleanup)
    binary = Path(tmp.name) / 'kioku'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/kioku'],
                   cwd=ROOT.parent, check=True, capture_output=True)
    return binary


def write_auth(home, server):
    auth = Path(home) / '.config/kioku/auth'
    auth.mkdir(parents=True, exist_ok=True)
    key = hashlib.sha256(server.encode()).hexdigest()
    (auth / (key + '.json')).write_text(json.dumps({'server': server, 'token': 'fixture'}))


def make_project(path, namespace, server='https://kioku.magicians.ee'):
    metadata = path / '.kioku'
    metadata.mkdir(parents=True, exist_ok=True)
    config = {'namespace': namespace, 'server': server}
    (metadata / 'config').write_text(json.dumps(config))
    return metadata, dict(config, path=str(path))


def wait_until(predicate, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if value := predicate():
            return value
        time.sleep(0.05)
    raise AssertionError(f'background operation did not complete within {timeout} seconds')
