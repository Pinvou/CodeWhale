//! OS-authenticated context for local API connections. Never read actor IDs
//! from JSON or trust a caller-supplied process identity.
use std::os::fd::AsRawFd;
use std::os::unix::net::UnixStream;

#[derive(Clone, Copy)]
pub struct LocalPeer {
    pub pid: u32,
    pub uid: u32,
}

impl LocalPeer {
    pub fn from_stream(stream: &UnixStream) -> std::io::Result<Self> {
        // SAFETY: getsockopt receives initialized writable buffers and a live FD.
        let mut credentials: libc::ucred = unsafe { std::mem::zeroed() };
        let mut size = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        let result = unsafe {
            libc::getsockopt(
                stream.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_PEERCRED,
                (&mut credentials as *mut libc::ucred).cast(),
                &mut size,
            )
        };
        if result != 0 || credentials.pid <= 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(Self {
            pid: credentials.pid as u32,
            uid: credentials.uid,
        })
    }

    #[cfg(test)]
    pub fn current() -> Self {
        // SAFETY: geteuid has no preconditions.
        Self {
            pid: std::process::id(),
            uid: unsafe { libc::geteuid() },
        }
    }
}

pub fn boot_id() -> String {
    std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .unwrap_or_default()
        .trim()
        .to_owned()
}

pub fn start_ticks(pid: u32) -> Option<String> {
    std::fs::read_to_string(format!("/proc/{pid}/stat"))
        .ok()?
        .rsplit_once(") ")?
        .1
        .split_whitespace()
        .nth(19)
        .map(str::to_owned)
}
