//! Bounded local IPC connection, shared by HTTP requests and OS identity probes.
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::{Duration, Instant};

pub(crate) fn try_connect(path: &Path) -> io::Result<UnixStream> {
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: zero is a valid initial representation of sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.len() >= address.sun_path.len() || bytes.contains(&0) {
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
    #[cfg(any(
        target_os = "macos",
        target_os = "ios",
        target_os = "freebsd",
        target_os = "netbsd",
        target_os = "openbsd",
        target_os = "dragonfly"
    ))]
    {
        address.sun_len = length as u8;
    }
    #[cfg(target_os = "linux")]
    let flags = libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC;
    #[cfg(not(target_os = "linux"))]
    let flags = libc::SOCK_STREAM;
    // SAFETY: socket has no pointer arguments; a successful descriptor is owned.
    let raw = unsafe { libc::socket(libc::AF_UNIX, flags, 0) };
    if raw < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: raw is a fresh descriptor returned by socket.
    let fd = unsafe { OwnedFd::from_raw_fd(raw) };
    // SAFETY: both fcntl calls operate on a live owned socket descriptor.
    if unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        return Err(io::Error::last_os_error());
    }
    let stream = UnixStream::from(fd);
    stream.set_nonblocking(true)?;
    // SAFETY: address is initialized; length covers its family and terminated path.
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
    Ok(stream)
}

pub(crate) fn connect_timeout(path: &Path, timeout: Duration) -> io::Result<UnixStream> {
    let start = Instant::now();
    loop {
        match try_connect(path) {
            Ok(stream) => {
                stream.set_nonblocking(false)?;
                return Ok(stream);
            }
            Err(error)
                if matches!(error.raw_os_error(), Some(libc::EAGAIN | libc::EINPROGRESS)) =>
            {
                let remaining = timeout.saturating_sub(start.elapsed());
                if remaining.is_zero() {
                    return Err(io::Error::new(
                        io::ErrorKind::TimedOut,
                        "Unix socket connection deadline exceeded",
                    ));
                }
                std::thread::sleep(remaining.min(Duration::from_millis(20)));
            }
            Err(error) => return Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_queue_has_a_connection_deadline() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("busy.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        // SAFETY: listener is a live AF_UNIX listening socket.
        assert_eq!(unsafe { libc::listen(listener.as_raw_fd(), 1) }, 0);
        let mut connections = Vec::new();
        for _ in 0..8 {
            match try_connect(&path) {
                Ok(stream) => connections.push(stream),
                Err(_) => break,
            }
        }
        assert_eq!(
            connect_timeout(&path, Duration::from_millis(30))
                .unwrap_err()
                .kind(),
            io::ErrorKind::TimedOut
        );
    }
}
