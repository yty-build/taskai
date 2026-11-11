// ABOUTME: Retired M7 fixture: a doctor-side scan of every live Claude card in the sessions directory.
// ABOUTME: Kept for reference only; the common Claude check is version/envelope, owned cards are M6's.

/// The cards of Claude sessions whose process still runs, read from `<pid>.json` only. A live
/// card outside the schema is a reason; keys are never opened; exited sessions are skipped.
fn live_cards(sessions_dir: &Path) -> (Vec<Card>, Vec<String>) {
    let mut cards = Vec::new();
    let mut reasons = Vec::new();
    let Ok(entries) = std::fs::read_dir(sessions_dir) else {
        return (cards, reasons);
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(pid) = name
            .to_str()
            .and_then(|name| name.strip_suffix(".json"))
            .and_then(|pid| pid.parse::<u32>().ok())
        else {
            continue;
        };
        if proc_start(pid).is_none() {
            continue;
        }
        let card = std::fs::read_to_string(entry.path())
            .map_err(|error| error.to_string())
            .and_then(|text| {
                serde_json::from_str::<Card>(&text).map_err(|error| error.to_string())
            });
        match card {
            Ok(card) => cards.push(card),
            Err(_) => reasons.push(format!(
                "the Claude card {pid}.json of a live process is not in the schema this adapter reads"
            )),
        }
    }
    cards.sort_by_key(|card| card.pid);
    reasons.sort();
    (cards, reasons)
}


#[cfg(test)]
mod retired_tests {
    // Expectation: Claude's gate passes only the tested version with every live card offering
    // protocol 1 and procStart; an exited session's card is ignored, a live card outside the
    // schema or without a socket fails closed, and a key file is never a reason. Why: spec §7.
    // Value: a hand-started older Claude cannot silently make the mirror misbehave.
    #[test]
    fn claude_gate_passes_only_the_tested_version_and_live_protocol_one_cards() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        let dir = &paths.claude_sessions_dir;
        assert!(matches!(
            messaging_check("claude", Some(CLAUDE_TESTED), &paths),
            Some(Check::Ready(detail)) if detail.contains("0 live card")
        ));
        let Some(Check::NotReady { reason, remedy }) =
            messaging_check("claude", Some("2.1.288"), &paths)
        else {
            panic!("an untested version must fail closed")
        };
        assert!(
            reason.contains("2.1.288 is not the tested 2.1.289"),
            "{reason}"
        );
        assert!(remedy.contains("re-verify"), "{remedy}");
        assert!(matches!(
            messaging_check("claude", None, &paths),
            Some(Check::NotReady { reason, .. }) if reason.contains("version is unknown")
        ));

        let me = std::process::id();
        let started = proc_start(me).unwrap();
        let card = |pid: u32, socket: Option<&str>| {
            let mut card = json!({
                "pid": pid, "sessionId": "ses_fixture", "startedAt": 1, "kind": "interactive",
                "procStart": started, "peerProtocol": 1, "peerFeatures": ["notify_idle"],
            });
            if let Some(socket) = socket {
                card["messagingSocketPath"] = json!(socket);
            }
            card.to_string()
        };
        std::fs::write(
            dir.join(format!("{me}.json")),
            card(me, Some("/tmp/cc-socks/1.sock")),
        )
        .unwrap();
        std::fs::write(dir.join("4000000.json"), card(4_000_000, None)).unwrap();
        std::fs::write(
            dir.join(format!("{me}.deadbeef.key")),
            "{\"peerToken\":\"SECRET\"}",
        )
        .unwrap();
        std::fs::set_permissions(
            dir.join(format!("{me}.deadbeef.key")),
            std::fs::Permissions::from_mode(0o000),
        )
        .unwrap();
        assert!(matches!(
            messaging_check("claude", Some(CLAUDE_TESTED), &paths),
            Some(Check::Ready(detail)) if detail.contains("1 live card")
        ));

        std::fs::write(dir.join(format!("{me}.json")), card(me, None)).unwrap();
        assert!(matches!(
            messaging_check("claude", Some(CLAUDE_TESTED), &paths),
            Some(Check::NotReady { reason, .. }) if reason.contains(&format!("pid {me} offers no peer protocol 1 socket"))
        ));

        std::fs::write(dir.join(format!("{me}.json")), "{not json").unwrap();
        assert!(matches!(
            messaging_check("claude", Some(CLAUDE_TESTED), &paths),
            Some(Check::NotReady { reason, .. }) if reason.contains(&format!("card {me}.json of a live process is not in the schema"))
        ));
        std::fs::set_permissions(
            dir.join(format!("{me}.deadbeef.key")),
            std::fs::Permissions::from_mode(0o600),
        )
        .unwrap();
    }

}
