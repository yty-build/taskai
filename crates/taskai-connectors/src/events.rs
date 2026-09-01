// ABOUTME: Hook event stream — reads appended JSONL hook payloads (SessionStart, Stop)
// ABOUTME: from a given byte offset with a timeout; the connector's terminal-signal read.

use std::path::Path;
use std::time::{Duration, Instant};

use crate::Error;

#[derive(Clone, Debug)]
pub struct Event {
    pub kind: String,
    pub session_id: Option<String>,
    pub offset: u64,
    pub raw: serde_json::Value,
}

/// Returns the first event of `kind` (optionally for one session) appended at or after
/// byte `from`, polling until `timeout`.
pub fn wait_for_event(
    path: &Path,
    kind: &str,
    session_id: Option<&str>,
    from: u64,
    timeout: Duration,
) -> Result<Event, Error> {
    let deadline = Instant::now() + timeout;
    loop {
        if path.exists() {
            let data = std::fs::read_to_string(path)?;
            let mut offset = 0u64;
            for line in data.split_inclusive('\n') {
                let start = offset;
                offset += line.len() as u64;
                if start < from {
                    continue;
                }
                let line = line.trim();
                if line.is_empty() {
                    continue;
                }
                if let Ok(v) = serde_json::from_str::<serde_json::Value>(line) {
                    let k = v["hook_event_name"].as_str().unwrap_or("");
                    let sid = v["session_id"].as_str().map(str::to_string);
                    if k == kind && session_id.is_none_or(|want| sid.as_deref() == Some(want)) {
                        return Ok(Event { kind: k.into(), session_id: sid, offset, raw: v });
                    }
                }
            }
        }
        if Instant::now() >= deadline {
            return Err(Error::Timeout(format!("no {kind} event in {path:?}")));
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}
