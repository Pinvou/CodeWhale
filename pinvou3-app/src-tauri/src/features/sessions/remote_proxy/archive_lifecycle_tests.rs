//! Real SessionStore lifecycle, OS ownership and durable recovery regressions.
use super::super::archive::{ArchiveStore, ArchiveTestPoint};
use super::*;
use std::io::{BufRead, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::AtomicBool;

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
#[ignore = "private child-process entry; invoked by seal_lock_contention_remains_retryable"]
fn archive_lock_child() {
    let path = std::env::var_os("PINVOU_ARCHIVE_LOCK_HELPER").expect("child-only path");
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(path)
        .unwrap();
    let mut ownership = fd_lock::RwLock::new(file);
    let _guard = ownership.try_write().unwrap();
    println!("PINVOU_LOCK_READY");
    std::io::stdout().flush().unwrap();
    let mut release = String::new();
    assert!(std::io::stdin().read_line(&mut release).unwrap() > 0);
    assert_eq!(release.trim(), "release");
}

fn spawn_lock_child(path: &Path) -> Child {
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "features::sessions::remote_proxy::tests::archive_lifecycle::archive_lock_child",
            "--ignored",
            "--nocapture",
            "--test-threads=1",
        ])
        .env("PINVOU_ARCHIVE_LOCK_HELPER", path)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let mut output = std::io::BufReader::new(child.stdout.take().unwrap());
    loop {
        let mut line = String::new();
        assert!(
            output.read_line(&mut line).unwrap() > 0,
            "child exited before locking"
        );
        if line.contains("PINVOU_LOCK_READY") {
            break;
        }
    }
    child.stdout = Some(output.into_inner());
    child
}

#[test]
fn seal_lock_contention_remains_retryable() {
    let temp = tempfile::tempdir().unwrap();
    let _home = isolate_home(temp.path());
    let (store, record) = fixture(temp.path());
    let before = raw_snapshot(temp.path());
    let active = std::fs::read(active_path(temp.path())).unwrap();
    let child = Arc::new(parking_lot::Mutex::new(None::<Child>));
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
    writeln!(child.stdin.as_mut().unwrap(), "release").unwrap();
    assert!(child.wait().unwrap().success());
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
