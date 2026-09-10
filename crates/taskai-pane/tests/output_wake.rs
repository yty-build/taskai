// ABOUTME: Output notifications wake a host independently of keyboard input.
// ABOUTME: Bounded/coalesced subscriptions never backpressure the PTY reader.

use std::sync::mpsc::{sync_channel, TryRecvError};
use std::time::{Duration, Instant};

use taskai_pane::Pane;

fn wait_until(mut condition: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(5);
    while !condition() {
        assert!(
            Instant::now() < deadline,
            "PTY condition did not become ready"
        );
        std::thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn output_alone_wakes_after_parsing_and_late_subscribers_observe_it() {
    let temp = tempfile::tempdir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &["-c", "while [ ! -e emit-now ]; do sleep 0.01; done; printf OUTPUT_READY; while [ ! -e exit-now ]; do sleep 0.01; done"],
        temp.path(), 24, 80, &[],
    ).unwrap();
    let (tx, rx) = sync_channel(1);
    pane.set_output_notifier(tx);
    rx.recv_timeout(Duration::from_secs(5)).unwrap(); // subscription wake
    assert_eq!(pane.output_revision(), 0);
    assert!(!pane.has_output());
    // The stimulus is external to the terminal: no keyboard/PTY input wakes the host.
    std::fs::write(temp.path().join("emit-now"), "").unwrap();
    rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(pane.output_revision() > 0);
    assert!(pane.has_output());
    assert!(pane.text().contains("OUTPUT_READY"));

    let revision = pane.output_revision();
    let (replacement, later) = sync_channel(1);
    pane.set_output_notifier(replacement);
    later.recv_timeout(Duration::from_secs(5)).unwrap();
    assert_eq!(
        pane.output_revision(),
        revision,
        "subscription is not new output"
    );
    assert!(pane.text().contains("OUTPUT_READY"));
    wait_until(|| rx.try_recv() == Err(TryRecvError::Disconnected));

    pane.clear_output_notifier();
    std::fs::write(temp.path().join("exit-now"), "").unwrap();
    wait_until(|| pane.output_revision() > revision);
    wait_until(|| later.try_recv() == Err(TryRecvError::Disconnected));
}

#[test]
fn silent_eof_wakes_without_claiming_visible_output() {
    let temp = tempfile::tempdir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &["-c", "while [ ! -e exit-now ]; do sleep 0.01; done"],
        temp.path(),
        24,
        80,
        &[],
    )
    .unwrap();
    let (tx, rx) = sync_channel(1);
    pane.set_output_notifier(tx);
    rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert_eq!(pane.output_revision(), 0);
    std::fs::write(temp.path().join("exit-now"), "").unwrap();
    rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(pane.output_revision() > 0);
    assert!(!pane.has_output());
}

#[test]
fn full_and_disconnected_notifiers_never_block_reader_progress() {
    let temp = tempfile::tempdir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &["-c", "while [ ! -e emit-now ]; do sleep 0.01; done; i=0; while [ \"$i\" -lt 2000 ]; do printf 'output-%s\\n' \"$i\"; i=$((i+1)); done; printf FULL_CHANNEL_DONE; while [ ! -e second-now ]; do sleep 0.01; done; printf DISCONNECTED_DONE; while [ ! -e exit-now ]; do sleep 0.01; done"],
        temp.path(), 24, 80, &[],
    ).unwrap();
    let (tx, rx) = sync_channel(1);
    pane.set_output_notifier(tx); // deliberately leave the capacity-one channel full
    std::fs::write(temp.path().join("emit-now"), "").unwrap();
    wait_until(|| pane.text().contains("FULL_CHANNEL_DONE"));
    assert!(
        pane.output_revision() > 1,
        "fixture should span multiple reader chunks"
    );
    rx.try_recv().unwrap();
    drop(rx);
    let before = pane.output_revision();
    std::fs::write(temp.path().join("second-now"), "").unwrap();
    wait_until(|| pane.text().contains("DISCONNECTED_DONE") && pane.output_revision() > before);
}
