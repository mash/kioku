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
            project = base / 'project'
            server_url = f'http://127.0.0.1:{server.server_port}'
            cli = str(Path(os.environ['KIOKU_TEST_BINARY']).resolve())
            env = dict(os.environ, HOME=str(base / 'home'))
            env.pop('KIOKU_AGENT_TOKEN', None)
            env.pop('KIOKU_SERVER', None)
            # The skill runs mkdir -p P, then calls the CLI path as the command name.
            project.mkdir(parents=True)
            first = subprocess.run(
                [cli, '-C', str(project), '--server', server_url, 'pull', 'org/p'],
                env=env, text=True, capture_output=True, timeout=600,
            )
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertEqual(json.loads((project / '.kioku/config').read_text()), {
                'server': server_url, 'namespace': 'org/p',
            })
            self.assertEqual((project / 'wiki/index.md').read_text(), body)
            count = len(calls)
            second = subprocess.run([cli, '-C', str(project), 'pull'],
                                    env=env, text=True, capture_output=True, timeout=600)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertGreater(len(calls), count)
            self.assertEqual((project / 'wiki/index.md').read_text(), body)


if __name__ == '__main__':
    unittest.main()
