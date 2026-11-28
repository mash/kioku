import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class ToolHooksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        build = tempfile.TemporaryDirectory(prefix="kioku-tool-hooks-")
        cls.addClassCleanup(build.cleanup)
        cls.binary = Path(build.name) / "kioku"
        subprocess.run(["go", "build", "-o", str(cls.binary), "./cmd/kioku"],
                       cwd=ROOT.parent, check=True, capture_output=True)

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.base = Path(tmp.name).resolve()
        self.data = self.base / "plugin-data"
        self.cli = self.data / "bin" / "kioku"
        self.cli.parent.mkdir(parents=True)
        self.cli.symlink_to(self.binary)
        self.token = "a" * 32
        self.metadata = self.base / ".kioku"
        self.snapshot = self.metadata / "capture-tmp" / self.token
        self.snapshot.mkdir(parents=True)
        self.artifact = "https://claude.ai/artifact/fixture"
        payload = ('2026-11-28\t' + self.artifact + '\t1\tqueued\t\n'
                   '2026-11-28\ttool:{"input":{"id":42},"tool":"mcp__linear__get_issue"}\t1\tqueued\t\n')
        (self.snapshot / "snapshot.tsv").write_text(payload)
        (self.metadata / "capture.lock").write_text(json.dumps({
            "token": self.token, "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "snapshot_hash": hashlib.sha256(payload.encode()).hexdigest(),
        }))
        self.env = dict(os.environ, CLAUDE_PLUGIN_ROOT=str(ROOT), CLAUDE_PLUGIN_DATA=str(self.data),
                        KIOKU_CAPTURE="1", KIOKU_CAPTURE_DIR=str(self.snapshot))

    def invoke(self, name, input, event="PreToolUse"):
        guard = "tool-guard" if event == "PreToolUse" else "tool"
        result = subprocess.run(["sh", str(ROOT / "hooks/kioku.sh"), guard],
                                env=self.env, text=True, capture_output=True, check=True,
                                input=json.dumps({"hook_event_name":event, "session_id":"session-1",
                                                  "tool_name":name, "tool_input":input}))
        self.assertEqual(result.stderr, "")
        return result.stdout

    def denied(self, result):
        self.assertEqual(json.loads(result)["hookSpecificOutput"]["permissionDecision"], "deny")

    def test_pre_hook_is_synchronous_and_denies_before_execution(self):
        hooks = json.loads((ROOT / "hooks/hooks.json").read_text())["hooks"]["PreToolUse"]
        self.assertEqual(hooks[0]["hooks"][0]["type"], "command")
        self.assertFalse(hooks[0]["hooks"][0].get("async", False))
        for name, arg, allow in [
            ("Artifact", {"action":"read", "url":self.artifact,
                          "path":str(self.snapshot / "artifact.html")}, True),
            ("Artifact", {"action":"publish", "url":self.artifact}, False),
            ("Artifact", {"file_path":"x.html"}, False),
            ("Artifact", {"action":"read", "url":self.artifact, "path":"/outside.html"}, False),
            ("mcp__linear__get_issue", {"id":42}, True),
            ("mcp__other__get_issue", {"id":42}, False),
            ("mcp__linear__update_issue", {"id":42}, False),
            ("Write", {"file_path":"/outside.md"}, False),
        ]:
            with self.subTest(name=name, arg=arg):
                actual = self.invoke(name, arg)
                if allow:
                    self.assertEqual(actual, "")
                else:
                    self.denied(actual)

    def test_missing_snapshot_or_guard_fails_closed_only_in_capture(self):
        (self.snapshot / "snapshot.tsv").unlink()
        self.denied(self.invoke("mcp__linear__get_issue", {"id":42}))
        self.cli.unlink()
        self.denied(self.invoke("mcp__linear__get_issue", {"id":42}))
        self.env.pop("KIOKU_CAPTURE")
        self.assertEqual(self.invoke("Artifact", {"action":"publish"}), "")


if __name__ == "__main__":
    unittest.main()
