# ABOUTME: Bounded Codex daemon and empty-thread TUI verifier for an explicitly approved live slot.
# ABOUTME: Emits fixed, sanitized evidence and archives only its exact owned thread.

"""Run only with an approved live slot. Requires websockets 15.x.

The original failed verifier sent thread/read with exactly
{"threadId": owned_id, "includeTurns": true}. Its error path made a label such as
"thread/read_rpc_-32602", then rejected the slash with an alphanumeric allowlist and
reported only "redacted". This verifier preserves the method and numeric RPC code.
"""

import argparse
import json
import os
import pty
import re
import select
import shutil
import struct
import subprocess
import tempfile
import termios
import time
import fcntl
from dataclasses import dataclass
from pathlib import Path

from websockets.exceptions import ConnectionClosed
from websockets.sync.client import unix_connect


USER_AGENT = "taskai-bridge/0.1.0"
PRODUCT_VERSION = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")
KNOWN_POLICIES = {"on-request", "never"}
KNOWN_STATUSES = {"idle", "active", "notLoaded", "systemError"}


@dataclass(frozen=True)
class Options:
    socket_path: str
    codex_bin: str
    work_root: str | None = None
    existing_thread_id: str | None = None
    unarchive_existing: bool = False
    request_timeout: float = 12.0
    observe_seconds: float = 8.0


@dataclass(frozen=True)
class SafeFailure(Exception):
    method: str
    reason: str
    rpc_code: int | None = None
    owned_id_match: bool | None = None

    def evidence(self):
        return {
            "method": self.method,
            "reason": self.reason,
            "rpc_code": self.rpc_code,
            "owned_id_match": self.owned_id_match,
        }


def filtered_child_env():
    """Filter names only; never inspect or log matching values."""
    return {name: value for name, value in os.environ.items() if not name.startswith("CLAUDE_CODE_MESSAGING_")}


class RpcWire:
    def __init__(self, connection, timeout):
        self.connection = connection
        self.timeout = timeout
        self.next_id = 1
        self.owned_id = None
        self.turn_started_events = 0

    @classmethod
    def connect(cls, path, timeout):
        try:
            connection = unix_connect(
                path,
                uri="ws://localhost/rpc",
                additional_headers={"User-Agent": USER_AGENT},
                open_timeout=timeout,
            )
        except Exception:
            raise SafeFailure("connect", "transport_error") from None
        return cls(connection, timeout)

    def close(self):
        try:
            self.connection.close()
        except Exception:
            pass

    def _observe(self, packet):
        if not self.owned_id or not isinstance(packet, dict):
            return
        params = packet.get("params")
        if not isinstance(params, dict) or params.get("threadId") != self.owned_id:
            return
        if packet.get("method") == "turn/started":
            self.turn_started_events += 1

    def poll_event(self, timeout):
        try:
            packet = json.loads(self.connection.recv(timeout=timeout))
        except TimeoutError:
            return
        except ConnectionClosed:
            raise SafeFailure("event", "transport_closed", owned_id_match=True) from None
        except (ValueError, TypeError):
            raise SafeFailure("event", "malformed_json", owned_id_match=True) from None
        self._observe(packet)

    def request(self, method, params):
        req_id = self.next_id
        self.next_id += 1
        matched = self.owned_id is not None and params.get("threadId") == self.owned_id
        try:
            self.connection.send(json.dumps({"id": req_id, "method": method, "params": params}))
        except Exception:
            raise SafeFailure(method, "transport_closed", owned_id_match=matched or None) from None
        deadline = time.monotonic() + self.timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise SafeFailure(method, "timeout", owned_id_match=matched or None)
            try:
                packet = json.loads(self.connection.recv(timeout=remaining))
            except TimeoutError:
                raise SafeFailure(method, "timeout", owned_id_match=matched or None) from None
            except ConnectionClosed:
                raise SafeFailure(method, "transport_closed", owned_id_match=matched or None) from None
            except (ValueError, TypeError):
                raise SafeFailure(method, "malformed_json", owned_id_match=matched or None) from None
            if not isinstance(packet, dict):
                raise SafeFailure(method, "malformed_reply", owned_id_match=matched or None)
            if packet.get("id") != req_id:
                self._observe(packet)
                continue
            if "error" in packet:
                error = packet["error"]
                code = error.get("code") if isinstance(error, dict) else None
                code = code if isinstance(code, int) and not isinstance(code, bool) else None
                raise SafeFailure(method, "rpc_error", rpc_code=code, owned_id_match=matched or None)
            if "result" not in packet or not isinstance(packet["result"], dict):
                raise SafeFailure(method, "malformed_reply", owned_id_match=matched or None)
            return packet["result"]

    def initialize(self):
        result = self.request(
            "initialize",
            {"clientInfo": {"name": "taskai-bridge", "title": "TaskAI bridge", "version": "0.1.0"},
             "capabilities": {"experimentalApi": True}},
        )
        agent = result.get("userAgent")
        if not isinstance(agent, str):
            raise SafeFailure("initialize", "missing_user_agent")
        match = PRODUCT_VERSION.match(agent)
        try:
            self.connection.send(json.dumps({"method": "initialized"}))
        except Exception:
            raise SafeFailure("initialized", "transport_closed") from None
        return match.group(0) if match else "unrecognized"


def dynamic_tools():
    return [{
        "type": "namespace", "name": "taskai",
        "description": "TaskAI cross-session messaging: find other sessions and message them.",
        "tools": [
            {"type": "function", "name": "list_sessions",
             "description": "List live sessions you can message: name, harness, status and waiting reason.",
             "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False}},
            {"type": "function", "name": "send_message",
             "description": "Send a message to a live session by name; optionally receive one idle notice.",
             "inputSchema": {"type": "object", "properties": {
                 "to": {"type": "string", "description": "Recipient session name."},
                 "text": {"type": "string", "description": "Message text."},
                 "notify_when_idle": {"type": "boolean", "description": "Receive one idle notice."}},
                 "required": ["to", "text"], "additionalProperties": False}},
        ],
    }]


def _same_thread(result, method, owned_id):
    thread = result.get("thread")
    if not isinstance(thread, dict):
        raise SafeFailure(method, "missing_thread", owned_id_match=None)
    if thread.get("id") != owned_id:
        raise SafeFailure(method, "owned_id_mismatch", owned_id_match=False)
    return thread


def _read_zero(wire, owned_id):
    # Exact shape sent by the first live verifier; official schema accepts both fields.
    result = wire.request("thread/read", {"threadId": owned_id, "includeTurns": True})
    thread = _same_thread(result, "thread/read", owned_id)
    turns = thread.get("turns")
    if not isinstance(turns, list):
        raise SafeFailure("thread/read", "missing_turns", owned_id_match=True)
    if turns:
        raise SafeFailure("thread/read", "nonzero_turns", owned_id_match=True)
    return 0


def _write_private(path, content):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as record:
        record.write(content)


def _screen_flags(raw):
    text = raw.decode("utf-8", "ignore")
    text = re.sub(r"\x1b\][^\x07]*(?:\x07|\x1b\\)", " ", text)
    text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", " ", text)
    lower = text.lower()
    return {
        "input_hint": any(token in lower for token in ("ask codex", "type a message", "enter your prompt", "send a message")),
        "trust_prompt": any(token in lower for token in ("trust this directory", "trust this workspace", "do you trust")),
        "login_prompt": any(token in lower for token in ("sign in", "log in", "login required")),
        "empty_thread_error": any(token in lower for token in ("no turns", "no messages", "empty thread", "empty session")),
        "missing_thread_error": any(token in lower for token in ("session not found", "thread not found", "conversation not found")),
        "workspace_error": any(token in lower for token in ("working directory does not exist", "workspace not found", "failed to canonicalize")),
    }


def _run_tui(options, owned_id, workspace, observer, report):
    master = None
    slave = None
    child = None
    screen = bytearray()
    try:
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 120, 0, 0))
        env = filtered_child_env()
        env.setdefault("TERM", "xterm-256color")
        report["tui_child_env_filtered"] = not any(name.startswith("CLAUDE_CODE_MESSAGING_") for name in env)
        if not report["tui_child_env_filtered"]:
            raise SafeFailure("tui/resume", "env_filter_failed", owned_id_match=True)
        report["tui_resume_attempts"] += 1
        try:
            child = subprocess.Popen(
                [options.codex_bin, "resume", owned_id],
                cwd=workspace, env=env,
                stdin=slave, stdout=slave, stderr=slave,
                close_fds=True, start_new_session=True,
            )
        except Exception:
            raise SafeFailure("tui/resume", "child_spawn_failed", owned_id_match=True) from None
        os.close(slave)
        slave = None
        deadline = time.monotonic() + options.observe_seconds
        while time.monotonic() < deadline and observer.turn_started_events == 0:
            readable, _, _ = select.select([master], [], [], 0.05)
            if readable:
                try:
                    chunk = os.read(master, 8192)
                except OSError:
                    chunk = b""
                if chunk:
                    screen.extend(chunk)
                    if len(screen) > 32768:
                        del screen[:-32768]
            observer.poll_event(0.05)
            if child.poll() is not None:
                break
        exited = child.poll() is not None
        report["tui_natural_exit"] = exited
        report["tui_natural_exit_code"] = child.poll() if exited else None
        flags = _screen_flags(bytes(screen))
        report["tui_screen_flags"] = flags
        if exited and flags["empty_thread_error"]:
            report["tui_result"] = "empty_thread_rejected"
        elif not exited and flags["input_hint"] and not flags["trust_prompt"] and not flags["login_prompt"]:
            report["tui_result"] = "opened"
        else:
            report["tui_result"] = "ambiguous"
    finally:
        if child is not None and child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=3)
            report["tui_terminated_by_test"] = True
        elif child is not None:
            report["tui_terminated_by_test"] = False
        if slave is not None:
            os.close(slave)
        if master is not None:
            os.close(master)


def run_check(options: Options):
    """Run once against the supplied socket. Tests supply only private fake sockets."""
    report = {
        "check": "stopped", "prompt_submissions": 0, "turn_start_calls": 0,
        "tui_resume_attempts": 0, "thread_archive": "not_needed", "record_retained": False,
        "turn_started_events": 0,
    }
    workspace = None
    try:
        workspace = Path(tempfile.mkdtemp(prefix="taskai-codex-zero-turn-", dir=options.work_root))
        workspace.chmod(0o700)
    except Exception:
        if workspace is not None:
            try:
                shutil.rmtree(workspace)
            except Exception:
                pass
        report["failure"] = SafeFailure("workspace", "create_failed").evidence()
        return report
    primary = None
    observer = None
    owned_id = None
    start_sent = False
    archive_needed = False
    stage = "connect"
    try:
        primary = RpcWire.connect(options.socket_path, options.request_timeout)
        report["daemon_user_agent"] = primary.initialize()
        report["initialize"] = "accepted"
        if options.existing_thread_id:
            stage = "thread/unarchive"
            if not options.unarchive_existing:
                raise SafeFailure("thread/unarchive", "explicit_scope_required", owned_id_match=True)
            owned_id = options.existing_thread_id
            primary.owned_id = owned_id
            report["owned_thread_id"] = owned_id
            _write_private(workspace / "owned-thread-id", owned_id + "\n")
            # The request may take effect even if its reply is lost. Re-archive this
            # exact ID on every uncertain unarchive outcome.
            archive_needed = True
            result = primary.request("thread/unarchive", {"threadId": owned_id})
            _same_thread(result, "thread/unarchive", owned_id)
            report["thread_unarchive"] = "accepted_exact_id"
        else:
            stage = "thread/start"
            _write_private(workspace / "creation-intent", "one TaskAI test-owned thread; no automatic retry\n")
            start_sent = True
            started = primary.request("thread/start", {"cwd": str(workspace), "dynamicTools": dynamic_tools()})
            thread = started.get("thread")
            if not isinstance(thread, dict) or not isinstance(thread.get("id"), str) or not thread["id"]:
                raise SafeFailure("thread/start", "missing_thread_id")
            owned_id = thread["id"]
            primary.owned_id = owned_id
            report["owned_thread_id"] = owned_id
            archive_needed = True
            _write_private(workspace / "owned-thread-id", owned_id + "\n")
            report["thread_start"] = "accepted"
            policy = started.get("approvalPolicy")
            report["effective_policy_at_start"] = policy if policy in KNOWN_POLICIES else "unknown"
        stage = "thread/read"
        report["turns_before"] = _read_zero(primary, owned_id)
        if primary.turn_started_events:
            raise SafeFailure("thread/read", "unexpected_turn_event", owned_id_match=True)
        stage = "thread/resume"
        observer = RpcWire.connect(options.socket_path, options.request_timeout)
        observer.owned_id = owned_id
        observer.initialize()
        resumed = observer.request("thread/resume", {"threadId": owned_id, "excludeTurns": True})
        thread = _same_thread(resumed, "thread/resume", owned_id)
        report["thread_resume"] = "accepted_exact_id"
        policy = resumed.get("approvalPolicy")
        report["effective_policy_at_resume"] = policy if policy in KNOWN_POLICIES else "unknown"
        status = thread.get("status")
        status_type = status.get("type") if isinstance(status, dict) else None
        report["resume_status"] = status_type if status_type in KNOWN_STATUSES else "unknown"
        if primary.turn_started_events or observer.turn_started_events:
            raise SafeFailure("thread/resume", "unexpected_turn_event", owned_id_match=True)
        stage = "tui/resume"
        _run_tui(options, owned_id, workspace, observer, report)
        stage = "thread/read-after"
        report["turns_after"] = _read_zero(primary, owned_id)
        if primary.turn_started_events or observer.turn_started_events:
            raise SafeFailure("thread/read", "unexpected_turn_event", owned_id_match=True)
        report["zero_turn_confirmed"] = True
        report["check"] = "completed"
    except SafeFailure as failure:
        if stage == "thread/start" and start_sent and owned_id is None:
            report["failure"] = SafeFailure("thread/start", "creation_uncertain").evidence()
        else:
            report["failure"] = failure.evidence()
    except Exception:
        report["failure"] = SafeFailure(stage, "unexpected_exception", owned_id_match=owned_id is not None).evidence()
    finally:
        if archive_needed and owned_id and primary is not None:
            try:
                primary.request("thread/archive", {"threadId": owned_id})
                report["thread_archive"] = "accepted"
            except SafeFailure as failure:
                report["thread_archive"] = "failed"
                report["archive_failure"] = failure.evidence()
                if report["check"] == "completed":
                    report["check"] = "cleanup_incomplete"
        elif start_sent and owned_id is None:
            report["thread_archive"] = "not_attempted_unknown_id"
        if observer is not None:
            report["turn_started_events"] += observer.turn_started_events
            observer.close()
        if primary is not None:
            report["turn_started_events"] += primary.turn_started_events
            primary.close()
        retain = (start_sent and owned_id is None) or (archive_needed and report["thread_archive"] != "accepted")
        if retain:
            report["record_retained"] = True
            report["record_dir"] = str(workspace)
            report["cleanup"] = "record_retained"
        else:
            try:
                shutil.rmtree(workspace)
                report["cleanup"] = "temporary_workspace_removed"
            except Exception:
                report["record_retained"] = True
                report["record_dir"] = str(workspace)
                report["cleanup"] = "temporary_workspace_cleanup_failed"
                if report["check"] == "completed":
                    report["check"] = "cleanup_incomplete"
    return report


def main():
    parser = argparse.ArgumentParser(description="Bounded Codex zero-turn verifier; live approval required")
    parser.add_argument("--live", action="store_true", help="acknowledge that T and the lead approved this exact run")
    parser.add_argument("--socket", required=True)
    parser.add_argument("--codex-bin", required=True)
    parser.add_argument("--work-root")
    choice = parser.add_mutually_exclusive_group(required=True)
    choice.add_argument("--create-new", action="store_true")
    choice.add_argument("--existing-thread-id")
    parser.add_argument("--unarchive-existing", action="store_true")
    parser.add_argument("--request-timeout", type=float, default=12.0)
    parser.add_argument("--observe-seconds", type=float, default=8.0)
    args = parser.parse_args()
    if not args.live:
        parser.error("no live contact without --live and explicit T approval plus lead slot")
    if args.unarchive_existing and not args.existing_thread_id:
        parser.error("--unarchive-existing requires --existing-thread-id")
    if args.existing_thread_id and not args.unarchive_existing:
        parser.error("reusing an archived ID requires explicit --unarchive-existing scope")
    if args.request_timeout <= 0 or args.observe_seconds <= 0:
        parser.error("timeouts must be positive")
    options = Options(
        socket_path=args.socket, codex_bin=args.codex_bin, work_root=args.work_root,
        existing_thread_id=args.existing_thread_id,
        unarchive_existing=args.unarchive_existing,
        request_timeout=args.request_timeout, observe_seconds=args.observe_seconds,
    )
    report = run_check(options)
    print(json.dumps(report, sort_keys=True))
    return 0 if report["check"] == "completed" and report["thread_archive"] == "accepted" else 2


if __name__ == "__main__":
    raise SystemExit(main())
