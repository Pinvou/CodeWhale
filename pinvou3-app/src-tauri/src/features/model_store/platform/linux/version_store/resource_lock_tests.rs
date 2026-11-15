use super::*;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
    mpsc,
};
use std::time::Duration;

#[test]
fn waiting_writer_proceeds_after_readers_finish() {
    let _temp = super::super::config::TempHome::new();
    let reader = resource_operation_lock(false).unwrap();
    let (waiting_tx, waiting_rx) = mpsc::channel();
    let (acquired_tx, acquired_rx) = mpsc::channel();
    let waiter = std::thread::spawn(move || {
        let _lock = wait_resource_operation_lock(
            true,
            || false,
            || {
                waiting_tx.send(()).unwrap();
            },
        )
        .unwrap();
        acquired_tx.send(()).unwrap();
    });
    waiting_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(
        acquired_rx
            .recv_timeout(Duration::from_millis(100))
            .is_err()
    );
    drop(reader);
    acquired_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    waiter.join().unwrap();
}

#[test]
fn blocked_operations_can_be_cancelled_without_mutation() {
    let _temp = super::super::config::TempHome::new();
    let writer = resource_operation_lock(true).unwrap();
    let cancelled = Arc::new(AtomicBool::new(false));
    let flag = cancelled.clone();
    let (waiting_tx, waiting_rx) = mpsc::channel();
    let waiter = std::thread::spawn(move || {
        wait_resource_operation_lock(
            false,
            || flag.load(Ordering::SeqCst),
            || {
                waiting_tx.send(()).unwrap();
            },
        )
    });
    waiting_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    cancelled.store(true, Ordering::SeqCst);
    assert!(waiter.join().unwrap().unwrap_err().contains("已取消"));
    drop(writer);
    assert!(resource_operation_lock(true).is_ok());
}

#[test]
fn all_version_lock_openers_refuse_links_without_changing_the_target() {
    use std::os::unix::fs::{PermissionsExt, symlink};
    let home = super::super::config::TempHome::new();
    super::super::db::init_db().unwrap();
    let target = home.path().join("unrelated");
    std::fs::write(&target, b"do not touch").unwrap();
    std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o644)).unwrap();
    let root = super::super::config::data_dir();
    for name in ["version-operations.lock", "version-quota.lock"] {
        symlink(&target, root.join(name)).unwrap();
    }
    assert!(resource_operation_lock(true).is_err());
    assert!(wait_quota_reservation_lock(|| false, || {}).is_err());
    let recovery = root.join("operation-locks");
    std::fs::create_dir_all(&recovery).unwrap();
    symlink(&target, recovery.join("card.lock")).unwrap();
    assert!(recover_interrupted("card").is_err());
    assert_eq!(std::fs::read(&target).unwrap(), b"do not touch");
    assert_eq!(
        std::fs::metadata(target).unwrap().permissions().mode() & 0o777,
        0o644
    );
}

#[test]
fn version_lock_files_are_tightened_in_place_at_their_existing_paths() {
    use std::os::unix::fs::{MetadataExt, PermissionsExt};
    let _home = super::super::config::TempHome::new();
    super::super::db::init_db().unwrap();
    let root = super::super::config::data_dir();
    for name in [
        "version-operations.lock",
        "version-quota.lock",
        "operation-locks/card.lock",
    ] {
        let path = root.join(name);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, []).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        let inode = path.metadata().unwrap().ino();
        let _file = if name.starts_with("operation-locks") {
            recover_interrupted("card").unwrap();
            None
        } else {
            Some(operation_lock_file(name).unwrap())
        };
        let metadata = path.metadata().unwrap();
        assert_eq!(metadata.ino(), inode);
        assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
    }
}
