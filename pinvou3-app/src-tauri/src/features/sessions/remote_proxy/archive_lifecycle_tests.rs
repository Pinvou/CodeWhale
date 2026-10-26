//! Real SessionStore lifecycle, OS ownership and durable recovery regressions.
use super::super::archive::{ArchiveStore, ArchiveTestPoint};
use super::*;
use std::io::{BufRead, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::atomic::AtomicBool;
use std::sync::mpsc;
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

// Restore before releasing the crate-wide lock, including panic unwinding.
// SessionStore's explicit root does not relocate every global sidecar.
struct IsolatedHome {
    _restore: crate::platform::test_support::EnvRestore,
    _lock: std::sync::MutexGuard<'static, ()>,
}

fn isolate_home(root: &Path) -> IsolatedHome {
    let (lock, restore) = crate::platform::test_support::locked_env(&[
        "PINVOU3_HOME",
        "CODEWHALE_HOME",
        "PINVOU3_TEST_KEYRING_FILE_FALLBACK",
    ]);
    // SAFETY: the crate-wide ENV_LOCK remains held until after EnvRestore.
    unsafe {
        std::env::set_var("PINVOU3_HOME", root);
        std::env::set_var("CODEWHALE_HOME", root);
        std::env::set_var("PINVOU3_TEST_KEYRING_FILE_FALLBACK", "1");
    }
    IsolatedHome {
        _restore: restore,
        _lock: lock,
    }
}

fn bank_path(root: &Path) -> PathBuf {
    root.join("sessions/_remote_proxies.archives.sqlite3")
}

fn active_path(root: &Path) -> PathBuf {
    root.join("sessions/_remote_proxies.json")
}

fn populated_record(store: &SessionStore, suffix: &str) -> super::super::schema::RemoteProxyRecord {
    let live = context();
    let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
    store
        .confirm_remote_mapping(
            &opened.local_session_id,
            &live,
            &AuthenticatedMapping {
                remote_session_ref: opened.remote_session_ref.clone(),
                controlled_session_id: format!("b-{suffix}"),
            },
        )
        .unwrap();
    for index in 0..2 {
        let client = format!("client-{suffix}-{index}");
        let task = format!("task-{suffix}-{index}");
        let fingerprint = "a".repeat(64);
        store
            .insert_remote_pending(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: client.clone(),
                    task_id: task.clone(),
                    request_fingerprint: fingerprint.clone(),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        store
            .abandon_unattempted_remote_pending(
                &opened.local_session_id,
                &client,
                &task,
                &fingerprint,
            )
            .unwrap();
    }
    store
        .remote_proxy_snapshot(&opened.local_session_id)
        .unwrap()
}

fn fixture(root: &Path) -> (SessionStore, super::super::schema::RemoteProxyRecord) {
    let store = make_store(root);
    let seed = populated_record(&store, "seed");
    store
        .archive_remote_proxy(&seed.local_session_id, None)
        .unwrap();
    let record = populated_record(&store, "target");
    populated_record(&store, "other");
    (store, record)
}

#[derive(Debug, PartialEq)]
struct BankSnapshot {
    anchor: serde_json::Value,
    incarnation: String,
    records: Vec<(i64, String, Vec<u8>)>,
    retired: Vec<(String, String, String)>,
}

/// Deliberately bypass ArchiveStore readers, which would finish pending work
/// before the test could observe the actual failure state.
fn raw_snapshot(root: &Path) -> BankSnapshot {
    let path = bank_path(root);
    let db =
        rusqlite::Connection::open_with_flags(&path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
            .unwrap();
    let records = db
        .prepare("SELECT sequence,local_id,record FROM archives ORDER BY sequence")
        .unwrap()
        .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))
        .unwrap()
        .collect::<rusqlite::Result<Vec<_>>>()
        .unwrap();
    let retired = db
        .prepare("SELECT client_id,task_id,local_id FROM retired_ids ORDER BY client_id")
        .unwrap()
        .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))
        .unwrap()
        .collect::<rusqlite::Result<Vec<_>>>()
        .unwrap();
    BankSnapshot {
        anchor: serde_json::from_slice(&std::fs::read(path.with_extension("anchor.json")).unwrap())
            .unwrap(),
        incarnation: db
            .query_row("SELECT id FROM archive_meta", [], |row| row.get(0))
            .unwrap(),
        records,
        retired,
    }
}

fn assert_local_usable(store: &SessionStore, root: &Path) {
    let local = store
        .create_new("/model".into(), None, root.into())
        .unwrap();
    assert!(store.load(&local.metadata.id).is_ok());
    for backend in [LocalBackendBinding::Engine, LocalBackendBinding::Acp] {
        store.set_local_backend_resolver(Arc::new(move |_| Ok(backend)));
        let route = store
            .route_session(&local.metadata.id, true, None, None)
            .unwrap();
        assert_eq!(
            route,
            match backend {
                LocalBackendBinding::Engine => SessionRoute::Engine,
                LocalBackendBinding::Acp => SessionRoute::Acp,
            }
        );
    }
    store.save(&local).unwrap();
    store.delete(&local.metadata.id).unwrap();
}

#[test]
#[ignore = "private child-process entry; invoked by bounded lock-helper parents"]
fn archive_lock_child() {
    let path = std::env::var_os("PINVOU_ARCHIVE_LOCK_HELPER").expect("child-only path");
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(path)
        .unwrap();
    let mut ownership = fd_lock::RwLock::new(file);
    let _guard = ownership.try_write().unwrap();
    let mode = std::env::var("PINVOU_ARCHIVE_LOCK_HELPER_MODE").unwrap_or_default();
    if mode == "stall-before-ready" {
        println!("PINVOU_LOCK_STALLED_BEFORE_READY");
        std::io::stdout().flush().unwrap();
        loop {
            std::thread::park();
        }
    }
    println!("PINVOU_LOCK_READY");
    std::io::stdout().flush().unwrap();
    let mut release = String::new();
    assert!(std::io::stdin().read_line(&mut release).unwrap() > 0);
    assert_eq!(release.trim(), "release");
    if mode == "stall-after-release" {
        println!("PINVOU_LOCK_STALLED_AFTER_RELEASE");
        std::io::stdout().flush().unwrap();
        loop {
            std::thread::park();
        }
    }
}

const LOCK_HELPER_TIMEOUT: Duration = Duration::from_secs(30);
const LOCK_HELPER_CLEANUP_TIMEOUT: Duration = Duration::from_secs(5);

/// Own the helper from spawn through reap, including assertion unwinding.
/// Only the reader thread blocks on stdout; the test thread always has a deadline.
struct LockChild {
    child: Child,
    output: mpsc::Receiver<Result<String, String>>,
    reader: Option<JoinHandle<()>>,
}

impl LockChild {
    fn spawn(path: &Path, mode: &str) -> Result<Self, String> {
        let child = Command::new(std::env::current_exe().map_err(|e| e.to_string())?)
            .args([
                "--exact",
                "features::sessions::remote_proxy::tests::archive_lifecycle::archive_lock_child",
                "--ignored",
                "--nocapture",
                "--test-threads=1",
            ])
            .env("PINVOU_ARCHIVE_LOCK_HELPER", path)
            .env("PINVOU_ARCHIVE_LOCK_HELPER_MODE", mode)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .map_err(|e| format!("spawn lock child: {e}"))?;
        let (tx, output) = mpsc::channel();
        let mut owned = Self {
            child,
            output,
            reader: None,
        };
        let stdout = owned.child.stdout.take().ok_or("missing child stdout")?;
        owned.reader = Some(
            std::thread::Builder::new()
                .name("archive-lock-stdout".into())
                .spawn(move || {
                    for line in std::io::BufReader::new(stdout).lines() {
                        let terminal = line.is_err();
                        if tx.send(line.map_err(|e| e.to_string())).is_err() || terminal {
                            break;
                        }
                    }
                })
                .map_err(|e| format!("spawn stdout reader: {e}"))?,
        );
        Ok(owned)
    }

    fn wait_marker(&self, marker: &str, timeout: Duration) -> Result<(), String> {
        let deadline = Instant::now() + timeout;
        loop {
            match self
                .output
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            {
                // libtest may prefix the first marker with its test-name line.
                Ok(Ok(line)) if line.trim_end().ends_with(marker) => return Ok(()),
                Ok(Ok(_)) => {}
                Ok(Err(error)) => return Err(format!("child stdout: {error}")),
                Err(mpsc::RecvTimeoutError::Timeout) => {
                    return Err(format!("{marker} wait timed out"));
                }
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err(format!("stdout closed before {marker}"));
                }
            }
            if Instant::now() >= deadline {
                return Err(format!("{marker} wait timed out"));
            }
        }
    }

    fn ready(&mut self, timeout: Duration) -> Result<(), String> {
        if let Err(error) = self.wait_marker("PINVOU_LOCK_READY", timeout) {
            return Err(self.failure_with_cleanup(error));
        }
        Ok(())
    }

    fn release(&mut self) -> Result<(), String> {
        let result = self
            .child
            .stdin
            .take()
            .ok_or_else(|| "missing child stdin".to_owned())
            .and_then(|mut input| {
                writeln!(input, "release").map_err(|e| format!("release child: {e}"))
            });
        result.map_err(|error| self.failure_with_cleanup(error))
    }

    // try_wait reaps the child when it returns Some. Never use unbounded wait.
    fn poll_exit(&mut self, deadline: Instant) -> Result<ExitStatus, String> {
        loop {
            if let Some(status) = self
                .child
                .try_wait()
                .map_err(|e| format!("reap child: {e}"))?
            {
                return Ok(status);
            }
            if Instant::now() >= deadline {
                return Err("child exit wait timed out".into());
            }
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    fn join_reader(&mut self, deadline: Instant) -> Result<(), String> {
        if let Some(reader) = self.reader.as_ref() {
            while !reader.is_finished() {
                if Instant::now() >= deadline {
                    return Err("child stdout reader did not exit".into());
                }
                std::thread::sleep(Duration::from_millis(5));
            }
        }
        if let Some(reader) = self.reader.take() {
            reader
                .join()
                .map_err(|_| "child stdout reader panicked".to_owned())?;
        }
        Ok(())
    }

    fn cleanup(&mut self) -> Result<(), String> {
        let deadline = Instant::now() + LOCK_HELPER_CLEANUP_TIMEOUT;
        drop(self.child.stdin.take());
        if self.child.try_wait().map_err(|e| e.to_string())?.is_none() {
            // kill may race a natural exit; accept it only if try_wait reaps it.
            if let Err(error) = self.child.kill() {
                if self.child.try_wait().map_err(|e| e.to_string())?.is_none() {
                    return Err(format!("kill lock child: {error}"));
                }
            }
        }
        self.poll_exit(deadline)?;
        self.join_reader(deadline)
    }

    fn failure_with_cleanup(&mut self, error: String) -> String {
        match self.cleanup() {
            Ok(()) => error,
            Err(cleanup) => format!("{error}; cleanup failed: {cleanup}"),
        }
    }

    fn wait_exit(&mut self, timeout: Duration) -> Result<ExitStatus, String> {
        match self.poll_exit(Instant::now() + timeout) {
            Ok(status) => {
                self.join_reader(Instant::now() + LOCK_HELPER_CLEANUP_TIMEOUT)?;
                Ok(status)
            }
            Err(error) => Err(self.failure_with_cleanup(error)),
        }
    }
}

impl Drop for LockChild {
    fn drop(&mut self) {
        if let Err(error) = self.cleanup() {
            eprintln!("archive lock helper cleanup: {error}");
        }
    }
}

fn spawn_lock_child(path: &Path) -> LockChild {
    let mut child = LockChild::spawn(path, "normal").unwrap();
    child.ready(LOCK_HELPER_TIMEOUT).unwrap();
    child
}

fn assert_stalled_child_is_reaped(mode: &str) {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("probe.lock");
    std::fs::File::create(&path).unwrap();
    {
        let _home = isolate_home(temp.path());
        let mut child = LockChild::spawn(&path, mode).unwrap();
        if mode == "stall-before-ready" {
            child
                .wait_marker("PINVOU_LOCK_STALLED_BEFORE_READY", LOCK_HELPER_TIMEOUT)
                .unwrap();
        } else {
            child.ready(LOCK_HELPER_TIMEOUT).unwrap();
            child.release().unwrap();
            child
                .wait_marker("PINVOU_LOCK_STALLED_AFTER_RELEASE", LOCK_HELPER_TIMEOUT)
                .unwrap();
        }
        let started = Instant::now();
        let timeout = Duration::from_millis(200);
        let error = if mode == "stall-before-ready" {
            child.ready(timeout).unwrap_err()
        } else {
            child.wait_exit(timeout).unwrap_err()
        };
        assert!(
            error.contains(if mode == "stall-before-ready" {
                "PINVOU_LOCK_READY wait timed out"
            } else {
                "child exit wait timed out"
            }),
            "{error}"
        );
        assert!(!error.contains("cleanup failed"), "{error}");
        assert!(started.elapsed() < timeout + LOCK_HELPER_CLEANUP_TIMEOUT + Duration::from_secs(5));
        assert!(child.child.try_wait().unwrap().is_some());
        assert!(child.child.stdin.is_none());
        assert!(child.reader.is_none());
    }
    // Reacquire the same environment lock and actual fd-lock after failure.
    let _home = isolate_home(temp.path());
    let mut next = spawn_lock_child(&path);
    next.release().unwrap();
    assert!(next.wait_exit(LOCK_HELPER_TIMEOUT).unwrap().success());
}

#[test]
fn lock_child_ready_stall_is_bounded_and_reaped() {
    assert_stalled_child_is_reaped("stall-before-ready");
}

#[test]
fn lock_child_exit_stall_is_bounded_and_reaped() {
    assert_stalled_child_is_reaped("stall-after-release");
}

#[test]
fn seal_lock_contention_remains_retryable() {
    let temp = tempfile::tempdir().unwrap();
    let _home = isolate_home(temp.path());
    let (store, record) = fixture(temp.path());
    let before = raw_snapshot(temp.path());
    let active = std::fs::read(active_path(temp.path())).unwrap();
    let child = Arc::new(parking_lot::Mutex::new(None::<LockChild>));
    let child_hook = child.clone();
    let lock_path = bank_path(temp.path()).with_extension("lock");
    let reached = Arc::new(AtomicUsize::new(0));
    let hit = reached.clone();
    store
        .remote_proxies
        .lock()
        .archives
        .set_test_hook(move |point| {
            if point == ArchiveTestPoint::BeforeSealLock && hit.fetch_add(1, Ordering::SeqCst) == 0
            {
                *child_hook.lock() = Some(spawn_lock_child(&lock_path));
            }
            Ok(())
        });
    assert!(
        store
            .archive_remote_proxy(&record.local_session_id, None)
            .is_err()
    );
    assert_eq!(reached.load(Ordering::SeqCst), 1);
    assert!(!store.remote_proxies.lock().load_failed);
    assert_eq!(
        store.remote_proxy_snapshot(&record.local_session_id),
        Some(record.clone())
    );
    assert_eq!(raw_snapshot(temp.path()), before);
    assert_eq!(std::fs::read(active_path(temp.path())).unwrap(), active);
    let mut child = child.lock().take().unwrap();
    child.release().unwrap();
    assert!(child.wait_exit(LOCK_HELPER_TIMEOUT).unwrap().success());
    assert!(store.prepare_remote_proxy_open(None, &context()).is_ok());
    assert_local_usable(&store, temp.path());
    assert!(
        store
            .archive_remote_proxy(&record.local_session_id, None)
            .unwrap()
    );
    assert!(
        make_store(temp.path())
            .is_archived_remote_proxy(&record.local_session_id)
            .unwrap()
    );
}

#[test]
fn indeterminate_seal_retains_exact_durable_state_until_restart() {
    for point in [
        ArchiveTestPoint::BeforePendingWrite,
        ArchiveTestPoint::AfterPendingWrite,
        ArchiveTestPoint::AfterInsertSeal,
        ArchiveTestPoint::AfterCompleteAnchorWrite,
    ] {
        let temp = tempfile::tempdir().unwrap();
        let _home = isolate_home(temp.path());
        let (store, record) = fixture(temp.path());
        let before = raw_snapshot(temp.path());
        let active = std::fs::read(active_path(temp.path())).unwrap();
        let fired = Arc::new(AtomicBool::new(false));
        let hook_fired = fired.clone();
        store
            .remote_proxies
            .lock()
            .archives
            .set_test_hook(move |at| {
                // An action error still runs the safety postlude. Also stop
                // that recovery to retain a real pre-SQL pending intent.
                if point == ArchiveTestPoint::AfterPendingWrite
                    && at == ArchiveTestPoint::BeforeRecoverPending
                    && hook_fired.load(Ordering::SeqCst)
                {
                    anyhow::bail!("injected archive boundary failure");
                }
                if at == point && !hook_fired.swap(true, Ordering::SeqCst) {
                    anyhow::bail!("injected archive boundary failure");
                }
                Ok(())
            });
        let error = store
            .archive_remote_proxy(&record.local_session_id, None)
            .unwrap_err();
        assert_eq!(
            error.to_string(),
            "injected archive boundary failure",
            "{point:?}"
        );
        assert!(fired.load(Ordering::SeqCst));
        let failed = raw_snapshot(temp.path());
        assert_eq!(failed.incarnation, before.incarnation);
        assert_eq!(failed.records[0], before.records[0]);
        assert!(
            before
                .retired
                .iter()
                .all(|row| failed.retired.contains(row))
        );
        let committed = matches!(
            point,
            ArchiveTestPoint::AfterInsertSeal | ArchiveTestPoint::AfterCompleteAnchorWrite
        );
        assert_eq!(failed.records.len(), 1 + usize::from(committed));
        assert_eq!(failed.retired.len(), 2 + 2 * usize::from(committed));
        if committed {
            assert_eq!(failed.records[1].0, 2);
            assert_eq!(failed.records[1].1, record.local_session_id);
            assert_eq!(failed.records[1].2, serde_json::to_vec(&record).unwrap());
            for retired in &record.retired_requests {
                assert!(failed.retired.contains(&(
                    retired.client_request_id.clone(),
                    retired.task_id.clone(),
                    record.local_session_id.clone()
                )));
            }
        }
        if matches!(
            point,
            ArchiveTestPoint::AfterPendingWrite | ArchiveTestPoint::AfterInsertSeal
        ) {
            assert_eq!(
                failed.anchor["pending"],
                serde_json::to_value(&record).unwrap()
            );
            assert_eq!(failed.anchor["committed_count"], 1);
        } else {
            assert!(failed.anchor["pending"].is_null());
            assert_eq!(
                failed.anchor["committed_count"],
                if committed { 2 } else { 1 }
            );
        }
        assert_eq!(std::fs::read(active_path(temp.path())).unwrap(), active);
        assert!(store.remote_proxies.lock().load_failed);
        assert!(store.prepare_remote_proxy_open(None, &context()).is_err());
        assert!(
            store
                .archive_remote_proxy(&record.local_session_id, None)
                .is_err()
        );
        assert_local_usable(&store, temp.path());
        let restarted = make_store(temp.path());
        assert!(!restarted.remote_proxies.lock().load_failed);
        if point == ArchiveTestPoint::BeforePendingWrite {
            assert!(
                restarted
                    .archive_remote_proxy(&record.local_session_id, None)
                    .unwrap()
            );
        } else {
            assert!(
                restarted
                    .is_archived_remote_proxy(&record.local_session_id)
                    .unwrap()
            );
            assert!(
                restarted
                    .prepare_remote_proxy_open(Some(&record.local_session_id), &context())
                    .is_err()
            );
        }
        let recovered = raw_snapshot(temp.path());
        assert_eq!(recovered.anchor["committed_count"], 2);
        assert!(recovered.anchor["pending"].is_null());
        assert_eq!(recovered.records.len(), 2);
        assert_eq!(recovered.retired.len(), 4);
        drop(restarted);
        assert!(
            make_store(temp.path())
                .is_archived_remote_proxy(&record.local_session_id)
                .unwrap()
        );
        assert_eq!(raw_snapshot(temp.path()), recovered);
    }
}

#[test]
fn seal_conflicts_do_not_set_permanent_load_failure() {
    for domain in ["identity", "reference", "client", "task", "duplicate"] {
        let temp = tempfile::tempdir().unwrap();
        let _home = isolate_home(temp.path());
        let (store, record) = fixture(temp.path());
        // Schema-valid competing snapshots exercise each defensive domain;
        // they are not claims that random-ID producers naturally collide.
        let mut conflict = record.clone();
        match domain {
            "identity" => conflict.ui.title = "Other immutable snapshot".into(),
            "reference" => conflict.local_session_id = "remote-competing".into(),
            "client" | "task" => {
                conflict.local_session_id = "remote-competing".into();
                conflict.remote_session_ref = "competing-ref".into();
                conflict.retired_requests.truncate(1);
                if domain == "client" {
                    conflict.retired_requests[0].task_id = "competing-task".into();
                } else {
                    conflict.retired_requests[0].client_request_id = "competing-client".into();
                }
            }
            _ => {}
        }
        let path = bank_path(temp.path());
        let root = temp.path().to_path_buf();
        let observed = Arc::new(parking_lot::Mutex::new(None));
        let observed_hook = observed.clone();
        let fired = Arc::new(AtomicBool::new(false));
        let hit = fired.clone();
        store
            .remote_proxies
            .lock()
            .archives
            .set_test_hook(move |point| {
                if point == ArchiveTestPoint::BeforeSealLock && !hit.swap(true, Ordering::SeqCst) {
                    ArchiveStore::at(path.clone()).seal(&conflict).unwrap();
                    *observed_hook.lock() = Some(raw_snapshot(&root));
                }
                Ok(())
            });
        let result = store.archive_remote_proxy(&record.local_session_id, None);
        assert!(fired.load(Ordering::SeqCst));
        assert!(!store.remote_proxies.lock().load_failed, "{domain}");
        if domain == "duplicate" {
            assert!(result.unwrap());
            assert!(
                store
                    .remote_proxy_snapshot(&record.local_session_id)
                    .is_none()
            );
        } else {
            assert_eq!(
                result.unwrap_err().to_string(),
                "remote_proxy_archive_conflict"
            );
            // The real conflict remains a guard failure; removing a poison
            // flag must never bypass permanent archive authority.
            assert!(store.remote_proxies.lock().persist().is_err());
            assert!(
                store
                    .archive_remote_proxy(&record.local_session_id, None)
                    .is_err()
            );
        }
        assert_eq!(raw_snapshot(temp.path()), observed.lock().take().unwrap());
        assert_local_usable(&store, temp.path());
    }
}

#[test]
fn prior_pending_recovery_failure_is_indeterminate() {
    for competitor_point in [
        ArchiveTestPoint::AfterPendingWrite,
        ArchiveTestPoint::AfterInsertSeal,
    ] {
        let temp = tempfile::tempdir().unwrap();
        let _home = isolate_home(temp.path());
        let (store, record) = fixture(temp.path());
        let path = bank_path(temp.path());
        let competing_record = record.clone();
        let prepared = Arc::new(AtomicBool::new(false));
        let prepared_hook = prepared.clone();
        let failed = Arc::new(AtomicBool::new(false));
        let failed_hook = failed.clone();
        store
            .remote_proxies
            .lock()
            .archives
            .set_test_hook(move |point| {
                if point == ArchiveTestPoint::BeforeSealLock
                    && !prepared_hook.swap(true, Ordering::SeqCst)
                {
                    let competitor = ArchiveStore::at(path.clone());
                    competitor.set_test_hook(move |at| {
                        if at == competitor_point
                            || (competitor_point == ArchiveTestPoint::AfterPendingWrite
                                && at == ArchiveTestPoint::BeforeRecoverPending)
                        {
                            anyhow::bail!("competitor lost its result");
                        }
                        Ok(())
                    });
                    assert!(
                        competitor
                            .seal(&competing_record)
                            .unwrap_err()
                            .requires_quarantine()
                    );
                }
                if point == ArchiveTestPoint::BeforeRecoverPending
                    && !failed_hook.swap(true, Ordering::SeqCst)
                {
                    anyhow::bail!("prior recovery unavailable");
                }
                Ok(())
            });
        assert_eq!(
            store
                .archive_remote_proxy(&record.local_session_id, None)
                .unwrap_err()
                .to_string(),
            "prior recovery unavailable"
        );
        assert!(prepared.load(Ordering::SeqCst) && failed.load(Ordering::SeqCst));
        let raw = raw_snapshot(temp.path());
        assert_eq!(
            raw.anchor["pending"],
            serde_json::to_value(&record).unwrap()
        );
        assert_eq!(raw.anchor["committed_count"], 1);
        assert_eq!(
            raw.records.len(),
            if competitor_point == ArchiveTestPoint::AfterInsertSeal {
                2
            } else {
                1
            }
        );
        assert!(store.remote_proxies.lock().load_failed);
        assert!(store.prepare_remote_proxy_open(None, &context()).is_err());
        assert_local_usable(&store, temp.path());
        let restarted = make_store(temp.path());
        assert!(!restarted.remote_proxies.lock().load_failed);
        assert!(
            restarted
                .is_archived_remote_proxy(&record.local_session_id)
                .unwrap()
        );
        let recovered = raw_snapshot(temp.path());
        assert_eq!(recovered.records.len(), 2);
        assert_eq!(recovered.retired.len(), 4);
        assert_eq!(recovered.anchor["committed_count"], 2);
        assert!(recovered.anchor["pending"].is_null());
    }
}

#[test]
fn actual_anchor_write_and_sql_failures_remain_quarantined() {
    for sql_failure in [false, true] {
        let temp = tempfile::tempdir().unwrap();
        let _home = isolate_home(temp.path());
        let (store, record) = fixture(temp.path());
        let before = raw_snapshot(temp.path());
        let path = bank_path(temp.path());
        let anchor = path.with_extension("anchor.json");
        let backup = path.with_extension("test-backup.json");
        let anchor_hook = anchor.clone();
        let backup_hook = backup.clone();
        let fired = Arc::new(AtomicBool::new(false));
        let hit = fired.clone();
        store.remote_proxies.lock().archives.set_test_hook(move |point| {
            if point == ArchiveTestPoint::BeforePendingWrite && !hit.swap(true, Ordering::SeqCst) {
                if sql_failure {
                    rusqlite::Connection::open(&path)?.execute_batch(
                        "CREATE TRIGGER test_seal_failure BEFORE INSERT ON archives BEGIN SELECT RAISE(ABORT,'test transaction rejected'); END;",
                    )?;
                } else {
                    // Make the actual anchored writer fail, without replacing
                    // that writer or assuming chmod works for administrators.
                    std::fs::rename(&anchor_hook, &backup_hook)?;
                    std::fs::create_dir(&anchor_hook)?;
                }
            }
            Ok(())
        });
        assert!(
            store
                .archive_remote_proxy(&record.local_session_id, None)
                .is_err()
        );
        assert!(fired.load(Ordering::SeqCst));
        assert!(store.remote_proxies.lock().load_failed);
        if !sql_failure {
            std::fs::remove_dir(&anchor).unwrap();
            std::fs::rename(&backup, &anchor).unwrap();
        }
        let raw = raw_snapshot(temp.path());
        assert_eq!(raw.incarnation, before.incarnation);
        assert_eq!(raw.records, before.records);
        assert_eq!(raw.retired, before.retired);
        if sql_failure {
            assert_eq!(
                raw.anchor["pending"],
                serde_json::to_value(&record).unwrap()
            );
            rusqlite::Connection::open(bank_path(temp.path()))
                .unwrap()
                .execute_batch("DROP TRIGGER test_seal_failure")
                .unwrap();
        } else {
            assert_eq!(raw.anchor, before.anchor);
        }
        assert!(
            store
                .archive_remote_proxy(&record.local_session_id, None)
                .is_err()
        );
        assert_local_usable(&store, temp.path());
        let restarted = make_store(temp.path());
        assert!(!restarted.remote_proxies.lock().load_failed);
        if sql_failure {
            assert!(
                restarted
                    .is_archived_remote_proxy(&record.local_session_id)
                    .unwrap()
            );
        } else {
            assert!(
                restarted
                    .archive_remote_proxy(&record.local_session_id, None)
                    .unwrap()
            );
        }
        assert_eq!(raw_snapshot(temp.path()).anchor["committed_count"], 2);
    }
}

#[test]
fn seal_action_sql_read_failure_without_durable_work_still_quarantines() {
    let temp = tempfile::tempdir().unwrap();
    let _home = isolate_home(temp.path());
    let (store, record) = fixture(temp.path());
    let before = raw_snapshot(temp.path());
    assert!(before.anchor["pending"].is_null());
    let active = std::fs::read(active_path(temp.path())).unwrap();
    let path = bank_path(temp.path());
    let path_hook = path.clone();
    let fired = Arc::new(AtomicBool::new(false));
    let fired_hook = fired.clone();
    let reached_pending = Arc::new(AtomicBool::new(false));
    let pending_hook = reached_pending.clone();
    store
        .remote_proxies
        .lock()
        .archives
        .set_test_hook(move |point| {
            if point == ArchiveTestPoint::BeforeSealLock && !fired_hook.swap(true, Ordering::SeqCst)
            {
                // Earlier contains() used the healthy schema. UUID/count/anchor
                // validation needs no record column, so the real seal SELECT below
                // is the failure source, through From<rusqlite::Error>.
                rusqlite::Connection::open(&path_hook)?.execute_batch(
                    "ALTER TABLE archives RENAME COLUMN record TO unreadable_record",
                )?;
            }
            if point == ArchiveTestPoint::BeforePendingWrite {
                pending_hook.store(true, Ordering::SeqCst);
            }
            Ok(())
        });
    let error = store
        .archive_remote_proxy(&record.local_session_id, None)
        .unwrap_err();
    assert!(
        error.downcast_ref::<rusqlite::Error>().is_some(),
        "{error:?}"
    );
    assert!(fired.load(Ordering::SeqCst));
    assert!(!reached_pending.load(Ordering::SeqCst));
    assert!(store.remote_proxies.lock().load_failed);
    assert_eq!(std::fs::read(active_path(temp.path())).unwrap(), active);
    assert!(store.prepare_remote_proxy_open(None, &context()).is_err());
    // Repair only the test-owned fault. Neither the durable history nor the
    // anchor may change, and this process remains quarantined after repair.
    rusqlite::Connection::open(&path)
        .unwrap()
        .execute_batch("ALTER TABLE archives RENAME COLUMN unreadable_record TO record")
        .unwrap();
    assert_eq!(raw_snapshot(temp.path()), before);
    assert!(
        store
            .archive_remote_proxy(&record.local_session_id, None)
            .is_err()
    );
    assert_local_usable(&store, temp.path());
    let restarted = make_store(temp.path());
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert!(
        restarted
            .archive_remote_proxy(&record.local_session_id, None)
            .unwrap()
    );
    assert_eq!(raw_snapshot(temp.path()).anchor["committed_count"], 2);
}

#[test]
fn initialization_errors_are_not_safe_action_rejections() {
    for point in [
        ArchiveTestPoint::BeforeBankCreate,
        ArchiveTestPoint::BeforeInitialAnchorWrite,
    ] {
        let temp = tempfile::tempdir().unwrap();
        let _home = isolate_home(temp.path());
        let store = make_store(temp.path());
        let record = populated_record(&store, "first");
        let active = std::fs::read(active_path(temp.path())).unwrap();
        let fired = Arc::new(AtomicBool::new(false));
        let hit = fired.clone();
        store
            .remote_proxies
            .lock()
            .archives
            .set_test_hook(move |at| {
                if at == point && !hit.swap(true, Ordering::SeqCst) {
                    anyhow::bail!("initialization unavailable");
                }
                Ok(())
            });
        assert_eq!(
            store
                .archive_remote_proxy(&record.local_session_id, None)
                .unwrap_err()
                .to_string(),
            "initialization unavailable"
        );
        assert!(fired.load(Ordering::SeqCst));
        assert!(store.remote_proxies.lock().load_failed);
        assert_eq!(std::fs::read(active_path(temp.path())).unwrap(), active);
        assert!(store.prepare_remote_proxy_open(None, &context()).is_err());
        assert_local_usable(&store, temp.path());
        let restarted = make_store(temp.path());
        assert!(!restarted.remote_proxies.lock().load_failed);
        assert!(
            restarted
                .archive_remote_proxy(&record.local_session_id, None)
                .unwrap()
        );
    }
}

#[test]
fn cleanup_failure_still_reports_permanent_archive_success() {
    let temp = tempfile::tempdir().unwrap();
    let _home = isolate_home(temp.path());
    let (store, record) = fixture(temp.path());
    let path = active_path(temp.path());
    let backup = path.with_extension("test-backup.json");
    let active_hook = path.clone();
    let backup_hook = backup.clone();
    let fired = Arc::new(AtomicBool::new(false));
    let hit = fired.clone();
    store
        .remote_proxies
        .lock()
        .archives
        .set_test_hook(move |point| {
            if point == ArchiveTestPoint::AfterCompleteAnchorWrite
                && !hit.swap(true, Ordering::SeqCst)
            {
                std::fs::rename(&active_hook, &backup_hook)?;
                std::fs::create_dir(&active_hook)?;
            }
            Ok(())
        });
    assert!(
        !store
            .archive_remote_proxy(&record.local_session_id, None)
            .unwrap()
    );
    assert!(fired.load(Ordering::SeqCst));
    assert!(!store.remote_proxies.lock().load_failed);
    assert!(
        store
            .remote_proxy_snapshot(&record.local_session_id)
            .is_none()
    );
    let raw = raw_snapshot(temp.path());
    assert_eq!(raw.anchor["committed_count"], 2);
    assert_eq!(raw.retired.len(), 4);
    std::fs::remove_dir(&path).unwrap();
    std::fs::rename(&backup, &path).unwrap();
    let restarted = make_store(temp.path());
    assert!(!restarted.remote_proxies.lock().load_failed);
    assert!(
        restarted
            .remote_proxy_snapshot(&record.local_session_id)
            .is_none()
    );
    assert!(
        restarted
            .is_archived_remote_proxy(&record.local_session_id)
            .unwrap()
    );
    assert_eq!(raw_snapshot(temp.path()), raw);
}

#[test]
fn quarantine_blocks_new_mutations_but_preserves_bound_attempted_cancel() {
    let temp = tempfile::tempdir().unwrap();
    let _home = isolate_home(temp.path());
    let (store, record) = fixture(temp.path());
    let other = store
        .remote_proxies
        .lock()
        .records
        .values()
        .find(|item| item.local_session_id != record.local_session_id)
        .unwrap()
        .clone();
    let live = context();
    let fingerprint = "a".repeat(64);
    for (client, task) in [
        ("cancel-client", "cancel-task"),
        ("prepared-client", "prepared-task"),
    ] {
        store
            .insert_remote_pending(
                &other.local_session_id,
                &other.remote_session_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: client.into(),
                    task_id: task.into(),
                    request_fingerprint: fingerprint.clone(),
                    reservation_fingerprint: None,
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                },
            )
            .unwrap();
    }
    store
        .mark_remote_dispatch_attempt(
            &other.local_session_id,
            &other.remote_session_ref,
            &live,
            "cancel-client",
            "cancel-task",
            &fingerprint,
        )
        .unwrap();
    store.remote_proxies.lock().archives.set_test_hook(|point| {
        if point == ArchiveTestPoint::AfterInsertSeal {
            anyhow::bail!("commit response lost");
        }
        Ok(())
    });
    assert!(
        store
            .archive_remote_proxy(&record.local_session_id, None)
            .is_err()
    );
    let raw = raw_snapshot(temp.path());
    assert_eq!(raw.records.len(), 2);
    assert_eq!(
        raw.anchor["pending"],
        serde_json::to_value(&record).unwrap()
    );
    // The I/O dependency recovers, but the original process remains quarantined.
    store
        .remote_proxies
        .lock()
        .archives
        .set_test_hook(|_| Ok(()));
    assert!(store.prepare_remote_proxy_open(None, &live).is_err());
    assert!(
        store
            .mark_remote_dispatch_attempt(
                &other.local_session_id,
                &other.remote_session_ref,
                &live,
                "prepared-client",
                "prepared-task",
                &fingerprint
            )
            .is_err()
    );
    assert!(
        store
            .insert_remote_pending(
                &other.local_session_id,
                &other.remote_session_ref,
                &live,
                PendingRemoteRequest {
                    client_request_id: "new-client".into(),
                    task_id: "new-task".into(),
                    request_fingerprint: fingerprint.clone(),
                    reservation_fingerprint: None,
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                }
            )
            .is_err()
    );
    assert!(
        store
            .require_attempted_remote_task(
                &other.local_session_id,
                &other.remote_session_ref,
                &live,
                "cancel-task"
            )
            .is_ok()
    );
    assert!(
        store
            .require_attempted_remote_task(
                &other.local_session_id,
                &other.remote_session_ref,
                &live,
                "prepared-task"
            )
            .is_err()
    );
    let mut stale = live.clone();
    stale.controller_lease_id = "stale-lease".into();
    assert!(
        store
            .require_attempted_remote_task(
                &other.local_session_id,
                &other.remote_session_ref,
                &stale,
                "cancel-task"
            )
            .is_err()
    );
    assert!(
        store
            .require_attempted_remote_task(
                &record.local_session_id,
                &record.remote_session_ref,
                &live,
                "cancel-task"
            )
            .is_err()
    );
    assert!(store.remote_proxies.lock().load_failed);
    assert_local_usable(&store, temp.path());
}
