// ABOUTME: Working-pane conduit — portable-pty spawn/resize/io and a vt100 grid
// ABOUTME: blitted into the TUI so the real CLI renders exactly as itself.

pub mod grid;

use std::io::{Read, Write};
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc::SyncSender;
use std::sync::{Arc, Mutex};

use portable_pty::{native_pty_system, Child, CommandBuilder, MasterPty, PtySize};

use grid::{Cell, CursorState, Grid};
pub use grid::{MouseProtocolEncoding, MouseProtocolMode, MouseState};

pub type Error = Box<dyn std::error::Error + Send + Sync>;

pub struct PaneSnapshot {
    pub cells: Vec<Vec<Cell>>,
    pub cursor: CursorState,
    pub bracketed_paste: bool,
    pub mouse: MouseState,
}

#[derive(Default)]
struct OutputSignal {
    revision: AtomicU64,
    notifier: Mutex<Option<SyncSender<()>>>,
}

impl OutputSignal {
    fn changed(&self) {
        self.revision.fetch_add(1, Ordering::Release);
        let notifier = self.notifier.lock().unwrap().clone();
        if let Some(notifier) = notifier {
            // Full means a redraw is already pending. A closed application is
            // equally harmless: the terminal reader must never wait for it.
            let _ = notifier.try_send(());
        }
    }
}

/// A real process in a real pty; its output feeds a terminal grid the TUI blits.
pub struct Pane {
    master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    child: Box<dyn Child + Send + Sync>,
    grid: Arc<Mutex<Grid>>,
    has_output: Arc<AtomicBool>,
    output: Arc<OutputSignal>,
}

impl Pane {
    pub fn spawn(
        prog: &str,
        args: &[&str],
        cwd: &Path,
        rows: u16,
        cols: u16,
        envs: &[(&str, &str)],
    ) -> Result<Pane, Error> {
        let pty = native_pty_system().openpty(PtySize {
            rows,
            cols,
            pixel_width: 0,
            pixel_height: 0,
        })?;
        let mut cmd = CommandBuilder::new(prog);
        cmd.args(args);
        cmd.cwd(cwd);
        cmd.env("COLORTERM", "truecolor");
        cmd.env("TERM", "xterm-256color");
        for (k, v) in envs {
            cmd.env(k, v);
        }
        let child = pty.slave.spawn_command(cmd)?;
        drop(pty.slave);
        let writer = pty.master.take_writer()?;
        let mut reader = pty.master.try_clone_reader()?;
        let grid = Arc::new(Mutex::new(Grid::new(rows as usize, cols as usize)));
        let g = Arc::clone(&grid);
        let has_output = Arc::new(AtomicBool::new(false));
        let output_received = Arc::clone(&has_output);
        let output = Arc::new(OutputSignal::default());
        let output_signal = Arc::clone(&output);
        std::thread::spawn(move || {
            let mut buf = [0u8; 8192];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) => break,
                    Ok(n) => {
                        g.lock().unwrap().process(&buf[..n]);
                        output_received.store(true, Ordering::Release);
                        output_signal.changed();
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(_) => break,
                }
            }
            // EOF (including a PTY's EIO end-of-stream) is observable even if
            // the child never printed anything. Wake lifecycle/death checks too.
            output_signal.changed();
        });
        Ok(Pane {
            master: pty.master,
            writer,
            child,
            grid,
            has_output,
            output,
        })
    }

    pub fn write_bytes(&mut self, bytes: &[u8]) -> Result<(), Error> {
        self.writer.write_all(bytes)?;
        self.writer.flush()?;
        Ok(())
    }

    pub fn resize(&mut self, rows: u16, cols: u16) -> Result<(), Error> {
        // Hold the parser lock across SIGWINCH and the parser resize. Otherwise
        // a fast child can redraw at its new width while we parse the bytes at
        // the old width, permanently wrapping/corrupting the frame.
        let mut grid = self.grid.lock().unwrap();
        if grid.size() == (rows as usize, cols as usize) {
            return Ok(());
        }
        self.master.resize(PtySize {
            rows,
            cols,
            pixel_width: 0,
            pixel_height: 0,
        })?;
        grid.set_size(rows as usize, cols as usize);
        Ok(())
    }

    pub fn size(&self) -> (usize, usize) {
        self.grid.lock().unwrap().size()
    }

    pub fn cells(&self) -> Vec<Vec<Cell>> {
        self.grid.lock().unwrap().cells()
    }

    /// Current mouse protocol and bounds without copying the rendered cells.
    pub fn mouse_state(&self) -> MouseState {
        self.grid.lock().unwrap().mouse()
    }

    /// Capture cells and cursor atomically from one parser state so the host
    /// never displays a cursor from a different child frame.
    pub fn snapshot(&self) -> PaneSnapshot {
        // Clone vt100's inline-cell row buffers while locked. Per-cell String
        // allocation/color conversion happens outside the reader's critical path.
        let grid = self.grid.lock().unwrap().snapshot();
        PaneSnapshot {
            cells: grid.cells(),
            cursor: grid.cursor(),
            bracketed_paste: grid.bracketed_paste(),
            mouse: grid.mouse(),
        }
    }

    pub fn text(&self) -> String {
        self.grid.lock().unwrap().text()
    }

    /// Whether the child has emitted terminal output. An ANSI-only initial
    /// frame counts: a ready terminal can be completely blank until input.
    pub fn has_output(&self) -> bool {
        self.has_output.load(Ordering::Acquire)
    }

    /// Subscribe to coalesced output/lifecycle wakes. Use a bounded channel of
    /// capacity one shared with the host event loop. Subscription itself wakes
    /// once so output that arrived before or during subscription cannot be lost.
    /// No callback or application object is retained, and sending never blocks.
    pub fn set_output_notifier(&self, notifier: SyncSender<()>) {
        *self.output.notifier.lock().unwrap() = Some(notifier.clone());
        let _ = notifier.try_send(());
    }

    /// Stop waking this subscriber while allowing output parsing to continue.
    /// A wake already queued or in flight may still arrive; consumers should
    /// check the revision of their currently displayed pane before rendering.
    pub fn clear_output_notifier(&self) {
        *self.output.notifier.lock().unwrap() = None;
    }

    /// Monotonic generation of parsed output and end-of-stream. Read this before
    /// taking a snapshot when caching a frame, so later output remains dirty.
    /// This counts reader updates, not terminal bytes or rendered frames.
    pub fn output_revision(&self) -> u64 {
        self.output.revision.load(Ordering::Acquire)
    }

    pub fn is_alive(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }
}

impl Drop for Pane {
    fn drop(&mut self) {
        if self.child.kill().is_ok() {
            // portable-pty escalates a stubborn viewer to SIGKILL, but does not
            // reap after that last signal. Do not accumulate zombie attach
            // clients when users repeatedly leave/reopen a terminal.
            let _ = self.child.wait();
        }
    }
}

#[cfg(test)]
mod output_signal_tests {
    use super::*;
    use std::sync::mpsc::{sync_channel, TryRecvError};

    #[test]
    fn notifications_coalesce_and_disconnection_does_not_block_revisions() {
        let (tx, rx) = sync_channel(1);
        let signal = OutputSignal {
            notifier: Mutex::new(Some(tx)),
            ..OutputSignal::default()
        };
        for _ in 0..10_000 {
            signal.changed();
        }
        assert_eq!(signal.revision.load(Ordering::Acquire), 10_000);
        rx.try_recv().unwrap();
        assert_eq!(rx.try_recv(), Err(TryRecvError::Empty));
        drop(rx);
        signal.changed();
        assert_eq!(signal.revision.load(Ordering::Acquire), 10_001);
    }
}
