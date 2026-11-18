// Windows-only witnesses for the two capture-retention fixtures.
// The launcher owns Child until a receiver pins the same creation identity.
use super::{
    DESCENDANT_STARTUP_HANDSHAKE, OUTPUT_FIXTURE_MARKER_ENV, REAP_GRACE, Reap, fixture_marker,
    lock_process_fixture, output_fixture_command, output_with_timeout_and_kill_tree,
    reap_killed_child, wait_for_fixture_marker,
};
use std::io::{self, Read as _, Write as _};
use std::os::windows::{
    io::{AsRawHandle, FromRawHandle, OwnedHandle},
    process::CommandExt as _,
};
use std::path::{Path, PathBuf};
use std::process::{Child, Stdio};
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};
use windows_sys::Win32::{
    Foundation::{FILETIME, HANDLE, WAIT_OBJECT_0, WAIT_TIMEOUT},
    System::Threading::{
        GetExitCodeProcess, GetProcessTimes, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
        PROCESS_SYNCHRONIZE, PROCESS_TERMINATE, TerminateProcess, WaitForSingleObject,
    },
};

fn creation_ticks(handle: HANDLE) -> io::Result<u64> {
    let mut created = FILETIME {
        dwLowDateTime: 0,
        dwHighDateTime: 0,
    };
    let mut exited = created;
    let mut kernel = created;
    let mut user = created;
    // SAFETY: the caller owns the process handle and each output is writable.
    if unsafe { GetProcessTimes(handle, &mut created, &mut exited, &mut kernel, &mut user) } == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((u64::from(created.dwHighDateTime) << 32) | u64::from(created.dwLowDateTime))
}

struct SpawnHandoff {
    child: Child,
    handed_off: bool,
}

impl Drop for SpawnHandoff {
    fn drop(&mut self) {
        if self.handed_off || matches!(self.child.try_wait(), Ok(Some(_))) {
            return;
        }
        let kill = self.child.kill();
        if !matches!(reap_killed_child(&mut self.child, REAP_GRACE), Reap::Reaped) {
            eprintln!("retention launcher cleanup unconfirmed: kill={kill:?}");
        }
    }
}

pub(super) fn launch(writer: bool) -> io::Result<()> {
    let mode = if writer {
        "retained_writer_child"
    } else {
        "retained_child"
    };
    let mut command = output_fixture_command(mode);
    command.stdout(Stdio::inherit()).stderr(Stdio::inherit());
    const DETACHED_PROCESS: u32 = 0x0000_0008;
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
    command.creation_flags(DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP);
    let mut owner = SpawnHandoff {
        child: command.spawn()?,
        handed_off: false,
    };
    let identity =
        fixture_marker("identity").ok_or_else(|| io::Error::other("missing retention scope"))?;
    let pending = identity.with_extension("pending");
    let ticks = creation_ticks(owner.child.as_raw_handle())?;
    std::fs::write(&pending, format!("{} {ticks}\n", owner.child.id()))?;
    std::fs::rename(pending, identity)?;
    if !wait_for_fixture_marker("bound", DESCENDANT_STARTUP_HANDSHAKE) {
        return Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "retention identity handoff timed out",
        ));
    }
    owner.handed_off = true;
    println!(
        "{}",
        if writer {
            "parent-output"
        } else {
            "descendant-started"
        }
    );
    Ok(())
}

fn phase(value: &str) -> io::Result<()> {
    let path =
        fixture_marker("phase").ok_or_else(|| io::Error::other("missing retention scope"))?;
    std::fs::write(path, value)
}

pub(super) fn child(writer: bool) -> io::Result<()> {
    let result = (|| {
        if writer {
            std::io::stdout().write_all(b"late-write\n")?;
            std::io::stdout().flush()?;
        }
        phase("running")?;
        std::fs::write(fixture_marker("running").unwrap(), b"running")?;
        let release = fixture_marker("release").unwrap();
        let started = Instant::now();
        while !release.is_file() {
            if started.elapsed() >= DESCENDANT_STARTUP_HANDSHAKE {
                phase("release-expired")?;
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "retention release did not arrive",
                ));
            }
            if writer {
                std::io::stdout().write_all(b"late-write\n")?;
                std::io::stdout().flush()?;
            }
            std::thread::sleep(Duration::from_millis(if writer { 100 } else { 25 }));
        }
        phase("released")?;
        std::fs::write(fixture_marker("finished").unwrap(), b"finished")?;
        phase("finished")
    })();
    if let Err(error) = &result {
        let _ = phase(&format!("io-error:{}", error.raw_os_error().unwrap_or(-1)));
    }
    result
}

fn read_identity(path: &Path) -> io::Result<(u32, u64)> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > 128 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid retention identity receipt",
        ));
    }
    let text = std::fs::read_to_string(path)?;
    let parts: Vec<_> = text.split_whitespace().collect();
    if parts.len() != 2 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid retention identity fields",
        ));
    }
    let pid = parts[0].parse::<u32>().ok().filter(|pid| *pid > 1);
    let ticks = parts[1].parse::<u64>().ok().filter(|ticks| *ticks != 0);
    pid.zip(ticks).ok_or_else(|| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid retention identity values",
        )
    })
}

struct BoundProcess {
    handle: OwnedHandle,
    marker: PathBuf,
    running_seen: Instant,
    natural_budget: Duration,
    release_attempted: bool,
    cleanup_attempted: bool,
    exit_confirmed: bool,
}

impl BoundProcess {
    fn bind(pid: u32, expected_ticks: u64, marker: PathBuf, budget: Duration) -> io::Result<Self> {
        // SAFETY: OpenProcess has no borrowed pointers; a non-null result is owned below.
        let raw = unsafe {
            OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_SYNCHRONIZE | PROCESS_TERMINATE,
                0,
                pid,
            )
        };
        if raw.is_null() {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: this is the newly owned process handle, closed once by OwnedHandle.
        let handle = unsafe { OwnedHandle::from_raw_handle(raw) };
        if creation_ticks(handle.as_raw_handle())? != expected_ticks {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "retention process creation identity changed",
            ));
        }
        let mut process = Self {
            handle,
            marker,
            running_seen: Instant::now(),
            natural_budget: budget,
            release_attempted: false,
            cleanup_attempted: false,
            exit_confirmed: false,
        };
        if !process.alive()? {
            return Err(io::Error::other(
                "retention descendant exited before identity handoff",
            ));
        }
        Ok(process)
    }

    fn alive(&mut self) -> io::Result<bool> {
        // SAFETY: the owned handle remains valid for this nonblocking observation.
        match unsafe { WaitForSingleObject(self.handle.as_raw_handle(), 0) } {
            WAIT_TIMEOUT => Ok(true),
            WAIT_OBJECT_0 => {
                self.exit_confirmed = true;
                Ok(false)
            }
            _ => Err(io::Error::last_os_error()),
        }
    }

    fn diagnostic(&self) -> String {
        let mut bytes = Vec::new();
        match std::fs::File::open(self.marker.with_extension("phase"))
            .and_then(|file| file.take(256).read_to_end(&mut bytes))
        {
            Ok(_) => String::from_utf8_lossy(&bytes).trim().to_owned(),
            Err(error) => format!("phase-unavailable:{}", error.raw_os_error().unwrap_or(-1)),
        }
    }

    fn wait_exit(&mut self, budget: Duration) -> io::Result<u32> {
        let millis = budget.as_millis().min(u128::from(u32::MAX)) as u32;
        // SAFETY: the same owned process object is retained through wait and exit-code read.
        match unsafe { WaitForSingleObject(self.handle.as_raw_handle(), millis) } {
            WAIT_OBJECT_0 => {
                self.exit_confirmed = true;
                let mut code = 259;
                // SAFETY: code is writable and the process handle remains owned.
                if unsafe { GetExitCodeProcess(self.handle.as_raw_handle(), &mut code) } == 0 {
                    return Err(io::Error::last_os_error());
                }
                Ok(code)
            }
            WAIT_TIMEOUT => Err(io::Error::new(
                io::ErrorKind::TimedOut,
                format!(
                    "retention descendant did not exit naturally; phase={}",
                    self.diagnostic()
                ),
            )),
            _ => Err(io::Error::last_os_error()),
        }
    }

    fn finish(&mut self) -> io::Result<()> {
        let started = Instant::now();
        self.release_attempted = true;
        std::fs::write(self.marker.with_extension("release"), b"release")?;
        let code = self.wait_exit(self.natural_budget.saturating_sub(started.elapsed()))?;
        if code != 0 || !self.marker.with_extension("finished").is_file() {
            return Err(io::Error::other(format!(
                "retention descendant exit={code}; phase={}",
                self.diagnostic()
            )));
        }
        Ok(())
    }

    fn cleanup(&mut self) {
        if self.exit_confirmed || self.cleanup_attempted {
            return;
        }
        // Fixture and BoundProcess both use this owner. One failed attempt
        // must remain unconfirmed rather than gaining another grace in Drop.
        self.cleanup_attempted = true;
        if !self.release_attempted
            && let Err(error) = self.finish()
        {
            eprintln!("retention cooperative cleanup failed: {error}");
        }
        if !self.exit_confirmed {
            // SAFETY: terminate the pinned object, never a later process found by PID/name.
            let killed = unsafe { TerminateProcess(self.handle.as_raw_handle(), 1) };
            let exit = self.wait_exit(REAP_GRACE);
            eprintln!("retention forced cleanup requested={killed}, exit={exit:?}");
        }
    }
}

impl Drop for BoundProcess {
    fn drop(&mut self) {
        self.cleanup();
    }
}

pub(super) struct Observation {
    pub(super) running_seen: Instant,
    pub(super) alive: bool,
}

pub(super) struct Fixture {
    temporary: Option<tempfile::TempDir>,
    binder: Option<JoinHandle<io::Result<BoundProcess>>>,
    process: Option<BoundProcess>,
    cancel: Arc<AtomicBool>,
    complete: bool,
}

impl Fixture {
    pub(super) fn new(temporary: tempfile::TempDir, marker: PathBuf, budget: Duration) -> Self {
        let cancel = Arc::new(AtomicBool::new(false));
        let stop = cancel.clone();
        let binder = std::thread::spawn(move || {
            let started = Instant::now();
            let identity = marker.with_extension("identity");
            let running = marker.with_extension("running");
            while !identity.is_file() || !running.is_file() {
                if stop.load(Ordering::Acquire)
                    || started.elapsed() >= DESCENDANT_STARTUP_HANDSHAKE + Duration::from_secs(15)
                {
                    return Err(io::Error::new(
                        io::ErrorKind::TimedOut,
                        "retention identity was not handed off",
                    ));
                }
                std::thread::sleep(Duration::from_millis(25));
            }
            let (pid, ticks) = read_identity(&identity)?;
            let process = BoundProcess::bind(pid, ticks, marker.clone(), budget)?;
            std::fs::write(marker.with_extension("bound"), b"bound")?;
            Ok(process)
        });
        Self {
            temporary: Some(temporary),
            binder: Some(binder),
            process: None,
            cancel,
            complete: false,
        }
    }

    fn bind(&mut self) -> io::Result<&mut BoundProcess> {
        if self.process.is_none() {
            let binder = self
                .binder
                .take()
                .ok_or_else(|| io::Error::other("retention identity unavailable"))?;
            self.process = Some(
                binder
                    .join()
                    .map_err(|_| io::Error::other("retention identity observer panicked"))??,
            );
        }
        Ok(self.process.as_mut().unwrap())
    }

    pub(super) fn observe(&mut self) -> io::Result<Observation> {
        let process = self.bind()?;
        Ok(Observation {
            running_seen: process.running_seen,
            alive: process.alive()?,
        })
    }

    pub(super) fn finish(&mut self) -> io::Result<()> {
        self.bind()?.finish()?;
        self.complete = true;
        Ok(())
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.cancel.store(true, Ordering::Release);
        let bound = self.bind();
        let exited = match bound {
            Ok(process) => {
                process.cleanup();
                process.exit_confirmed
            }
            Err(error) => {
                eprintln!("retention handoff cleanup unconfirmed: {error}");
                false
            }
        };
        self.process.take();
        if !exited && let Some(temporary) = self.temporary.take() {
            // The outer owned process scope removes this only after its Job is empty.
            let _quarantine = temporary.keep();
            eprintln!("retention directory quarantined until the outer scope retires");
        }
        assert!(
            self.complete || std::thread::panicking(),
            "retention fixture did not complete a verified natural exit"
        );
    }
}

fn started_fixture() -> (Fixture, PathBuf) {
    let temporary = tempfile::tempdir().unwrap();
    let marker = temporary.path().join("retained-child");
    let mut fixture = Fixture::new(temporary, marker.clone(), Duration::from_secs(7));
    let mut command = output_fixture_command("retained_launcher");
    command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
    let output = output_with_timeout_and_kill_tree(command, Duration::from_secs(90)).unwrap();
    assert!(
        output.status.success(),
        "retention launcher failed: {:?}",
        output.status
    );
    assert!(fixture.observe().unwrap().alive);
    (fixture, marker)
}

#[test]
fn panicking_consumer_retires_the_bound_child_before_removing_its_directory() {
    let _lock = lock_process_fixture();
    let (mut fixture, _) = started_fixture();
    let probe = fixture.bind().unwrap().handle.try_clone().unwrap();
    let directory = fixture.temporary.as_ref().unwrap().path().to_path_buf();
    let failure = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
        let _owned = fixture;
        panic!("injected consumer failure after identity handoff");
    }));
    assert!(failure.is_err());
    // SAFETY: this independent owned handle pins the exact object observed before panic.
    assert_eq!(
        unsafe { WaitForSingleObject(probe.as_raw_handle(), 0) },
        WAIT_OBJECT_0
    );
    let mut exit_code = 259;
    // SAFETY: probe stays owned and exit_code is writable.
    assert_ne!(
        unsafe { GetExitCodeProcess(probe.as_raw_handle(), &mut exit_code) },
        0
    );
    assert_eq!(
        exit_code, 0,
        "panic cleanup must first permit cooperative exit"
    );
    assert!(
        !directory.exists(),
        "fixture directory outlived its verified child exit"
    );
}

#[test]
fn mismatched_creation_identity_does_not_terminate_the_live_fixture() {
    let _lock = lock_process_fixture();
    let (mut fixture, marker) = started_fixture();
    let (pid, ticks) = read_identity(&marker.with_extension("identity")).unwrap();
    let rejected = BoundProcess::bind(pid, ticks.wrapping_add(1), marker, Duration::from_secs(7));
    assert!(
        rejected.is_err(),
        "an unmatching creation receipt must not be adopted"
    );
    assert!(
        fixture.observe().unwrap().alive,
        "refusing identity must not terminate the live child"
    );
    fixture.finish().unwrap();
}
