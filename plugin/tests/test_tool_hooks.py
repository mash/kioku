from concurrent.futures import ThreadPoolExecutor
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from hook_test_helpers import ROOT, make_project, build_test_binary, write_auth


class ToolHooksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binary = build_test_binary(cls)

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.base = Path(tmp.name).resolve()
        self.data = self.base / "plugin-data"
        self.cli = self.data / "bin" / "kioku"
        self.cli.parent.mkdir(parents=True)
        self.cli.symlink_to(self.binary)
        self.guard_cli = self.base / "capture CLI's binary"
        self.guard_cli.symlink_to(self.binary)
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
                        KIOKU_CAPTURE="1", KIOKU_CAPTURE_DIR=str(self.snapshot),
                        KIOKU_CAPTURE_CLI=str(self.guard_cli))

    def invoke(self, name, input, event="PreToolUse", **metadata):
        guard = "tool-guard" if event == "PreToolUse" else "tool"
        result = subprocess.run(["sh", str(ROOT / "hooks/kioku.sh"), guard],
                                env=self.env, text=True, capture_output=True, check=True,
                                input=json.dumps({"hook_event_name":event, "session_id":"session-1",
                                                  "tool_name":name, "tool_input":input, **metadata}))
        self.assertEqual(result.stderr, "")
        return result.stdout

    def denied(self, result):
        self.assertEqual(json.loads(result)["hookSpecificOutput"]["permissionDecision"], "deny")

    def test_capture_guard_wiring(self):
        hooks = json.loads((ROOT / "hooks/hooks.json").read_text())["hooks"]
        self.assertNotIn("PreToolUse", hooks)
        read = {"action": "read", "url": self.artifact, "path": "index.html"}
        self.env["CLAUDE_PLUGIN_DATA"] = str(self.base / "stale-plugin-data")
        self.assertEqual(self.invoke("Artifact", read), "")
        self.denied(self.invoke("Artifact", {"action": "publish", "url": self.artifact}))
        self.env.pop("CLAUDE_PLUGIN_DATA")
        self.assertEqual(self.invoke("Artifact", read), "")

        # A working ordinary plugin CLI must not bypass guard failures.
        self.env["CLAUDE_PLUGIN_DATA"] = str(self.data)
        self.env["KIOKU_CAPTURE_CLI"] = str(self.base / "missing-cli")
        self.denied(self.invoke("Artifact", read))
        failing_cli = self.base / "failing-cli"
        failing_cli.write_text("#!/bin/sh\nprintf 'partial response'\nexit 1\n")
        failing_cli.chmod(0o700)
        self.env["KIOKU_CAPTURE_CLI"] = str(failing_cli)
        self.denied(self.invoke("Artifact", read))
        self.env["KIOKU_CAPTURE_CLI"] = str(self.guard_cli)
        self.env.pop("KIOKU_CAPTURE")
        self.denied(self.invoke("Artifact", read))

    def test_post_hook_queues_sources_only_for_existing_local_writer(self):
        self.env.update(HOME=str(self.base / "home"), XDG_STATE_HOME=str(self.base / "state"))
        self.env.pop("XDG_CONFIG_HOME", None)
        self.env.pop("KIOKU_CAPTURE")
        event = "PostToolUse"
        hook = json.loads((ROOT / "hooks/hooks.json").read_text())["hooks"][event][0]
        self.assertTrue(hook["hooks"][0]["async"])
        self.assertIn("Artifact", hook["matcher"])
        meta, config = make_project(self.base / "project", "org/p", "https://example.invalid")
        queue = meta / "queue.tsv"
        page = {"url":"https://example.test/source"}
        self.assertEqual(self.invoke("WebFetch", page, event), "")
        self.assertFalse(queue.exists())
        state = self.base / "state" / "kioku" / "sessions" / "session-1.json"
        state.parent.mkdir(parents=True)
        state.write_text(json.dumps(dict(config, mode="local")))
        write_auth(self.env["HOME"], config["server"])
        self.assertEqual(self.invoke("WebFetch", page, event, agent_id="child", tool_response="discarded"), "")
        first = queue.read_bytes()
        self.assertIn(b"example.test/source", first)
        self.assertNotIn(b"discarded", first)
        self.assertEqual(self.invoke("WebFetch", page, event), "")
        self.assertEqual(first, queue.read_bytes())
        published = f"Published «fixture» at {self.artifact} (Version 1, version id 1790000000-aa11)"
        self.assertEqual(self.invoke("Artifact", {"file_path":"fixture.html"}, event, tool_response=published), "")
        self.assertIn(self.artifact.encode(), queue.read_bytes())
        before = queue.read_bytes()
        self.assertEqual(self.invoke("Artifact", {"url":self.artifact, "file_path":"fixture.html"},
                                     event, tool_response=published), "")
        self.assertEqual(before, queue.read_bytes())

        # Separate hook processes contend for the same project's queue. Each
        # source must survive and duplicate calls must not add another row.
        urls = [f"https://example.test/parallel/{n}" for n in range(12)]
        with ThreadPoolExecutor(max_workers=12) as pool:
            outputs = list(pool.map(lambda url: self.invoke("WebFetch", {"url":url}, event), urls * 2))
        self.assertEqual(outputs, [""] * (len(urls) * 2))
        before = queue.read_bytes()
        rows = [line.split("\t") for line in before.decode().splitlines()]
        self.assertCountEqual([row[1] for row in rows], [page["url"], self.artifact, *urls])
        for row in rows:
            self.assertEqual(row[2:], ["0", "queued", ""])

        self.env["KIOKU_CAPTURE"] = "1"
        self.assertEqual(self.invoke("mcp__linear__get_issue", {"id":42}, event), "")
        self.assertEqual(before, queue.read_bytes())
        self.assertFalse((meta / "capture.lock").exists())


if __name__ == "__main__":
    unittest.main()
