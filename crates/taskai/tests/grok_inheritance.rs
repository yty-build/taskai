// ABOUTME: Private MCP process proofs for Grok's per-pane environment inheritance.
// ABOUTME: The installed-provider check is ignored by default and requires a live-test slot.

use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;

use taskai_connectors::grok::install_mcp;
use taskai_core::mail::strip_messaging_env;

// The wrapper records only comparisons against this probe's own values, then execs the actual
// taskai MCP binary. It never records other environment variables or any protocol contents.
const MCP_CHILD: &str = r#"#!/usr/bin/env python3
import json, os, sys
record = {
    'socket_matches': os.environ.get('TASKAI_TOOL_SOCKET') == os.environ['TASKAI_PROBE_EXPECT_SOCKET'],
    'session_matches': os.environ.get('GROK_SESSION_ID') == os.environ['TASKAI_PROBE_EXPECT_SESSION'],
    'messaging_env_absent': not any(k.startswith('CLAUDE_CODE_MESSAGING_') for k in os.environ),
    'mcp_argv': sys.argv[1:] == ['mcp'],
    'process_group': os.getpgrp(),
}
fd = os.open(os.environ['TASKAI_PROBE_MARKER'], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as out:
    json.dump(record, out)
binary = os.environ['TASKAI_PROBE_BINARY']
os.execve(binary, [binary] + sys.argv[1:], os.environ)
"#;

// One bounded subprocess group, one named MCP server, no native session and no model call.
// Provider output stays in memory for fixed diagnostic flags, then is discarded without recording.
const SUPERVISOR: &str = r#"
import json, os, signal, subprocess, sys, time
grok, root, binary, server_name = sys.argv[1:]
marker = os.path.join(root, 'child-observed.json')
socket = os.path.join(root, 'owned.tools.sock')
session = 'taskai-environment-probe'
env = {k:v for k,v in os.environ.items() if not k.startswith('CLAUDE_CODE_MESSAGING_')}
env.update(TASKAI_HOME=os.path.join(root, 'taskai-home'), TASKAI_TOOL_SOCKET=socket,
           GROK_SESSION_ID=session, TASKAI_PROBE_EXPECT_SOCKET=socket,
           TASKAI_PROBE_EXPECT_SESSION=session, TASKAI_PROBE_MARKER=marker,
           TASKAI_PROBE_BINARY=binary)
args = [grok, '--cwd', root, '--leader-socket', os.path.join(root, 'owned.leader.sock'),
        'mcp', 'doctor', server_name, '--json']
child = subprocess.Popen(args, cwd=root, env=env, stdin=subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         start_new_session=True)
status, timed_out = None, False
stdout, stderr = b'', b''
try:
    stdout, stderr = child.communicate(timeout=20)
    status = child.returncode
except subprocess.TimeoutExpired:
    timed_out = True
finally:
    try:
        os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        child.wait(timeout=1)
    except subprocess.TimeoutExpired:
        os.killpg(child.pid, signal.SIGKILL)
        child.wait(timeout=1)
    # The doctor may have exited before its MCP child. Stop any remaining member of our group.
    try:
        os.killpg(child.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    if timed_out:
        try:
            stdout, stderr = child.communicate(timeout=1)
        except subprocess.TimeoutExpired:
            stdout, stderr = b'', b''
try:
    with open(marker) as source:
        observed = json.load(source)
except (OSError, ValueError):
    observed = {}
report = {key: observed.get(key) is True for key in
          ('socket_matches', 'session_matches', 'messaging_env_absent', 'mcp_argv')}
group = observed.get('process_group')
group_clean = False
if isinstance(group, int) and group > 0:
    # Native Grok may give MCP its own group. Do not kill that group by a stale numeric ID;
    # require it to disappear after doctor closes its stdio. A survivor is explicit failure.
    deadline = time.monotonic() + 1
    while time.monotonic() < deadline:
        try:
            os.killpg(group, 0)
        except ProcessLookupError:
            group_clean = True
            break
        except PermissionError:
            break
        time.sleep(.02)
report.update(doctor_exit=status, timed_out=timed_out, marker_seen=bool(observed),
              child_shared_doctor_group=group == child.pid, mcp_group_clean=group_clean)
if observed and not group_clean:
    report['mcp_group_needing_cleanup'] = group
diagnostic = (stdout[:65536] + stderr[:65536]).decode('utf-8', errors='replace').lower()
for name, needles in {
    'server_missing': ('not found', 'not configured', 'no mcp server', 'unknown mcp server'),
    'config_mentioned': ('parse', 'toml', 'invalid type', 'config error'),
    'workspace_trust_required': (
        'repo-local (project-scoped) server not started for an untrusted folder',
        're-run with --trust to allow repo-local servers',
    ),
    'argument_problem': ('unexpected argument', 'unknown option', 'unrecognized'),
    'spawn_problem': ('failed to spawn', 'permission denied', 'no such file or directory'),
    'login_required': ('log in', 'sign in', 'authentication required'),
}.items():
    report[name] = any(needle in diagnostic for needle in needles)
print(json.dumps(report))
"#;

fn verify_inheritance(grok: &Path) -> Result<(), String> {
    let workspace = tempfile::Builder::new()
        .prefix("grok-env-")
        .tempdir_in("/tmp")
        .map_err(|e| e.to_string())?;
    let shim = workspace.path().join("taskai-probe");
    std::fs::write(&shim, MCP_CHILD).map_err(|e| e.to_string())?;
    std::fs::set_permissions(&shim, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())?;
    install_mcp(workspace.path(), &shim).map_err(|e| e.to_string())?;
    let config = std::fs::read_to_string(workspace.path().join(".grok/config.toml"))
        .map_err(|e| e.to_string())?;
    if config.contains("TASKAI_TOOL_SOCKET") {
        return Err("a per-agent socket leaked into the shared MCP configuration".into());
    }
    // A unique alias ensures a broken workspace lookup cannot select somebody's global server.
    // Only the probe changes the name; the production installer still writes `taskai`.
    let server_name = format!(
        "taskai_probe_{}",
        workspace
            .path()
            .file_name()
            .unwrap()
            .to_string_lossy()
            .replace('-', "_")
    );
    let config = config
        .replace("mcp_servers.taskai", &format!("mcp_servers.{server_name}"))
        .replace("MCPTool(taskai__*)", &format!("MCPTool({server_name}__*)"));
    std::fs::write(workspace.path().join(".grok/config.toml"), config)
        .map_err(|e| e.to_string())?;
    let mut supervisor = Command::new("python3");
    supervisor
        .args(["-c", SUPERVISOR])
        .arg(grok)
        .arg(workspace.path())
        .arg(env!("CARGO_BIN_EXE_taskai"))
        .arg(server_name)
        .env("CLAUDE_CODE_MESSAGING_PROBE", "synthetic-test-value");
    strip_messaging_env(&mut supervisor);
    let output = supervisor.output().map_err(|e| e.to_string())?;
    if !output.status.success() {
        return Err(format!(
            "probe supervisor exited {:?}",
            output.status.code()
        ));
    }
    let evidence: serde_json::Value = serde_json::from_slice(&output.stdout)
        .map_err(|_| "probe supervisor returned no sanitized evidence")?;
    let passed = evidence["doctor_exit"] == 0
        && evidence["timed_out"] == false
        && [
            "socket_matches",
            "session_matches",
            "messaging_env_absent",
            "mcp_argv",
            "marker_seen",
            "mcp_group_clean",
        ]
        .iter()
        .all(|key| evidence[*key] == true);
    if passed {
        println!("Grok MCP inheritance evidence: {evidence}");
        Ok(())
    } else {
        Err(format!("Grok MCP inheritance not confirmed: {evidence}"))
    }
}

fn fake_grok(strip_socket: bool) -> tempfile::TempDir {
    let fake = tempfile::Builder::new()
        .prefix("fake-grok-env-")
        .tempdir_in("/tmp")
        .unwrap();
    let script = format!(
        r#"#!/usr/bin/env python3
import json, os, subprocess, sys
root = sys.argv[sys.argv.index('--cwd') + 1]
assert sys.argv[-4:-2] == ['mcp', 'doctor'] and sys.argv[-1] == '--json'
assert sys.argv[-2].startswith('taskai_probe_')
with open(os.path.join(root, '.grok/config.toml')) as source:
    command = next(line.split('=',1)[1].strip() for line in source if line.startswith('command ='))
env = dict(os.environ)
if {strip_socket}:
    env.pop('TASKAI_TOOL_SOCKET', None)
child = subprocess.Popen([json.loads(command), 'mcp'], env=env, stdin=subprocess.PIPE,
                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
for request in [{{'jsonrpc':'2.0','id':1,'method':'initialize','params':{{'protocolVersion':'2024-11-05'}}}},
                {{'jsonrpc':'2.0','id':2,'method':'tools/list'}}]:
    child.stdin.write(json.dumps(request) + '\n')
    child.stdin.flush()
    response = json.loads(child.stdout.readline())
    assert response['id'] == request['id']
    assert 'result' in response
    if request['method'] == 'tools/list':
        assert [t['name'] for t in response['result']['tools']] == ['list_sessions','send_message']
child.stdin.close()
assert child.wait(timeout=3) == 0
"#,
        strip_socket = if strip_socket { "True" } else { "False" }
    );
    let bin = fake.path().join("grok");
    std::fs::write(&bin, script).unwrap();
    std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o700)).unwrap();
    fake
}

// Expectation: the probe reaches the actual taskai MCP through the installed workspace entry.
// Why: checking env on a directly spawned MCP process does not test the Grok child boundary.
// Value: the offline driver proves that the live probe can distinguish inherited and missing env.
#[test]
fn inheritance_probe_accepts_a_child_with_the_pane_environment() {
    let fake = fake_grok(false);
    verify_inheritance(&fake.path().join("grok")).unwrap();
}

// Grok 1.0.77 puts MCP children in their own process group, which it closes on doctor exit.
#[test]
fn inheritance_probe_accepts_a_cleaned_separate_mcp_process_group() {
    let fake = fake_grok(false);
    let path = fake.path().join("grok");
    let script = std::fs::read_to_string(&path).unwrap().replace(
        "stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)",
        "stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, start_new_session=True)",
    );
    std::fs::write(path, script).unwrap();
    verify_inheritance(&fake.path().join("grok")).unwrap();
}

#[test]
fn inheritance_probe_rejects_a_child_missing_the_socket_environment() {
    let fake = fake_grok(true);
    let error = verify_inheritance(&fake.path().join("grok")).unwrap_err();
    assert!(error.contains("\"socket_matches\":false"), "{error}");
    assert!(error.contains("\"doctor_exit\":0"), "{error}");
    assert!(error.contains("\"session_matches\":true"), "{error}");
}

#[test]
fn inheritance_probe_classifies_errors_without_recording_their_contents() {
    let fake = fake_grok(false);
    std::fs::write(
        fake.path().join("grok"),
        concat!(
            "#!/usr/bin/env python3\nimport sys\n",
            "print('MCP server not found: PRIVATE-DIAGNOSTIC', file=sys.stderr)\n",
            "raise SystemExit(1)\n",
        ),
    )
    .unwrap();
    let error = verify_inheritance(&fake.path().join("grok")).unwrap_err();
    assert!(error.contains("\"server_missing\":true"), "{error}");
    assert!(!error.contains("PRIVATE-DIAGNOSTIC"), "{error}");
}

#[test]
fn inheritance_probe_distinguishes_native_folder_trust_from_config_syntax() {
    let fake = fake_grok(false);
    for (diagnostic, trust) in [
        (
            "repo-local (project-scoped) server not started for an untrusted folder",
            true,
        ),
        ("config.toml parse error: unknown key trust", false),
    ] {
        std::fs::write(
            fake.path().join("grok"),
            format!("#!/usr/bin/env python3\nimport sys\nprint({diagnostic:?}, file=sys.stderr)\nraise SystemExit(1)\n"),
        ).unwrap();
        let error = verify_inheritance(&fake.path().join("grok")).unwrap_err();
        assert!(
            error.contains(&format!("\"workspace_trust_required\":{trust}")),
            "{error}"
        );
    }
}

// T approved this zero-turn inheritance check; the lead must still assign its exclusive slot.
#[test]
#[ignore = "live: requires the lead's exclusive Grok MCP environment-check slot; zero model turns"]
fn installed_grok_inherits_the_pane_environment_into_mcp() {
    verify_inheritance(Path::new("grok")).unwrap();
}
