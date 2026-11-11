// ABOUTME: Owner-only Unix socket listeners that bind at once, accept once served, and close for
// ABOUTME: certain: only a stale socket of the directory owner's is replaced, only their own removed.

use std::{
    fs,
    net::Shutdown,
    os::unix::{
        fs::{FileTypeExt, MetadataExt, PermissionsExt},
        net::{UnixListener, UnixStream},
    },
    path::{Path, PathBuf},
    sync::{Arc, Mutex, MutexGuard, PoisonError},
    thread::{self, JoinHandle},
    time::Duration,
};

use rustix::{
    event::{poll, PollFd, PollFlags},
    io::Errno,
};

use crate::{remove_if_present, Error};

/// Pause after a failed accept, so a persistent error cannot spin a core.
const ACCEPT_RETRY: Duration = Duration::from_millis(50);

/// A file's device and inode, which tell apart two files that held one pathname.
pub(crate) type FileId = (u64, u64);

pub(crate) fn file_id(found: &fs::Metadata) -> FileId {
    (found.dev(), found.ino())
}

/// An owner-only Unix socket at a path: bound at once, accepting only once served, and closed for
/// certain, its accept loop joined and its socket file removed while the path still holds it.
pub struct Listener {
    path: PathBuf,
    bound: FileId,
    /// The listener and the far end of its wake pair, until `serve` takes them.
    unserved: Mutex<Option<(UnixListener, UnixStream)>>,
    /// Ends the accept loop at close, wherever the socket's pathname now leads.
    wake: UnixStream,
    /// The accept loop's thread, joined at close.
    accepting: Mutex<Option<JoinHandle<()>>>,
}

impl Listener {
    /// Binds an owner-only socket at `path`. Only a stale socket owned by the directory's owner is
    /// replaced; an ordinary file, a live socket or another user's file stays and fails the bind.
    /// Connections wait in the backlog until `serve`.
    pub fn bind(path: &Path) -> Result<Self, Error> {
        let directory = path
            .parent()
            .ok_or_else(|| Error::Invalid(format!("{} has no directory", path.display())))?;
        clear_stale_socket(path, fs::metadata(directory)?.uid())?;
        let (wake, woken) = UnixStream::pair()?;
        let listener = UnixListener::bind(path)?;
        let made = fs::set_permissions(path, fs::Permissions::from_mode(0o600))
            .and_then(|()| listener.set_nonblocking(true))
            .and_then(|()| fs::symlink_metadata(path));
        let made = match made {
            Ok(made) => made,
            Err(error) => {
                let _ = remove_if_present(path);
                return Err(error.into());
            }
        };
        Ok(Self {
            path: path.into(),
            bound: file_id(&made),
            unserved: Mutex::new(Some((listener, woken))),
            wake,
            accepting: Mutex::new(None),
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Accepts connections until close, handing each to `handle` on its own thread, in blocking
    /// mode: macOS gives an accepted socket the listener's non-blocking mode, and a connection
    /// whose mode cannot be set back is dropped. Serving again does nothing.
    pub fn serve(&self, handle: impl Fn(UnixStream) + Send + Sync + 'static) {
        let Some((listener, woken)) = locked(&self.unserved).take() else {
            return;
        };
        let handle = Arc::new(handle);
        let accepting = thread::spawn(move || loop {
            let mut ready = [
                PollFd::new(&listener, PollFlags::IN),
                PollFd::new(&woken, PollFlags::IN),
            ];
            match poll(&mut ready, None) {
                Ok(_) => {}
                Err(Errno::INTR) => continue,
                Err(_) => {
                    thread::sleep(ACCEPT_RETRY);
                    continue;
                }
            }
            if !ready[1].revents().is_empty() {
                return;
            }
            if ready[0].revents().is_empty() {
                continue;
            }
            match listener.accept() {
                Ok((stream, _)) => {
                    if stream.set_nonblocking(false).is_ok() {
                        let handle = Arc::clone(&handle);
                        thread::spawn(move || handle(stream));
                    }
                }
                // A connection that poll saw can be gone before it is accepted.
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(_) => thread::sleep(ACCEPT_RETRY),
            }
        });
        *locked(&self.accepting) = Some(accepting);
    }

    /// Stops accepting and waits for the accept loop to drop the listener, which closes it
    /// whatever its pathname now holds; then removes the socket file while the path still holds
    /// the one bound. Closing again does nothing.
    pub fn close(&self) {
        let _ = self.wake.shutdown(Shutdown::Both);
        drop(locked(&self.unserved).take());
        let accepting = locked(&self.accepting).take();
        if let Some(accepting) = accepting {
            let _ = accepting.join();
        }
        if fs::symlink_metadata(&self.path).is_ok_and(|found| file_id(&found) == self.bound) {
            let _ = remove_if_present(&self.path);
        }
    }
}

impl Drop for Listener {
    fn drop(&mut self) {
        self.close();
    }
}

fn locked<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// Makes way for a socket: nothing is there, or a stale socket of `owner`'s that nothing listens
/// on, which is removed. An ordinary file, someone else's file or a live socket stays where it is.
fn clear_stale_socket(socket: &Path, owner: u32) -> Result<(), Error> {
    clear_stale_socket_with(socket, owner, |path| UnixStream::connect(path))
}

fn clear_stale_socket_with(
    socket: &Path,
    owner: u32,
    connect: impl FnOnce(&Path) -> std::io::Result<UnixStream>,
) -> Result<(), Error> {
    let found = match fs::symlink_metadata(socket) {
        Ok(found) => found,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    if !found.file_type().is_socket() || found.uid() != owner {
        return Err(Error::Invalid(format!(
            "{} is not a socket this user left; it stays in place",
            socket.display()
        )));
    }
    match connect(socket) {
        Ok(_) => {
            return Err(Error::Invalid(format!(
                "{} is a live socket; it stays in place",
                socket.display()
            )));
        }
        Err(error) if error.kind() == std::io::ErrorKind::ConnectionRefused => {}
        Err(error) => return Err(error.into()),
    }
    let current = fs::symlink_metadata(socket)?;
    if file_id(&current) != file_id(&found)
        || current.uid() != owner
        || !current.file_type().is_socket()
    {
        return Err(Error::Invalid(format!(
            "{} changed during the stale-socket check; it stays in place",
            socket.display()
        )));
    }
    remove_if_present(socket)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io;

    // A failed probe is not generally proof that no listener exists. Refusing an uncertain
    // probe preserves the endpoint instead of converting permission or transient failure to loss.
    #[test]
    fn uncertain_socket_probe_never_removes_the_path() {
        let root = tempfile::tempdir().unwrap();
        let socket = root.path().join("peer.sock");
        drop(UnixListener::bind(&socket).unwrap());
        let before = fs::symlink_metadata(&socket).unwrap();
        for kind in [
            io::ErrorKind::PermissionDenied,
            io::ErrorKind::WouldBlock,
            io::ErrorKind::TimedOut,
            io::ErrorKind::Interrupted,
            io::ErrorKind::Other,
        ] {
            let result =
                clear_stale_socket_with(&socket, before.uid(), |_| Err(io::Error::from(kind)));
            assert!(result.is_err(), "uncertain probe {kind:?} was accepted");
            assert_eq!(
                file_id(&fs::symlink_metadata(&socket).unwrap()),
                file_id(&before),
                "uncertain probe {kind:?} removed the endpoint"
            );
        }
    }

    // The stale inode that failed a connection cannot authorize removing a newer live socket.
    // The injected connection operation deterministically makes that otherwise narrow race.
    #[test]
    fn replaced_socket_is_preserved_after_a_refused_probe() {
        let root = tempfile::tempdir().unwrap();
        let socket = root.path().join("peer.sock");
        let archived = root.path().join("old.sock");
        drop(UnixListener::bind(&socket).unwrap());
        let owner = fs::symlink_metadata(&socket).unwrap().uid();
        let mut replacement = None;
        let mut identity = None;
        let result = clear_stale_socket_with(&socket, owner, |path| {
            fs::rename(path, &archived).unwrap();
            replacement = Some(UnixListener::bind(path).unwrap());
            identity = Some(file_id(&fs::symlink_metadata(path).unwrap()));
            Err(io::Error::from(io::ErrorKind::ConnectionRefused))
        });
        assert!(
            result.is_err(),
            "a replacement cannot be reclaimed as stale"
        );
        assert_eq!(
            file_id(&fs::symlink_metadata(&socket).unwrap()),
            identity.unwrap()
        );
        assert!(UnixStream::connect(&socket).is_ok());
        drop(replacement);
    }
}
