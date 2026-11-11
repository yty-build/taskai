# Archived first Codex zero-turn verifier

This is the Python body submitted in the first T-approved live slot, preserved for review. The shell command first ran `unset -m 'CLAUDE_CODE_MESSAGING_*'`, then executed this body with `python3 - <<'PY'`. It created one thread, stopped at `thread/read`, archived that exact ID, and removed its private temporary workspace. Do not run this archived version; use `tools/codex_zero_turn_verifier.py` after a separately approved slot.

The failed request was `thread/read` with `{"threadId": owned_id, "includeTurns": True}`. `request()` raised a `RuntimeError` labelled `thread/read_rpc_<numeric code>`. The final exception handler accepted only `[A-Za-z0-9_-]`, so the slash caused the whole label to become `redacted`, losing the numeric code.

```python
import json, os, pty, re, select, shutil, signal, struct, subprocess, tempfile, termios, time, fcntl
from websockets.sync.client import unix_connect

SOCKET = os.path.expanduser('~/.codex/app-server-control/app-server-control.sock')
CODEX = '/opt/homebrew/bin/codex'
WORK = tempfile.mkdtemp(prefix='taskai-codex-zero-turn-')
os.chmod(WORK, 0o700)
result = {'workspace': 'private_temp', 'thread_created': False, 'prompt_submissions': 0, 'turn_started_events': 0, 'tui_resume_attempts': 0, 'cleanup': 'pending'}
owned_id = None
primary = None
observer = None
child = None
master_fd = None
turn_seen = False
next_id = 1
stage = 'connect'

def event(packet):
    global turn_seen
    if not owned_id or not isinstance(packet, dict):
        return
    params = packet.get('params')
    if not isinstance(params, dict) or params.get('threadId') != owned_id:
        return
    if packet.get('method') == 'turn/started':
        turn_seen = True
        result['turn_started_events'] += 1

def request(conn, method, params, timeout=12):
    global next_id
    req_id = next_id
    next_id += 1
    conn.send(json.dumps({'id': req_id, 'method': method, 'params': params}))
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError(method + '_timeout')
        packet = json.loads(conn.recv(timeout=remaining))
        if not isinstance(packet, dict):
            continue
        if packet.get('id') != req_id:
            event(packet)
            continue
        if 'error' in packet:
            error = packet['error'] if isinstance(packet['error'], dict) else {}
            code = error.get('code')
            raise RuntimeError(method + '_rpc_' + str(code)[:30])
        return packet.get('result') or {}

def connect_and_initialize():
    conn = unix_connect(SOCKET, uri='ws://localhost/rpc', additional_headers={'User-Agent':'taskai-bridge/0.1.0'}, open_timeout=8)
    init = request(conn, 'initialize', {'clientInfo': {'name':'taskai-bridge','title':'TaskAI bridge','version':'0.1.0'}, 'capabilities': {'experimentalApi': True}})
    conn.send(json.dumps({'method':'initialized'}))
    agent = init.get('userAgent', '')
    match = re.match(r'^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', agent) if isinstance(agent, str) else None
    return conn, match.group(0) if match else 'unrecognized'

def thread_turn_count(conn):
    reply = request(conn, 'thread/read', {'threadId': owned_id, 'includeTurns': True})
    thread = reply.get('thread') if isinstance(reply, dict) else None
    if not isinstance(thread, dict) or thread.get('id') != owned_id:
        raise RuntimeError('thread_read_id_mismatch')
    turns = thread.get('turns')
    if not isinstance(turns, list):
        raise RuntimeError('thread_read_turns_missing')
    return len(turns)

def classify_screen(raw):
    plain = raw.decode('utf-8', 'ignore')
    plain = re.sub(r'\x1b\][^\x07]*(?:\x07|\x1b\\)', ' ', plain)
    plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', ' ', plain)
    low = plain.lower()
    return {
        'input_hint': any(x in low for x in ('ask codex', 'type a message', 'enter your prompt', 'send a message')),
        'trust_prompt': any(x in low for x in ('trust this directory', 'trust this workspace', 'do you trust')),
        'login_prompt': any(x in low for x in ('sign in', 'log in', 'login required')),
        'empty_thread_error': any(x in low for x in ('no turns', 'no messages', 'empty thread', 'empty session')),
        'missing_thread_error': any(x in low for x in ('session not found', 'thread not found', 'conversation not found')),
    }

try:
    primary, result['daemon_user_agent'] = connect_and_initialize()
    result['initialize'] = 'accepted'
    stage = 'thread_start'
    tools = [{'type':'namespace','name':'taskai','description':'TaskAI cross-session messaging: find other sessions and message them.','tools':[
        {'type':'function','name':'list_sessions','description':'List live sessions you can message: name, harness, status and waiting reason.','inputSchema':{'type':'object','properties':{},'additionalProperties':False}},
        {'type':'function','name':'send_message','description':'Send a message to a live session by name; optionally receive one idle notice.','inputSchema':{'type':'object','properties':{'to':{'type':'string','description':'Recipient session name.'},'text':{'type':'string','description':'Message text.'},'notify_when_idle':{'type':'boolean','description':'Receive one idle notice.'}},'required':['to','text'],'additionalProperties':False}}
    ]}]
    started = request(primary, 'thread/start', {'cwd':WORK, 'dynamicTools':tools})
    thread = started.get('thread') if isinstance(started, dict) else None
    owned_id = thread.get('id') if isinstance(thread, dict) else None
    if not isinstance(owned_id, str) or not owned_id:
        raise RuntimeError('thread_start_id_missing')
    fd = os.open(os.path.join(WORK, 'owned-thread-id'), os.O_CREAT|os.O_EXCL|os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as record:
        record.write(owned_id + '\n')
    result['thread_created'] = True
    result['owned_thread_id'] = owned_id
    policy = started.get('approvalPolicy')
    result['effective_policy_at_start'] = policy if policy in ('on-request','never') else 'unknown'
    stage = 'read_before'
    result['turns_before'] = thread_turn_count(primary)
    if result['turns_before'] != 0 or turn_seen:
        raise RuntimeError('nonzero_turns_before_resume')
    stage = 'observer_resume'
    observer, observer_agent = connect_and_initialize()
    result['observer_initialize'] = 'accepted'
    resumed = request(observer, 'thread/resume', {'threadId':owned_id, 'excludeTurns':True})
    resumed_thread = resumed.get('thread') if isinstance(resumed, dict) else None
    if not isinstance(resumed_thread, dict) or resumed_thread.get('id') != owned_id:
        raise RuntimeError('thread_resume_id_mismatch')
    result['thread_resume'] = 'accepted_exact_id'
    status = resumed_thread.get('status')
    status_type = status.get('type') if isinstance(status, dict) else None
    result['resume_status'] = status_type if status_type in ('idle','active','notLoaded','systemError') else 'unknown'
    policy = resumed.get('approvalPolicy')
    result['effective_policy_at_resume'] = policy if policy in ('on-request','never') else 'unknown'
    if turn_seen:
        raise RuntimeError('unexpected_turn_before_tui')
    stage = 'tui_resume'
    master_fd, slave_fd = pty.openpty()
    fcntl.ioctl(slave_fd, termios.TIOCSWINSZ, struct.pack('HHHH', 32, 120, 0, 0))
    child_env = {k:v for k,v in os.environ.items() if not k.startswith('CLAUDE_CODE_MESSAGING_')}
    child_env.setdefault('TERM', 'xterm-256color')
    result['tui_child_env_filtered'] = not any(k.startswith('CLAUDE_CODE_MESSAGING_') for k in child_env)
    if not result['tui_child_env_filtered']:
        raise RuntimeError('tui_env_filter_failed')
    child = subprocess.Popen([CODEX, 'resume', owned_id], cwd=WORK, env=child_env, stdin=slave_fd, stdout=slave_fd, stderr=slave_fd, close_fds=True, start_new_session=True)
    os.close(slave_fd)
    result['tui_resume_attempts'] = 1
    screen = bytearray()
    deadline = time.monotonic() + 8.0
    while time.monotonic() < deadline and not turn_seen:
        readable, _, _ = select.select([master_fd], [], [], 0.1)
        if readable:
            try:
                chunk = os.read(master_fd, 8192)
            except OSError:
                chunk = b''
            if chunk:
                screen.extend(chunk)
                if len(screen) > 32768:
                    del screen[:-32768]
        try:
            packet = json.loads(observer.recv(timeout=0.05))
            event(packet)
        except TimeoutError:
            pass
        if child.poll() is not None:
            break
    result['tui_natural_exit'] = child.poll() is not None
    result['tui_natural_exit_code'] = child.poll() if child.poll() is not None else None
    result['tui_screen_flags'] = classify_screen(bytes(screen))
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=3)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=3)
        result['tui_terminated_by_test'] = True
    else:
        result['tui_terminated_by_test'] = False
    stage = 'read_after'
    result['turns_after'] = thread_turn_count(primary)
    if result['turns_after'] != 0 or turn_seen:
        raise RuntimeError('unexpected_model_turn')
    result['zero_turn_confirmed'] = True
    result['check'] = 'completed'
except Exception as exc:
    result['check'] = 'stopped'
    result['stopped_at'] = stage
    result['error_class'] = type(exc).__name__
    label = str(exc)
    result['error_label'] = label if re.fullmatch(r'[A-Za-z0-9_-]{1,100}', label) else 'redacted'
finally:
    if child is not None and child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=3)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=3)
    if master_fd is not None:
        os.close(master_fd)
    if owned_id and primary is not None:
        try:
            archived = request(primary, 'thread/archive', {'threadId':owned_id}, timeout=12)
            result['thread_archive'] = 'accepted' if isinstance(archived, dict) else 'unexpected_reply'
        except Exception as exc:
            result['thread_archive'] = 'failed_' + type(exc).__name__
    if observer is not None:
        try: observer.close()
        except Exception: pass
    if primary is not None:
        try: primary.close()
        except Exception: pass
    if not owned_id or result.get('thread_archive') == 'accepted':
        shutil.rmtree(WORK)
        result['cleanup'] = 'test_thread_archived_temp_removed' if owned_id else 'temp_removed_no_thread'
    else:
        result['cleanup'] = 'thread_not_confirmed_archived_temp_record_retained'
        result['retained_record_dir'] = WORK
    print(json.dumps(result, sort_keys=True))
```
