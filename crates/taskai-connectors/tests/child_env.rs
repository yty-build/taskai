// ABOUTME: Connector child processes inherit no Claude messaging variables: the version probes, the
// ABOUTME: OpenCode server and stdio JSON-RPC children, each run as a fake that reports by name.

use std::{
    ffi::OsStr,
    os::unix::{ffi::OsStrExt, fs::PermissionsExt},
    path::{Path, PathBuf},
    process::Command,
    time::{Duration, Instant},
};

use taskai_connectors::{
    claude::ClaudeConnector,
    codex::CodexConnector,
    grok::{GrokConnector, JoinConfig, LeaderClient},
    opencode::OpenCodeConnector,
    rpc::StdioRpc,
};
use taskai_core::mail::{inherited_messaging_env_names, strip_messaging_env};

/// Shell that prints whether any Claude messaging variable reached it, judged by name only.
const MESSAGING_VERDICT: &str = r#"LC_ALL=C awk 'BEGIN { for (name in ENVIRON) if (name ~ /^CLAUDE_CODE_MESSAGING_/) found = 1; print (found ? "messaging=true" : "messaging=false") }'"#;

/// True in the copy of this test binary that `copy_with_messaging_env` runs.
fn is_copy() -> bool {
    std::env::var_os("TASKAI_CHILD_ENV_COPY").is_some()
}

/// Runs `test` in a copy of this test binary that inherits fake Claude messaging variables, one
/// named in UTF-8 and one not, and returns what the copy printed once it confirmed the inheritance.
fn copy_with_messaging_env(test: &str) -> String {
    let mut copy = Command::new(std::env::current_exe().unwrap());
    strip_messaging_env(&mut copy);
    let output = copy
        .args(["--exact", test, "--nocapture"])
        .env("TASKAI_CHILD_ENV_COPY", "1")
        .env("CLAUDE_CODE_MESSAGING_SITE_PROBE", "fake")
        .env(OsStr::from_bytes(b"CLAUDE_CODE_MESSAGING_\xffRAW"), "fake")
        .output()
        .unwrap();
    assert!(output.status.success(), "the copy passes");
    let printed = String::from_utf8(output.stdout).unwrap();
    assert!(
        reported(&printed, "inherited=true"),
        "the copy inherits the fake variable"
    );
    printed
}

/// What a copy prints first: whether it really inherited a messaging variable.
fn report_inheritance() {
    println!("inherited={}", !inherited_messaging_env_names().is_empty());
}

fn reported(printed: &str, verdict: &str) -> bool {
    printed.lines().any(|line| line == verdict)
}

/// An executable shell script at `dir/name` running `body`.
fn script(dir: &Path, name: &str, body: &str) -> PathBuf {
    let path = dir.join(name);
    std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    path
}

// Expectation: each harness's version probe runs its CLI without any CLAUDE_CODE_MESSAGING_*
// variable TaskAI inherited. Why: TaskAI is often started from a Claude session, and a provider
// CLI holding that session's socket and token could speak as it. Value: probing a harness never
// hands it the person's Claude identity. Each probe runs a fake CLI that prints a version and its
// verdict.
#[test]
fn version_probes_inherit_no_claude_messaging_variables() {
    if !is_copy() {
        let printed =
            copy_with_messaging_env("version_probes_inherit_no_claude_messaging_variables");
        for harness in ["claude", "codex", "grok", "opencode"] {
            let clean = format!("{harness}: 9.9.9 messaging=false");
            assert!(reported(&printed, &clean), "{harness}");
        }
        return;
    }
    report_inheritance();
    let dir = tempfile::tempdir().unwrap();
    let cli = script(
        dir.path(),
        "cli",
        &format!("printf '9.9.9 '; {MESSAGING_VERDICT}"),
    );
    let bin = cli.to_string_lossy().into_owned();
    let claude = ClaudeConnector { bin: bin.clone() }.probe();
    println!("claude: {}", claude.detail);
    let codex = CodexConnector { bin: bin.clone() }.probe();
    println!("codex: {}", codex.detail);
    let grok = GrokConnector { bin: bin.clone() }.probe();
    println!("grok: {}", grok.detail);
    let opencode = OpenCodeConnector { bin }.probe();
    println!("opencode: {}", opencode.detail);
}

// Expectation: the OpenCode server and a stdio JSON-RPC child (the transport of the codex
// app-server and grok ACP clients) start without any CLAUDE_CODE_MESSAGING_* variable TaskAI
// inherited. Why: both are long-lived provider processes that run tools and further children.
// Value: no provider process TaskAI serves through can speak as the person's Claude session. The
// fake server only records its verdict and never answers, so the copy exits without waiting for
// it.
#[test]
fn server_and_rpc_children_inherit_no_claude_messaging_variables() {
    if !is_copy() {
        let printed = copy_with_messaging_env(
            "server_and_rpc_children_inherit_no_claude_messaging_variables",
        );
        assert!(reported(&printed, "opencode serve: messaging=false"));
        assert!(reported(&printed, "stdio rpc: messaging=false"));
        return;
    }
    report_inheritance();
    let dir = tempfile::tempdir().unwrap();
    let verdict = dir.path().join("serve-verdict");
    let server = script(
        dir.path(),
        "opencode",
        &format!("{MESSAGING_VERDICT} > '{}'", verdict.display()),
    );
    let bin = server.to_string_lossy().into_owned();
    let workspace = dir.path().to_path_buf();
    std::thread::spawn(move || OpenCodeConnector { bin }.start_server(&workspace));
    let deadline = Instant::now() + Duration::from_secs(5);
    while !std::fs::read_to_string(&verdict).is_ok_and(|text| text.ends_with('\n')) {
        assert!(Instant::now() < deadline, "the fake server ran");
        std::thread::sleep(Duration::from_millis(20));
    }
    let served = std::fs::read_to_string(&verdict).unwrap();
    println!("opencode serve: {}", served.trim());
    let notice = String::from(r#"printf '{"method":"verdict","params":{"verdict":"%s"}}\n' "$("#)
        + MESSAGING_VERDICT
        + ")\"";
    let child = script(dir.path(), "rpc", &notice);
    let rpc = StdioRpc::spawn(child.to_str().unwrap(), &[]).unwrap();
    let note = rpc
        .notifications()
        .recv_timeout(Duration::from_secs(5))
        .unwrap();
    println!("stdio rpc: {}", note["params"]["verdict"].as_str().unwrap());
}

// Expectation: the Grok leader client starts its Grok process without any CLAUDE_CODE_MESSAGING_*
// variable, inherited or among its extra variables, whatever bytes the name holds, and keeps its
// other extra variables; a name that is not UTF-8 does not stop the launch. Why: the joining
// client is a Grok process whose MCP children inherit its environment, and TaskAI's environment
// can hold names that are not UTF-8. Value: joining a Grok leader never hands it the person's
// Claude identity. The fake leader only records its verdict and exits, so the join itself fails.
#[test]
fn the_grok_leader_client_inherits_no_claude_messaging_variables() {
    if !is_copy() {
        let printed = copy_with_messaging_env(
            "the_grok_leader_client_inherits_no_claude_messaging_variables",
        );
        assert!(reported(&printed, "grok leader: messaging=false kept=true"));
        return;
    }
    report_inheritance();
    let dir = tempfile::tempdir().unwrap();
    let verdict = dir.path().join("leader-verdict");
    let leader = script(
        dir.path(),
        "grok",
        &format!(
            r#"LC_ALL=C awk 'BEGIN {{ for (name in ENVIRON) {{ if (name ~ /^CLAUDE_CODE_MESSAGING_/) found = 1; if (name == "TASKAI_KEEP_EXPLICIT") kept = 1 }} print "messaging=" (found ? "true" : "false") " kept=" (kept ? "true" : "false") }}' > '{}'"#,
            verdict.display()
        ),
    );
    std::fs::write(dir.path().join("leader.log"), "").unwrap();
    let joined = LeaderClient::join(JoinConfig {
        bin: leader.to_string_lossy().into_owned(),
        args: Vec::new(),
        cwd: dir.path().to_path_buf(),
        leader_socket: dir.path().join("leader.sock"),
        leader_log: dir.path().join("leader.log"),
        session_id: "session-1".into(),
        timeout: Duration::from_secs(1),
        extra_env: vec![
            (
                "CLAUDE_CODE_MESSAGING_EXPLICIT".into(),
                "set-on-the-client".into(),
            ),
            ("TASKAI_KEEP_EXPLICIT".into(), "kept".into()),
        ],
    });
    assert!(joined.is_err(), "the fake leader never answers");
    let deadline = Instant::now() + Duration::from_secs(5);
    while !std::fs::read_to_string(&verdict).is_ok_and(|text| text.ends_with('\n')) {
        assert!(Instant::now() < deadline, "the fake leader ran");
        std::thread::sleep(Duration::from_millis(20));
    }
    let reported = std::fs::read_to_string(&verdict).unwrap();
    println!("grok leader: {}", reported.trim());
}
