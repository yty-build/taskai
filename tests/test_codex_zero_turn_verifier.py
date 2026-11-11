# ABOUTME: Exercises the Codex zero-turn verifier against a private Unix WebSocket daemon.
# ABOUTME: Uses fake replies and a fake TUI; no installed provider or model is contacted.

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

from websockets.exceptions import ConnectionClosed
from websockets.sync.server import unix_serve

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tools"))

from codex_zero_turn_verifier import Options, run_check  # noqa: E402


OWNED = "01a1b2b8-45d0-7672-bdb0-18c0a9d3ef0b"


class FakeDaemon:
    def __init__(self, root: Path, scenario: str = "success"):
        self.path = str(root / "daemon.sock")
        self.scenario = scenario
        self.methods = []
        self.requests = []
        self.errors = []
        self.lock = threading.Lock()
        self.server = None
        self.thread = None

    def __enter__(self):
        self.server = unix_serve(self.handle, path=self.path)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.server.shutdown()
        self.thread.join(timeout=2)
        if self.errors:
            raise AssertionError(self.errors)

    def handle(self, ws):
        try:
            while True:
                try:
                    request = json.loads(ws.recv(timeout=2))
                except ConnectionClosed:
                    return
                method = request.get("method")
                with self.lock:
                    self.methods.append(method)
                    self.requests.append(request)
                if method == "initialized":
                    continue
                req_id = request["id"]
                if method == "initialize":
                    assert request["params"]["capabilities"]["experimentalApi"] is True
                    self.send(ws, req_id, {"userAgent": "codex-tui/0.167.0 (fake)"})
                elif method == "thread/start":
                    assert request["params"]["dynamicTools"][0]["name"] == "taskai"
                    if self.scenario == "uncertain_create":
                        ws.close()
                        return
                    self.send(ws, req_id, {"thread": {"id": OWNED}, "approvalPolicy": "on-request"})
                elif method == "thread/unarchive":
                    assert request["params"] == {"threadId": OWNED}
                    if self.scenario == "unarchive_error":
                        ws.send(json.dumps({"id": req_id, "error": {"code": -32602, "message": "private unarchive error"}}))
                    else:
                        self.send(ws, req_id, {"thread": {"id": OWNED}})
                elif method == "thread/read":
                    assert request["params"] == {"threadId": OWNED, "includeTurns": True}
                    if self.scenario == "rpc_error":
                        ws.send(json.dumps({"id": req_id, "error": {"code": -32602, "message": "private error text"}}))
                    elif self.scenario == "missing_turns":
                        self.send(ws, req_id, {"thread": {"id": OWNED}})
                    elif self.scenario == "malformed_reply":
                        ws.send(json.dumps({"id": req_id, "wrong": {"secret": "never-log"}}))
                    elif self.scenario == "timeout":
                        continue
                    else:
                        if self.scenario == "unrelated_event":
                            ws.send(json.dumps({"method": "turn/started", "params": {"threadId": "other-secret-id", "body": "private event text"}}))
                        self.send(ws, req_id, {"thread": {"id": OWNED, "turns": []}})
                elif method == "thread/resume":
                    assert request["params"] == {"threadId": OWNED, "excludeTurns": True}
                    self.send(ws, req_id, {"thread": {"id": OWNED, "status": {"type": "idle"}}, "approvalPolicy": "on-request"})
                elif method == "thread/archive":
                    assert request["params"] == {"threadId": OWNED}
                    if self.scenario == "archive_error":
                        ws.send(json.dumps({"id": req_id, "error": {"code": -32001, "message": "private archive error"}}))
                    else:
                        self.send(ws, req_id, {})
                else:
                    raise AssertionError(f"unexpected method {method}")
        except ConnectionClosed:
            pass
        except Exception as error:
            self.errors.append(type(error).__name__)

    @staticmethod
    def send(ws, req_id, result):
        ws.send(json.dumps({"id": req_id, "result": result}))


class VerifierSocketTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="taskai-verifier-test-")
        self.root = Path(self.temp.name)
        self.tui = self.root / "fake-codex"
        self.tui.write_text(
            "#!/usr/bin/env python3\n"
            "# ABOUTME: Acts as an inert Codex TUI for the offline verifier test.\n"
            "# ABOUTME: Reports only whether inherited messaging variable names are present.\n"
            "import os, time\n"
            "from pathlib import Path\n"
            "Path(os.environ['TASKAI_TEST_ENV_MARKER']).write_text(str(any(\n"
            "    name.startswith('CLAUDE_CODE_MESSAGING_') for name in os.environ)))\n"
            "print('Ask Codex to do anything', flush=True)\n"
            "time.sleep(2)\n"
        )
        self.tui.chmod(0o700)

    def tearDown(self):
        self.temp.cleanup()

    def options(self, daemon, *, existing=False):
        return Options(
            socket_path=daemon.path,
            codex_bin=str(self.tui),
            work_root=str(self.root),
            existing_thread_id=OWNED if existing else None,
            unarchive_existing=existing,
            request_timeout=0.2,
            observe_seconds=0.15,
        )

    def test_successful_zero_turn_read_and_one_tui_resume(self):
        with FakeDaemon(self.root) as daemon:
            marker = self.root / "tui-env-marker"
            with patch.dict(os.environ, {"CLAUDE_CODE_MESSAGING_TEST": "must-not-pass", "TASKAI_TEST_ENV_MARKER": str(marker)}):
                report = run_check(self.options(daemon))
        self.assertEqual(report["check"], "completed")
        self.assertEqual(report["turns_before"], 0)
        self.assertEqual(report["turns_after"], 0)
        self.assertEqual(report["tui_resume_attempts"], 1)
        self.assertTrue(report["tui_child_env_filtered"])
        self.assertEqual(marker.read_text(), "False")
        self.assertEqual(report["thread_archive"], "accepted")
        self.assertFalse(report["record_retained"])
        self.assertEqual(daemon.methods.count("thread/start"), 1)
        self.assertEqual(daemon.methods.count("thread/resume"), 1)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)
        self.assertNotIn("turn/start", daemon.methods)

    def test_rpc_error_reports_numeric_code_without_server_message(self):
        with FakeDaemon(self.root, "rpc_error") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["failure"], {"method": "thread/read", "reason": "rpc_error", "rpc_code": -32602, "owned_id_match": True})
        self.assertEqual(report["thread_archive"], "accepted")
        self.assertEqual(report["tui_resume_attempts"], 0)
        self.assertNotIn("private error text", json.dumps(report))

    def test_missing_turns_has_fixed_structural_reason(self):
        with FakeDaemon(self.root, "missing_turns") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["failure"]["reason"], "missing_turns")
        self.assertEqual(report["failure"]["owned_id_match"], True)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)

    def test_malformed_reply_has_fixed_reason_and_archives(self):
        with FakeDaemon(self.root, "malformed_reply") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["failure"]["reason"], "malformed_reply")
        self.assertNotIn("never-log", json.dumps(report))
        self.assertEqual(daemon.methods.count("thread/archive"), 1)

    def test_timeout_has_fixed_reason_and_does_not_retry(self):
        with FakeDaemon(self.root, "timeout") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["failure"]["reason"], "timeout")
        self.assertEqual(daemon.methods.count("thread/read"), 1)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)

    def test_unrelated_events_leave_no_content_or_other_id(self):
        with FakeDaemon(self.root, "unrelated_event") as daemon:
            report = run_check(self.options(daemon))
        encoded = json.dumps(report)
        self.assertEqual(report["turn_started_events"], 0)
        self.assertNotIn("other-secret-id", encoded)
        self.assertNotIn("private event text", encoded)

    def test_uncertain_creation_ack_is_not_retried_or_archived_without_id(self):
        with FakeDaemon(self.root, "uncertain_create") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["failure"]["reason"], "creation_uncertain")
        self.assertEqual(daemon.methods.count("thread/start"), 1)
        self.assertEqual(daemon.methods.count("thread/archive"), 0)
        self.assertEqual(report["tui_resume_attempts"], 0)
        self.assertTrue(report["record_retained"])
        self.assertTrue((Path(report["record_dir"]) / "creation-intent").exists())
        shutil.rmtree(report["record_dir"])

    def test_existing_owned_id_uses_unarchive_without_another_thread(self):
        with FakeDaemon(self.root) as daemon:
            report = run_check(self.options(daemon, existing=True))
        self.assertEqual(report["check"], "completed")
        self.assertEqual(daemon.methods.count("thread/start"), 0)
        self.assertEqual(daemon.methods.count("thread/unarchive"), 1)
        self.assertEqual(daemon.methods.count("thread/resume"), 1)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)
        self.assertEqual(report["tui_resume_attempts"], 1)

    def test_uncertain_unarchive_still_attempts_exact_id_archive(self):
        with FakeDaemon(self.root, "unarchive_error") as daemon:
            report = run_check(self.options(daemon, existing=True))
        self.assertEqual(report["failure"], {"method": "thread/unarchive", "reason": "rpc_error", "rpc_code": -32602, "owned_id_match": True})
        self.assertEqual(daemon.methods.count("thread/start"), 0)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)
        self.assertEqual(report["thread_archive"], "accepted")

    def test_cli_without_explicit_live_flag_never_contacts_socket(self):
        with FakeDaemon(self.root) as daemon:
            process = subprocess.run(
                [sys.executable, str(Path(__file__).resolve().parents[1] / "tools" / "codex_zero_turn_verifier.py"),
                 "--socket", daemon.path, "--codex-bin", str(self.tui), "--create-new"],
                capture_output=True, text=True, timeout=3,
            )
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual(daemon.methods, [])

    def test_missing_original_workspace_is_ambiguous_not_gate_evidence(self):
        self.tui.write_text(
            "#!/bin/sh\n"
            "# ABOUTME: Simulates a resume that cannot locate its saved workspace.\n"
            "# ABOUTME: Emits no model prompt or provider side effect.\n"
            "printf 'Working directory does not exist\\n'\n"
        )
        with FakeDaemon(self.root) as daemon:
            report = run_check(self.options(daemon, existing=True))
        self.assertEqual(report["tui_result"], "ambiguous")
        self.assertTrue(report["tui_screen_flags"]["workspace_error"])
        self.assertEqual(report["turns_after"], 0)
        self.assertEqual(daemon.methods.count("thread/archive"), 1)

    def test_failed_archive_retains_owner_record_and_numeric_code(self):
        with FakeDaemon(self.root, "archive_error") as daemon:
            report = run_check(self.options(daemon))
        self.assertEqual(report["check"], "cleanup_incomplete")
        self.assertEqual(report["archive_failure"], {"method": "thread/archive", "reason": "rpc_error", "rpc_code": -32001, "owned_id_match": True})
        self.assertTrue(report["record_retained"])
        self.assertTrue((Path(report["record_dir"]) / "owned-thread-id").exists())
        self.assertNotIn("private archive error", json.dumps(report))
        shutil.rmtree(report["record_dir"])


if __name__ == "__main__":
    unittest.main()
