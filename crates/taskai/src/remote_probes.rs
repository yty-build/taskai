// ABOUTME: Nonblocking, per-host provider readiness cache for remote task forms.
// ABOUTME: Harmless version probes run in disposable threads, never on the input loop.

use std::{
    collections::HashMap,
    sync::mpsc::{self, Receiver, Sender},
    time::{Duration, Instant},
};

use taskai_connectors::{
    claude::{parse_version, ClaudeConnector},
    codex::CodexConnector,
    grok::GrokConnector,
    opencode::OpenCodeConnector,
    version_ok,
};
use taskai_core::registry::{ConnectionTarget, ServerProfile};

use crate::tmux;

const DISABLED_REASON: &str = "Disabled in this server profile";
const REMOTE_OPENCODE_REASON: &str = "Remote OpenCode is not supported yet: its shared-live serve/attach launcher currently requires local TaskAI. Local OpenCode remains available.";
const OPENCODE_MODEL_REASON: &str =
    "OpenCode attach does not support a model launch override; use native OpenCode settings";

/// Native-reported saved login only. This does not validate a token against the
/// service or establish model access, quota, entitlement, or billing readiness.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum AuthState {
    SignedIn,
    SignedOut,
    #[default]
    Unknown,
}

/// Every configured provider stays visible, including unavailable providers.
/// A discovered version and the reason it cannot be launched are separate facts.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HarnessAvailability {
    pub hkey: String,
    pub version: Option<String>,
    pub unavailable: Option<String>,
    pub auth: AuthState,
}

impl HarnessAvailability {
    pub fn is_ready(&self) -> bool {
        self.unavailable.is_none()
    }

    pub fn authentication_status(&self) -> &'static str {
        match self.auth {
            AuthState::SignedIn => "CLI reports signed in",
            AuthState::SignedOut => "Sign in needed",
            AuthState::Unknown => "Couldn't verify",
        }
    }

    pub fn status_label(&self) -> &'static str {
        if let Some(reason) = &self.unavailable {
            if reason == DISABLED_REASON {
                return "Disabled";
            }
            if reason == OPENCODE_MODEL_REASON || reason.ends_with(REMOTE_OPENCODE_REASON) {
                return "Unsupported";
            }
        }
        let minimum = match self.hkey.as_str() {
            "claude" => Some("2.0.0"),
            "codex" => Some("0.100.0"),
            "opencode" => Some("1.18.0"),
            "grok" => Some("1.0.0"),
            _ => None,
        };
        let Some((minimum, version)) = minimum.zip(self.version.as_deref()) else {
            return "Couldn't verify";
        };
        if !version_ok(version, minimum) {
            return "Update needed";
        }
        if self.unavailable.is_some() {
            return "Couldn't verify";
        }
        match self.auth {
            AuthState::SignedIn => "Ready",
            AuthState::SignedOut => "Sign in needed",
            AuthState::Unknown => "Couldn't verify",
        }
    }

    pub fn actionable_status(&self) -> String {
        if let Some(reason) = &self.unavailable {
            return reason.clone();
        }
        if self.status_label() == "Update needed" {
            return "This CLI version is below the connector requirement. Update it in the server Terminal, then check again.".into();
        }
        if self.version.is_none() {
            return "The CLI version could not be identified; compatibility has not been verified."
                .into();
        }
        match self.auth {
            AuthState::SignedIn => "Compatible CLI and saved sign-in found. Model access and quota have not been tested.".into(),
            AuthState::SignedOut => "The CLI reports no saved sign-in. Open Configure to sign in, then return to check again.".into(),
            AuthState::Unknown => "Authentication could not be verified safely. Open Configure to inspect native settings; no model-access test was run.".into(),
        }
    }
}

pub type ProbeResult = Result<Vec<HarnessAvailability>, String>;

struct Entry {
    result: Option<ProbeResult>,
    updated: Instant,
}

#[derive(Clone)]
struct ProbeSpec {
    target: ConnectionTarget,
    // Fixed order, including disabled providers, keeps the settings catalogue stable.
    candidates: Vec<(String, String, String, bool, Option<String>)>,
}

impl ProbeSpec {
    fn target(target: &ConnectionTarget) -> Self {
        Self {
            target: target.clone(),
            candidates: [
                (
                    "claude",
                    if *target == ConnectionTarget::Local {
                        ClaudeConnector::default().bin
                    } else {
                        "claude".into()
                    },
                    "2.0.0",
                ),
                ("codex", CodexConnector::default().bin, "0.100.0"),
                ("opencode", OpenCodeConnector::default().bin, "1.18.0"),
                ("grok", GrokConnector::default().bin, "1.0.0"),
            ]
            .into_iter()
            .map(|(key, program, minimum)| (key.into(), program, minimum.into(), true, None))
            .collect(),
        }
    }

    fn profile(profile: &ServerProfile) -> Self {
        let mut spec = Self::target(&profile.target);
        for (key, program, _, enabled, model) in &mut spec.candidates {
            if let Some(settings) = profile.harnesses.get(key) {
                if let Some(executable) = &settings.executable {
                    *program = executable.clone();
                }
                *enabled = settings.enabled;
                *model = settings.model.clone();
            }
        }
        spec
    }

    fn key(&self) -> String {
        // Include resolved executable/defaults, so edits and an older in-flight
        // result can never populate the cache for a different configuration.
        serde_json::to_string(&(&self.target, &self.candidates)).expect("string-only probe key")
    }
}

pub struct RemoteProbes {
    entries: HashMap<String, Entry>,
    sender: Sender<(String, ProbeResult)>,
    receiver: Receiver<(String, ProbeResult)>,
}

impl Default for RemoteProbes {
    fn default() -> Self {
        let (sender, receiver) = mpsc::channel();
        Self {
            entries: HashMap::new(),
            sender,
            receiver,
        }
    }
}

impl RemoteProbes {
    /// Explicit refresh bypasses completed cache entries, but never creates a
    /// second in-flight probe or blocks input while the target is being checked.
    pub fn refresh(&mut self, target: &ConnectionTarget) -> bool {
        if *target == ConnectionTarget::Local {
            return false;
        }
        self.request_spec(ProbeSpec::target(target), true, probe_ready)
    }

    pub fn request_profile(&mut self, profile: &ServerProfile) -> bool {
        self.request_spec(ProbeSpec::profile(profile), false, probe_ready)
    }

    pub fn refresh_profile(&mut self, profile: &ServerProfile) -> bool {
        self.request_spec(ProbeSpec::profile(profile), true, probe_ready)
    }

    pub fn get_profile(&self, profile: &ServerProfile) -> Option<&ProbeResult> {
        self.entries
            .get(&ProbeSpec::profile(profile).key())?
            .result
            .as_ref()
    }

    #[cfg(test)]
    fn request_using<F>(&mut self, target: &ConnectionTarget, force: bool, probe: F) -> bool
    where
        F: FnOnce(ConnectionTarget) -> ProbeResult + Send + 'static,
    {
        if *target == ConnectionTarget::Local {
            return false;
        }
        self.request_spec(ProbeSpec::target(target), force, |spec| probe(spec.target))
    }

    fn request_spec<F>(&mut self, spec: ProbeSpec, force: bool, probe: F) -> bool
    where
        F: FnOnce(ProbeSpec) -> ProbeResult + Send + 'static,
    {
        let key = spec.key();
        self.poll();
        if let Some(entry) = self.entries.get(&key) {
            let Some(result) = &entry.result else {
                return false;
            };
            // An unavailable provider can have been installed/upgraded in the
            // task Terminal. Partial failures need the same retry opportunity
            // as connection failures. OpenCode's explicit remote capability
            // block is permanent here, so it alone does not expire the cache.
            let has_retryable_failure = match result {
                Err(_) => true,
                Ok(entries) => entries
                    .iter()
                    .any(|entry| !entry.is_ready() && entry.hkey != "opencode"),
            };
            let ttl = if has_retryable_failure { 5 } else { 60 };
            if !force && entry.updated.elapsed() < Duration::from_secs(ttl) {
                return false;
            }
        }
        self.entries.insert(
            key.clone(),
            Entry {
                result: None,
                updated: Instant::now(),
            },
        );
        let sender = self.sender.clone();
        std::thread::spawn(move || {
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| probe(spec)))
                .unwrap_or_else(|_| {
                    Err("Provider check failed unexpectedly; press F5 to retry".into())
                });
            let _ = sender.send((key, result));
        });
        true
    }

    /// Drain completed work without waiting. Call from the ordinary event loop.
    pub fn poll(&mut self) -> bool {
        let mut changed = false;
        while let Ok((host, result)) = self.receiver.try_recv() {
            self.entries.insert(
                host,
                Entry {
                    result: Some(result),
                    updated: Instant::now(),
                },
            );
            changed = true;
        }
        changed
    }

    /// None means not requested or still checking. No empty list is substituted
    /// for a failed connection, so the form can report an actionable error.
    #[cfg(test)]
    pub fn get(&self, target: &ConnectionTarget) -> Option<&ProbeResult> {
        if *target == ConnectionTarget::Local {
            return None;
        }
        self.entries
            .get(&ProbeSpec::target(target).key())?
            .result
            .as_ref()
    }
}

fn availability_on_target(
    target: &ConnectionTarget,
    hkey: &str,
    minimum: &str,
    detail: Result<String, String>,
) -> HarnessAvailability {
    let (version, mut unavailable) = match detail {
        Err(error) => (None, Some(error)),
        Ok(detail) => {
            let version = parse_version(&detail);
            let reason = match &version {
                None => Some("Could not identify this provider's version from --version".into()),
                Some(version) if !version_ok(version, minimum) => Some(format!(
                    "Installed version {version}; this connector requires {minimum} or newer"
                )),
                Some(_) => None,
            };
            (version, reason)
        }
    };
    if hkey == "opencode" && *target != ConnectionTarget::Local {
        let reason = REMOTE_OPENCODE_REASON;
        unavailable = Some(match unavailable {
            Some(existing) => format!("{existing}. {reason}"),
            None => reason.into(),
        });
    }
    HarnessAvailability {
        hkey: hkey.into(),
        version,
        unavailable,
        auth: AuthState::Unknown,
    }
}

fn probe_ready(spec: ProbeSpec) -> ProbeResult {
    let target = spec.target;
    let results = std::thread::scope(|scope| {
        spec.candidates
            .into_iter()
            .map(|(harness, program, minimum, enabled, model)| {
                let target = &target;
                scope.spawn(move || {
                    let mut available = availability_on_target(
                        target,
                        &harness,
                        &minimum,
                        tmux::probe_program(target, &program).map_err(|error| error.to_string()),
                    );
                    if !enabled {
                        available.unavailable = Some(DISABLED_REASON.into());
                    } else if harness == "opencode" && model.is_some() {
                        available.unavailable = Some(OPENCODE_MODEL_REASON.into());
                    }
                    if available.version.is_some() {
                        available.auth = probe_auth(target, &harness, &program);
                    }
                    available
                })
            })
            .collect::<Vec<_>>()
            .into_iter()
            .map(|handle| {
                handle
                    .join()
                    .map_err(|_| "remote provider probe failed unexpectedly".to_string())
            })
            .collect::<Result<Vec<_>, _>>()
    })?;
    Ok(results)
}

fn probe_auth(target: &ConnectionTarget, harness: &str, program: &str) -> AuthState {
    probe_auth_using(target, harness, program, tmux::probe_auth_status)
}

fn probe_auth_using<F>(
    target: &ConnectionTarget,
    harness: &str,
    program: &str,
    probe: F,
) -> AuthState
where
    F: FnOnce(&ConnectionTarget, &str, &[String]) -> Result<std::process::Output, tmux::Error>,
{
    let args: Vec<String> = match harness {
        "claude" => ["auth", "status", "--json"]
            .into_iter()
            .map(String::from)
            .collect(),
        "codex" => ["login", "status"].into_iter().map(String::from).collect(),
        // OpenCode's list is a credential inventory, not a supported model-login
        // proof. Grok exposes no dedicated read-only native status command.
        _ => return AuthState::Unknown,
    };
    match probe(target, program, &args) {
        Ok(output) => parse_auth_status(harness, &output),
        // Never stringify status output or errors. Key material and account
        // details may occur in native stdout/stderr, including Codex API status.
        Err(_) => AuthState::Unknown,
    }
}

fn parse_auth_status(harness: &str, output: &std::process::Output) -> AuthState {
    match harness {
        "claude" => {
            let value = serde_json::from_slice::<serde_json::Value>(&output.stdout).ok();
            if value
                .as_ref()
                .and_then(|value| value.get("error"))
                .is_some_and(|error| !error.is_null())
            {
                return AuthState::Unknown;
            }
            let logged_in = value
                .as_ref()
                .and_then(|value| value.get("loggedIn"))
                .and_then(serde_json::Value::as_bool);
            match (output.status.code(), logged_in) {
                (Some(0), Some(true)) => AuthState::SignedIn,
                (Some(0 | 1), Some(false)) => AuthState::SignedOut,
                _ => AuthState::Unknown,
            }
        }
        "codex" => {
            let (Ok(stdout), Ok(stderr)) = (
                std::str::from_utf8(&output.stdout),
                std::str::from_utf8(&output.stderr),
            ) else {
                return AuthState::Unknown;
            };
            let logged_in = stdout
                .lines()
                .chain(stderr.lines())
                .any(|line| line.trim().starts_with("Logged in using "));
            let signed_out = stdout
                .lines()
                .chain(stderr.lines())
                .any(|line| line.trim() == "Not logged in");
            match (output.status.code(), logged_in, signed_out) {
                (Some(0), true, false) => AuthState::SignedIn,
                (Some(1), false, true) => AuthState::SignedOut,
                _ => AuthState::Unknown,
            }
        }
        _ => AuthState::Unknown,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };

    fn availability(
        hkey: &str,
        minimum: &str,
        detail: Result<String, String>,
    ) -> HarnessAvailability {
        availability_on_target(
            &ConnectionTarget::ssh("fixture").unwrap(),
            hkey,
            minimum,
            detail,
        )
    }

    fn profile(target: ConnectionTarget) -> ServerProfile {
        ServerProfile {
            server_id: "test-profile".into(),
            name: "Test profile".into(),
            target,
            repository_root: None,
            harnesses: Default::default(),
            archived: false,
            setup_runtimes: Default::default(),
        }
    }

    fn wait_for_profile(cache: &mut RemoteProbes, profile: &ServerProfile) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while cache.get_profile(profile).is_none() {
            assert!(
                Instant::now() < deadline,
                "profile probe result never arrived"
            );
            cache.poll();
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    #[test]
    fn profile_cache_keys_exact_configuration_and_ignores_late_edited_results() {
        let mut cache = RemoteProbes::default();
        let mut profile = profile(ConnectionTarget::Local);
        // Other integration tests temporarily replace the process-wide legacy
        // Claude executable. This injected cache test must own its entire
        // effective configuration rather than observing another test's env.
        profile.harnesses.insert(
            "claude".into(),
            taskai_core::registry::HarnessSettings {
                enabled: true,
                executable: Some("/original fixture cli".into()),
                model: None,
            },
        );
        let original = profile.clone();
        let (release, waiting) = mpsc::channel();
        assert!(
            cache.request_spec(ProbeSpec::profile(&profile), false, move |_| {
                waiting.recv_timeout(Duration::from_secs(3)).unwrap();
                Ok(vec![ready("old executable")])
            })
        );
        profile.harnesses.insert(
            "claude".into(),
            taskai_core::registry::HarnessSettings {
                enabled: true,
                executable: Some("/different cli".into()),
                model: Some("model-two".into()),
            },
        );
        assert!(cache.get_profile(&profile).is_none());
        assert!(
            cache.request_spec(ProbeSpec::profile(&profile), false, |_| Ok(vec![ready(
                "new executable"
            )]))
        );
        wait_for_profile(&mut cache, &profile);
        release.send(()).unwrap();
        wait_for_profile(&mut cache, &original);
        assert_eq!(
            cache.get_profile(&profile),
            Some(&Ok(vec![ready("new executable")]))
        );
        assert_eq!(
            cache.get_profile(&original),
            Some(&Ok(vec![ready("old executable")]))
        );
        let mut renamed = profile.clone();
        renamed.name = "Rename does not change destination or launch".into();
        assert_eq!(cache.get_profile(&profile), cache.get_profile(&renamed));
        renamed.harnesses.get_mut("claude").unwrap().model = Some("model-three".into());
        assert!(cache.get_profile(&renamed).is_none());
        renamed = profile.clone();
        renamed.target = ConnectionTarget::ssh("another-target").unwrap();
        assert!(cache.get_profile(&renamed).is_none());
    }

    #[cfg(unix)]
    #[test]
    fn local_profile_probes_configured_executables_without_claiming_authentication() {
        use std::os::unix::fs::PermissionsExt;
        let temp = tempfile::tempdir().unwrap();
        let bin = temp.path().join("fixture provider with spaces");
        std::fs::write(
            &bin,
            "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '2.5.0 (test provider)\\n'\n",
        )
        .unwrap();
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o700)).unwrap();
        let mut profile = profile(ConnectionTarget::Local);
        for harness in ["claude", "codex", "opencode", "grok"] {
            profile.harnesses.insert(
                harness.into(),
                taskai_core::registry::HarnessSettings {
                    enabled: harness != "grok",
                    executable: Some(bin.display().to_string()),
                    model: None,
                },
            );
        }
        let mut cache = RemoteProbes::default();
        assert!(cache.request_profile(&profile));
        wait_for_profile(&mut cache, &profile);
        let catalogue = cache.get_profile(&profile).unwrap().as_ref().unwrap();
        assert!(catalogue
            .iter()
            .all(|entry| entry.version.as_deref() == Some("2.5.0")));
        assert!(catalogue
            .iter()
            .all(|entry| entry.authentication_status() == "Couldn't verify"));
        assert!(catalogue
            .iter()
            .find(|entry| entry.hkey == "opencode")
            .unwrap()
            .is_ready());
        assert!(catalogue
            .iter()
            .find(|entry| entry.hkey == "grok")
            .unwrap()
            .unavailable
            .as_deref()
            .unwrap()
            .contains("Disabled"));
        profile.harnesses.get_mut("opencode").unwrap().model = Some("explicit-model".into());
        assert!(cache.refresh_profile(&profile));
        wait_for_profile(&mut cache, &profile);
        assert!(cache
            .get_profile(&profile)
            .unwrap()
            .as_ref()
            .unwrap()
            .iter()
            .find(|entry| entry.hkey == "opencode")
            .unwrap()
            .unavailable
            .as_deref()
            .unwrap()
            .contains("does not support a model"));
    }

    fn ready(hkey: &str) -> HarnessAvailability {
        HarnessAvailability {
            hkey: hkey.into(),
            version: Some("2.1.0".into()),
            unavailable: None,
            auth: AuthState::Unknown,
        }
    }

    fn status_output(code: i32, stdout: &[u8], stderr: &[u8]) -> std::process::Output {
        let mut output = std::process::Command::new("/bin/sh")
            .args(["-c", &format!("exit {code}")])
            .output()
            .unwrap();
        output.stdout = stdout.into();
        output.stderr = stderr.into();
        output
    }

    #[test]
    fn native_auth_parser_reduces_sensitive_output_to_enums_only() {
        type AuthCase<'a> = (&'a str, i32, &'a [u8], &'a [u8], AuthState);
        let cases: &[AuthCase<'_>] = &[
            ("claude", 0, br#"{"loggedIn":true,"email":"private@example.test","accessToken":"fixture-secret-never-emit"}"#, b"", AuthState::SignedIn),
            ("claude", 0, br#"{"loggedIn":false}"#, b"", AuthState::SignedOut),
            ("claude", 1, br#"{"loggedIn":false}"#, b"", AuthState::SignedOut),
            ("claude", 255, br#"{"loggedIn":false}"#, b"network error", AuthState::Unknown),
            ("claude", 0, br#"{"loggedIn":"true"}"#, b"", AuthState::Unknown),
            ("claude", 1, b"", b"keychain locked fixture-secret-never-emit", AuthState::Unknown),
            ("codex", 0, b"", b"Logged in using API key - fixture-secret-never-emit\n", AuthState::SignedIn),
            ("codex", 0, b"", b"Logged in using ChatGPT\n", AuthState::SignedIn),
            ("codex", 1, b"", b"Not logged in\n", AuthState::SignedOut),
            ("codex", 1, b"", b"network error fixture-secret-never-emit", AuthState::Unknown),
            ("codex", 255, b"", b"Not logged in", AuthState::Unknown),
            ("codex", 0, b"Usage: codex login", b"", AuthState::Unknown),
            ("codex", 0, b"Not logged in", b"Logged in using ChatGPT", AuthState::Unknown),
        ];
        for (harness, code, stdout, stderr, expected) in cases {
            let state = parse_auth_status(harness, &status_output(*code, stdout, stderr));
            assert_eq!(state, *expected, "{harness} code {code}");
            let mut entry = ready(harness);
            entry.auth = state;
            let safe = format!(
                "{entry:?} {} {} {}",
                entry.authentication_status(),
                entry.status_label(),
                entry.actionable_status()
            );
            assert!(!safe.contains("fixture-secret-never-emit"));
            assert!(!safe.contains("private@example.test"));
        }
    }

    #[test]
    fn auth_errors_are_unknown_and_unverified_harnesses_run_no_auth_command() {
        let target = ConnectionTarget::ssh("fixture-host").unwrap();
        let state = probe_auth_using(
            &target,
            "claude",
            "/configured cli",
            |observed, program, args| {
                assert_eq!(observed, &target);
                assert_eq!(program, "/configured cli");
                assert_eq!(args, ["auth", "status", "--json"]);
                Err(tmux::Error::Timeout {
                    operation: "authentication status",
                    after: Duration::from_secs(10),
                    detail: "fixture-secret-never-emit".into(),
                })
            },
        );
        assert_eq!(state, AuthState::Unknown);
        assert_eq!(
            probe_auth_using(
                &ConnectionTarget::Local,
                "codex",
                "custom-codex",
                |target, program, args| {
                    assert_eq!(target, &ConnectionTarget::Local);
                    assert_eq!(program, "custom-codex");
                    assert_eq!(args, ["login", "status"]);
                    Ok(status_output(1, b"", b"Not logged in"))
                }
            ),
            AuthState::SignedOut
        );
        for harness in ["opencode", "grok", "unknown"] {
            assert_eq!(
                probe_auth_using(&target, harness, harness, |_, _, _| panic!(
                    "must not inspect credentials or invoke a model catalogue"
                )),
                AuthState::Unknown
            );
        }
    }

    #[test]
    fn actionable_status_requires_compatibility_and_known_login_without_gating_native_launch() {
        let mut entry = ready("claude");
        for (auth, label) in [
            (AuthState::SignedIn, "Ready"),
            (AuthState::SignedOut, "Sign in needed"),
            (AuthState::Unknown, "Couldn't verify"),
        ] {
            entry.auth = auth;
            assert_eq!(entry.status_label(), label);
            assert!(
                entry.is_ready(),
                "login discovery does not disable existing/custom native providers"
            );
        }
        entry.auth = AuthState::SignedIn;
        entry.version = None;
        assert_eq!(entry.status_label(), "Couldn't verify");
        entry.version = Some("1.0.0".into());
        assert_eq!(entry.status_label(), "Update needed");
        entry.unavailable = Some(DISABLED_REASON.into());
        assert_eq!(entry.status_label(), "Disabled");
        let mut remote_opencode = availability("opencode", "1.18.0", Ok("1.18.0".into()));
        remote_opencode.auth = AuthState::SignedIn;
        assert_eq!(remote_opencode.status_label(), "Unsupported");
    }

    #[test]
    fn configured_ssh_profile_probe_respects_executable_edits() {
        let Ok(host) = std::env::var("TASKAI_REMOTE_TEST_HOST") else {
            return;
        };
        let mut profile = profile(ConnectionTarget::ssh(host).unwrap());
        profile.harnesses.insert(
            "claude".into(),
            taskai_core::registry::HarnessSettings {
                enabled: true,
                executable: Some("/taskai-no-such-profile-executable".into()),
                model: None,
            },
        );
        let mut cache = RemoteProbes::default();
        for missing in [true, false] {
            if !missing {
                profile.harnesses.get_mut("claude").unwrap().executable = Some("claude".into());
                assert!(cache.get_profile(&profile).is_none());
            }
            assert!(cache.request_profile(&profile));
            let deadline = Instant::now() + Duration::from_secs(35);
            while cache.get_profile(&profile).is_none() {
                assert!(
                    Instant::now() < deadline,
                    "SSH profile result never completed"
                );
                cache.poll();
                std::thread::sleep(Duration::from_millis(10));
            }
            let catalogue = cache.get_profile(&profile).unwrap().as_ref().unwrap();
            let claude = catalogue
                .iter()
                .find(|entry| entry.hkey == "claude")
                .unwrap();
            assert_eq!(claude.is_ready(), !missing, "{claude:?}");
            assert!(matches!(
                claude.authentication_status(),
                "CLI reports signed in" | "Sign in needed" | "Couldn't verify"
            ));
            eprintln!("SSH Claude configured executable missing={missing}: {claude:?}");
        }
    }

    #[test]
    fn configured_native_auth_checks_emit_only_sanitized_states() {
        let mut targets = Vec::new();
        if std::env::var_os("TASKAI_AUTH_TEST_LOCAL").is_some() {
            targets.push(ConnectionTarget::Local);
        }
        if let Ok(host) = std::env::var("TASKAI_REMOTE_TEST_HOST") {
            targets.push(ConnectionTarget::ssh(host).unwrap());
        }
        for target in targets {
            for (harness, program, _, _, _) in ProbeSpec::target(&target).candidates {
                if !matches!(harness.as_str(), "claude" | "codex") {
                    continue;
                }
                let state = probe_auth(&target, &harness, &program);
                eprintln!("Native saved-login check {target} {harness}: {state:?}");
                assert_ne!(state, AuthState::Unknown, "native auth status was unavailable or unrecognized on {target}; no raw response is logged");
            }
        }
    }

    fn wait_for(cache: &mut RemoteProbes, target: &ConnectionTarget) {
        let deadline = Instant::now() + Duration::from_secs(3);
        while cache.get(target).is_none() {
            assert!(Instant::now() < deadline, "probe result never arrived");
            cache.poll();
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    #[test]
    fn slow_remote_probe_never_blocks_input_and_reuses_its_result() {
        let mut cache = RemoteProbes::default();
        let target = ConnectionTarget::ssh("probe-test-host").unwrap();
        let calls = Arc::new(AtomicUsize::new(0));
        let count = calls.clone();
        let (release, wait) = mpsc::channel();
        assert!(cache.request_using(&target, false, move |_| {
            count.fetch_add(1, Ordering::SeqCst);
            wait.recv_timeout(Duration::from_secs(3)).unwrap();
            Ok(vec![ready("claude"), ready("codex")])
        }));
        assert!(cache.get(&target).is_none());
        assert!(!cache.request_using(&target, false, |_| panic!("duplicate probe")));
        assert!(!cache.request_using(&target, true, |_| panic!("duplicate refresh")));
        release.send(()).unwrap();
        wait_for(&mut cache, &target);
        assert_eq!(
            cache.get(&target),
            Some(&Ok(vec![ready("claude"), ready("codex")]))
        );
        assert!(!cache.request_using(&target, false, |_| panic!("uncached probe")));
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn failed_host_is_truthful_retryable_and_never_reused_for_a_peer() {
        let mut cache = RemoteProbes::default();
        let target = ConnectionTarget::ssh("probe-failed-host").unwrap();
        let peer = ConnectionTarget::ssh("probe-peer-host").unwrap();
        cache.request_using(&target, false, |_| Err("SSH permission denied".into()));
        wait_for(&mut cache, &target);
        assert_eq!(
            cache.get(&target),
            Some(&Err("SSH permission denied".into()))
        );
        assert!(cache.get(&peer).is_none());
        assert!(!cache.request_using(&target, false, |_| panic!("rapid retry")));
        cache
            .entries
            .get_mut(&ProbeSpec::target(&target).key())
            .unwrap()
            .updated = Instant::now() - Duration::from_secs(6);
        assert!(cache.request_using(&target, false, |_| Ok(vec![ready("grok")])));
        wait_for(&mut cache, &target);
        assert_eq!(cache.get(&target), Some(&Ok(vec![ready("grok")])));
        assert!(
            !cache.request_using(&ConnectionTarget::Local, false, |_| panic!(
                "local SSH probe"
            ))
        );
        assert!(!cache.refresh(&ConnectionTarget::Local));
    }

    #[test]
    fn partial_catalogue_preserves_every_provider_version_and_failure_reason() {
        let catalogue = [
            availability("claude", "2.0.0", Ok("2.1.226 (Claude Code)".into())),
            availability(
                "codex",
                "0.100.0",
                Err("codex: command not found in login PATH".into()),
            ),
            availability("opencode", "1.18.0", Ok("1.18.2".into())),
            availability("grok", "1.0.0", Ok("0.2.117".into())),
        ];
        assert_eq!(
            catalogue
                .iter()
                .map(|entry| entry.hkey.as_str())
                .collect::<Vec<_>>(),
            ["claude", "codex", "opencode", "grok"]
        );
        assert!(catalogue[0].is_ready());
        assert_eq!(catalogue[0].version.as_deref(), Some("2.1.226"));
        assert_eq!(
            catalogue[1].unavailable.as_deref(),
            Some("codex: command not found in login PATH")
        );
        assert_eq!(catalogue[2].version.as_deref(), Some("1.18.2"));
        assert!(catalogue[2]
            .unavailable
            .as_deref()
            .unwrap()
            .contains("shared-live serve/attach"));
        assert_eq!(catalogue[3].version.as_deref(), Some("0.2.117"));
        assert!(catalogue[3]
            .unavailable
            .as_deref()
            .unwrap()
            .contains("requires 1.0.0"));
        let unknown = availability("claude", "2.0.0", Ok("unknown build".into()));
        assert!(!unknown.is_ready());
        assert!(unknown.version.is_none());
    }

    #[test]
    fn explicit_refresh_replaces_partial_catalogue_without_waiting_for_ttl() {
        let mut cache = RemoteProbes::default();
        let target = ConnectionTarget::ssh("refresh-host").unwrap();
        cache.request_using(&target, false, |_| {
            Ok(vec![
                ready("claude"),
                availability("codex", "0.100.0", Err("not installed".into())),
            ])
        });
        wait_for(&mut cache, &target);
        let (release, waiting) = mpsc::channel();
        assert!(cache.request_using(&target, true, move |_| {
            waiting.recv_timeout(Duration::from_secs(3)).unwrap();
            Ok(vec![ready("claude"), ready("codex")])
        }));
        assert!(cache.get(&target).is_none());
        assert!(!cache.request_using(&target, true, |_| panic!("overlapping refresh")));
        release.send(()).unwrap();
        wait_for(&mut cache, &target);
        assert!(cache
            .get(&target)
            .unwrap()
            .as_ref()
            .unwrap()
            .iter()
            .all(HarnessAvailability::is_ready));
    }

    #[test]
    fn partial_failures_retry_after_short_ttl_and_cache_is_not_global() {
        let mut first = RemoteProbes::default();
        let mut second = RemoteProbes::default();
        let target = ConnectionTarget::ssh("same-host").unwrap();
        let peer = ConnectionTarget::ssh("peer-host").unwrap();
        first.request_using(&target, false, |_| {
            Ok(vec![
                ready("claude"),
                availability("codex", "0.100.0", Err("temporarily missing".into())),
            ])
        });
        wait_for(&mut first, &target);
        assert!(second.get(&target).is_none());
        assert!(first.get(&peer).is_none());
        second.request_using(
            &target,
            false,
            |_| Err("different connection result".into()),
        );
        wait_for(&mut second, &target);
        assert!(first.get(&target).unwrap().is_ok());
        assert!(second.get(&target).unwrap().is_err());
        first
            .entries
            .get_mut(&ProbeSpec::target(&target).key())
            .unwrap()
            .updated = Instant::now() - Duration::from_secs(6);
        assert!(first.request_using(&target, false, |_| Ok(vec![
            ready("claude"),
            ready("codex")
        ])));
        wait_for(&mut first, &target);
        assert!(first
            .get(&target)
            .unwrap()
            .as_ref()
            .unwrap()
            .iter()
            .all(HarnessAvailability::is_ready));
        assert!(second.get(&target).unwrap().is_err());
    }

    #[test]
    fn failed_worker_reports_an_error_instead_of_staying_checking_forever() {
        let mut cache = RemoteProbes::default();
        let target = ConnectionTarget::ssh("failed-worker").unwrap();
        cache.request_using(&target, false, |_| panic!("injected probe panic"));
        wait_for(&mut cache, &target);
        assert!(cache
            .get(&target)
            .unwrap()
            .as_ref()
            .unwrap_err()
            .contains("F5 to retry"));
        assert!(cache.request_using(&target, true, |_| Ok(vec![ready("claude")])));
        wait_for(&mut cache, &target);
        assert!(cache.get(&target).unwrap().is_ok());
    }
}
