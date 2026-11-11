// ABOUTME: CLI tests — the real taskai binary answering context self/session/tree with
// ABOUTME: process-bound identity, honest errors, and a hook inject that never fails.

use std::path::Path;
use std::process::Command;

use taskai_core::registry::{Registry, Source};

fn taskai() -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_taskai"));
    c.env_remove("TASKAI_SESSION_REF").env_remove("TASKAI_HOME");
    c
}

fn seed(home: &Path) -> (String, String, String) {
    let mut reg = Registry::open(&home.join("state")).unwrap();
    let t = reg
        .create_task(
            "Repository evaluation",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let lead = reg
        .create_session(
            &t,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let arch = reg
        .create_session(
            &t,
            Some(&lead),
            "Architecture",
            "opencode",
            Path::new("/tmp/wt"),
            Source::HumanUi,
        )
        .unwrap();
    (t, lead, arch)
}

// Expectation: `context self` refuses when no registered binding exists — identity is
// process-bound, never assumed. Why: backend §5 binds self to the registered process.
// Value: an unmanaged shell can't impersonate a session.
#[test]
fn context_self_requires_binding() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "self"])
        .output()
        .unwrap();
    assert!(!out.status.success());
    let err = String::from_utf8_lossy(&out.stderr);
    assert!(err.contains("TASKAI_SESSION_REF"), "{err}");
}

// Expectation: `context self` inside a bound session explains identity; json form
// carries the full snapshot. Why: the session's shell inherits the binding from its
// pane. Value: self-awareness works from any subprocess of the session.
#[test]
fn context_self_and_session_answer() {
    let tmp = tempfile::tempdir().unwrap();
    let (_t, lead, arch) = seed(tmp.path());

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .env("TASKAI_SESSION_REF", &arch)
        .args(["context", "self"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.contains("You are Subordinate Architecture"), "{text}");
    assert!(text.contains(&lead), "{text}");

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "session", &lead, "--format", "json"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["self"]["session_ref"], lead.as_str());
    assert_eq!(v["self"]["role"], "Manager");
}

// Expectation: `context tree` prints the full chain with `└─` subordinate connectors;
// unknown session refs error cleanly. Why: the tree is the whole relevant team chain.
// Value: agents and humans read the same truth.
#[test]
fn context_tree_and_unknown_ref() {
    let tmp = tempfile::tempdir().unwrap();
    seed(tmp.path());

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "tree"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.contains("Repository evaluation"), "{text}");
    assert!(text.contains("└─"), "{text}");
    assert!(text.contains("Architecture"), "{text}");

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "session", "s999"])
        .output()
        .unwrap();
    assert!(!out.status.success());
}

// Expectation: the inject hook never breaks the hosting CLI — no binding means silent
// success with empty output; a binding emits the additionalContext envelope.
// Why: a failing hook would disturb the native CLI (backend §5: report stale, never
// break). Value: context delivery is safe by construction.
#[test]
fn inject_never_fails() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "inject"])
        .output()
        .unwrap();
    assert!(out.status.success());
    assert!(out.stdout.is_empty());

    let (_t, lead, _arch) = seed(tmp.path());
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .env("TASKAI_SESSION_REF", &lead)
        .args(["context", "inject"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["hookSpecificOutput"]["hookEventName"], "UserPromptSubmit");
    assert!(v["hookSpecificOutput"]["additionalContext"]
        .as_str()
        .unwrap()
        .contains("You are Manager Lead"));
}

fn fixture_cli(path: &Path, body: &str) {
    use std::os::unix::fs::PermissionsExt;
    std::fs::write(path, body).unwrap();
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)).unwrap();
}

// Expectation: `taskai doctor` judges the three messaging adapters from fixtures alone — a
// tested, signed-in Claude; a Codex whose daemon is absent; a Grok with a credentials file it
// never opens — as stable text and JSON, exits 1 while any gate fails, checks one workspace's
// Grok trust on request, and never echoes the native auth output it saw. Why: build-model M7 —
// one truthful place that says why messaging does not work. Value: M9 gates on `doctor: green`.
#[test]
fn doctor_reports_each_adapter_from_private_fixtures() {
    use std::os::unix::fs::PermissionsExt;
    let tmp = tempfile::Builder::new()
        .prefix("d7")
        .tempdir_in("/tmp")
        .unwrap();
    let bin = tmp.path().join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    fixture_cli(
        &bin.join("claude"),
        "#!/bin/sh\ncase \"$1\" in\n--version) echo '2.1.322 (Claude Code)';;\nauth) echo '{\"loggedIn\":true,\"email\":\"FIXTURE-SECRET\"}';;\n*) exit 9;;\nesac\n",
    );
    fixture_cli(
        &bin.join("codex"),
        "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.167.0';;\nlogin) echo 'Logged in using ChatGPT as FIXTURE-SECRET';;\n*) exit 9;;\nesac\n",
    );
    fixture_cli(
        &bin.join("grok"),
        "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\necho 'grok 1.0.77 (abc) [stable]'\n",
    );
    let home = tmp.path().join("home");
    std::fs::create_dir_all(home.join(".grok")).unwrap();
    std::fs::write(home.join(".grok/auth.json"), "FIXTURE-SECRET").unwrap();
    std::fs::set_permissions(
        home.join(".grok/auth.json"),
        std::fs::Permissions::from_mode(0o000),
    )
    .unwrap();
    let workspace = tmp.path().join("ws");
    std::fs::create_dir_all(&workspace).unwrap();
    let path = format!(
        "{}:{}",
        bin.display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let doctor = |args: &[&str]| {
        taskai()
            .env("TASKAI_HOME", tmp.path().join("taskai"))
            .env("HOME", &home)
            .env("PATH", &path)
            .env_remove("TASKAI_CLAUDE_BIN")
            .env_remove("CODEX_HOME")
            .env_remove("GROK_HOME")
            .arg("doctor")
            .args(args)
            .output()
            .unwrap()
    };

    let out = doctor(&["--json", "--workspace", &workspace.display().to_string()]);
    assert_eq!(
        out.status.code(),
        Some(1),
        "{}",
        String::from_utf8_lossy(&out.stderr)
    );
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(!text.contains("SECRET"), "{text}");
    let report: serde_json::Value = serde_json::from_str(&text).unwrap();
    assert_eq!(report["green"], false);
    let rows = report["harnesses"].as_array().unwrap();
    assert_eq!(rows.len(), 3);
    assert_eq!(rows[0]["hkey"], "claude");
    assert_eq!(rows[0]["version"], "2.1.322");
    assert_eq!(rows[0]["sign_in"], "signed in");
    assert_eq!(rows[0]["messaging"]["status"], "ready");
    assert_eq!(rows[1]["hkey"], "codex");
    assert_eq!(rows[1]["sign_in"], "signed in");
    assert_eq!(rows[1]["messaging"]["status"], "not ready");
    assert!(rows[1]["messaging"]["detail"]
        .as_str()
        .unwrap()
        .contains("socket is absent"));
    assert_eq!(rows[2]["hkey"], "grok");
    assert_eq!(rows[2]["sign_in"], "credentials present");
    assert_eq!(rows[2]["messaging"]["status"], "ready");
    assert_eq!(report["workspace"]["checks"][0]["hkey"], "grok");
    assert_eq!(
        report["workspace"]["checks"][0]["detail"],
        "workspace not trusted by Grok"
    );

    std::fs::write(
        home.join(".grok/trusted_folders.toml"),
        format!(
            "[folders.\"{}\"]\ntrusted = true\ndecided_at = 1\n",
            workspace.canonicalize().unwrap().display()
        ),
    )
    .unwrap();
    let out = doctor(&["--workspace", &workspace.display().to_string()]);
    assert_eq!(out.status.code(), Some(1));
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.starts_with("TaskAI doctor\n"), "{text}");
    for line in [
        "Claude Code · claude\n",
        "  installed   2.1.322\n",
        "  sign-in     CLI reports signed in\n",
        "  messaging   Ready · Claude Code 2.1.322",
        "  messaging   Not ready · Codex daemon socket is absent at ",
        "  sign-in     Saved credentials present · not verified\n",
        &format!(
            "  grok        Ready · Grok trusts {}\n",
            workspace.display()
        ),
        "doctor: red · 1 not ready\n",
    ] {
        assert!(text.contains(line), "missing {line:?} in\n{text}");
    }
    assert!(!text.contains("SECRET"), "{text}");

    let out = doctor(&["--bogus"]);
    assert!(!out.status.success());
    assert!(String::from_utf8_lossy(&out.stderr).contains("usage: taskai doctor"));

    // Green needs every row launchable, signed in and gated: a private daemon answering as the
    // tested version turns the verdict, the exit status and the JSON together.
    let control = home.join(".codex").join("app-server-control");
    std::fs::create_dir_all(&control).unwrap();
    let daemon = fake_codex_daemon(&control.join("app-server-control.sock"));
    let out = doctor(&["--workspace", &workspace.display().to_string()]);
    daemon.join().unwrap();
    let text = String::from_utf8_lossy(&out.stdout);
    assert_eq!(out.status.code(), Some(0), "{text}");
    assert!(text.ends_with("doctor: green\n"), "{text}");
    assert!(text.contains("checked here: the handshake only"), "{text}");
    let daemon = fake_codex_daemon(&control.join("app-server-control.sock"));
    let out = doctor(&["--json"]);
    daemon.join().unwrap();
    let report: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(out.status.code(), Some(0));
    assert_eq!(report["green"], true);

    // A relative GROK_HOME is an explicit refusal, not a look into ~/.grok.
    let out = taskai()
        .env("TASKAI_HOME", tmp.path().join("taskai"))
        .env("HOME", &home)
        .env("PATH", &path)
        .env("GROK_HOME", "relative-grok-home")
        .env_remove("CODEX_HOME")
        .args(["doctor", "--json"])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(1));
    let report: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(report["harnesses"][2]["messaging"]["status"], "unverified");
    assert!(report["harnesses"][2]["messaging"]["detail"]
        .as_str()
        .unwrap()
        .contains("GROK_HOME"));
    assert_eq!(report["harnesses"][2]["sign_in"], "unknown");
}

fn private_tmux(server: &str, args: &[&str]) -> std::process::Output {
    let mut command = Command::new("tmux");
    command
        .env_remove("TMUX")
        .env_remove("TMUX_PANE")
        .env_remove("TMUX_TMPDIR")
        .args(["-f", "/dev/null", "-L", server])
        .args(args);
    taskai_core::mail::strip_messaging_env(&mut command);
    command.output().unwrap()
}

fn private_tmux_text(server: &str, args: &[&str]) -> String {
    let output = private_tmux(server, args);
    assert!(
        output.status.success(),
        "private tmux {args:?} failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn eventually(mut ready: impl FnMut() -> bool) -> bool {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    while std::time::Instant::now() < deadline {
        if ready() {
            return true;
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
    }
    false
}

/// Ends whatever runtime the registry under this state folder recorded for `s1`, at whichever
/// step the test stops, and removes that private server's socket once the server is gone, so
/// nothing of the test stays running or in the shared tmux socket folder.
struct LaunchedRuntime(std::path::PathBuf);

impl Drop for LaunchedRuntime {
    fn drop(&mut self) {
        let Some(server) = Registry::open(&self.0).ok().and_then(|registry| {
            registry
                .session("s1")?
                .runtime
                .as_ref()
                .map(|runtime| runtime.private_server.clone())
        }) else {
            return;
        };
        let _ = private_tmux(&server, &["kill-session", "-t", "=s1"]);
        if eventually(|| !private_tmux(&server, &["list-sessions"]).status.success()) {
            // SAFETY: getuid has no arguments and cannot fail.
            let uid = unsafe { libc::getuid() };
            let _ = std::fs::remove_file(format!("/tmp/tmux-{uid}/{server}"));
        }
    }
}

// Expectation: before a Grok pane starts, the folder its leader socket goes in exists and only
// its owner can enter it. Why: Grok binds the socket where `--leader-socket` says and does not
// create the folder; without it Grok runs with no leader and the session can never be messaged.
// Value: a Grok agent's first launch has a leader for its bridge to join.
#[test]
fn a_grok_launch_finds_the_folder_for_its_leader_socket() {
    use std::os::unix::fs::PermissionsExt;
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    let tmp = tempfile::Builder::new()
        .prefix("gl")
        .tempdir_in("/tmp")
        .unwrap();
    let home = tmp.path().join("taskai");
    let workspace = tmp.path().join("ws");
    let grok_home = tmp.path().join("grok-home");
    std::fs::create_dir_all(&workspace).unwrap();
    std::fs::create_dir_all(&grok_home).unwrap();
    std::fs::write(grok_home.join("auth.json"), "").unwrap();
    std::fs::write(
        grok_home.join("trusted_folders.toml"),
        format!("[folders.\"{}\"]\ntrusted = true\n", workspace.display()),
    )
    .unwrap();
    let seen = tmp.path().join("leader-folder-seen");
    // Like Grok, the fixture does not create the folder; it records whether it was there.
    let provider = tmp.path().join("fake-grok");
    fixture_cli(
        &provider,
        &format!(
            "#!/bin/sh\n[ \"$1\" = --version ] && {{ echo 'grok 1.0.77'; exit 0; }}\nwhile [ $# -gt 0 ]; do\n  [ \"$1\" = --leader-socket ] && socket=$2\n  shift\ndone\nfolder=$(dirname \"$socket\")\nif [ -d \"$folder\" ]; then stat -f '%Lp' \"$folder\" > '{seen}'; else echo missing > '{seen}'; fi\nexec sleep 30\n",
            seen = seen.display()
        ),
    );
    {
        let mut registry = Registry::open(&home.join("state")).unwrap();
        registry
            .set_server_harness(
                "local",
                "grok",
                taskai_core::registry::HarnessSettings {
                    enabled: true,
                    executable: Some(provider.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task = registry
            .create_task("Launch", &workspace, Source::HumanUi)
            .unwrap();
        registry
            .create_session(&task, None, "Lead", "grok", &workspace, Source::HumanUi)
            .unwrap();
    }
    let _guard = LaunchedRuntime(home.join("state"));
    let mut command = taskai();
    command
        .env("TASKAI_HOME", &home)
        .env("HOME", tmp.path().join("user-home"))
        .env("GROK_HOME", &grok_home)
        .env("CLAUDE_CONFIG_DIR", tmp.path().join("claude-home"))
        .env("CODEX_HOME", tmp.path().join("codex-home"))
        .env("TASKAI_CLAUDE_BIN", tmp.path().join("absent-claude"))
        .args(["session", "launch", "s1"]);
    taskai_core::mail::strip_messaging_env(&mut command);
    let launched = command.output().unwrap();
    assert!(
        launched.status.success(),
        "{}",
        String::from_utf8_lossy(&launched.stderr)
    );
    assert!(eventually(|| seen.exists()), "the provider never started");
    assert_eq!(
        std::fs::read_to_string(&seen).unwrap().trim(),
        "700",
        "the leader socket's folder must exist, owner-only, before Grok starts"
    );
    assert_eq!(
        std::fs::metadata(home.join("run/grok"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o700
    );
}

// Expectation: `taskai session launch <ref>` starts that session's durable runtime and its
// messaging sidecar with no viewer left attached and not one byte typed into the native pane;
// run again it restarts nothing, leaves the window size alone and only puts back a missing
// sidecar; it refuses from inside a managed session. Why: a script that drives the UI types into
// whichever CLI holds focus, and a launch named by session ref has no keyboard path at all.
// Value: the release proof, and any script, starts exact sessions without risking a model turn.
#[test]
fn session_launch_starts_and_reconciles_a_runtime_without_typing() {
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    let tmp = tempfile::Builder::new()
        .prefix("sl")
        .tempdir_in("/tmp")
        .unwrap();
    let home = tmp.path().join("taskai");
    let workspace = tmp.path().join("ws");
    std::fs::create_dir_all(&workspace).unwrap();
    let typed = tmp.path().join("typed-into-native-pane");
    let provider = tmp.path().join("fake-claude");
    fixture_cli(
        &provider,
        &format!(
            "#!/bin/sh\ncase \"$1\" in\n--version) echo '0.0.0 (fixture)'; exit 0;;\nauth) echo '{{}}'; exit 0;;\nesac\nstty raw -echo\nexec cat > '{}'\n",
            typed.display()
        ),
    );
    let session_ref = {
        let mut registry = Registry::open(&home.join("state")).unwrap();
        registry
            .set_server_harness(
                "local",
                "claude",
                taskai_core::registry::HarnessSettings {
                    enabled: true,
                    executable: Some(provider.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task = registry
            .create_task("Launch", &workspace, Source::HumanUi)
            .unwrap();
        registry
            .create_session(&task, None, "Lead", "claude", &workspace, Source::HumanUi)
            .unwrap()
    };
    assert_eq!(session_ref, "s1");
    let _guard = LaunchedRuntime(home.join("state"));
    let launch = |session: &str, inside: Option<&str>| {
        let mut command = taskai();
        command
            .env("TASKAI_HOME", &home)
            .env("HOME", tmp.path().join("user-home"))
            .env("CLAUDE_CONFIG_DIR", tmp.path().join("claude-home"))
            .env("CODEX_HOME", tmp.path().join("codex-home"))
            .env("GROK_HOME", tmp.path().join("grok-home"))
            .args(["session", "launch", session]);
        if let Some(inside) = inside {
            command.env("TASKAI_SESSION_REF", inside);
        }
        taskai_core::mail::strip_messaging_env(&mut command);
        command.output().unwrap()
    };

    let refused = launch("s1", Some("s1"));
    assert!(!refused.status.success());
    assert!(
        String::from_utf8_lossy(&refused.stderr).contains("inside a TaskAI session"),
        "{}",
        String::from_utf8_lossy(&refused.stderr)
    );
    assert!(
        Registry::open(&home.join("state"))
            .unwrap()
            .session("s1")
            .unwrap()
            .runtime
            .is_none(),
        "a refused launch binds nothing"
    );
    assert!(!launch("s999", None).status.success());

    let first = launch("s1", None);
    assert!(
        first.status.success(),
        "{}",
        String::from_utf8_lossy(&first.stderr)
    );
    let runtime = Registry::open(&home.join("state"))
        .unwrap()
        .session("s1")
        .unwrap()
        .runtime
        .clone()
        .expect("the launch saves its tmux mapping");
    let server = runtime.private_server.as_str();
    let report: serde_json::Value = serde_json::from_slice(&first.stdout).unwrap();
    assert_eq!(report["session"], "s1");
    assert_eq!(report["created"], true);
    assert!(eventually(|| typed.exists()), "the provider never started");
    let windows = |server: &str| {
        private_tmux_text(
            server,
            &["list-windows", "-t", "=s1", "-F", "#{window_name}"],
        )
    };
    assert_eq!(windows(server), "native\nobserver");
    assert_eq!(
        private_tmux_text(server, &["list-clients", "-t", "=s1"]),
        "",
        "no viewer stays attached"
    );
    let pane_pid = |server: &str, window: &str| {
        private_tmux_text(
            server,
            &[
                "list-panes",
                "-t",
                &format!("=s1:{window}"),
                "-F",
                "#{pane_pid}",
            ],
        )
    };
    let native = pane_pid(server, "native");
    let observer = pane_pid(server, "observer");

    // A person's viewer may have sized the window; a second launch must leave that alone.
    private_tmux_text(
        server,
        &["resize-window", "-t", "=s1:0", "-x", "100", "-y", "30"],
    );
    private_tmux_text(server, &["kill-window", "-t", "=s1:1"]);
    assert_eq!(windows(server), "native");
    let second = launch("s1", None);
    assert!(
        second.status.success(),
        "{}",
        String::from_utf8_lossy(&second.stderr)
    );
    let report: serde_json::Value = serde_json::from_slice(&second.stdout).unwrap();
    assert_eq!(report["created"], false);
    assert_eq!(windows(server), "native\nobserver");
    assert_eq!(pane_pid(server, "native"), native, "the provider restarted");
    assert_ne!(pane_pid(server, "observer"), observer);
    assert_eq!(
        private_tmux_text(
            server,
            &[
                "list-windows",
                "-t",
                "=s1:0",
                "-F",
                "#{window_width}x#{window_height}"
            ]
        )
        .lines()
        .next(),
        Some("100x30")
    );
    assert_eq!(
        std::fs::read(&typed).unwrap(),
        b"",
        "a launch must not type into the native pane"
    );
    // The record above is only evidence if a real keystroke would have shown in it.
    private_tmux_text(server, &["send-keys", "-t", "=s1:0", "-l", "k"]);
    assert!(
        eventually(|| std::fs::read(&typed).unwrap() == b"k"),
        "the fixture does not record what its pane receives"
    );
}

/// A private daemon that answers one initialize as the tested version, then hangs up.
fn fake_codex_daemon(socket: &Path) -> std::thread::JoinHandle<()> {
    let _ = std::fs::remove_file(socket);
    let listener = std::os::unix::net::UnixListener::bind(socket).unwrap();
    std::thread::spawn(move || {
        let (stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(std::time::Duration::from_secs(5)))
            .unwrap();
        let mut ws = tungstenite::accept(stream).unwrap();
        let init: serde_json::Value =
            serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(init["method"], "initialize");
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"id": init["id"], "result": {"userAgent": "codex-tui/0.167.1 (fixture)"}})
                .to_string(),
        ))
        .unwrap();
        let _ = ws.read();
    })
}
