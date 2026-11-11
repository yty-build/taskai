// ABOUTME: Reads the recent tail of a task's message log, one JSON line per mailbox event, into
// ABOUTME: rows for the message log view: local time, sender, recipient, outcome, body's first line.

use std::fs::OpenOptions;
use std::io::{ErrorKind, Read, Seek, SeekFrom};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

use chrono::{DateTime, Local};
use taskai_tui::view::{MessageLog, MessageRow};

/// How much of the end of the log is read. The view shows recent letters, so older history
/// stays on disk and the panel says it is there.
pub const TAIL_BYTES: u64 = 64 * 1024;

/// Where every mailbox of a task appends its log lines.
pub fn log_path(state_dir: &Path, task_ref: &str) -> PathBuf {
    state_dir.join("messages").join(format!("{task_ref}.jsonl"))
}

/// The readable records in the log's recent tail, oldest first. A missing log is an empty one.
/// Any other failure to read is reported as unavailable, never as empty. The tail is read as one
/// bounded snapshot of at most `TAIL_BYTES`; a record cut by its start is left out unless the
/// tail begins exactly on a record boundary, and the final record is left out until its newline
/// has landed, because the producer writes a record and its newline together. Records are
/// decoded one by one, so one that is not valid UTF-8 or not a JSON object hides only itself.
pub fn read_log(path: &Path) -> MessageLog {
    let unavailable = |error: std::io::Error| MessageLog {
        unavailable: Some(error.to_string()),
        ..Default::default()
    };
    let mut file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == ErrorKind::NotFound => return MessageLog::default(),
        Err(error) => return unavailable(error),
    };
    let len = match file.metadata() {
        Ok(metadata) if metadata.is_file() => metadata.len(),
        Ok(_) => {
            return unavailable(std::io::Error::new(
                ErrorKind::InvalidData,
                "message log is not a regular file",
            ));
        }
        Err(error) => return unavailable(error),
    };
    let truncated = len > TAIL_BYTES;
    let start = len.saturating_sub(TAIL_BYTES);
    let mut first_complete = true;
    if truncated {
        let mut before = [0u8; 1];
        if let Err(error) = file
            .seek(SeekFrom::Start(start - 1))
            .and_then(|_| file.read_exact(&mut before))
        {
            return unavailable(error);
        }
        first_complete = before[0] == b'\n';
    }
    let mut bytes = Vec::with_capacity((len - start) as usize);
    if let Err(error) = (&mut file).take(len - start).read_to_end(&mut bytes) {
        return unavailable(error);
    }
    let complete_end = bytes
        .iter()
        .rposition(|byte| *byte == b'\n')
        .map_or(0, |at| at + 1);
    let mut records = bytes[..complete_end].split(|byte| *byte == b'\n');
    if truncated && !first_complete {
        records.next();
    }
    let rows = records
        .filter(|record| !record.is_empty())
        .filter_map(|record| std::str::from_utf8(record).ok())
        .filter_map(parse_line)
        .collect();
    MessageLog {
        rows,
        truncated,
        unavailable: None,
    }
}

fn parse_line(line: &str) -> Option<MessageRow> {
    let value: serde_json::Value = serde_json::from_str(line).ok()?;
    let object = value.as_object()?;
    let field = |name: &str| {
        object
            .get(name)
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string()
    };
    let note = field("note");
    let body = field("body");
    let text = body
        .lines()
        .find(|l| !l.trim().is_empty())
        .map(str::to_string)
        .unwrap_or_else(|| note.clone());
    Some(MessageRow {
        ts: local_clock(&field("ts")),
        from: field("from"),
        to: field("to"),
        outcome: field("outcome"),
        text,
        mismatch: note.contains("mode mismatch"),
    })
}

/// The local time of day of an RFC 3339 timestamp; anything else is shown as written.
fn local_clock(ts: &str) -> String {
    DateTime::parse_from_rfc3339(ts)
        .map(|t| t.with_timezone(&Local).format("%H:%M:%S").to_string())
        .unwrap_or_else(|_| ts.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::Write;

    fn log_in(dir: &Path) -> PathBuf {
        let path = log_path(dir, "t1");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        path
    }

    fn record(i: usize) -> String {
        format!(
            r#"{{"ts":"2026-11-06T01:02:03Z","msg_id":"m{i}","from":"claude-lead","to":"codex-delivery","direction":"out","outcome":"delivered","body":"letter-{i:04}"}}"#
        ) + "\n"
    }

    // Expectation: letters, receipts and local notes come back in file order with the body's
    // first non-empty line as text, the note as text when there is no body, the mismatch flag
    // taken from the note, and unreadable lines skipped. Why: the log is the person's only
    // record of who asked whom for what. Value: one bad line never hides the conversation.
    #[test]
    fn reads_letters_receipts_and_notes_in_order() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        std::fs::write(
            &path,
            concat!(
                r#"{"ts":"2026-11-06T01:02:03Z","msg_id":"m1","from":"claude-lead","to":"codex-delivery","direction":"out","outcome":"delivered","body":"\nLand the branch.\nSecond line."}"#,
                "\n",
                r#"{"ts":"2026-11-06T01:02:04Z","msg_id":"m1","from":"claude-lead","to":"codex-delivery","direction":"in","outcome":"admitted","note":"hop chain 0; mode mismatch: from-mode bypass, recipient prompting","body":"Land the branch."}"#,
                "\n",
                "not json at all\n",
                "[1, 2, 3]\n",
                r#"{"ts":"2026-11-06T01:03:00Z","msg_id":null,"from":"codex-delivery","to":"codex-delivery","direction":"local","outcome":"status not published","note":"permission denied"}"#,
                "\n",
            ),
        )
        .unwrap();
        let log = read_log(&path);
        assert!(!log.truncated);
        assert_eq!(log.unavailable, None);
        let rows = log.rows;
        assert_eq!(rows.len(), 3, "{rows:?}");
        assert_eq!(rows[0].from, "claude-lead");
        assert_eq!(rows[0].to, "codex-delivery");
        assert_eq!(rows[0].outcome, "delivered");
        assert_eq!(rows[0].text, "Land the branch.");
        assert!(!rows[0].mismatch);
        assert!(rows[1].mismatch);
        assert_eq!(rows[1].outcome, "admitted");
        assert_eq!(rows[2].outcome, "status not published");
        assert_eq!(rows[2].text, "permission denied");
        assert!(!rows[2].mismatch);
    }

    // Expectation: a task without a log yet reads as no messages, with nothing unavailable.
    // Why: the view opens before the first letter is sent. Value: the normal empty case is
    // never shown as a fault.
    #[test]
    fn missing_log_is_empty() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(read_log(&log_path(dir.path(), "t9")), MessageLog::default());
    }

    // A pipe is not a log. Bound the old blocking open in a child so this regression can fail
    // and clean up without freezing either the UI or the surrounding test process.
    #[test]
    fn fifo_log_reports_unavailable_without_blocking() {
        use std::{
            ffi::CString,
            os::unix::ffi::OsStrExt,
            process::{Child, Command, Stdio},
            time::{Duration, Instant},
        };
        const CHILD_PATH: &str = "TASKAI_MESSAGE_FIFO_TEST_PATH";
        if let Some(path) = std::env::var_os(CHILD_PATH) {
            let path = PathBuf::from(path);
            std::fs::write(path.with_extension("opening"), b"ready").unwrap();
            let log = read_log(&path);
            assert!(log.rows.is_empty());
            assert!(log
                .unavailable
                .is_some_and(|reason| reason.contains("not a regular file")));
            return;
        }

        struct OwnedChild(Child);
        impl Drop for OwnedChild {
            fn drop(&mut self) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }

        let root = tempfile::tempdir().unwrap();
        let path = log_in(root.path());
        let name = CString::new(path.as_os_str().as_bytes()).unwrap();
        // SAFETY: name is a live NUL-terminated path and mkfifo retains no pointer.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "messages::tests::fifo_log_reports_unavailable_without_blocking",
            ])
            .env(CHILD_PATH, &path)
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        taskai_core::mail::strip_messaging_env(&mut command);
        let mut child = OwnedChild(command.spawn().unwrap());
        let startup_deadline = Instant::now() + Duration::from_secs(5);
        while !path.with_extension("opening").exists() {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "private child exited before opening"
            );
            assert!(
                Instant::now() < startup_deadline,
                "private child did not reach the log"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        let finish_deadline = Instant::now() + Duration::from_secs(2);
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(
                    status.success(),
                    "private child did not report an unavailable log"
                );
                break;
            }
            assert!(
                Instant::now() < finish_deadline,
                "message-log reader blocked on a FIFO"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    // Expectation: a record that is not valid UTF-8 hides only itself; the valid records
    // before and after it still show. Why: one corrupt write must not blank the whole log.
    // Value: the conversation stays readable around a bad byte.
    #[test]
    fn invalid_bytes_hide_only_their_own_record() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        let mut file = File::create(&path).unwrap();
        file.write_all(record(1).as_bytes()).unwrap();
        file.write_all(br#"{"ts":"2026-11-06T01:02:03Z","from":"x"#)
            .unwrap();
        file.write_all(&[0xff, 0xfe]).unwrap();
        file.write_all(b"\"}\n").unwrap();
        file.write_all(record(3).as_bytes()).unwrap();
        drop(file);
        let log = read_log(&path);
        assert_eq!(log.unavailable, None);
        let texts: Vec<_> = log.rows.iter().map(|r| r.text.as_str()).collect();
        assert_eq!(texts, ["letter-0001", "letter-0003"]);
    }

    // Expectation: a log that exists but cannot be read reports the reason and no rows.
    // Why: an unreadable log is a fault, and an empty log is the normal start; the person
    // must never see a false "No messages yet". Value: the fault is visible where they look.
    #[test]
    fn unreadable_log_reports_the_reason_instead_of_emptiness() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        std::fs::create_dir(&path).unwrap();
        let log = read_log(&path);
        assert!(log.rows.is_empty());
        assert!(
            log.unavailable.as_deref().is_some_and(|r| !r.is_empty()),
            "{log:?}"
        );
    }

    // Expectation: a log larger than the tail bound yields only the complete records inside
    // the bound, oldest first, ending with the newest, and says older history exists. Why:
    // the view is a recent log, and reading everything would grow with the task forever.
    // Value: bounded memory, no half record, and an honest marker.
    #[test]
    fn large_log_reads_only_its_tail_and_marks_it() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        let total = 1500;
        let text: String = (1..=total).map(record).collect();
        assert!(text.len() as u64 > TAIL_BYTES);
        std::fs::write(&path, &text).unwrap();
        let log = read_log(&path);
        assert!(log.truncated);
        assert_eq!(log.unavailable, None);
        let rows = log.rows;
        assert!(!rows.is_empty() && rows.len() < total, "{}", rows.len());
        let first = total - rows.len() + 1;
        assert_eq!(rows[0].text, format!("letter-{first:04}"));
        assert_eq!(rows.last().unwrap().text, format!("letter-{total:04}"));
        let record_len = record(1).len();
        assert!(
            rows.len() * record_len <= TAIL_BYTES as usize,
            "{} rows of {record_len} bytes exceed the bound",
            rows.len()
        );
    }

    /// A record padded inside its body to exactly `len` bytes including the newline.
    fn record_of_len(i: usize, len: usize) -> String {
        let bare = record(i);
        let pad = len.checked_sub(bare.len()).expect("record fits the length");
        let (head, tail) = bare.split_at(bare.rfind('"').unwrap());
        format!("{head}{}{tail}", " ".repeat(pad))
    }

    // Expectation: when the tail begins exactly where a record begins, that record is kept;
    // when it begins inside a record, only that partial record is dropped. Why: the byte bound
    // must not throw away a complete letter for alignment's sake. Value: the view starts on a
    // complete record whichever way the boundary falls.
    #[test]
    fn tail_starting_on_a_record_boundary_keeps_its_first_record() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        let len = 256;
        let count = TAIL_BYTES as usize / len;
        let mut aligned = record_of_len(1, 300);
        for i in 2..=count + 1 {
            aligned.push_str(&record_of_len(i, len));
        }
        std::fs::write(&path, &aligned).unwrap();
        let log = read_log(&path);
        assert!(log.truncated);
        assert_eq!(
            log.rows.len(),
            count,
            "every record inside the tail is complete"
        );
        assert_eq!(log.rows[0].text.trim(), "letter-0002");

        let mut unaligned = aligned.clone();
        unaligned.push_str(&record_of_len(count + 2, 200));
        std::fs::write(&path, &unaligned).unwrap();
        let log = read_log(&path);
        assert!(log.truncated);
        assert_eq!(
            log.rows.len(),
            count,
            "the record cut by the boundary is dropped and the appended one counted"
        );
        assert_eq!(log.rows[0].text.trim(), "letter-0003");
        assert_eq!(
            log.rows.last().unwrap().text.trim(),
            format!("letter-{:04}", count + 2)
        );
    }

    // Expectation: a final record that is complete JSON but has no newline yet is omitted, and
    // appears once its newline lands. Why: the producer writes a record and its newline in one
    // write, so a missing newline means the write is still in flight. Value: the view never
    // shows a record the log has not finished.
    #[test]
    fn final_record_without_newline_waits_for_it() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        let second = record(2);
        std::fs::write(&path, format!("{}{}", record(1), second.trim_end())).unwrap();
        let texts: Vec<_> = read_log(&path).rows.into_iter().map(|r| r.text).collect();
        assert_eq!(texts, ["letter-0001"]);
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(&path)
            .unwrap();
        file.write_all(b"\n").unwrap();
        drop(file);
        let texts: Vec<_> = read_log(&path).rows.into_iter().map(|r| r.text).collect();
        assert_eq!(texts, ["letter-0001", "letter-0002"]);
    }

    // Expectation: a final record without its newline, still being written, is left out and
    // the complete records before it show. Why: a bridge appends while the person reads.
    // Value: no garbage row, no missing history.
    #[test]
    fn truncated_final_record_is_skipped() {
        let dir = tempfile::tempdir().unwrap();
        let path = log_in(dir.path());
        std::fs::write(
            &path,
            format!(
                "{}{}",
                record(1),
                r#"{"ts":"2026-11-06T01:02:03Z","msg_id":"m2","from":"claude-lead","to":"codex-de"#
            ),
        )
        .unwrap();
        let log = read_log(&path);
        assert!(!log.truncated);
        assert_eq!(log.unavailable, None);
        let texts: Vec<_> = log.rows.iter().map(|r| r.text.as_str()).collect();
        assert_eq!(texts, ["letter-0001"]);
    }

    // Expectation: timestamps show as the local time of day, and a value that is not a
    // timestamp shows as written. Why: the person compares the log with the clock in the
    // masthead, which is local. Value: no mental UTC arithmetic while reading a hang.
    #[test]
    fn clock_is_local_time_of_day() {
        let ts = "2026-11-06T01:02:03Z";
        let want = DateTime::parse_from_rfc3339(ts)
            .unwrap()
            .with_timezone(&Local)
            .format("%H:%M:%S")
            .to_string();
        assert_eq!(local_clock(ts), want);
        assert_eq!(local_clock("17:02"), "17:02");
    }
}
