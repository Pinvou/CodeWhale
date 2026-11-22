//! Bounded local connection even when the listening Unix backlog is full.
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::{ffi::OsStrExt, net::UnixStream};
use std::path::Path;
use std::time::{Duration, Instant};

fn attempt(path: &Path) -> io::Result<UnixStream> {
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: an all-zero sockaddr_un is initialized before use.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.is_empty() || bytes.len() >= address.sun_path.len() || bytes.contains(&0) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid Unix socket path",
        ));
    }
    address.sun_family = libc::AF_UNIX as libc::sa_family_t;
    for (target, byte) in address.sun_path.iter_mut().zip(bytes) {
        *target = *byte as libc::c_char;
    }
    let length =
        (std::mem::offset_of!(libc::sockaddr_un, sun_path) + bytes.len() + 1) as libc::socklen_t;
    // SAFETY: socket has no pointers; a successful descriptor is newly owned.
    let raw = unsafe {
        libc::socket(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if raw < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: raw is the freshly allocated socket descriptor.
    let fd = unsafe { OwnedFd::from_raw_fd(raw) };
    let stream = UnixStream::from(fd);
    // SAFETY: address is initialized and length covers its terminated path.
    if unsafe {
        libc::connect(
            stream.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            length,
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    stream.set_nonblocking(false)?;
    Ok(stream)
}

pub(super) fn connect(path: &Path, timeout: Duration) -> io::Result<UnixStream> {
    let started = Instant::now();
    loop {
        match attempt(path) {
            Ok(stream) => return Ok(stream),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::EAGAIN | libc::EINPROGRESS | libc::EINTR)
                ) =>
            {
                let remaining = timeout.saturating_sub(started.elapsed());
                if remaining.is_zero() {
                    return Err(io::Error::new(
                        io::ErrorKind::TimedOut,
                        "Unix connection deadline exceeded",
                    ));
                }
                std::thread::sleep(remaining.min(Duration::from_millis(10)));
            }
            Err(error) => return Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_backlog_does_not_block_connect_or_leak_descriptors() {
        let root = crate::docker::fixtures::tmpdir("store-backlog");
        let path = root.join("store.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        // SAFETY: listener is a live socket; reduce only this fixture's backlog.
        assert_eq!(unsafe { libc::listen(listener.as_raw_fd(), 1) }, 0);
        let mut holders = Vec::new();
        for _ in 0..8 {
            match attempt(&path) {
                Ok(s) => holders.push(s),
                Err(_) => break,
            }
        }
        let started = Instant::now();
        assert_eq!(
            connect(&path, Duration::from_millis(40))
                .unwrap_err()
                .kind(),
            io::ErrorKind::TimedOut
        );
        assert!(started.elapsed() < Duration::from_millis(500));
        drop(holders);
        drop(listener);
        std::fs::remove_dir_all(root).unwrap();
    }
}
