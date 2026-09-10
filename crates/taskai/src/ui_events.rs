// ABOUTME: In-process host input and coalesced wakeups for the terminal UI.
// ABOUTME: PTY output and input wake the same loop; neither waits for a UI tick.

use std::io;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender, TryRecvError};
use std::sync::Arc;
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

use crossterm::event::{self, Event};

pub const UI_TICK: Duration = Duration::from_millis(100);
// Coalesce bursts to a display-sized interval, avoiding floods of intermediate
// full-row updates that trigger tmux output backoff. The first frame after idle
// is immediate; input forwarding is never held for this render deadline.
pub const FRAME_INTERVAL: Duration = Duration::from_millis(16);
pub const INPUT_BATCH: usize = 64;
pub const INPUT_BUDGET: Duration = Duration::from_millis(2);
const INPUT_STOP_CHECK: Duration = Duration::from_millis(20);

pub struct InputPump {
    events: Option<Receiver<io::Result<Event>>>,
    stop: Arc<AtomicBool>,
    worker: Option<JoinHandle<()>>,
}

impl InputPump {
    pub fn start(wake: SyncSender<()>) -> io::Result<Self> {
        Self::with_reader(wake, |timeout| {
            if event::poll(timeout)? {
                event::read().map(Some)
            } else {
                Ok(None)
            }
        })
    }

    fn with_reader(
        wake: SyncSender<()>,
        mut read: impl FnMut(Duration) -> io::Result<Option<Event>> + Send + 'static,
    ) -> io::Result<Self> {
        // Bound memory without dropping/reordering keyboard or mouse events.
        // Only the reader can wait on this queue, never the PTY output thread.
        let (tx, events) = sync_channel(256);
        let stop = Arc::new(AtomicBool::new(false));
        let stopped = Arc::clone(&stop);
        let worker = thread::Builder::new()
            .name("taskai-input".into())
            .spawn(move || {
                while !stopped.load(Ordering::Acquire) {
                    let result = match read(INPUT_STOP_CHECK) {
                        Ok(Some(event)) => Ok(event),
                        Ok(None) => continue,
                        Err(error) => Err(error),
                    };
                    if stopped.load(Ordering::Acquire) {
                        break;
                    }
                    let failed = result.is_err();
                    if tx.send(result).is_err() {
                        break;
                    }
                    let _ = wake.try_send(());
                    if failed {
                        break;
                    }
                }
            })?;
        Ok(Self {
            events: Some(events),
            stop,
            worker: Some(worker),
        })
    }

    pub fn try_event(&self) -> io::Result<Option<Event>> {
        match self.events.as_ref().unwrap().try_recv() {
            Ok(event) => event.map(Some),
            Err(TryRecvError::Empty) => Ok(None),
            Err(TryRecvError::Disconnected) => Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "terminal input reader stopped",
            )),
        }
    }
}

impl Drop for InputPump {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        // Disconnect a full input queue before joining its potentially blocked
        // producer. No background reader may outlive terminal restoration.
        self.events.take();
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

/// A render deadline, independent of input delivery and animation/housekeeping.
pub struct FramePacer {
    next: Instant,
}

impl FramePacer {
    pub fn new(now: Instant) -> Self {
        Self { next: now }
    }

    pub fn due(&self, now: Instant) -> bool {
        now >= self.next
    }

    pub fn committed(&mut self, now: Instant) {
        self.next = now + FRAME_INTERVAL;
    }

    pub fn wait(&self, now: Instant) -> Duration {
        self.next.saturating_duration_since(now)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

    #[test]
    fn first_and_post_idle_frames_are_immediate_bursts_are_bounded() {
        let now = Instant::now();
        let mut pacer = FramePacer::new(now);
        assert!(pacer.due(now));
        pacer.committed(now);
        assert!(!pacer.due(now + Duration::from_millis(1)));
        assert_eq!(pacer.wait(now), FRAME_INTERVAL);
        assert!(pacer.due(now + FRAME_INTERVAL));
        assert!(pacer.due(now + UI_TICK));
        assert!(FRAME_INTERVAL < UI_TICK);
    }

    #[test]
    fn coalesced_wakes_do_not_drop_or_reorder_input() {
        let (wake, wakes) = sync_channel(1);
        let (send, incoming) = sync_channel(512);
        let pump =
            InputPump::with_reader(wake, move |timeout| Ok(incoming.recv_timeout(timeout).ok()))
                .unwrap();
        for index in 0..400 {
            send.send(Event::Key(KeyEvent::new(
                KeyCode::Char(char::from(b'a' + (index % 26) as u8)),
                KeyModifiers::NONE,
            )))
            .unwrap();
        }
        wakes.recv_timeout(Duration::from_secs(2)).unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut received = 0;
        while received < 400 {
            if let Some(Event::Key(key)) = pump.try_event().unwrap() {
                assert_eq!(
                    key.code,
                    KeyCode::Char(char::from(b'a' + (received % 26) as u8))
                );
                received += 1;
            } else {
                assert!(Instant::now() < deadline);
                thread::yield_now();
            }
        }
    }

    #[test]
    fn shutdown_disconnects_a_full_queue_and_joins_reader() {
        let (wake, _wakes) = sync_channel(1);
        let reads = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let worker_reads = Arc::clone(&reads);
        let pump = InputPump::with_reader(wake, move |_| {
            worker_reads.fetch_add(1, Ordering::Release);
            Ok(Some(Event::Key(KeyEvent::new(
                KeyCode::Null,
                KeyModifiers::NONE,
            ))))
        })
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        while reads.load(Ordering::Acquire) < 257 {
            assert!(Instant::now() < deadline);
            thread::yield_now();
        }
        // Queue capacity is 256: the 257th send cannot finish until Drop
        // disconnects its receiver. Joining before disconnecting would hang.
        drop(pump);
    }

    #[test]
    fn input_errors_wake_and_reach_the_ui() {
        let (wake, wakes) = sync_channel(1);
        let pump =
            InputPump::with_reader(wake, |_| Err(io::Error::other("input fixture"))).unwrap();
        wakes.recv_timeout(Duration::from_secs(2)).unwrap();
        assert_eq!(pump.try_event().unwrap_err().to_string(), "input fixture");
    }
}
