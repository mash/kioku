import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import shutil
import re
import shlex
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

    def test_stage_one_hooks(self):
        config = json.loads((ROOT / 'hooks/hooks.json').read_text())
        groups = []
        for matcher, argument in [('startup|clear|fork', 'start'), ('compact|resume', 'compact')]:
            groups.append({'matcher': matcher, 'hooks': [{
                'type': 'command',
                'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" ' + argument,
                'timeout': 120,
            }]})
        self.assertEqual(config, {'hooks': {
            'SessionStart': groups,
            'PreCompact': [{'hooks': [{
                'type': 'command', 'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" precompact', 'timeout': 10,
            }]}],
            'UserPromptSubmit': [{'hooks': [{
                'type': 'command', 'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" capture', 'timeout': 10,
            }]}],
        }})
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
                        XDG_STATE_HOME=str(self.work / 'state'), CLAUDE_CODE_SESSION_ID='',
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


class SessionHooksTest(unittest.TestCase):
    """Run the manifest's commands with the actual CLI and isolated session state."""

    @classmethod
    def setUpClass(cls):
        binary = os.environ.get('KIOKU_BIN') or os.environ.get('KIOKU_TEST_BINARY')
        if binary:
            cls.cli = Path(binary).resolve()
        else:
            tmp = tempfile.TemporaryDirectory(prefix='kioku-session-cli-')
            cls.addClassCleanup(tmp.cleanup)
            cls.cli = Path(tmp.name) / 'kioku'
            subprocess.run(['go', 'build', '-o', str(cls.cli), './cmd/kioku'],
                           cwd=ROOT.parent, check=True, capture_output=True)

    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix='kioku session hook ')
        self.addCleanup(tmp.cleanup)
        self.work = Path(tmp.name).resolve()
        self.root = self.work / 'plugin'
        for directory in ('hooks', 'prompts', '.claude-plugin'):
            shutil.copytree(ROOT / directory, self.root / directory)
        self.data = self.work / 'data'
        (self.data / 'bin').mkdir(parents=True)
        shutil.copy2(self.cli, self.data / 'bin/kioku')
        self.env = dict(os.environ, HOME=str(self.work / 'home'),
                        XDG_STATE_HOME=str(self.work / 'state'),
                        CLAUDE_PLUGIN_DATA=str(self.data),
                        CLAUDE_CODE_SESSION_ID='environment-is-not-the-hook-id',
                        KIOKU_CLI=str(self.cli))
        for key in ('KIOKU_SERVER', 'KIOKU_AGENT_TOKEN'):
            self.env.pop(key, None)
        self.state_path = self.work / 'state/kioku/sessions/session-1.json'
        self.state = {
            'server': 'https://kioku.magicians.ee', 'namespace': 'org/project',
            'mode': 'local', 'path': str(self.work / 'project'),
            'last_pull_at': '2026-11-11T11:40:00Z', 'last_pull_result': 'success',
        }
        self.save_state()

    def save_state(self):
        self.state_path.parent.mkdir(parents=True, exist_ok=True)
        self.state_path.write_text(json.dumps(self.state))

    def hook(self, event, **fields):
        payload = dict(session_id='session-1', hook_event_name=event, **fields)
        config = json.loads((self.root / 'hooks/hooks.json').read_text())
        results = []
        for group in config['hooks'][event]:
            match = fields.get('source', '') if event == 'SessionStart' else fields.get('tool_name', '')
            if 'matcher' in group and not re.fullmatch(group['matcher'], match):
                continue
            for command in group['hooks']:
                args = shlex.split(command['command'].replace('${CLAUDE_PLUGIN_ROOT}', str(self.root)))
                result = subprocess.run(args, input=json.dumps(payload), text=True,
                                        capture_output=True, env=self.env, timeout=15)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stderr, '')
                results.append(result.stdout)
        return ''.join(results)

    def context(self, output, event):
        obj = json.loads(output)
        self.assertEqual(set(obj), {'hookSpecificOutput'})
        self.assertEqual(obj['hookSpecificOutput']['hookEventName'], event)
        return obj['hookSpecificOutput']['additionalContext']

    def test_compact_resume_and_failed_pull_context(self):
        for result in ('success', 'failure'):
            self.state.update(last_pull_result=result)
            self.save_state()
            for source in ('compact', 'resume'):
                with self.subTest(result=result, source=source):
                    context = self.context(self.hook('SessionStart', source=source), 'SessionStart')
                    self.assertIn('org/project', context)
                    self.assertIn('失敗' if result == 'failure' else '成功', context)
                    self.assertNotIn('\n', context)
                    if result == 'failure':
                        self.assertNotIn('再 pull は不要', context)

    def test_unpulled_selection_context_and_notification_refresh(self):
        del self.state['last_pull_at']
        del self.state['last_pull_result']
        self.save_state()
        old = time.time() - 29 * 86400
        os.utime(self.state_path, (old, old))
        for source in ('compact', 'resume'):
            context = self.context(self.hook('SessionStart', source=source), 'SessionStart')
            self.assertIn('pull 未実行', context)
            self.assertIn('再選択は不要。最初の pull と Wiki の参照を行う。', context)
        self.assertEqual(self.state_path.stat().st_mtime, old)
        self.assertNotEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')
        self.assertGreater(self.state_path.stat().st_mtime, old)
        state = json.loads(self.state_path.read_text())
        self.assertEqual(set(state), {'server', 'namespace', 'mode', 'path', 'notified_urls'})
        self.assertEqual(state['notified_urls'], ['https://example.com/doc'])
        self.assertEqual(self.prompt_context(prompt='https://example.com/doc'), self.turn_rule())

    def test_new_session_sources_never_read_or_write_state(self):
        original = self.state_path.read_bytes()
        for source in ('startup', 'clear', 'fork'):
            with self.subTest(source=source):
                output = self.hook('SessionStart', source=source)
                self.assertEqual(output, (self.root / 'prompts/hook-start.txt').read_text())
                self.assertNotIn('org/project', output)
                self.assertEqual(self.state_path.read_bytes(), original)
                self.assertEqual(list(self.state_path.parent.glob('*.json')), [self.state_path])

    def test_missing_corrupt_and_expired_state_fall_back(self):
        for content in (None, '{', '{}', json.dumps(self.state)):
            if content is None:
                self.state_path.unlink()
            else:
                self.state_path.write_text(content)
                if content == json.dumps(self.state):
                    expired = time.time() - 31 * 86400
                    os.utime(self.state_path, (expired, expired))
            for source in ('compact', 'resume'):
                self.assertEqual(self.hook('SessionStart', source=source),
                                 (self.root / 'prompts/hook-compact.txt').read_text())
            self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')
            self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com'), '')

    def test_precompact_works_without_cli_or_state(self):
        (self.data / 'bin/kioku').unlink()
        self.state_path.unlink()
        self.assertEqual(self.hook('PreCompact'),
                         (self.root / 'prompts/hook-precompact.txt').read_text())

    def test_prompt_urls_and_deduplication(self):
        prompt_url = 'https://example.com/prompt'
        context = self.prompt_context(prompt=prompt_url + ' ' + prompt_url)
        self.assertEqual(context.count(prompt_url), 1)
        self.assertIn('org/project', context)
        self.assertIn('kioku-capture', context)
        self.assertEqual(self.prompt_context(prompt=prompt_url), self.turn_rule())
        state = json.loads(self.state_path.read_text())
        self.assertEqual(state['notified_urls'], [prompt_url])
        self.assertEqual(state['last_pull_result'], 'success')

    def test_E23_E24_post_tool_use_is_silent_and_preserves_state(self):
        original = self.state_path.read_bytes()
        mtime = self.state_path.stat().st_mtime_ns
        cases = [
            ('E23', dict(tool_name='Grep', tool_response='https://a.example/1')),
            ('E24', dict(tool_name='WebFetch', tool_input={'url': 'https://a.example/1'})),
        ]
        for example, fields in cases:
            payload = dict(session_id='session-1', hook_event_name='PostToolUse', **fields)
            commands = [
                [str(self.cli), 'session', 'hook', '--prompts', str(self.root / 'prompts')],
                [str(self.cli), 'session', 'hook', '--event', 'PostToolUse',
                 '--prompts', str(self.root / 'prompts')],
                ['sh', str(self.root / 'hooks/kioku.sh'), 'capture'],
            ]
            for command in commands:
                with self.subTest(example=example, command=command):
                    result = subprocess.run(command, input=json.dumps(payload), text=True,
                                            capture_output=True, env=self.env, timeout=15)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, '')
                    self.assertEqual(result.stderr, '')
                    self.assertEqual(self.state_path.read_bytes(), original)
                    self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_E25_only_prompt_supplies_urls(self):
        original = self.state_path.read_bytes()
        mtime = self.state_path.stat().st_mtime_ns
        for fields in [
            {'cwd': '/work/https://a.example/1'},
            {'transcript_path': 'https://a.example/transcript',
             'extra': [{'url': 'https://a.example/nested'}]},
        ]:
            with self.subTest(fields=fields):
                self.assertEqual(self.prompt_context(prompt='続けて', **fields), self.turn_rule())
                self.assertEqual(self.state_path.read_bytes(), original)
                self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_notification_history_cap(self):
        urls = [f'https://example.com/{n}' for n in range(1001)]
        output = self.hook('UserPromptSubmit', prompt=' '.join(urls))
        context = self.context(output, 'UserPromptSubmit')
        for url in urls:
            self.assertIn(url, context)
        state = json.loads(self.state_path.read_text())
        self.assertEqual(state['notified_urls'], urls[1:])
        self.assertEqual(self.context(self.hook('UserPromptSubmit', prompt=urls[-1]), 'UserPromptSubmit'), self.turn_rule())
        self.assertNotEqual(self.hook('UserPromptSubmit', prompt=urls[0]), '')
        self.assertEqual(len(json.loads(self.state_path.read_text())['notified_urls']), 1000)

    def test_child_operations_do_not_notify_or_update(self):
        original = self.state_path.read_bytes()
        for agent_id in ('child', '', None):
            self.assertEqual(self.hook('UserPromptSubmit', agent_id=agent_id, prompt='https://example.com'), '')
            self.assertEqual(self.state_path.read_bytes(), original)
        self.assertNotEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')

    def test_capture_cli_failures_never_block_a_turn(self):
        before = self.state_path.read_bytes()
        binary = self.data / 'bin/kioku'
        binary.unlink()
        self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')
        binary.write_text('#!/bin/sh\nprintf "failure\\n" >&2\nexit 2\n')
        binary.chmod(0o755)
        self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')
        self.assertEqual(self.state_path.read_bytes(), before)

    def test_invalid_session_and_event_mismatch_preserve_state(self):
        before = self.state_path.read_bytes()
        for sid, event in [('session-1', 'SessionStart'), ('../session-1', 'UserPromptSubmit')]:
            with self.subTest(sid=sid, event=event):
                payload = dict(session_id=sid, hook_event_name=event, source='resume',
                               prompt='https://example.com/doc')
                result = subprocess.run([str(self.cli), 'session', 'hook', '--event', 'UserPromptSubmit',
                                         '--prompts', str(self.root / 'prompts')],
                                        input=json.dumps(payload), text=True, capture_output=True,
                                        env=self.env, timeout=15)
                self.assertEqual((result.returncode, result.stdout, result.stderr), (0, '', ''))
                self.assertEqual(self.state_path.read_bytes(), before)

    def test_no_url_keeps_prompt_rule_but_no_namespace_is_silent(self):
        self.assertEqual(self.context(self.hook('UserPromptSubmit', prompt='http://example.com'), 'UserPromptSubmit'), self.turn_rule())
        self.state['namespace'] = ''
        self.save_state()
        self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')
        self.assertEqual(self.hook('UserPromptSubmit', prompt='https://example.com'), '')

    def test_no_jq_or_python_in_hook_runtime(self):
        path = self.work / 'path'
        path.mkdir()
        for command in ('sh', 'dirname', 'cat', 'sed'):
            (path / command).symlink_to(shutil.which(command))
        self.env['PATH'] = str(path)
        self.assertIn('org/project', self.context(self.hook('SessionStart', source='compact'), 'SessionStart'))
        self.assertNotEqual(self.hook('UserPromptSubmit', prompt='https://example.com/doc'), '')

    def test_real_pull_failure_then_compact(self):
        status = [200]

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if status[0] != 200:
                    self.send_error(status[0])
                    return
                self.send_response(200)
                self.end_headers()
                data = [{'namespace': 'org/project'}] if self.path == '/api/projects' else {'revision': 'r', 'files': {}}
                self.wfile.write(json.dumps(data).encode())

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        env = dict(self.env, CLAUDE_CODE_SESSION_ID='session-1')
        command = [str(self.cli), '--server', f'http://127.0.0.1:{server.server_port}', '-n', 'org/project', 'pull']
        first = subprocess.run(command, env=env, capture_output=True, text=True, timeout=20)
        self.assertEqual(first.returncode, 0, first.stderr)
        status[0] = 403
        failure = subprocess.run(command, env=env, capture_output=True, text=True, timeout=20)
        self.assertNotEqual(failure.returncode, 0)
        output = self.context(self.hook('SessionStart', source='compact'), 'SessionStart')
        self.assertIn('最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。', output)
        self.assertNotIn('再 pull は不要', output)

    @staticmethod
    def candidate(namespace):
        return (f'Kioku: このセッションではプロジェクトを未選択。候補は {namespace}。'
                '作業がこのプロジェクトなら、kioku skill で pull して wiki を読む。違えばユーザーに聞く。')

    def turn_rule(self, state=None):
        state = self.state if state is None else state
        return (f'Kioku: {state["namespace"]}（wiki: {state["path"]}/wiki/）。'
                '調べる時はここを grep する。参照・作成した HTTPS URL の原本は kioku-capture に渡す。')

    def prompt_context(self, **fields):
        output = self.hook('UserPromptSubmit', **fields)
        return self.context(output, 'UserPromptSubmit') if output else ''

    def test_E1_to_E6_E11_to_E13_E20_candidates(self):
        self.state_path.unlink()
        multiple = ('Kioku: このセッションではプロジェクトを未選択。候補は次のとおり。'
                    'kioku skill で 1 つ選んで pull する。決められなければユーザーに聞く。')
        # id, prompt, config, subdir, branch, catalog namespaces, expected
        cases = [
            ('E1', '続けて', 'org/p', False, 'main', [], self.candidate('org/p')),
            ('E2', '続けて', 'org/p', True, 'main', [], self.candidate('org/p')),
            ('E3', 'tesla', None, False, 'main', ['natureglobal/tesla'], self.candidate('natureglobal/tesla')),
            ('E4', 'uses', None, False, 'main', ['org/ses'], ''),
            ('E5', 'a/tesla', None, False, 'main', ['a/tesla', 'b/tesla'], self.candidate('a/tesla')),
            ('E6', 'org/b', 'org/a', False, 'main', ['org/b'], self.candidate('org/b')),
            ('E11', 'tesla', None, False, 'main', ['a/tesla', 'b/tesla'], multiple + '\na/tesla\nb/tesla'),
            ('E12', '続けて', None, False, 'feat/tesla', ['natureglobal/tesla'], self.candidate('natureglobal/tesla')),
            ('E13', 'org/p', None, False, 'main', ['org/p', 'org/p'], multiple + '\norg/p（server=https://kioku.magicians.ee）\norg/p（server=https://other.example）'),
            ('E20', '電気自動車', None, False, 'main', ['org/tesla'], ''),
        ]
        catalog_path = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog_path.parent.mkdir(parents=True)
        for example, prompt, config, subdir, branch, namespaces, expected in cases:
            with self.subTest(example=example):
                root = self.work / example
                subprocess.run(['git', 'init', '-q', '-b', branch, str(root)], check=True, capture_output=True)
                cwd = root
                if subdir:
                    cwd = root / 'src'
                    cwd.mkdir()
                config_path = root / '.kioku/config'
                if config:
                    config_path.parent.mkdir()
                    config_path.write_text(json.dumps({'server': 'https://kioku.magicians.ee', 'namespace': config}))
                projects = [{'server': 'https://other.example' if example == 'E13' and i else 'https://kioku.magicians.ee',
                             'namespace': ns, 'path': str(self.work / f'p{i}'), 'description': '電気自動車'}
                            for i, ns in enumerate(namespaces)]
                catalog_path.write_text(json.dumps({'projects': projects}))
                catalog_before = catalog_path.read_bytes()
                config_before = config_path.read_bytes() if config else None
                for _ in range(2):
                    actual = self.prompt_context(cwd=str(cwd), prompt=prompt)
                    self.assertEqual(actual.splitlines()[:1], expected.splitlines()[:1])
                    self.assertCountEqual(actual.splitlines()[1:], expected.splitlines()[1:])
                    self.assertFalse(self.state_path.exists())
                    self.assertEqual(catalog_path.read_bytes(), catalog_before)
                    if config:
                        self.assertEqual(config_path.read_bytes(), config_before)

    def test_E7_E8_E9_E10_E22_selected_output_and_state(self):
        base = dict(self.state, namespace='org/p', path='/work/p', last_pull_at='2026-11-14T00:00:00Z')
        failure = '最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。'
        url = 'https://example.com/doc'
        capture = ('Kioku: org/p（local, server=https://kioku.magicians.ee, P=/work/p）。'
                   '親は次の URL を kioku-capture に渡す。kioku-capture が原本の全文を put する。'
                   'namespace・server・mode・P・CLI の絶対パス・共有範囲・除外条件も渡す。\n' + url)
        for example in ('E7', 'E8', 'E9', 'E10', 'E22'):
            with self.subTest(example=example):
                self.state = dict(base)
                if example == 'E8':
                    self.state['last_pull_result'] = 'failure'
                if example == 'E22':
                    self.state['notified_urls'] = [url]
                self.save_state()
                before, mtime = self.state_path.read_bytes(), self.state_path.stat().st_mtime_ns
                fields = {'prompt': url if example in ('E9', 'E10', 'E22') else '続けて', 'cwd': '/work/p'}
                expected = self.turn_rule()
                if example == 'E8':
                    expected += '\n' + failure
                if example == 'E9':
                    expected += '\n' + capture
                if example == 'E10':
                    fields['agent_id'] = 'child-1'
                    expected = ''
                self.assertEqual(self.prompt_context(**fields), expected)
                if example == 'E9':
                    self.assertEqual(json.loads(self.state_path.read_text()), dict(self.state, notified_urls=[url]))
                else:
                    self.assertEqual(self.state_path.read_bytes(), before)
                    self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_E14_to_E18_real_pull_transitions(self):
        failing = set()
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(self.path)
                if any('/' + ns + '/' in self.path for ns in failing):
                    self.send_error(503)
                    return
                self.send_response(200)
                self.end_headers()
                data = ([{'namespace': ns} for ns in ('org/p', 'org/a', 'org/b')]
                        if self.path == '/api/projects' else {'revision': 'r', 'files': {}})
                self.wfile.write(json.dumps(data).encode())

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        origin = f'http://127.0.0.1:{server.server_port}'
        env = dict(self.env, CLAUDE_CODE_SESSION_ID='session-1')
        root = self.work / 'p'
        (root / '.kioku').mkdir(parents=True)
        (root / '.kioku/config').write_text(json.dumps({'server': origin, 'namespace': 'org/p'}))

        def pull(ns):
            directory = root if ns == 'org/p' else self.work / ns.split('/')[1]
            directory.mkdir(exist_ok=True)
            command = [str(self.cli), '-C', str(directory), '--server', origin, '-n', ns, 'pull']
            result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=20)
            self.assertEqual(result.returncode == 0, ns not in failing, result.stderr)
            return result

        for example in ('E14', 'E15', 'E16', 'E17', 'E18'):
            with self.subTest(example=example):
                self.state_path.unlink(missing_ok=True)
                failing.clear()
                calls.clear()
                fields = dict(cwd=str(root), prompt='続けて')
                for _ in range(2):
                    self.assertEqual(self.prompt_context(**fields), self.candidate('org/p'))
                    self.assertFalse(self.state_path.exists())
                self.assertEqual(calls, [])  # Discovery never contacts the server.
                if example == 'E15':
                    failing.add('org/p')
                    pull('org/p')
                    self.assertFalse(self.state_path.exists())
                    self.assertEqual(self.prompt_context(**fields), self.candidate('org/p'))
                    continue
                if example in ('E14', 'E16'):
                    pull('org/p')
                    first = json.loads(self.state_path.read_text())
                    if example == 'E16':
                        failing.add('org/p')
                        pull('org/p')
                        state = json.loads(self.state_path.read_text())
                        self.assertEqual(state['last_pull_result'], 'failure')
                        self.assertGreater(state['last_pull_at'], first['last_pull_at'])
                    else:
                        state = first
                        self.assertEqual(state['last_pull_result'], 'success')
                    expected = self.turn_rule(state)
                    if example == 'E16':
                        expected += '\n最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。'
                else:
                    # Execute the skill's selected-last order, then check its state.
                    pull('org/b')
                    before = self.state_path.read_bytes()
                    if example == 'E18':
                        failing.add('org/a')
                    pull('org/a')
                    state = json.loads(self.state_path.read_text())
                    self.assertEqual(state['namespace'], 'org/b' if example == 'E18' else 'org/a')
                    if example == 'E18':
                        self.assertEqual(self.state_path.read_bytes(), before)
                    expected = self.turn_rule(state)
                    self.assertLess(next(i for i, path in enumerate(calls) if '/org/b/' in path),
                                    next(i for i, path in enumerate(calls) if '/org/a/' in path))
                self.assertEqual(self.prompt_context(**fields), expected)
                self.assertEqual(self.state_path.stat().st_mode & 0o777, 0o600)

        selection = (ROOT / 'skills/kioku/references/selection.md').read_text()
        flow = selection.split('## 人のログイン後の全プロジェクト取得')[1]
        self.assertIn('選んだ対象以外を先に pull し、選んだ対象を最後に pull する。', flow)
        self.assertIn('説明を保存するための pull も含め、選んだ対象を最後にする。', flow)
        self.assertIn('巡回後に重ねて pull しない。', flow)
        self.assertIn('その取得失敗をユーザーへ伝える。', flow)

    def test_E19_malformed_input_preserves_state(self):
        before = self.state_path.read_bytes()
        mtime = self.state_path.stat().st_mtime_ns
        for command in ([str(self.cli), 'session', 'hook', '--event', 'UserPromptSubmit', '--prompts', str(self.root / 'prompts')],
                        ['sh', str(self.root / 'hooks/kioku.sh'), 'capture']):
            result = subprocess.run(command, input='{', capture_output=True, text=True, env=self.env, timeout=15)
            self.assertEqual((result.stdout, result.stderr, result.returncode), ('', '', 0))
            self.assertEqual(self.state_path.read_bytes(), before)
            self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_E21_startup_exact_text(self):
        expected = ('Kioku: この会話で選んだプロジェクトがあれば保持する。hook が候補を出す。'
                    '無ければ kioku skill で選ぶ。最初のメッセージの後、対象を pull して wiki/ を grep とファイルツールで読み、作業を始める。'
                    '対象が曖昧ならユーザーに聞く。フィードバックだけの依頼では、プロジェクトを設定せず、skill のフィードバック手順に従う。\n')
        self.assertEqual(self.hook('SessionStart', source='startup'), expected)

    def test_notification_save_failure_can_notify_again(self):
        if os.geteuid() == 0:
            self.skipTest('root bypasses filesystem permissions')
        before = self.state_path.read_bytes()
        payload = json.dumps({'session_id': 'session-1', 'hook_event_name': 'UserPromptSubmit',
                              'prompt': 'https://example.com/unsaved'})
        commands = [([str(self.cli), 'session', 'hook', '--prompts', str(self.root / 'prompts')], 1),
                    (['sh', str(self.root / 'hooks/kioku.sh'), 'capture'], 0)]
        try:
            self.state_path.parent.chmod(0o500)
            for command, code in commands:
                result = subprocess.run(command, input=payload, capture_output=True, text=True, env=self.env, timeout=15)
                self.assertEqual(result.returncode, code)
                self.assertIn('https://example.com/unsaved', self.context(result.stdout, 'UserPromptSubmit'))
                if code:
                    self.assertTrue(result.stderr.startswith('kioku: '), result.stderr)
                else:
                    self.assertEqual(result.stderr, '')
                self.assertEqual(self.state_path.read_bytes(), before)
        finally:
            self.state_path.parent.chmod(0o700)
        self.assertIn('https://example.com/unsaved', self.prompt_context(prompt='https://example.com/unsaved'))
        self.assertEqual(json.loads(self.state_path.read_text())['notified_urls'], ['https://example.com/unsaved'])


if __name__ == '__main__':
    unittest.main()
