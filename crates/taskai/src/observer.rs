// ABOUTME: Passive native Claude registration observer retained beside the provider pane.
// ABOUTME: It refreshes only the exact owned card and clears only matching TaskAI metadata.

use std::{
    path::Path,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    thread,
    time::Duration,
};

use taskai_connectors::claude::{
    drift::drift_check,
    peer::{card_for, native_entry, Card},
};
use taskai_core::{
    mail::{is_live, Directory, MailPeer},
    registry::{AddressBook, ConnectionTarget, Registry, TmuxRuntimeLocator},
    Error,
};

use crate::{
    doctor,
    tmux::{self, NativeIdentity},
};

const OBSERVE_INTERVAL: Duration = Duration::from_millis(250);

/// The observer is the detached sidecar process, not a UI frame watcher.
/// It reads only its exact native pane and `<pid>.json` card.
pub fn run(home: &Path, session_ref: &str) -> Result<(), String> {
    if std::env::var("TASKAI_SESSION_REF").ok().as_deref() != Some(session_ref) {
        return Err("observer session does not match its launch identity".into());
    }
    let terminating = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(signal_hook::consts::SIGTERM, Arc::clone(&terminating))
        .map_err(|error| error.to_string())?;
    let state_dir = home.join("state");
    let registry = Registry::open(&state_dir).map_err(|error| error.to_string())?;
    let session = registry
        .session(session_ref)
        .ok_or("observer has no saved session")?;
    let runtime = session
        .runtime
        .clone()
        .ok_or("observer has no saved runtime")?;
    let executable = doctor::configured_executable("claude", session.launch.executable.as_deref());
    let expected = match tmux::native_identity(&runtime) {
        Ok(Some(identity)) => identity,
        Ok(None) => {
            return initial_observation_failed(
                &state_dir,
                session,
                "observer has no live native pane".into(),
            );
        }
        Err(error) => return initial_observation_failed(&state_dir, session, error.to_string()),
    };
    let mut observer = NativeObserver::new(registry, &state_dir, session_ref, expected.clone())
        .map_err(|error| error.to_string())?;
    let result = (|| {
        let sessions_dir = crate::bridge::native_sessions_dir()?;
        let availability = doctor::check_harness("claude", &executable, &doctor::Paths::from_env());
        let common_ready = availability.is_ready()
            && availability
                .messaging
                .as_ref()
                .is_some_and(doctor::Check::is_ready);
        loop {
            if terminating.load(Ordering::Relaxed) {
                return Ok(());
            }
            let observed = tmux::native_identity(&runtime).map_err(|error| error.to_string())?;
            let card = if observed.as_ref() == Some(&expected) {
                card_for(&sessions_dir, expected.pid, &expected.proc_start)
                    .map_err(|error| error.to_string())?
            } else {
                None
            };
            let ready = common_ready
                && card.as_ref().is_some_and(|card| {
                    drift_check(availability.version.as_deref(), std::slice::from_ref(card)).enabled
                });
            match observer
                .refresh(observed.as_ref(), card.as_ref(), ready)
                .map_err(|error| error.to_string())?
            {
                RefreshOutcome::Stopped => return Ok(()),
                RefreshOutcome::Waiting | RefreshOutcome::Registered => {
                    thread::sleep(OBSERVE_INTERVAL);
                }
            }
        }
    })();
    finish_observation(&mut observer, result)
}

fn finish_observation(
    observer: &mut NativeObserver,
    result: Result<(), String>,
) -> Result<(), String> {
    let cleanup = observer
        .clear_registered()
        .map_err(|error| error.to_string());
    combine_cleanup(result, cleanup)
}

fn initial_observation_failed(
    state_dir: &Path,
    session: &taskai_core::registry::Session,
    error: String,
) -> Result<(), String> {
    let cleanup = if session.harness == "claude"
        && session.runtime.as_ref().is_some_and(|runtime| {
            runtime.target == ConnectionTarget::Local && runtime.session == session.session_ref
        }) {
        session.messaging.as_ref().map_or(Ok(()), |entry| {
            AddressBook::new(state_dir, &session.task_ref)
                .clear_if_matches(
                    &session.session_ref,
                    entry.pid,
                    &entry.proc_start,
                    &entry.mailbox,
                )
                .map(|_| ())
                .map_err(|error| error.to_string())
        })
    } else {
        Ok(())
    };
    combine_cleanup(Err(error), cleanup)
}

fn combine_cleanup(result: Result<(), String>, cleanup: Result<(), String>) -> Result<(), String> {
    match (result, cleanup) {
        (Ok(()), Ok(())) => Ok(()),
        (Err(error), Ok(())) => Err(error),
        (Ok(()), Err(cleanup)) => Err(format!("observer metadata cleanup failed: {cleanup}")),
        (Err(error), Err(cleanup)) => Err(format!(
            "{error}; observer metadata cleanup failed: {cleanup}"
        )),
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RefreshOutcome {
    Waiting,
    Registered,
    Stopped,
}

pub struct NativeObserver {
    registry: Registry,
    book: AddressBook,
    session_ref: String,
    task_ref: String,
    workspace: std::path::PathBuf,
    runtime: TmuxRuntimeLocator,
    expected: NativeIdentity,
    registered: Option<MailPeer>,
}

impl NativeObserver {
    pub fn new(
        registry: Registry,
        state_dir: &Path,
        session_ref: &str,
        expected: NativeIdentity,
    ) -> Result<Self, Error> {
        let session = registry
            .session(session_ref)
            .ok_or_else(|| Error::NotFound(format!("session {session_ref}")))?;
        let runtime = session
            .runtime
            .as_ref()
            .filter(|runtime| {
                runtime.target == ConnectionTarget::Local && runtime.session == session_ref
            })
            .ok_or_else(|| Error::Invalid("native Claude has no exact local runtime".into()))?
            .clone();
        if session.harness != "claude" {
            return Err(Error::Invalid(
                "observer requires a native Claude session".into(),
            ));
        }
        let registered = session.messaging.as_ref().and_then(|entry| {
            (entry.pid == expected.pid && entry.proc_start == expected.proc_start).then(|| {
                MailPeer {
                    session_ref: session.session_ref.clone(),
                    task_ref: session.task_ref.clone(),
                    name: session.address.clone(),
                    harness: session.harness.clone(),
                    workspace: session.workspace.clone(),
                    pid: entry.pid,
                    proc_start: entry.proc_start.clone(),
                    status: entry.status,
                    waiting_for: entry.waiting_for.clone(),
                    mailbox: entry.mailbox.clone(),
                    features: entry.features.clone(),
                    mode: entry.mode,
                }
            })
        });
        Ok(Self {
            book: AddressBook::new(state_dir, &session.task_ref),
            session_ref: session_ref.into(),
            task_ref: session.task_ref.clone(),
            workspace: session.workspace.clone(),
            runtime,
            expected,
            registered,
            registry,
        })
    }

    /// One bounded observation. A missing card is retried; a replaced pane
    /// stops the observer. The supplied readiness is the caller's M3/M7 gate.
    pub fn refresh(
        &mut self,
        observed: Option<&NativeIdentity>,
        card: Option<&Card>,
        ready: bool,
    ) -> Result<RefreshOutcome, Error> {
        if observed != Some(&self.expected)
            || !is_live(self.expected.pid, &self.expected.proc_start)
        {
            self.clear_registered()?;
            return Ok(RefreshOutcome::Stopped);
        }
        self.registry.reload()?;
        let Some(session) = self.registry.session(&self.session_ref) else {
            self.clear_registered()?;
            return Ok(RefreshOutcome::Stopped);
        };
        if session.task_ref != self.task_ref
            || session.workspace != self.workspace
            || session.runtime.as_ref() != Some(&self.runtime)
            || session.harness != "claude"
        {
            self.clear_registered()?;
            return Ok(RefreshOutcome::Stopped);
        }
        let entry = card
            .filter(|card| {
                ready
                    && card.pid == self.expected.pid
                    && card.proc_start.as_deref() == Some(self.expected.proc_start.as_str())
            })
            .and_then(|card| native_entry(session, card, None));
        let Some(entry) = entry else {
            self.clear_registered()?;
            return Ok(RefreshOutcome::Waiting);
        };
        let current = session.messaging.as_ref();
        let already_current = current.is_some_and(|current| {
            current.pid == entry.pid
                && current.proc_start == entry.proc_start
                && current.mailbox == entry.mailbox
                && current.status == entry.status
                && current.waiting_for == entry.waiting_for
                && current.features == entry.features
                && current.mode == entry.mode
        });
        if !already_current {
            self.book.register(&entry)?;
        }
        self.registered = Some(entry);
        Ok(RefreshOutcome::Registered)
    }

    fn clear_registered(&mut self) -> Result<(), Error> {
        if let Some(entry) = self.registered.as_ref() {
            self.book.clear_if_matches(
                &entry.session_ref,
                entry.pid,
                &entry.proc_start,
                &entry.mailbox,
            )?;
            self.registered = None;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use taskai_connectors::claude::peer::Card;
    use taskai_core::{
        mail::{proc_start, Activity, Directory},
        registry::{AddressBook, Registry, Source},
    };
    use tempfile::TempDir;

    use super::{finish_observation, NativeObserver, RefreshOutcome};
    use crate::tmux::NativeIdentity;

    fn card(pid: u32, start: &str, socket: &Path, status: &str) -> Card {
        Card {
            pid,
            session_id: "claude-owned".into(),
            started_at: 1,
            kind: "interactive".into(),
            proc_start: Some(start.into()),
            peer_protocol: Some(1),
            peer_features: vec!["notify_idle".into()],
            messaging_socket_path: Some(socket.into()),
            name: None,
            status: Some(status.into()),
            waiting_for: (status == "waiting").then(|| "approval".into()),
        }
    }

    #[test]
    fn native_claude_refreshes_while_detached_and_clears_only_its_entry() {
        let root = TempDir::new().unwrap();
        let state = root.path().join("state");
        let workspace = root.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
            .unwrap();
        registry.bind_runtime(&session_ref, "taskai-test").unwrap();
        let pid = std::process::id();
        let start = proc_start(pid).unwrap();
        let identity = NativeIdentity {
            pane_id: "%42".into(),
            pid,
            proc_start: start.clone(),
        };
        let socket = root.path().join("claude.sock");
        let mut observer =
            NativeObserver::new(registry, &state, &session_ref, identity.clone()).unwrap();
        assert_eq!(
            observer.refresh(Some(&identity), None, true).unwrap(),
            RefreshOutcome::Waiting
        );
        let mut wrong = card(pid, &start, &socket, "idle");
        wrong.pid += 1;
        assert_eq!(
            observer
                .refresh(Some(&identity), Some(&wrong), true)
                .unwrap(),
            RefreshOutcome::Waiting
        );
        let mut stale = card(pid, &start, &socket, "idle");
        stale.proc_start = Some("old-process".into());
        assert_eq!(
            observer
                .refresh(Some(&identity), Some(&stale), true)
                .unwrap(),
            RefreshOutcome::Waiting
        );
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "idle")),
                    true
                )
                .unwrap(),
            RefreshOutcome::Registered
        );
        let mut view = Registry::open(&state).unwrap();
        assert_eq!(view.live_sessions(&task)[0].status, Activity::Idle);
        assert_eq!(view.live_sessions(&task)[0].mode, None);
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "waiting")),
                    true,
                )
                .unwrap(),
            RefreshOutcome::Registered
        );
        view.reload().unwrap();
        assert_eq!(
            view.live_sessions(&task)[0].waiting_for.as_deref(),
            Some("approval")
        );
        assert_eq!(
            observer.refresh(Some(&identity), None, true).unwrap(),
            RefreshOutcome::Waiting
        );
        view.reload().unwrap();
        assert!(view.live_sessions(&task).is_empty());
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "idle")),
                    false,
                )
                .unwrap(),
            RefreshOutcome::Waiting
        );
        view.reload().unwrap();
        assert!(view.live_sessions(&task).is_empty());
        assert_eq!(
            observer.refresh(None, None, true).unwrap(),
            RefreshOutcome::Stopped
        );
        assert!(socket.parent().unwrap().exists());

        // A tmux observation error while the native process is still live
        // retracts only this observer's exact metadata.
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "idle")),
                    true
                )
                .unwrap(),
            RefreshOutcome::Registered
        );
        assert_eq!(
            finish_observation(&mut observer, Err("native observation failed".into())).unwrap_err(),
            "native observation failed"
        );
        view.reload().unwrap();
        assert!(view.live_sessions(&task).is_empty());
        assert!(taskai_core::mail::is_live(pid, &start));

        // Another registration may replace the same native PID's mailbox.
        // Cleanup must leave that entry intact.
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "idle")),
                    true
                )
                .unwrap(),
            RefreshOutcome::Registered
        );
        view.reload().unwrap();
        let mut replacement = view
            .session(&session_ref)
            .unwrap()
            .messaging
            .clone()
            .unwrap();
        replacement.mailbox = root.path().join("replacement.sock");
        let mut replacement_peer = card(pid, &start, &replacement.mailbox, "idle");
        replacement_peer.session_id = "claude-owned".into();
        let entry = taskai_connectors::claude::peer::native_entry(
            view.session(&session_ref).unwrap(),
            &replacement_peer,
            None,
        )
        .unwrap();
        AddressBook::new(&state, &task).register(&entry).unwrap();
        finish_observation(&mut observer, Ok(())).unwrap();
        view.reload().unwrap();
        assert_eq!(
            view.session(&session_ref)
                .unwrap()
                .messaging
                .as_ref()
                .unwrap()
                .mailbox,
            replacement.mailbox
        );

        // A failed clear keeps the identity for a later exact retry and
        // reports failure together with the original observation error.
        assert_eq!(
            observer
                .refresh(
                    Some(&identity),
                    Some(&card(pid, &start, &socket, "idle")),
                    true
                )
                .unwrap(),
            RefreshOutcome::Registered
        );
        let backup = root.path().join("state-backup");
        std::fs::rename(&state, &backup).unwrap();
        std::fs::write(&state, b"not a registry directory").unwrap();
        let error =
            finish_observation(&mut observer, Err("native observation failed".into())).unwrap_err();
        assert!(error.contains("native observation failed"));
        assert!(error.contains("cleanup failed"));
        assert!(observer.registered.is_some());
        std::fs::remove_file(&state).unwrap();
        std::fs::rename(&backup, &state).unwrap();
        finish_observation(&mut observer, Ok(())).unwrap();
        view.reload().unwrap();
        assert!(view.live_sessions(&task).is_empty());
    }
}
