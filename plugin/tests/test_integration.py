"""Offline integration of the skill's pull commands with a real CLI."""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest


@unittest.skipUnless(os.environ.get('KIOKU_TEST_BINARY'), 'set KIOKU_TEST_BINARY for real CLI integration')
class IntegrationTest(unittest.TestCase):
    def test_initial_and_configured_pull(self):
        calls = []
        body = '# Shared wiki\n'
        digest = hashlib.sha256(body.encode()).hexdigest()

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(self.path)
                if self.path == '/api/projects':
                    data = [{'id': 'p', 'namespace': 'org/p'}]
                elif 'manifest=1' in self.path:
                    data = {'revision': 'revision', 'files': {
                        'wiki/index.md': {'hash': digest, 'bytes': len(body)},
                    }}
                elif 'file=' in self.path:
                    data = {'body': body, 'hash': digest}
                else:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(data).encode())

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        with tempfile.TemporaryDirectory(prefix='kioku integration ') as tmp:
            base = Path(tmp).resolve()
            project = base / 'home' / '.local/share/kioku/projects/org/p'
            server_url = f'http://127.0.0.1:{server.server_port}'
            cli = str(Path(os.environ['KIOKU_TEST_BINARY']).resolve())
            env = dict(os.environ, HOME=str(base / 'home'))
            env.pop('KIOKU_AGENT_TOKEN', None)
            env.pop('KIOKU_SERVER', None)
            # The CLI chooses and creates the directory, then records the binding.
            first = subprocess.run(
                [cli, '--server', server_url, '-n', 'org/p',
                 'pull', '--description', 'Shared integration wiki'],
                cwd=base, env=env, text=True, capture_output=True, timeout=600,
            )
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertEqual(first.stderr, "")
            self.assertFalse((base / '.kioku').exists())
            self.assertEqual(json.loads((project / '.kioku/config').read_text()), {
                'server': server_url, 'namespace': 'org/p',
            })
            self.assertEqual((project / 'wiki/index.md').read_text(), body)
            projects_path = base / 'home/.config/kioku/projects.json'
            expected = {'projects': [{
                'server': server_url, 'namespace': 'org/p',
                'path': str(project), 'description': 'Shared integration wiki',
            }]}
            self.assertEqual(json.loads(projects_path.read_text()), expected)
            self.assertEqual(projects_path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(project.stat().st_mode & 0o777, 0o700)
            count = len(calls)
            second = subprocess.run(
                [cli, '--server', server_url, '-n', 'org/p', 'pull'],
                cwd=base / 'home', env=env, text=True, capture_output=True, timeout=600,
            )
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertEqual(second.stderr, "")
            self.assertGreater(len(calls), count)
            self.assertEqual(json.loads(projects_path.read_text()), expected)
            self.assertEqual((project / 'wiki/index.md').read_text(), body)

            # A malformed list stops before requests and remains untouched.
            projects_path.write_text('{')
            count = len(calls)
            broken = subprocess.run(
                [cli, '--server', server_url, '-n', 'org/p', 'pull'],
                cwd=base, env=env, text=True, capture_output=True, timeout=600,
            )
            self.assertEqual(broken.returncode, 1)
            self.assertIn(str(projects_path), broken.stderr)
            self.assertEqual(projects_path.read_text(), '{')
            self.assertEqual(len(calls), count)

            # Failure to write the list does not fail a configured pull.
            projects_path.unlink()
            projects_path.mkdir()
            warning = subprocess.run(
                [cli, '--server', server_url, '-C', str(project), '-n', 'org/p', 'pull'],
                cwd=base, env=env, text=True, capture_output=True, timeout=600,
            )
            self.assertEqual(warning.returncode, 0, warning.stderr)
            self.assertIn('warning:', warning.stderr)
            self.assertIn('next pull', warning.stderr)
            self.assertNotIn('warning:', warning.stdout)
            self.assertEqual((project / 'wiki/index.md').read_text(), body)


if __name__ == '__main__':
    unittest.main()
