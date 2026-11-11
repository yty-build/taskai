// ABOUTME: Owner-only metadata for a Codex thread creation and first-turn write.
// ABOUTME: A persisted intent blocks any automatic replay after an uncertain reply.

use std::{
    fs::{self, OpenOptions},
    io::{Read, Write},
    os::unix::fs::{OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};

use serde_json::{json, Value};
use taskai_core::{mail::is_live, write_private};

use crate::tmux::NativeIdentity;

const MAX_INTENT_BYTES: u64 = 4096;

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CreationState {
    Missing,
    Pending,
    Created(String),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FirstTurnState {
    Missing,
    Pending,
    Accepted,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SavedThreadOrigin {
    FreshGate,
    DirectResume,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SavedCreationPlan {
    FirstHumanGate,
    NativeResume,
    LegacyResume,
}

pub struct CodexIntents {
    dir: PathBuf,
    session_ref: String,
    native: NativeIdentity,
}

impl CodexIntents {
    pub fn new(home: &Path, session_ref: &str, native: NativeIdentity) -> Result<Self, String> {
        if session_ref.is_empty()
            || !session_ref
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_')
        {
            return Err("Codex intent needs a plain session ref".into());
        }
        let dir = home.join("run").join("codex");
        fs::create_dir_all(&dir).map_err(|error| error.to_string())?;
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700))
            .map_err(|error| error.to_string())?;
        Ok(Self {
            dir,
            session_ref: session_ref.into(),
            native,
        })
    }

    fn creation_path(&self) -> PathBuf {
        self.dir.join(format!("{}.creation.json", self.session_ref))
    }

    fn first_turn_path(&self) -> PathBuf {
        self.dir
            .join(format!("{}.first-turn.json", self.session_ref))
    }

    fn resume_path(&self) -> PathBuf {
        self.dir.join(format!("{}.resume.json", self.session_ref))
    }

    fn native_human_path(&self) -> PathBuf {
        self.dir
            .join(format!("{}.native-human.json", self.session_ref))
    }

    fn gate_identity_path(&self) -> PathBuf {
        self.dir.join(format!("{}.gate.json", self.session_ref))
    }

    fn adoption_path(&self) -> PathBuf {
        self.dir.join(format!("{}.adoption.json", self.session_ref))
    }

    fn base(&self) -> Value {
        json!({
            "sessionRef": self.session_ref,
            "paneId": self.native.pane_id,
            "pid": self.native.pid,
            "procStart": self.native.proc_start,
        })
    }

    fn valid_base(&self, value: &Value) -> bool {
        value["sessionRef"] == self.session_ref
            && value["paneId"] == self.native.pane_id
            && value["pid"] == self.native.pid
            && value["procStart"] == self.native.proc_start
    }

    pub fn creation_state(&self) -> Result<CreationState, String> {
        let Some(value) = read_intent(&self.creation_path())? else {
            return Ok(CreationState::Missing);
        };
        if !self.valid_base(&value) || value["kind"] != "creation" {
            return Err("Codex creation intent has a different native identity".into());
        }
        match value["threadId"].as_str() {
            Some(id) if !id.is_empty() => Ok(CreationState::Created(id.into())),
            None if value["threadId"].is_null() => Ok(CreationState::Pending),
            _ => Err("Codex creation intent has an invalid thread ID".into()),
        }
    }

    /// Before a dead native pane is recreated, distinguish an unattempted fresh gate from an
    /// accepted turn or an older saved ID with no M6 creation record. The original gate identity
    /// (or a previously staged adoption) must match the creation record exactly.
    pub fn saved_creation_plan(
        home: &Path,
        session_ref: &str,
        thread_id: &str,
    ) -> Result<SavedCreationPlan, String> {
        if thread_id.is_empty() || session_ref.is_empty() {
            return Err("Codex saved creation needs an exact thread and session".into());
        }
        let dir = home.join("run/codex");
        let Some(creation) = read_intent(&dir.join(format!("{session_ref}.creation.json")))? else {
            for suffix in ["gate", "first-turn", "adoption"] {
                if read_intent(&dir.join(format!("{session_ref}.{suffix}.json")))?.is_some() {
                    return Err(
                        "Codex saved creation record is missing beside gate metadata".into(),
                    );
                }
            }
            return Ok(SavedCreationPlan::LegacyResume);
        };
        if creation["sessionRef"] != session_ref
            || creation["kind"] != "creation"
            || creation["threadId"] != thread_id
            || creation["paneId"].as_str().is_none_or(str::is_empty)
            || creation["pid"]
                .as_u64()
                .and_then(|pid| u32::try_from(pid).ok())
                .is_none()
            || creation["procStart"].as_str().is_none_or(str::is_empty)
        {
            return Err("Codex saved creation does not prove this exact thread".into());
        }
        if let Some(first) = read_intent(&dir.join(format!("{session_ref}.first-turn.json")))? {
            if first["kind"] != "first-turn"
                || first["threadId"] != thread_id
                || !same_native_base(&first, &creation)
            {
                return Err("Codex saved first-turn intent differs from creation".into());
            }
            return match first["accepted"].as_bool() {
                Some(true) => Ok(SavedCreationPlan::NativeResume),
                Some(false) => Err("Codex first turn outcome is unknown; no retry was sent".into()),
                None => Err("Codex saved first-turn intent has invalid state".into()),
            };
        }
        let gate = read_intent(&dir.join(format!("{session_ref}.gate.json")))?;
        let adoption = read_intent(&dir.join(format!("{session_ref}.adoption.json")))?;
        let original_gate = gate
            .as_ref()
            .is_some_and(|gate| same_native_base(gate, &creation));
        let staged = adoption.as_ref().is_some_and(|adoption| {
            adoption["kind"] == "gate-adoption"
                && adoption["sessionRef"] == session_ref
                && adoption["threadId"] == thread_id
                && adoption["source"] == creation
        });
        if !original_gate && !staged {
            return Err("Codex creation metadata was replaced before cold reopen".into());
        }
        let pid = creation["pid"]
            .as_u64()
            .and_then(|pid| u32::try_from(pid).ok())
            .unwrap();
        if is_live(pid, creation["procStart"].as_str().unwrap()) {
            return Err("Codex original gate is still live; cold reopen is refused".into());
        }
        Ok(SavedCreationPlan::FirstHumanGate)
    }

    /// Called by launch before creating a replacement gate. It preserves the exact original
    /// creation record while the new gate updates its own current identity file.
    pub fn stage_gate_adoption(
        home: &Path,
        session_ref: &str,
        thread_id: &str,
    ) -> Result<(), String> {
        if Self::saved_creation_plan(home, session_ref, thread_id)?
            != SavedCreationPlan::FirstHumanGate
        {
            return Err("Codex cold reopen has no unattempted fresh gate".into());
        }
        let dir = home.join("run/codex");
        let source = read_intent(&dir.join(format!("{session_ref}.creation.json")))?
            .ok_or("Codex cold reopen lost its creation record")?;
        let marker = json!({
            "sessionRef": session_ref, "kind": "gate-adoption", "threadId": thread_id,
            "source": source,
        });
        write_private(
            &dir.join(format!("{session_ref}.adoption.json")),
            &marker.to_string(),
        )
        .map_err(|error| error.to_string())
    }

    /// A recreated bridge can adopt only the exact staged creation, once the new gate has
    /// published its current pane/PID/start. No thread/start or first turn is written here.
    pub fn has_gate_adoption(&self, thread_id: &str) -> Result<bool, String> {
        let Some(marker) = read_intent(&self.adoption_path())? else {
            return Ok(false);
        };
        let Some(creation) = read_intent(&self.creation_path())? else {
            return Err("Codex staged adoption lost its creation record".into());
        };
        Ok(marker["sessionRef"] == self.session_ref
            && marker["kind"] == "gate-adoption"
            && marker["threadId"] == thread_id
            && marker["source"] == creation)
    }

    pub fn adoption_staged(&self, thread_id: &str) -> Result<bool, String> {
        if !self.has_gate_adoption(thread_id)? {
            return Ok(false);
        }
        let Some(gate) = read_intent(&self.gate_identity_path())? else {
            return Ok(false);
        };
        Ok(self.valid_base(&gate))
    }

    pub fn adopt_saved_creation(&self, thread_id: &str) -> Result<(), String> {
        if !self.adoption_staged(thread_id)? {
            return Err("Codex replacement gate has no exact staged creation".into());
        }
        if read_intent(&self.first_turn_path())?.is_some() {
            return Err("Codex first turn was already attempted; no retry was sent".into());
        }
        let mut value = self.base();
        value["kind"] = json!("creation");
        value["threadId"] = json!(thread_id);
        write_private(&self.creation_path(), &value.to_string()).map_err(|error| error.to_string())
    }

    /// A saved ID is never, by itself, proof that the fresh gate finished. A direct native TUI
    /// resume must have been recorded after TaskAI created that exact pane.
    pub fn saved_thread_origin(&self, thread_id: &str) -> Result<SavedThreadOrigin, String> {
        if thread_id.is_empty() {
            return Err("Codex saved thread ID is empty".into());
        }
        let creation = read_intent(&self.creation_path())?;
        if let Some(value) = creation.as_ref().filter(|value| self.valid_base(value)) {
            if value["kind"] != "creation" || value["threadId"] != thread_id {
                return Err("Codex saved thread differs from its exact gate intent".into());
            }
            return Ok(SavedThreadOrigin::FreshGate);
        }
        let resume = read_intent(&self.resume_path())?
            .ok_or("Codex saved thread has no exact native resume evidence")?;
        if !self.valid_base(&resume)
            || resume["kind"] != "direct-resume"
            || resume["threadId"] != thread_id
        {
            return Err("Codex saved thread has no exact native resume evidence".into());
        }
        Ok(SavedThreadOrigin::DirectResume)
    }

    /// Called only by launch after creating a direct `codex resume <exact ID>` native pane.
    pub fn record_direct_resume(&self, thread_id: &str) -> Result<(), String> {
        if thread_id.is_empty() {
            return Err("Codex direct resume needs an exact thread ID".into());
        }
        let mut value = self.base();
        value["kind"] = json!("direct-resume");
        value["threadId"] = json!(thread_id);
        write_private(&self.resume_path(), &value.to_string()).map_err(|error| error.to_string())
    }

    /// A prior gate acceptance or an observed native TUI turn establishes human origin across
    /// bridge recreation. Missing/pending records do not unlock peer deliveries.
    pub fn human_established(&self, thread_id: &str) -> Result<bool, String> {
        if let Some(value) = read_intent(&self.native_human_path())? {
            if value["sessionRef"] != self.session_ref
                || value["kind"] != "native-human"
                || value["threadId"] != thread_id
                || value["observed"] != true
            {
                return Err(
                    "Codex native human-turn evidence differs from its saved thread".into(),
                );
            }
            return Ok(true);
        }
        let Some(first) = read_intent(&self.first_turn_path())? else {
            return Ok(false);
        };
        let Some(creation) = read_intent(&self.creation_path())? else {
            return Err("Codex first-turn evidence has no creation record".into());
        };
        if creation["sessionRef"] != self.session_ref
            || creation["kind"] != "creation"
            || creation["threadId"] != thread_id
            || first["kind"] != "first-turn"
            || first["threadId"] != thread_id
            || !["sessionRef", "paneId", "pid", "procStart"]
                .iter()
                .all(|field| first[*field] == creation[*field])
        {
            return Err("Codex first-turn evidence differs from its creation record".into());
        }
        first["accepted"]
            .as_bool()
            .ok_or("Codex first-turn evidence has invalid acceptance state".into())
    }

    pub fn record_native_human_turn(&self, thread_id: &str) -> Result<(), String> {
        if self.saved_thread_origin(thread_id)? != SavedThreadOrigin::DirectResume {
            return Err("Codex native human turn requires an exact direct resume".into());
        }
        if self.human_established(thread_id)? {
            return Ok(());
        }
        let mut value = self.base();
        value["kind"] = json!("native-human");
        value["threadId"] = json!(thread_id);
        value["observed"] = json!(true);
        create_once(&self.native_human_path(), &value)
    }

    /// Durably record the write intent before the one allowed thread/start.
    pub fn begin_creation(&self) -> Result<(), String> {
        if self.creation_state()? != CreationState::Missing {
            return Err(
                "Codex thread creation already attempted; automatic retry is blocked".into(),
            );
        }
        let mut value = self.base();
        value["kind"] = json!("creation");
        value["threadId"] = Value::Null;
        create_once(&self.creation_path(), &value)
    }

    /// Record the exact returned ID before publishing a registry mapping or
    /// letting the native gate receive an input-ready signal.
    pub fn record_created(&self, thread_id: &str) -> Result<(), String> {
        if thread_id.is_empty() || self.creation_state()? != CreationState::Pending {
            return Err("Codex creation cannot record this thread ID".into());
        }
        let mut value = self.base();
        value["kind"] = json!("creation");
        value["threadId"] = json!(thread_id);
        write_private(&self.creation_path(), &value.to_string()).map_err(|error| error.to_string())
    }

    /// The message body is never written here. A pending marker prevents a
    /// duplicate first human turn after a lost acknowledgment or process exit.
    pub fn begin_first_turn(&self, thread_id: &str) -> Result<(), String> {
        if thread_id.is_empty()
            || self.creation_state()? != CreationState::Created(thread_id.into())
        {
            return Err("Codex first turn has no exact saved thread".into());
        }
        let mut value = self.base();
        value["kind"] = json!("first-turn");
        value["threadId"] = json!(thread_id);
        value["accepted"] = json!(false);
        create_once(&self.first_turn_path(), &value)
    }

    pub fn first_turn_state(&self, thread_id: &str) -> Result<FirstTurnState, String> {
        let Some(value) = read_intent(&self.first_turn_path())? else {
            return Ok(FirstTurnState::Missing);
        };
        if !self.valid_base(&value)
            || value["kind"] != "first-turn"
            || value["threadId"] != thread_id
        {
            return Err("Codex first-turn intent has a different native identity".into());
        }
        match value["accepted"].as_bool() {
            Some(true) => Ok(FirstTurnState::Accepted),
            Some(false) => Ok(FirstTurnState::Pending),
            None => Err("Codex first-turn intent has invalid state".into()),
        }
    }

    pub fn record_first_turn_accepted(&self, thread_id: &str) -> Result<(), String> {
        if self.first_turn_state(thread_id)? != FirstTurnState::Pending {
            return Err("Codex first turn was not pending".into());
        }
        let mut value = self.base();
        value["kind"] = json!("first-turn");
        value["threadId"] = json!(thread_id);
        value["accepted"] = json!(true);
        write_private(&self.first_turn_path(), &value.to_string())
            .map_err(|error| error.to_string())
    }
}

fn same_native_base(left: &Value, right: &Value) -> bool {
    ["sessionRef", "paneId", "pid", "procStart"]
        .iter()
        .all(|field| left[*field] == right[*field])
}

fn read_intent(path: &Path) -> Result<Option<Value>, String> {
    let mut file = match fs::File::open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.to_string()),
    };
    if file.metadata().map_err(|error| error.to_string())?.len() > MAX_INTENT_BYTES {
        return Err("Codex intent is too large".into());
    }
    let mut bytes = Vec::new();
    file.read_to_end(&mut bytes)
        .map_err(|error| error.to_string())?;
    serde_json::from_slice(&bytes)
        .map(Some)
        .map_err(|_| "Codex intent is invalid JSON".into())
}

fn create_once(path: &Path, value: &Value) -> Result<(), String> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .map_err(|error| error.to_string())?;
    file.write_all(value.to_string().as_bytes())
        .and_then(|()| file.sync_all())
        .map_err(|error| error.to_string())
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use tempfile::TempDir;

    use super::{
        CodexIntents, CreationState, FirstTurnState, SavedCreationPlan, SavedThreadOrigin,
    };
    use crate::tmux::NativeIdentity;

    fn native() -> NativeIdentity {
        NativeIdentity {
            pane_id: "%9".into(),
            pid: 42,
            proc_start: "Fri Nov  6 12:00:00 2026".into(),
        }
    }

    #[test]
    fn creation_intent_blocks_second_thread_and_recovers_only_its_exact_id() {
        let root = TempDir::new().unwrap();
        let intents = CodexIntents::new(root.path(), "s1", native()).unwrap();
        assert_eq!(intents.creation_state().unwrap(), CreationState::Missing);
        intents.begin_creation().unwrap();
        assert_eq!(intents.creation_state().unwrap(), CreationState::Pending);
        assert!(intents.begin_creation().is_err());
        intents.record_created("owned-thread").unwrap();
        assert_eq!(
            intents.creation_state().unwrap(),
            CreationState::Created("owned-thread".into())
        );
        assert!(intents.record_created("replacement").is_err());
        let foreign = CodexIntents::new(
            root.path(),
            "s1",
            NativeIdentity {
                pid: 43,
                ..native()
            },
        )
        .unwrap();
        assert!(foreign.creation_state().is_err());
        let metadata = std::fs::metadata(intents.creation_path()).unwrap();
        assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
    }

    #[test]
    fn first_turn_intent_contains_no_message_and_cannot_repeat() {
        let root = TempDir::new().unwrap();
        let intents = CodexIntents::new(root.path(), "s1", native()).unwrap();
        intents.begin_creation().unwrap();
        intents.record_created("owned-thread").unwrap();
        intents.begin_first_turn("owned-thread").unwrap();
        assert_eq!(
            intents.first_turn_state("owned-thread").unwrap(),
            FirstTurnState::Pending
        );
        assert!(intents.begin_first_turn("owned-thread").is_err());
        intents.record_first_turn_accepted("owned-thread").unwrap();
        assert_eq!(
            intents.first_turn_state("owned-thread").unwrap(),
            FirstTurnState::Accepted
        );
        assert!(intents.record_first_turn_accepted("owned-thread").is_err());
        let bytes = std::fs::read(intents.first_turn_path()).unwrap();
        let record: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert!(record
            .as_object()
            .is_some_and(|fields| !fields.contains_key("text") && !fields.contains_key("prompt")));
        assert_eq!(
            std::fs::metadata(intents.first_turn_path())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
    }

    #[test]
    fn saved_mapping_needs_exact_gate_or_recorded_direct_resume() {
        let root = TempDir::new().unwrap();
        let gate = CodexIntents::new(root.path(), "s1", native()).unwrap();
        assert!(gate.saved_thread_origin("owned-thread").is_err());
        gate.begin_creation().unwrap();
        assert!(gate.saved_thread_origin("owned-thread").is_err());
        gate.record_created("owned-thread").unwrap();
        assert_eq!(
            gate.saved_thread_origin("owned-thread").unwrap(),
            SavedThreadOrigin::FreshGate
        );
        assert!(gate.saved_thread_origin("foreign-thread").is_err());
        let other = CodexIntents::new(
            root.path(),
            "s1",
            NativeIdentity {
                pid: 43,
                ..native()
            },
        )
        .unwrap();
        assert!(other.saved_thread_origin("owned-thread").is_err());
        other.record_direct_resume("owned-thread").unwrap();
        assert_eq!(
            other.saved_thread_origin("owned-thread").unwrap(),
            SavedThreadOrigin::DirectResume
        );
        assert!(!other.human_established("owned-thread").unwrap());
        assert!(other.saved_thread_origin("foreign-thread").is_err());
        std::fs::write(other.resume_path(), b"corrupt").unwrap();
        assert!(other.saved_thread_origin("owned-thread").is_err());
        std::fs::write(gate.creation_path(), b"corrupt").unwrap();
        assert!(gate.saved_thread_origin("owned-thread").is_err());
    }

    #[test]
    fn direct_resume_waits_for_native_human_turn_after_gate_eof_or_unknown_ack() {
        for pending in [false, true] {
            let root = TempDir::new().unwrap();
            let gate = CodexIntents::new(root.path(), "s1", native()).unwrap();
            gate.begin_creation().unwrap();
            gate.record_created("owned-thread").unwrap();
            if pending {
                gate.begin_first_turn("owned-thread").unwrap();
            }
            let resumed = CodexIntents::new(
                root.path(),
                "s1",
                NativeIdentity {
                    pid: 44,
                    ..native()
                },
            )
            .unwrap();
            resumed.record_direct_resume("owned-thread").unwrap();
            assert_eq!(
                resumed.saved_thread_origin("owned-thread").unwrap(),
                SavedThreadOrigin::DirectResume
            );
            assert!(!resumed.human_established("owned-thread").unwrap());
            resumed.record_native_human_turn("owned-thread").unwrap();
            assert!(resumed.human_established("owned-thread").unwrap());
        }
    }

    #[test]
    fn accepted_gate_turn_survives_exact_direct_resume_without_another_gate() {
        let root = TempDir::new().unwrap();
        let gate = CodexIntents::new(root.path(), "s1", native()).unwrap();
        gate.begin_creation().unwrap();
        gate.record_created("owned-thread").unwrap();
        gate.begin_first_turn("owned-thread").unwrap();
        gate.record_first_turn_accepted("owned-thread").unwrap();
        let resumed = CodexIntents::new(
            root.path(),
            "s1",
            NativeIdentity {
                pid: 45,
                ..native()
            },
        )
        .unwrap();
        resumed.record_direct_resume("owned-thread").unwrap();
        assert!(resumed.human_established("owned-thread").unwrap());
    }

    #[test]
    fn cold_reopen_adopts_only_a_saved_unattempted_gate_for_the_same_thread() {
        let root = TempDir::new().unwrap();
        let first = CodexIntents::new(root.path(), "s1", native()).unwrap();
        first.begin_creation().unwrap();
        first.record_created("owned-thread").unwrap();
        std::fs::write(
            first.gate_identity_path(),
            serde_json::json!({
                "sessionRef":"s1", "paneId":"%9", "pid":42,
                "procStart":"Fri Nov  6 12:00:00 2026"
            })
            .to_string(),
        )
        .unwrap();
        assert_eq!(
            CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").unwrap(),
            SavedCreationPlan::FirstHumanGate
        );
        CodexIntents::stage_gate_adoption(root.path(), "s1", "owned-thread").unwrap();
        let next_native = NativeIdentity {
            pane_id: "%10".into(),
            pid: 43,
            proc_start: "later".into(),
        };
        let second = CodexIntents::new(root.path(), "s1", next_native.clone()).unwrap();
        std::fs::write(
            second.gate_identity_path(),
            serde_json::json!({
                "sessionRef":"s1", "paneId":"%10", "pid":43, "procStart":"later"
            })
            .to_string(),
        )
        .unwrap();
        assert!(second.adoption_staged("owned-thread").unwrap());
        second.adopt_saved_creation("owned-thread").unwrap();
        assert_eq!(
            second.creation_state().unwrap(),
            CreationState::Created("owned-thread".into())
        );
        assert_eq!(
            second.first_turn_state("owned-thread").unwrap(),
            FirstTurnState::Missing
        );
        assert_eq!(
            second.saved_thread_origin("owned-thread").unwrap(),
            SavedThreadOrigin::FreshGate
        );
        assert!(second.saved_thread_origin("replacement-thread").is_err());
    }

    #[test]
    fn cold_reopen_rejects_uncertain_corrupt_or_replaced_creation_evidence() {
        let root = TempDir::new().unwrap();
        assert_eq!(
            CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").unwrap(),
            SavedCreationPlan::LegacyResume
        );
        let first = CodexIntents::new(root.path(), "s1", native()).unwrap();
        first.begin_creation().unwrap();
        first.record_created("owned-thread").unwrap();
        std::fs::write(
            first.gate_identity_path(),
            serde_json::json!({
                "sessionRef":"s1", "paneId":"%9", "pid":99,
                "procStart":"Fri Nov  6 12:00:00 2026"
            })
            .to_string(),
        )
        .unwrap();
        assert!(CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").is_err());
        assert!(CodexIntents::saved_creation_plan(root.path(), "s1", "other-thread").is_err());
        std::fs::write(
            first.gate_identity_path(),
            serde_json::json!({
                "sessionRef":"s1", "paneId":"%9", "pid":42,
                "procStart":"Fri Nov  6 12:00:00 2026"
            })
            .to_string(),
        )
        .unwrap();
        first.begin_first_turn("owned-thread").unwrap();
        assert!(CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").is_err());
        first.record_first_turn_accepted("owned-thread").unwrap();
        assert_eq!(
            CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").unwrap(),
            SavedCreationPlan::NativeResume
        );
        std::fs::write(first.creation_path(), b"corrupt").unwrap();
        assert!(CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").is_err());
        std::fs::remove_file(first.creation_path()).unwrap();
        assert!(
            CodexIntents::saved_creation_plan(root.path(), "s1", "owned-thread").is_err(),
            "a missing creation record beside M6 gate metadata is not a legacy resume"
        );
    }
}
