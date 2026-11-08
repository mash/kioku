import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import queue
import threading
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('package', ROOT / 'codex-plugin/tools/package.py')
pkg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pkg)
VERSION = 'v' + json.loads((ROOT / 'plugin/.claude-plugin/plugin.json').read_text())['version']
COMMIT = 'a' * 40


def metadata(target, version=VERSION, commit=COMMIT):
    os_name, arch = target.split('-')
    flags = f'-s -w -X {pkg.BUILDINFO}.Version={version} -X {pkg.BUILDINFO}.Commit={commit}'
    return f'cli: go1.27.3\n\tbuild\t-ldflags={json.dumps(flags)}\n\tbuild\tGOOS={os_name}\n\tbuild\tGOARCH={arch}\n'


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='kioku codex ')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.binaries = self.root / 'binaries'
        self.output = self.root / 'output'
        for target in pkg.TARGETS:
            binary = self.binaries / target.replace('-', '_') / 'kioku'
            binary.parent.mkdir(parents=True)
            binary.write_text('#!/bin/sh\nprintf "cwd=%s\\n" "$PWD"\nprintf "arg=%s\\n" "$@"\nexit 17\n')
            binary.chmod(0o755)
            pkg.write_json(binary.with_name('build.json'), {'version': VERSION, 'commit': COMMIT, 'target': target, 'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()})

    def build(self):
        def get_metadata(command, **kwargs):
            return metadata(Path(command[-1]).parent.name.replace('_', '-'))
        with patch.object(pkg.subprocess, 'check_output', side_effect=get_metadata):
            pkg.package(self.binaries, self.output, VERSION, COMMIT, 'kioku-test-package')
        return self.output / 'marketplace/plugins/kioku'

    def test_complete_archives_no_symlinks_or_claude_hooks(self):
        plugin = self.build()
        for pattern, prefix in [('*.tar.gz', 'kioku-codex'), ('*.tgz', 'package')]:
            archive = next(self.output.glob(pattern))
            with tarfile.open(archive) as tar:
                members = tar.getmembers()
                self.assertTrue(all(m.name.startswith(prefix + '/') for m in members))
                self.assertFalse(any(m.issym() or m.islnk() for m in members))
                self.assertEqual(sum(m.name.endswith('/kioku') and '/libexec/' in m.name for m in members), 4)
                self.assertFalse(any('/.claude-plugin/' in m.name for m in members))
                hook = next(m for m in members if m.name.endswith('/hooks/session-start.sh'))
                self.assertEqual(hook.mode & 0o111, 0o111)
        manifest = json.loads((plugin / '.codex-plugin/plugin.json').read_text())
        self.assertEqual(manifest['version'], VERSION[1:])
        self.assertTrue(manifest['requires_local_executor'])
        self.assertNotIn('scripts', json.loads((plugin / 'package.json').read_text()))
        sums = json.loads((plugin / 'checksums.json').read_text())
        self.assertEqual(sums['commit'], COMMIT)
        for name, digest in sums['sha256'].items():
            self.assertEqual(hashlib.sha256((plugin / name).read_bytes()).hexdigest(), digest)

    def test_canonical_skill_adapted_and_references_preserved(self):
        plugin = self.build()
        for src in (ROOT / 'plugin/skills/kioku').rglob('*.md'):
            dest = plugin / 'skills/kioku' / src.relative_to(ROOT / 'plugin/skills/kioku')
            self.assertTrue(dest.is_file())
            text = dest.read_text()
            for forbidden in ('CLAUDE_', '/reload-plugins', '! kioku', '! /abs', 'Claude'):
                self.assertNotIn(forbidden, text)
        text = (plugin / 'skills/kioku/SKILL.md').read_text()
        self.assertIn('原本の HTTPS', text)
        self.assertIn('426', text)
        self.assertIn('checksum', text)

    def test_binary_changed_after_verification_is_rejected(self):
        original = pkg.check_binary
        def verify_then_replace(path, target, version, commit):
            digest = original(path, target, version, commit)
            if target == pkg.TARGETS[-1]:
                (self.binaries / 'darwin_arm64/kioku').write_text('changed after validation')
            return digest
        with patch.object(pkg, 'check_binary', side_effect=verify_then_replace):
            with self.assertRaisesRegex(ValueError, 'changed after verification'):
                self.build()
        self.assertFalse(self.output.exists())

    @unittest.skipUnless(os.environ.get('CODEX_TEST_BINARY'), 'set CODEX_TEST_BINARY for real Codex discovery')
    def test_real_codex_discovers_skill_and_untrusted_hook(self):
        self.build()
        cli = os.environ['CODEX_TEST_BINARY']
        home = self.root / 'codex-home'
        home.mkdir()
        env = dict(os.environ, CODEX_HOME=str(home))
        marketplace = self.output / 'marketplace'
        for args in [('marketplace', 'add', str(marketplace)), ('add', 'kioku@kioku')]:
            subprocess.run([cli, 'plugin', *args, '--json'], env=env, cwd=self.root,
                           capture_output=True, text=True, check=True, timeout=30)
        with subprocess.Popen([cli, 'app-server'], env=env, cwd=self.root, text=True,
                              stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL) as server:
            messages = queue.Queue()
            def read():
                for line in server.stdout:
                    messages.put(json.loads(line))
            reader = threading.Thread(target=read, daemon=True)
            reader.start()
            def request(i, method, params):
                server.stdin.write(json.dumps({'id': i, 'method': method, 'params': params}) + '\n')
                server.stdin.flush()
                while True:
                    result = messages.get(timeout=20)
                    if result.get('id') == i:
                        self.assertNotIn('error', result)
                        return result['result']
            try:
                request(1, 'initialize', {'clientInfo': {'name': 'kioku-test', 'version': '1'},
                                         'capabilities': {'experimentalApi': True}})
                server.stdin.write('{"method":"initialized"}\n')
                server.stdin.flush()
                skills = request(2, 'skills/list', {'cwds': [str(self.root)]})['data'][0]
                self.assertFalse(skills['errors'])
                self.assertTrue(any(s.get('pluginId') == 'kioku@kioku' and s['enabled'] for s in skills['skills']))
                hooks = request(3, 'hooks/list', {'cwds': [str(self.root)]})['data'][0]
                self.assertFalse(hooks['errors'])
                selected = [h for h in hooks['hooks'] if h.get('pluginId') == 'kioku@kioku']
                self.assertEqual(len(selected), 1)
                self.assertEqual(selected[0]['eventName'], 'sessionStart')
                self.assertEqual(selected[0]['trustStatus'], 'untrusted')
            finally:
                server.terminate()
                server.wait(timeout=10)
                reader.join(timeout=5)

    def test_existing_output_is_never_overwritten(self):
        self.build()
        with self.assertRaisesRegex(ValueError, 'overwrite'):
            self.build()

    def test_wrong_version_rejected(self):
        with self.assertRaisesRegex(ValueError, 'version must match'):
            pkg.package(self.binaries, self.output, 'v999.0.0', COMMIT, 'kioku-test')
        self.assertFalse(self.output.exists())

    def test_missing_binary_rejected(self):
        path = self.binaries / 'linux_arm64/kioku'
        path.unlink()
        with self.assertRaisesRegex(ValueError, 'missing'):
            pkg.check_binary(path, 'linux-arm64', VERSION, COMMIT)

    def test_wrong_build_target_version_commit_rejected(self):
        binary = self.binaries / 'darwin_arm64/kioku'
        with patch.object(pkg.subprocess, 'check_output', return_value=metadata('linux-arm64')):
            with self.assertRaises(ValueError):
                pkg.check_binary(binary, 'darwin-arm64', VERSION, COMMIT)
        for field, bad in [('version', 'v999.0.0'), ('commit', 'b' * 40), ('sha256', '0' * 64)]:
            receipt = json.loads(binary.with_name('build.json').read_text())
            receipt[field] = bad
            pkg.write_json(binary.with_name('build.json'), receipt)
            with patch.object(pkg.subprocess, 'check_output', return_value=metadata('darwin-arm64')):
                with self.assertRaises(ValueError):
                    pkg.check_binary(binary, 'darwin-arm64', VERSION, COMMIT)

    def test_upstream_instruction_changes_fail_closed(self):
        with self.assertRaises(ValueError):
            pkg.adapt_selection('## changed upstream\n')

    def test_launcher_preserves_args_cwd_and_exit_code(self):
        plugin = self.build()
        result = subprocess.run([str(plugin / 'bin/kioku'), '-C', 'path with spaces', 'a;$(echo no)'],
                                cwd=self.root, text=True, capture_output=True)
        self.assertEqual(result.returncode, 17, result.stderr)
        self.assertIn('arg=path with spaces\n', result.stdout)
        self.assertIn('arg=a;$(echo no)\n', result.stdout)
        self.assertIn(f'cwd={self.root.resolve()}\n', result.stdout)

    def host_binary(self, plugin):
        os_name = {'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()]
        arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'amd64': 'amd64'}[platform.machine()]
        return plugin / f'libexec/{os_name}-{arch}/kioku'

    def test_corruption_rejected_before_execute(self):
        plugin = self.build()
        self.host_binary(plugin).write_text('#!/bin/sh\necho should-not-execute\n')
        result = subprocess.run([str(plugin / 'bin/kioku')], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('checksum mismatch', result.stderr)
        self.assertNotIn('should-not-execute', result.stdout)

    def test_missing_binary_does_not_fall_back_to_path(self):
        plugin = self.build()
        self.host_binary(plugin).unlink()
        result = subprocess.run([str(plugin / 'bin/kioku')], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('reinstall', result.stderr)

    def test_unsupported_platform(self):
        plugin = self.build()
        fake = self.root / 'fake'
        fake.mkdir()
        (fake / 'uname').write_text('#!/bin/sh\necho Unsupported\n')
        (fake / 'uname').chmod(0o755)
        env = dict(os.environ, PATH=str(fake) + ':' + os.environ['PATH'])
        result = subprocess.run([str(plugin / 'bin/kioku')], env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unsupported operating system', result.stderr)

    def test_hook_is_context_only_and_repeatable(self):
        plugin = self.build()
        # Delete binaries: hook must not need executable code or network to remind.
        shutil.rmtree(plugin / 'libexec')
        for source in ['startup', 'resume', 'compact', 'fork', 'clear']:
            result = subprocess.run(['sh', str(plugin / 'hooks/session-start.sh')],
                                    input=json.dumps({'source': source}), text=True, capture_output=True)
            self.assertEqual(result.returncode, 0)
            self.assertIn(str(plugin / 'bin/kioku'), result.stdout)
            self.assertIn('do not repeat a completed pull', result.stdout)
            self.assertIn('has not authenticated', result.stdout)
            self.assertNotIn('cwd=', result.stdout)
        config = json.loads((plugin / 'hooks/hooks.json').read_text())
        self.assertEqual(list(config['hooks']), ['SessionStart'])


if __name__ == '__main__':
    unittest.main()
