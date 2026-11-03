import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import threading
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]


def cli(version, status=0):
    """A fixture CLI that prints `kioku version` output like the real CLI."""
    return ('#!/bin/sh\n'
            'if [ "$1" = version ]; then\n'
            '    printf \'{"arch":"arm64","go_version":"go1.24.0","os":"darwin","version":"%s"}\\n\' ' + version + '\n'
            '    exit ' + str(status) + '\n'
            'fi\n').encode()


class ManifestTest(unittest.TestCase):
    def test_native_plugin_manifest(self):
        plugin = json.loads((ROOT / '.claude-plugin/plugin.json').read_text())
        marketplace = json.loads((ROOT.parent / '.claude-plugin/marketplace.json').read_text())
        self.assertEqual(marketplace['name'], 'kioku')
        self.assertEqual(marketplace['plugins'][0]['name'], plugin['name'])
        self.assertEqual(marketplace['plugins'][0]['source'], './plugin')
        self.assertFalse((ROOT / '.claude-plugin/marketplace.json').exists())
        self.assertRegex(plugin['version'], r'^\d+\.\d+\.\d+$')
        self.assertEqual(plugin['mcpServers']['kioku'], {
            'type': 'http', 'url': 'https://kioku.magicians.ee/mcp',
        })
        self.assertFalse((ROOT / 'bin').exists())
        self.assertFalse((ROOT / 'cli-pin').exists())
        self.assertFalse((ROOT / 'tools').exists())

    def test_only_session_start(self):
        config = json.loads((ROOT / 'hooks/hooks.json').read_text())
        groups = []
        for matcher, argument in [('startup|resume|clear|fork', 'start'), ('compact', 'compact')]:
            groups.append({'matcher': matcher, 'hooks': [{
                'type': 'command',
                'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" ' + argument,
                'timeout': 120,
            }]})
        self.assertEqual(config, {'hooks': {'SessionStart': groups}})
        self.assertNotIn('python', json.dumps(config))
        self.assertEqual(list((ROOT / 'hooks').glob('*.py')), [])


class HooksTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='kioku hook test ')
        self.addCleanup(temp.cleanup)
        self.work = Path(temp.name).resolve()
        self.root = self.work / 'plugin'
        for directory in ('hooks', 'prompts', '.claude-plugin'):
            shutil.copytree(ROOT / directory, self.root / directory)
        self.manifest = self.root / '.claude-plugin/plugin.json'
        self.data = self.work / 'data'
        self.requests = []
        self.archives = {}
        self.delay = 0
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                fixture.requests.append(self.path)
                time.sleep(fixture.delay)
                content = fixture.archives.get(self.path)
                if content is None:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(content)

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.env = dict(os.environ, HOME=str(self.work / 'home'),
                        CLAUDE_PLUGIN_DATA=str(self.data),
                        KIOKU_CLI_DOWNLOAD_BASE=f'http://127.0.0.1:{server.server_port}')
        self.env.pop('KIOKU_CLI', None)
        self.make_release('v9.9.9')

    def make_release(self, version):
        sums = []
        binary = cli(version)
        for system in ('darwin', 'linux'):
            for arch in ('amd64', 'arm64'):
                archive = f'kioku_{version}_{system}_{arch}.tar.gz'
                content = io.BytesIO()
                with tarfile.open(fileobj=content, mode='w:gz') as tar:
                    member = tarfile.TarInfo('kioku')
                    member.mode = 0o755
                    member.size = len(binary)
                    tar.addfile(member, io.BytesIO(binary))
                body = content.getvalue()
                self.archives[f'/{version}/{archive}'] = body
                sums.append(f'{hashlib.sha256(body).hexdigest()}  {archive}\n')
        self.archives[f'/{version}/SHA256SUMS.txt'] = ''.join(sums).encode()
        self.set_plugin_version(version[1:])

    def set_plugin_version(self, version):
        manifest = json.loads(self.manifest.read_text())
        manifest['version'] = version
        self.manifest.write_text(json.dumps(manifest, indent=2) + '\n')

    def sums_path(self):
        return '/v9.9.9/SHA256SUMS.txt'

    def set_sums(self, transform):
        lines = self.archives[self.sums_path()].decode().splitlines()
        self.archives[self.sums_path()] = ''.join(line + '\n' for line in transform(lines)).encode()

    def hook(self, operation='start'):
        return subprocess.run(['sh', str(self.root / 'hooks/kioku.sh'), operation],
                              input='{}', text=True, capture_output=True, env=self.env, timeout=15)

    def assert_text(self, result, *names):
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, '')
        expected = ''.join((self.root / f'prompts/hook-{name}.txt').read_text()
                           for name in names or ('start',))
        self.assertEqual(result.stdout, expected)

    def platform(self):
        system = {'Darwin': 'darwin', 'Linux': 'linux'}[os.uname().sysname]
        arch = {'x86_64': 'amd64', 'amd64': 'amd64', 'arm64': 'arm64', 'aarch64': 'arm64'}[os.uname().machine]
        return f'{system}_{arch}'

    def binary(self):
        return self.data / 'bin' / 'kioku'

    def install(self, content):
        self.binary().parent.mkdir(parents=True, exist_ok=True)
        self.binary().write_bytes(content)
        self.binary().chmod(0o755)

    def assert_installed(self, content):
        self.assertTrue(self.binary().is_file())
        self.assertFalse(self.binary().is_symlink())
        self.assertTrue(os.access(self.binary(), os.X_OK))
        self.assertEqual(self.binary().read_bytes(), content)

    def assert_tmp_empty(self):
        self.assertEqual(list((self.data / 'tmp').glob('*')), [])

    def assert_no_cli(self):
        self.assert_text(self.hook(), 'no-cli')
        self.assertFalse(self.binary().exists())
        self.assert_tmp_empty()

    def corrupt_sums(self):
        self.set_sums(lambda lines: ['0' * 64 + '  ' + line.split()[1] for line in lines])

    def test_first_cached_and_compact(self):
        self.assert_text(self.hook())
        self.assertEqual(self.requests, ['/v9.9.9/SHA256SUMS.txt',
                                         f'/v9.9.9/kioku_v9.9.9_{self.platform()}.tar.gz'])
        self.assert_installed(cli('v9.9.9'))
        self.requests.clear()
        self.assert_text(self.hook())
        self.assert_text(self.hook('compact'), 'compact')
        self.assertEqual(self.requests, [])

    def test_equal_version_no_download(self):
        self.install(cli('v9.9.9'))
        self.assert_text(self.hook())
        self.assertEqual(self.requests, [])
        self.assert_installed(cli('v9.9.9'))

    def test_different_version_installs_release(self):
        for version in ('v9.9.8', 'v9.10.0', 'v9.9.9-rc.1'):
            with self.subTest(version=version):
                self.requests.clear()
                self.install(cli(version))
                self.assert_text(self.hook())
                self.assertEqual(len(self.requests), 2)
                self.assert_installed(cli('v9.9.9'))
                self.assert_tmp_empty()

    def test_version_change_installs_new_release(self):
        self.assert_text(self.hook())
        self.make_release('v9.9.10')
        self.assert_text(self.hook())
        self.assertEqual(len(self.requests), 4)
        self.assert_installed(cli('v9.9.10'))
        self.assertFalse((self.data / 'cli').exists())
        self.assert_tmp_empty()

    def test_compact_never_installs(self):
        self.install(cli('v9.9.8'))
        self.assert_text(self.hook('compact'), 'compact')
        self.assertEqual(self.requests, [])
        self.assert_installed(cli('v9.9.8'))

    def test_compact_without_cli(self):
        self.assert_text(self.hook('compact'), 'compact', 'no-cli')
        self.assertEqual(self.requests, [])
        self.assertFalse(self.binary().exists())

    def test_broken_binary_reinstalled(self):
        self.install(cli('v9.9.9', status=1))
        self.assert_text(self.hook())
        self.assertEqual(len(self.requests), 2)
        self.assert_installed(cli('v9.9.9'))

    def test_wrong_hash(self):
        self.corrupt_sums()
        self.assert_no_cli()
        self.assertEqual(len(self.requests), 2)

    def test_wrong_hash_keeps_old_binary(self):
        self.install(cli('v9.9.8'))
        self.corrupt_sums()
        self.assert_text(self.hook())
        self.assertEqual(len(self.requests), 2)
        self.assert_installed(cli('v9.9.8'))
        self.assert_tmp_empty()

    def test_bad_sum_line(self):
        archive = f'kioku_v9.9.9_{self.platform()}.tar.gz'
        cases = {
            'missing': lambda lines: [line for line in lines if not line.endswith(archive)],
            'duplicate': lambda lines: lines + [line for line in lines if line.endswith(archive)],
            'upper case': lambda lines: ['A' * 64 + line[64:] for line in lines],
            'short': lambda lines: [line[1:] for line in lines],
            'not hex': lambda lines: ['g' + line[1:] for line in lines],
        }
        original = self.archives[self.sums_path()]
        for name, transform in cases.items():
            with self.subTest(name):
                self.archives[self.sums_path()] = original
                self.set_sums(transform)
                self.requests.clear()
                self.assert_no_cli()
                self.assertEqual(self.requests, [self.sums_path()])

    def test_http_404(self):
        self.archives.clear()
        self.assert_no_cli()
        self.assertEqual(len(self.requests), 1)

    def test_missing_release_keeps_old_binary(self):
        self.install(cli('v9.9.8'))
        self.archives.clear()
        self.assert_text(self.hook())
        self.assertEqual(self.requests, [self.sums_path()])
        self.assert_installed(cli('v9.9.8'))
        self.assert_tmp_empty()

    def test_missing_archive_keeps_old_binary(self):
        self.install(cli('v9.9.8'))
        del self.archives[f'/v9.9.9/kioku_v9.9.9_{self.platform()}.tar.gz']
        self.assert_text(self.hook())
        self.assertEqual(len(self.requests), 2)
        self.assert_installed(cli('v9.9.8'))
        self.assert_tmp_empty()

    def restricted_path(self, omit=(), system=None):
        path = self.work / 'path'
        path.mkdir()
        for name in ('cat', 'dirname', 'uname', 'mkdir', 'curl', 'tar', 'sha256sum',
                     'shasum', 'grep', 'sed', 'mktemp', 'chmod', 'cp', 'mv', 'rm'):
            target = shutil.which(name)
            if name not in omit and target:
                (path / name).symlink_to(target)
        if system:
            (path / 'uname').write_text('#!/bin/sh\nprintf "%s\\n" ' + system + '\n')
            (path / 'uname').chmod(0o755)
        self.env['PATH'] = str(path)
        # The subprocess launches sh itself through this restricted PATH.
        (path / 'sh').symlink_to(shutil.which('sh'))

    def test_no_curl(self):
        self.restricted_path(omit=('curl',))
        self.assert_no_cli()
        self.assertEqual(self.requests, [])

    def test_unsupported_platform(self):
        self.restricted_path(omit=('uname',), system='FreeBSD')
        self.assert_no_cli()
        self.assertEqual(self.requests, [])

    def test_bad_plugin_version(self):
        for version in ('', 'v9.9.9', '9.9', '9.9.9-rc.1', '9.9.9 '):
            with self.subTest(version=version):
                self.set_plugin_version(version)
                self.assert_no_cli()
                self.assertEqual(self.requests, [])
        with self.subTest('missing manifest'):
            self.manifest.unlink()
            self.assert_no_cli()
            self.assertEqual(self.requests, [])
        with self.subTest('working CLI without a valid version'):
            self.install(cli('v9.9.8'))
            self.assert_text(self.hook())
            self.assertEqual(self.requests, [])
            self.assert_installed(cli('v9.9.8'))

    def test_developer_override_copies(self):
        self.install(cli('v9.9.9'))
        override = self.work / 'developer' / 'kioku'
        override.parent.mkdir()
        override.write_bytes(cli('dev'))
        override.chmod(0o755)
        self.env['KIOKU_CLI'] = str(override)
        self.assert_text(self.hook())
        self.assert_installed(cli('dev'))
        override.write_bytes(cli('dev-2'))
        self.assert_text(self.hook('compact'), 'compact')
        self.assert_installed(cli('dev'))
        self.assert_text(self.hook())
        self.assert_installed(cli('dev-2'))
        self.assertEqual(self.requests, [])
        self.assert_tmp_empty()
        del self.env['KIOKU_CLI']
        self.assert_text(self.hook())
        self.assertEqual(len(self.requests), 2)
        self.assert_installed(cli('v9.9.9'))

    def test_fallback_data_directory(self):
        self.env.pop('CLAUDE_PLUGIN_DATA')
        self.data = Path(self.env['HOME']) / '.local/share/kioku'
        self.assert_text(self.hook())
        self.assert_installed(cli('v9.9.9'))

    def test_quoted_data_directory(self):
        self.data = self.work / "data with space and ' quote"
        self.env['CLAUDE_PLUGIN_DATA'] = str(self.data)
        self.assert_text(self.hook())
        self.assert_installed(cli('v9.9.9'))

    def test_concurrent_install(self):
        self.delay = 0.5
        processes = []
        for _ in range(2):
            process = subprocess.Popen(['sh', str(self.root / 'hooks/kioku.sh'), 'start'],
                                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, text=True, env=self.env)
            processes.append(process)
            self.addCleanup(lambda p=process: p.kill() if p.poll() is None else None)
        for process in processes:
            stdout, stderr = process.communicate(timeout=15)
            self.assert_text(subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr))
        # Each hook downloads SHA256SUMS.txt and the archive, unless the other hook installed first.
        self.assertIn(len(self.requests), (2, 4))
        self.assert_installed(cli('v9.9.9'))
        self.assert_tmp_empty()


if __name__ == '__main__':
    unittest.main()
