// ABOUTME: Working-pane conduit — portable-pty spawn/resize/io and a vt100 grid
// ABOUTME: blitted into the TUI so the real CLI renders exactly as itself.

pub mod grid;

use std::io::{Read, Write};
use std::path::Path;
use std::sync::{Arc, Mutex};

use portable_pty::{native_pty_system, Child, CommandBuilder, MasterPty, PtySize};

use grid::{Cell, Grid};

pub type Error = Box<dyn std::error::Error + Send + Sync>;

/// A real process in a real pty; its output feeds a terminal grid the TUI blits.
pub struct Pane {
    master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    child: Box<dyn Child + Send + Sync>,
    grid: Arc<Mutex<Grid>>,
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
        let pty = native_pty_system().openpty(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })?;
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
        std::thread::spawn(move || {
            let mut buf = [0u8; 8192];
            while let Ok(n) = reader.read(&mut buf) {
                if n == 0 {
                    break;
                }
                g.lock().unwrap().process(&buf[..n]);
            }
        });
        Ok(Pane { master: pty.master, writer, child, grid })
    }

    pub fn write_bytes(&mut self, bytes: &[u8]) -> Result<(), Error> {
        self.writer.write_all(bytes)?;
        self.writer.flush()?;
        Ok(())
    }

    pub fn resize(&mut self, rows: u16, cols: u16) -> Result<(), Error> {
        self.master.resize(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })?;
        self.grid.lock().unwrap().set_size(rows as usize, cols as usize);
        Ok(())
    }

    pub fn size(&self) -> (usize, usize) {
        self.grid.lock().unwrap().size()
    }

    pub fn cells(&self) -> Vec<Vec<Cell>> {
        self.grid.lock().unwrap().cells()
    }

    pub fn text(&self) -> String {
        self.grid.lock().unwrap().text()
    }

    pub fn is_alive(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }
}

impl Drop for Pane {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}
