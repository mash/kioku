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
import sys
import tarfile
import tempfile
import threading
import time
import unittest

from hook_test_helpers import ROOT, make_project, build_test_binary, wait_until, write_auth


def cli(version, status=0):
    """A fixture CLI that prints `kioku version` output like the real CLI."""
    return ('#!/bin/sh\n'
            'if [ "$1" = version ]; then\n'
            '    printf \'{"arch":"arm64","go_version":"go1.24.0","os":"darwin","version":"%s"}\\n\' ' + version + '\n'
            '    exit ' + str(status) + '\n'
            'fi\n'
            'case "$(cat)" in\n'
            '    *compact*) cat "$6/hook-compact.txt" ;;\n'
            '    *) cat "$6/hook-start.txt" ;;\n'
            'esac\n').encode()


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
            'PostToolUse': [{'matcher': '^(WebFetch|Bash|Artifact|mcp__.*)$', 'hooks': [{
                'type': 'command', 'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" tool', 'async': True, 'timeout': 10,
            }]}],
            'Stop': [{'hooks': [{
                'type': 'command', 'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" stop', 'timeout': 10,
            }]}],
            'SessionEnd': [{'hooks': [{
                'type': 'command', 'command': 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/kioku.sh" end', 'timeout': 10,
            }]}],
        }})
        script = (ROOT / 'hooks/kioku.sh').read_text()
        self.assertIn('capture) EVENT=UserPromptSubmit', script)
        self.assertIn('"$TARGET" session hook --event "$EVENT" --prompts "$ROOT/prompts"', script)
        self.assertNotIn('python', json.dumps(config))
        self.assertEqual(list((ROOT / 'hooks').glob('*.py')), [])


    def test_local_put_permission_is_only_in_the_skill(self):
        skill = (ROOT / 'skills/kioku/SKILL.md').read_text()
        turn = (ROOT / 'prompts/hook-turn.txt').read_text()
        rules = (ROOT / 'rules/kioku.md').read_text()
        self.assertIn('put file:///絶対パス', skill)
        self.assertIn('を継続して許可する', skill)
        self.assertIn('利用者の保存禁止を優先する', skill)
        self.assertIn('PostToolUse', turn)
        self.assertIn('queue の行はサーバーの受付を示さない', turn)
        self.assertNotIn(' put ', turn)
        self.assertNotIn('継続して許可する', turn + rules)
        self.assertIn('skills/kioku/SKILL.md', rules)
        for text in (skill, turn, rules):
            for old in ('queue add', '--date', '--now'):
                self.assertNotIn(old, text)


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
                              input=json.dumps({'source': operation}), text=True, capture_output=True,
                              env=self.env, timeout=15)

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
        cls.cli = build_test_binary(cls)

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
        for key in ('KIOKU_SERVER', 'KIOKU_AGENT_TOKEN', 'XDG_CONFIG_HOME'):
            self.env.pop(key, None)
        self.state_path = self.work / 'state/kioku/sessions/session-1.json'
        self.state = {
            'server': 'https://kioku.magicians.ee', 'namespace': 'org/project',
            'mode': 'local', 'path': str(self.work / 'project'),
            'last_pull_at': '2026-11-11T11:40:00Z', 'last_pull_result': 'success',
        }
        self.save_state()
        (self.work / '.git').mkdir()
        (self.work / '.git/HEAD').write_text('a' * 40 + '\n')
        (self.work / '.kioku').mkdir()
        (self.work / '.kioku/config').write_text(json.dumps({'server':'https://kioku.magicians.ee','namespace':'boundary/unused'}))

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

    def test_unpulled_selection_context_preserves_state(self):
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
        self.assertEqual(self.state_path.stat().st_mtime, old)
        state = json.loads(self.state_path.read_text())
        self.assertEqual(set(state), {'server', 'namespace', 'mode', 'path'})
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
            self.assertEqual(self.prompt_context(prompt='https://example.com/doc'), self.list_header())
            self.assertEqual(self.prompt_context(prompt='https://example.com'), self.list_header())

    def test_session_start_falls_back_when_an_older_cli_is_silent(self):
        older = self.work / 'older-kioku'
        older.write_text("""#!/bin/sh
if [ "$1" = version ]; then
    printf '%s\n' '{"version":"v0.3.0"}'
fi
""")
        older.chmod(0o755)
        shutil.copy2(older, self.data / 'bin/kioku')
        self.env['KIOKU_CLI'] = str(older)
        for source, prompt in [('startup', 'hook-start.txt'), ('resume', 'hook-compact.txt')]:
            with self.subTest(source=source):
                self.assertEqual(self.hook('SessionStart', source=source),
                                 (self.root / 'prompts' / prompt).read_text())

    def test_precompact_works_without_cli_or_state(self):
        (self.data / 'bin/kioku').unlink()
        self.state_path.unlink()
        self.assertEqual(self.hook('PreCompact'),
                         (self.root / 'prompts/hook-precompact.txt').read_text())


    def test_E23_E24_post_tool_use_is_silent_and_preserves_state(self):
        original = self.state_path.read_bytes()
        mtime = self.state_path.stat().st_mtime_ns
        payload = dict(session_id='session-1', hook_event_name='PostToolUse',
                       tool_name='WebFetch', tool_input={'url': 'https://a.example/1'},
                       tool_response='https://a.example/1')
        result = subprocess.run(
            [str(self.cli), 'session', 'hook', '--prompts', str(self.root / 'prompts')],
            input=json.dumps(payload), text=True, capture_output=True, env=self.env, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '')
        self.assertEqual(result.stderr, '')
        self.assertEqual(self.state_path.read_bytes(), original)
        self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_E25_urls_in_input_never_notify(self):
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

    def test_no_url_keeps_selection_and_missing_namespace_offers_general(self):
        self.assertEqual(self.context(self.hook('UserPromptSubmit', prompt='http://example.com'), 'UserPromptSubmit'), self.turn_rule())
        self.state['namespace'] = ''
        self.save_state()
        self.assertEqual(self.prompt_context(prompt='https://example.com/doc'), self.list_header())
        self.assertEqual(self.prompt_context(prompt='https://example.com'), self.list_header())

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

    def list_header(self):
        command = shlex.quote(str(self.data / 'bin/kioku'))
        return (self.root / 'prompts/hook-candidates.txt').read_text().strip().replace(
            '{{kioku}}', command).replace('{{general_kioku}}', command)

    def candidate(self, namespace):
        return self.list_header() + f'\n- {namespace}: （説明なし）［ヒント: cwd の .kioku/config］'

    def turn_rule(self, state=None):
        state = self.state if state is None else state
        line = (f'Kioku: {state["namespace"]}（wiki: {state["path"]}/wiki/、入口は index.md）。'
                '調べる時はここを grep する。PostToolUse は対象の呼び出しから取得元を queue に足す。'
                'queue の行はサーバーの受付を示さない。')
        if 'last_pull_result' not in state:
            line += '\n背景の pull は終わっていない。wiki/ は 1 回前の pull のままか、まだ無い。'
        return line

    def prompt_context(self, **fields):
        output = self.hook('UserPromptSubmit', **fields)
        return self.context(output, 'UserPromptSubmit') if output else ''

    def test_E1_E3_E23_candidates(self):
        self.state_path.unlink()
        cases = [
            ('E1', 'テスト通して', None),
            ('E3', '続けて', 'org/a'),
            ('E23', 'Tesla の充電', None),
        ]
        catalog_path = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog_path.parent.mkdir(parents=True)
        for example, prompt, config in cases:
            with self.subTest(example=example):
                root = self.work / example
                cwd = root / 'src'
                cwd.mkdir(parents=True)
                config_path = root / '.kioku/config'
                if config:
                    config_path.parent.mkdir()
                    config_path.write_text(json.dumps({'server': 'https://kioku.magicians.ee', 'namespace': config}))
                projects = [{'server': 'https://kioku.magicians.ee', 'namespace': 'org/a',
                             'path': str(root), 'description': 'A 案件の説明', 'triggers': ['tesla']},
                            {'server': 'https://kioku.magicians.ee', 'namespace': 'org/b',
                             'path': str(root), 'triggers': ['充電']}]
                catalog_path.write_text(json.dumps({'projects': projects}))
                catalog_before = catalog_path.read_bytes()
                config_before = config_path.read_bytes() if config else None
                lines = ['- org/a: A 案件の説明', '- org/b: （説明なし）']
                if example == 'E3':
                    lines[0] += '［ヒント: cwd の .kioku/config］'
                if example == 'E23':
                    lines[0] += '［ヒント: trigger 語 tesla］'
                    lines[1] += '［ヒント: trigger 語 充電］'
                expected = self.list_header() + '\n' + '\n'.join(lines)
                for _ in range(2):
                    actual = self.prompt_context(cwd=str(cwd), prompt=prompt)
                    self.assertEqual(actual, expected)
                    self.assertFalse(self.state_path.exists())
                    self.assertEqual(catalog_path.read_bytes(), catalog_before)
                    if config:
                        self.assertEqual(config_path.read_bytes(), config_before)

    def test_E11_E25_E4_E30_selected_output_and_state(self):
        base = dict(self.state, namespace='org/p', path='/work/p', last_pull_at='2026-11-14T00:00:00Z')
        failure = '最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。'
        url = 'https://example.com/doc'
        for example in ('E11', 'E25', 'E11_url', 'E4', 'E30'):
            with self.subTest(example=example):
                self.state = dict(base)
                if example == 'E25':
                    self.state['last_pull_result'] = 'failure'
                self.save_state()
                before, mtime = self.state_path.read_bytes(), self.state_path.stat().st_mtime_ns
                fields = {'prompt': url + ' を調べて' if example == 'E30' else url if example in ('E11_url', 'E4') else '続けて', 'cwd': '/work/p'}
                expected = self.turn_rule()
                if example == 'E25':
                    expected += '\n' + failure
                if example == 'E4':
                    fields['agent_id'] = 'child-1'
                    expected = ''
                self.assertEqual(self.prompt_context(**fields), expected)
                self.assertEqual(self.state_path.read_bytes(), before)
                self.assertEqual(self.state_path.stat().st_mtime_ns, mtime)

    def test_real_pull_selection_transitions(self):
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

        for example in ('success', 'initial_failure', 'same_failure', 'selected_last', 'selected_last_failure'):
            with self.subTest(example=example):
                self.state_path.unlink(missing_ok=True)
                failing.clear()
                calls.clear()
                fields = dict(cwd=str(root), prompt='続けて')
                for _ in range(2):
                    self.assertRegex(self.prompt_context(**fields), r'- org/p(?:［server: [^］]+］)?: （説明なし）［ヒント: cwd の .kioku/config］')
                    self.assertFalse(self.state_path.exists())
                self.assertEqual(calls, [])  # Discovery never contacts the server.
                if example == 'initial_failure':
                    failing.add('org/p')
                    pull('org/p')
                    self.assertFalse(self.state_path.exists())
                    self.assertRegex(self.prompt_context(**fields), r'- org/p(?:［server: [^］]+］)?: （説明なし）［ヒント: cwd の .kioku/config］')
                    continue
                if example in ('success', 'same_failure'):
                    pull('org/p')
                    first = json.loads(self.state_path.read_text())
                    if example == 'same_failure':
                        failing.add('org/p')
                        pull('org/p')
                        state = json.loads(self.state_path.read_text())
                        self.assertEqual(state['last_pull_result'], 'failure')
                        self.assertGreater(state['last_pull_at'], first['last_pull_at'])
                    else:
                        state = first
                        self.assertEqual(state['last_pull_result'], 'success')
                    expected = self.turn_rule(state)
                    if example == 'same_failure':
                        expected += '\n最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。'
                else:
                    # Execute the skill's selected-last order, then check its state.
                    pull('org/b')
                    before = self.state_path.read_bytes()
                    if example == 'selected_last_failure':
                        failing.add('org/a')
                    pull('org/a')
                    state = json.loads(self.state_path.read_text())
                    self.assertEqual(state['namespace'], 'org/b' if example == 'selected_last_failure' else 'org/a')
                    if example == 'selected_last_failure':
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

    def test_hook_only_autoselection_writes_refreshes_or_prunes_state(self):
        before = self.state_path.read_bytes()
        old = time.time() - 29 * 86400
        os.utime(self.state_path, (old, old))
        expired = self.state_path.with_name('expired.json')
        expired.write_bytes(before)
        expiry = time.time() - 31 * 86400
        os.utime(expired, (expiry, expiry))
        self.state_path.parent.chmod(0o500)
        try:
            for _ in range(2):
                self.assertEqual(self.prompt_context(prompt='https://example.com/doc'), self.turn_rule())
            for source in ('compact', 'resume'):
                self.context(self.hook('SessionStart', source=source), 'SessionStart')
            self.assertEqual(self.state_path.read_bytes(), before)
            self.assertEqual(self.state_path.stat().st_mtime, old)
            self.assertEqual(expired.read_bytes(), before)
            self.assertEqual(expired.stat().st_mtime, expiry)
        finally:
            self.state_path.parent.chmod(0o700)

    def test_E3_parent_config_above_git_root(self):
        self.state_path.unlink()
        outer = self.work / 'outer'
        repo = outer / 'repo'
        subprocess.run(['git', 'init', '-q', str(repo)], check=True, capture_output=True)
        cwd = repo / 'src'
        cwd.mkdir()
        config = outer / '.kioku/config'
        config.parent.mkdir()
        config.write_text(json.dumps({'server': 'https://kioku.magicians.ee', 'namespace': 'org/p'}))
        self.assertEqual(self.prompt_context(cwd=str(cwd), prompt='続けて'), self.candidate('org/p'))
        self.assertFalse(self.state_path.exists())
        nearest = repo / '.kioku/config'
        nearest.parent.mkdir()
        nearest.write_text(json.dumps({'server': 'https://kioku.magicians.ee', 'namespace': 'org/nearest'}))
        self.assertEqual(self.prompt_context(cwd=str(cwd), prompt='続けて'), self.candidate('org/nearest'))

    def test_removed_matching_inputs(self):
        # Without triggers, names and descriptions are displayed without auto-selection.
        self.state_path.unlink()
        repo = self.work / 'repo'
        repo.mkdir()
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)
        catalog.write_text(json.dumps({'projects': [
            {'namespace': 'org/tesla', 'server': 'https://kioku.magicians.ee',
             'path': str(repo), 'description': '電気自動車'},
            {'namespace': 'org/ses', 'server': 'https://kioku.magicians.ee', 'path': str(repo)},
        ]}))
        for prompt in ('tesla', '「tesla」の変更', 'ses', 'uses', '電気自動車', '続けて'):
            context = self.prompt_context(cwd=str(repo), prompt=prompt)
            self.assertIn('- org/tesla: 電気自動車', context)
            self.assertIn('- org/ses: （説明なし）', context)
            self.assertNotIn('［ヒント:', '\n'.join(context.splitlines()[1:]))
            self.assertFalse(self.state_path.exists())

    def test_E24_E25_background_pull(self):
        status = [200]
        entered, release = threading.Event(), threading.Event()

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                entered.set()
                release.wait(10)
                if status[0] != 200:
                    self.send_error(status[0])
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps({'revision': 'r', 'files': {}}).encode())

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        origin = f'http://127.0.0.1:{server.server_port}'
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)
        self.env['KIOKU_SERVER'] = 'https://wrong.example'
        for result in ('success', 'failure'):
            with self.subTest(result=result):
                status[0] = 200 if result == 'success' else 403
                entered.clear()
                release.clear()
                self.state_path.unlink(missing_ok=True)
                root = self.work / result
                (root / '.kioku').mkdir(parents=True)
                (root / '.kioku/config').write_text(json.dumps({'server': origin, 'namespace': 'org/a'}))
                (root / 'wiki').mkdir()
                (root / 'wiki/index.md').write_text('# local copy')
                catalog.write_text(json.dumps({'projects': [{'server': origin, 'namespace': 'org/a',
                    'path': str(root), 'description': 'A', 'triggers': ['tesla']}]}))
                try:
                    began = time.monotonic()
                    context = self.prompt_context(cwd=str(root), prompt='Tesla のログ')
                    self.assertLess(time.monotonic() - began, 2)
                    self.assertIn('# local copy', context)
                    self.assertIn('理由: trigger 語 tesla', context)
                    self.assertTrue(entered.wait(2))
                    state = json.loads(self.state_path.read_text())
                    self.assertEqual(state['namespace'], 'org/a')
                    self.assertNotIn('last_pull_result', state)
                    self.assertIn('背景の pull は終わっていない。', self.prompt_context(prompt='続けて'))
                finally:
                    release.set()
                wait_until(lambda: json.loads(self.state_path.read_text()).get('last_pull_result') == result)
                state = json.loads(self.state_path.read_text())
                self.assertEqual(state['server'], origin)
                expected = self.turn_rule(state)
                if result == 'failure':
                    expected += '\n最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。'
                self.assertEqual(self.prompt_context(prompt='続けて'), expected)
                log = root / '.kioku/pull.log'
                self.assertEqual(log.stat().st_mode & 0o777, 0o600)
                self.assertTrue(log.read_text())
                if result == 'failure':
                    self.assertIn('kioku:', log.read_text())
                self.assertFalse(self.state_path.with_name('environment-is-not-the-hook-id.json').exists())

    def fake_claude(self, script, *, restricted=False):
        bin_dir = self.work / 'fake-bin'
        bin_dir.mkdir()
        executable = bin_dir / 'claude'
        executable.write_text(script)
        executable.chmod(0o755)
        self.env['PYTHONPATH'] = str(Path(__file__).parent)
        if restricted:
            for command in ('sh', 'dirname'):
                (bin_dir / command).symlink_to(shutil.which(command))
            self.env['PATH'] = str(bin_dir)
        else:
            self.env['PATH'] = str(bin_dir) + os.pathsep + self.env['PATH']
        return executable

    def test_E13_E37_background_capture(self):
        self.fake_claude('#!' + sys.executable + '\n' + r'''import json, os, pathlib, subprocess, sys
log = pathlib.Path(os.environ['CAPTURE_FIXTURE_LOG'])
assert sys.argv[1] == '--bg'
instructions = (pathlib.Path(os.environ['KIOKU_CAPTURE_DIR']) / 'instructions.md').read_text()
log.write_text(json.dumps({'args': sys.argv[1:], 'cwd': os.getcwd(), 'instructions':instructions,
                          'sid': os.environ.get('CLAUDE_CODE_SESSION_ID')}))
assert sys.stdin.read() == ''
# A detached fixture session reports acceptance only after its launcher returns.
code = """import json, os, pathlib, subprocess
from hook_test_helpers import wait_until
p = pathlib.Path(os.environ['CAPTURE_FIXTURE_PROJECT'])
def live_lock():
    lock = json.loads((p / '.kioku/capture.lock').read_text())
    return lock if lock.get('bg_id') else None
lock = wait_until(live_lock)
cli = os.environ['CAPTURE_FIXTURE_CLI']
urls = [row.split('\t')[1] for row in (p / '.kioku/queue.tsv').read_text().splitlines()]
for url in urls:
    subprocess.run([cli, '-C', str(p), '-n', 'org/a', 'capture', 'result', '--capture-token', lock['token'],
                    '--result', 'skipped', '--reason', 'not a document fixture', url], check=True)
"""
subprocess.Popen([sys.executable, '-c', code], stdin=subprocess.DEVNULL,
                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
print('backgrounded · 1a2b3c4d')
''', restricted=True)
        self.env['KIOKU_SERVER'] = 'https://wrong.example'
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)
        for event, count in [('Stop', 1), ('SessionEnd', 1)]:
            with self.subTest(event=event):
                root = self.work / event
                metadata, project = make_project(root, 'org/a')
                _, other = make_project(self.work / ('other-' + event), 'org/a')
                catalog.write_text(json.dumps({'projects': [other, project]}))
                self.state = dict(project, mode='local')
                self.save_state()
                queue = metadata / 'queue.tsv'
                write_auth(self.env['HOME'], self.state['server'])
                for i in range(count):
                    self.assertEqual(self.hook('PostToolUse', cwd=str(root), tool_name='WebFetch',
                                               tool_input={'url': f'https://x.example/{i}'},
                                               agent_id='fixture-child'), '')
                self.assertIn('https://x.example/0', queue.read_text())
                log = root / 'fixture.json'
                self.env.update(CAPTURE_FIXTURE_LOG=str(log), CAPTURE_FIXTURE_PROJECT=str(root),
                                CAPTURE_FIXTURE_CLI=str(self.cli))
                output = self.hook(event, cwd=str(root))
                self.assertEqual(output, '')
                wait_until(lambda: (metadata / 'capture.log').exists() and 'claude logs 1a2b3c4d で見る。' in (metadata / 'capture.log').read_text())
                wait_until(lambda: not (metadata / 'capture.lock').exists())
                self.assertEqual(queue.read_text(), '')
                self.assertEqual((metadata / 'capture.log').stat().st_mode & 0o777, 0o600)
                recorded = json.loads(log.read_text())
                self.assertEqual(recorded['cwd'], str(root))
                self.assertIsNone(recorded['sid'])
                self.assertEqual(recorded['args'][0], '--bg')
                self.assertNotIn('-p', recorded['args'])
                args = recorded['args']
                pre = json.loads(args[args.index('--settings') + 1])['hooks']['PreToolUse']
                self.assertEqual(len(pre), 1)
                self.assertEqual(pre[0]['matcher'], '^(Artifact|mcp__.*|Bash|Read|Write)$')
                self.assertFalse(pre[0]['hooks'][0].get('async', False))
                self.assertEqual(shlex.split(pre[0]['hooks'][0]['command']),
                                 ['sh', str(self.root / 'hooks/kioku.sh'), 'tool-guard'])
                self.assertIn('instructions.md', args[-1])
                self.assertIn('https://x.example/0', recorded['instructions'])
                self.assertNotIn('https://x.example/0', recorded['args'][-1])
                self.assertNotIn('capture unlock', recorded['args'][-1])

    def test_session_start_recovers_all_projects_and_reports_failed_rows(self):
        self.fake_claude("#!/bin/sh\nprintf 'backgrounded · 5e6f7a8b\n'\n")
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)

        for source in ('startup', 'clear', 'fork', 'compact', 'resume'):
            with self.subTest(source=source):
                projects = []
                for name in ('one', 'two', 'busy', 'failed', 'exhausted', 'empty'):
                    root = self.work / source / name
                    server = 'https://other.example' if name == 'two' else 'https://kioku.magicians.ee'
                    metadata, project = make_project(root, f'org/{name}', server)
                    projects.append(project)
                    if name == 'empty':
                        continue
                    state, attempts, reason = {
                        'failed': ('failed', 2, 'login'),
                        'exhausted': ('queued', 2, ''),
                    }.get(name, ('queued', 0, ''))
                    (metadata / 'queue.tsv').write_text(
                        f'2026-11-18\thttps://example.com/{name}\t{attempts}\t{state}\t{reason}\n')
                    if name == 'busy':
                        (metadata / 'capture.lock').write_text(json.dumps({
                            'started_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
                            'bg_id': 'abcdef00', 'token': 'a' * 32,
                        }))
                catalog.write_text(json.dumps({'projects': projects}))
                self.state.update(projects[3])
                self.save_state()
                before = self.state_path.read_bytes()

                obj = json.loads(self.hook('SessionStart', source=source, cwd=str(self.work)))
                self.assertEqual(obj['hookSpecificOutput']['hookEventName'], 'SessionStart')
                context = obj['hookSpecificOutput']['additionalContext']
                self.assertIn('org/failed' if source in ('compact', 'resume') else 'trigger 語', context)
                message = obj['systemMessage']
                for name in ('one', 'two'):
                    self.assertNotIn(f'org/{name}', message)
                    metadata = self.work / source / name / '.kioku'
                    self.assertEqual(json.loads((metadata / 'capture.lock').read_text())['bg_id'], '5e6f7a8b')
                    self.assertIn('\t1\tqueued\t', (metadata / 'queue.tsv').read_text())
                for name in ('failed', 'exhausted'):
                    self.assertIn(f'org/{name} で受付済みにならなかった資料 1 件', message)
                for name in ('busy', 'empty'):
                    self.assertNotIn(f'org/{name}', message)
                self.assertEqual(self.state_path.read_bytes(), before)

                obj = json.loads(self.hook('SessionStart', source=source, cwd=str(self.work)))
                self.assertNotIn('capture を起動した', obj['systemMessage'])
                self.assertIn('org/failed で受付済みにならなかった資料 1 件', obj['systemMessage'])
                obj = json.loads(self.hook('Stop', cwd=str(self.work)))
                self.assertIn('受付済みにならなかった資料 1 件', obj['systemMessage'])
                self.assertNotIn('capture 実行中', obj['systemMessage'])

    def test_late_stop_launcher_failure_is_visible_at_next_hook(self):
        root = self.work / 'project'
        metadata, _ = make_project(root, 'org/project')
        queue = metadata / 'queue.tsv'
        pending = '2026-11-28\thttps://example.com/fixture\t0\tqueued\t\n'
        queue.write_text(pending)
        launcher = self.fake_claude('#!/bin/sh\nsleep 1\necho fixture failure >&2\nexit 1\n')

        self.assertEqual(self.hook('Stop', cwd=str(root)), '')
        marker = metadata / 'capture-launch.failed'
        wait_until(lambda: marker.exists() and not (metadata / 'capture.lock').exists())
        self.assertEqual(queue.read_text(), pending)
        self.assertIn('fixture failure', marker.read_text())

        launcher.write_text("#!/bin/sh\nprintf 'backgrounded · 5e6f7a8b\n'\n")
        output = json.loads(self.hook('Stop', cwd=str(root)))
        self.assertIn('起動に失敗', output['systemMessage'])
        self.assertFalse(marker.exists())
        self.assertEqual(json.loads((metadata / 'capture.lock').read_text())['bg_id'], '5e6f7a8b')
        status = subprocess.run([str(self.cli), '-C', str(root), 'status'],
                                capture_output=True, text=True, env=self.env)
        self.assertIn('fixture failure', json.loads(status.stdout)['last_capture_launch_error'])

    def test_delayed_success_does_not_clear_a_newer_launch_failure(self):
        root = self.work / 'project'
        metadata, _ = make_project(root, 'org/project')
        queue = metadata / 'queue.tsv'
        first = 'https://example.com/first'
        queue.write_text(f'2026-11-28\t{first}\t0\tqueued\t\n')
        release = self.work / 'release-first-launch'
        self.env['CAPTURE_TEST_RELEASE'] = str(release)
        self.fake_claude('#!' + sys.executable + '\n' + """
import os
from pathlib import Path
import sys
from hook_test_helpers import wait_until

instructions = (Path(os.environ['KIOKU_CAPTURE_DIR']) / 'instructions.md').read_text()
if 'https://example.com/first' in instructions:
    wait_until(Path(os.environ['CAPTURE_TEST_RELEASE']).exists, timeout=8)
    print('backgrounded · 5e6f7a8b')
else:
    sys.exit('newer launcher failed')
""")
        self.assertEqual(self.hook('Stop', cwd=str(root)), '')
        token = json.loads((metadata / 'capture.lock').read_text())['token']
        result = subprocess.run([str(self.cli), '-C', str(root), 'capture', 'result',
                                 '--capture-token', token, '--result', 'skipped', '--reason',
                                 'fixture', first], capture_output=True, text=True, env=self.env)
        self.assertEqual(result.returncode, 0, result.stderr)
        queue.write_text('2026-11-28\thttps://example.com/second\t0\tqueued\t\n')
        self.assertEqual(self.hook('SessionEnd', cwd=str(root)), '')
        marker = metadata / 'capture-launch.failed'
        wait_until(marker.exists)
        release.touch()
        log = metadata / 'capture.log'
        wait_until(lambda: 'claude logs 5e6f7a8b' in log.read_text())
        self.assertIn('newer launcher failed', marker.read_text())

    def test_session_start_does_not_wait_for_one_project_before_launching_another(self):
        self.fake_claude('#!' + sys.executable + '\n' + """
import os
from pathlib import Path
from hook_test_helpers import wait_until

instructions = (Path(os.environ['KIOKU_CAPTURE_DIR']) / 'instructions.md').read_text()
marker = Path(os.environ['CAPTURE_TEST_MARKER'])
if 'https://example.com/one' in instructions:
    wait_until(marker.exists, timeout=2)
elif 'https://example.com/two' in instructions:
    marker.touch()
print('backgrounded · 5e6f7a8b')
""")
        self.env['CAPTURE_TEST_MARKER'] = str(self.work / 'second-started')
        projects = []
        for name in ('one', 'two'):
            root = self.work / name
            metadata, project = make_project(root, f'org/{name}')
            (metadata / 'queue.tsv').write_text(
                f'2026-11-18\thttps://example.com/{name}\t0\tqueued\t\n')
            projects.append(project)
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)
        catalog.write_text(json.dumps({'projects': projects}))

        output = self.hook('SessionStart', source='startup')
        self.assertNotIn('systemMessage', output)
        for name in ('one', 'two'):
            lock = self.work / name / '.kioku/capture.lock'
            self.assertEqual(json.loads(lock.read_text())['bg_id'], '5e6f7a8b')

    def test_session_start_does_not_cancel_a_slow_capture_launcher(self):
        self.fake_claude('#!' + sys.executable + '\n' + """
import os
from pathlib import Path
from hook_test_helpers import wait_until

wait_until(Path(os.environ['CAPTURE_TEST_RELEASE']).exists, timeout=2)
print('backgrounded · 5e6f7a8b')
""")
        release = self.work / 'release-launcher'
        self.env.update(KIOKU_SESSION_START_CAPTURE_WAIT='100ms', CAPTURE_TEST_RELEASE=str(release))
        project = self.work / 'project'
        metadata, config = make_project(project, 'org/project')
        queue = metadata / 'queue.tsv'
        queue.write_text('2026-11-18\thttps://example.com/slow\t0\tqueued\t\n')
        catalog = Path(self.env['HOME']) / '.config/kioku/projects.json'
        catalog.parent.mkdir(parents=True)
        catalog.write_text(json.dumps({'projects': [config]}))

        output = self.hook('SessionStart', source='startup', cwd=str(self.work))
        release.touch()
        self.assertNotIn('systemMessage', output)
        lock = metadata / 'capture.lock'
        log = metadata / 'capture.log'

        def launched():
            if '\t0\tqueued\t' in queue.read_text() and (metadata / 'capture-launch.failed').exists():
                self.fail('SessionStart cancelled the launcher while waiting for its ID')
            return (lock.exists() and json.loads(lock.read_text()).get('bg_id') == '5e6f7a8b'
                    and 'claude logs 5e6f7a8b' in log.read_text())

        wait_until(launched, timeout=3)


if __name__ == '__main__':
    unittest.main()
