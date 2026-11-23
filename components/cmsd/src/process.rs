//! Bounded backend CLI execution. Never wait forever or buffer arbitrary output
//! from a stalled Docker daemon, D-Bus call, or inherited child pipe.
use std::io::{self, Read};
use std::os::fd::AsRawFd;
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Output, Stdio};
use std::time::{Duration, Instant};

struct Guard(Child, bool);
impl Drop for Guard {
    fn drop(&mut self) {
        // Child is created in its own process group. Never signal a caller's
        // group; group remains useful if a descendant inherited a pipe.
        if self.1 {
            unsafe {
                libc::kill(-(self.0.id() as i32), libc::SIGKILL);
            }
        }
        let _ = self.0.wait();
    }
}
fn nonblocking(fd: i32) -> io::Result<()> {
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 || unsafe { libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}
fn drain(reader: &mut impl Read, bytes: &mut Vec<u8>, maximum: usize) -> io::Result<bool> {
    let mut buffer = [0u8; 8192];
    // Yield to the deadline/other pipe even when a producer never pauses.
    for _ in 0..32 {
        match reader.read(&mut buffer) {
            Ok(0) => return Ok(true),
            Ok(n) => {
                if n > maximum.saturating_sub(bytes.len()) {
                    return Err(io::Error::other("backend output limit exceeded"));
                }
                bytes.extend_from_slice(&buffer[..n]);
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => return Ok(false),
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        }
    }
    Ok(false)
}

pub fn output(command: &mut Command, timeout: Duration) -> io::Result<Output> {
    command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    let mut child = Guard(command.spawn()?, true);
    let mut stdout = child
        .0
        .stdout
        .take()
        .ok_or_else(|| io::Error::other("missing stdout"))?;
    let mut stderr = child
        .0
        .stderr
        .take()
        .ok_or_else(|| io::Error::other("missing stderr"))?;
    nonblocking(stdout.as_raw_fd())?;
    nonblocking(stderr.as_raw_fd())?;
    let deadline = Instant::now() + timeout;
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut status = None;
    loop {
        let out_eof = drain(&mut stdout, &mut out, 8 * 1024 * 1024)?;
        let err_eof = drain(&mut stderr, &mut err, 256 * 1024)?;
        if status.is_none() {
            status = child.0.try_wait()?;
        }
        if out_eof && err_eof {
            if let Some(status) = status {
                child.1 = false;
                return Ok(Output {
                    status,
                    stdout: out,
                    stderr: err,
                });
            }
        }
        if Instant::now() >= deadline {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "backend deadline exceeded; external operation outcome requires reconciliation",
            ));
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn captures_both_pipes_and_status() {
        let out = output(
            Command::new("/bin/sh").args(["-c", "printf good; printf bad >&2; exit 7"]),
            Duration::from_secs(2),
        )
        .unwrap();
        assert_eq!(out.stdout, b"good");
        assert_eq!(out.stderr, b"bad");
        assert_eq!(out.status.code(), Some(7));
    }
    #[test]
    fn timeout_kills_descendants_with_inherited_pipes() {
        let now = Instant::now();
        let error = output(
            Command::new("/bin/sh").args(["-c", "sleep 30 & wait"]),
            Duration::from_millis(100),
        )
        .unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::TimedOut);
        assert!(now.elapsed() < Duration::from_secs(2));
    }
    #[test]
    fn infinite_output_is_bounded() {
        let error = output(
            Command::new("/bin/sh").args(["-c", "yes x >&2"]),
            Duration::from_secs(2),
        )
        .unwrap_err();
        assert!(error.to_string().contains("output limit"));
    }
}
