//! WAL contention must be resolved before the uninstall guard reads its snapshot.
use super::*;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::sync::mpsc::{self, Receiver, Sender};
use std::time::Duration;

thread_local! {
    static WAIT_FOR_WRITER: RefCell<Option<(Sender<()>, Receiver<()>)>> = const { RefCell::new(None) };
}

// SQLite calls this only when waiting for a lock can make progress. A deferred
// transaction's read-to-write upgrade returns BUSY without entering it. Channels
// prove that the delete is waiting before the other writer commits; no sleeps or
// timing-dependent race are needed to exercise the regression.
fn wait_for_writer(_: i32) -> bool {
    WAIT_FOR_WRITER.with(|slot| {
        slot.borrow_mut().take().is_some_and(|(waiting, release)| {
            waiting.send(()).is_ok() && release.recv_timeout(Duration::from_secs(10)).is_ok()
        })
    })
}

fn fixture() -> (PathBuf, String) {
    let root = config::data_dir().join("shared-download-root");
    fs::create_dir_all(&root).unwrap();
    fs::write(root.join("weights"), b"preserve shared model data").unwrap();
    add_store_model_with_root(
        AddStoreModelParams {
            card_id: "delete-target",
            model_name: "delete-target",
            card_name: "Delete target",
            org: None,
            description: None,
            engine_name: "sglang",
            engine_version: None,
            size_bytes: 11,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::default(),
            card_source: "store",
        },
        root.to_str().unwrap(),
    )
    .unwrap();
    let key = add_api_key("delete-target", Some("preserve until successful delete"))
        .unwrap()
        .key_value;
    add_api_key("other", Some("before concurrent writer")).unwrap();
    let mut reservation = super::super::super::version_store::DownloadReservation::acquire(
        "delete-target",
        &root,
        || false,
        || {},
    )
    .unwrap();
    reservation.publish(11).unwrap();
    reservation.start_scripts().unwrap();
    reservation.complete().unwrap();
    let conn = connect().unwrap();
    conn.execute(
        "INSERT INTO model_versions(card_id,version_id,state,runtime_card_id,created_at)
         VALUES('delete-target','active','active','delete-target','fixture')",
        [],
    )
    .unwrap();
    conn.execute(
        "UPDATE store_models SET active_version_id='active' WHERE card_id='delete-target'",
        [],
    )
    .unwrap();
    (root, key)
}

fn delete_after_competing_writer(add_retained_version: bool) -> Result<(), DeleteStoreModelError> {
    // Open both connections before acquiring the competing writer lock so this
    // handshake tests the delete transaction, not connect()'s WAL initialization.
    let mut deleting = connect().unwrap();
    let mut writing = connect().unwrap();
    let writer = writing
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .unwrap();
    writer
        .execute(
            "UPDATE api_keys SET label='concurrent writer committed' WHERE card_id='other'",
            [],
        )
        .unwrap();
    if add_retained_version {
        writer
            .execute(
                "INSERT INTO model_versions(card_id,version_id,state,runtime_card_id,created_at)
                 VALUES('delete-target','retained','ready','retained-runtime','fixture')",
                [],
            )
            .unwrap();
    }
    let (waiting_tx, waiting_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let worker = std::thread::spawn(move || {
        WAIT_FOR_WRITER.with(|slot| slot.replace(Some((waiting_tx, release_rx))));
        deleting.busy_handler(Some(wait_for_writer)).unwrap();
        delete_store_model_on(&mut deleting, "delete-target")
    });
    let waiting = waiting_rx.recv_timeout(Duration::from_secs(10));
    // Always release the writer and join before assertions, including when a
    // regression makes the worker return early instead of entering busy_handler.
    let committed = writer.commit();
    let _ = release_tx.send(());
    let result = worker.join().unwrap();
    committed.unwrap();
    assert!(
        waiting.is_ok(),
        "delete did not wait for the writer: {result:?}"
    );
    assert_eq!(
        list_api_keys_for_card("other").unwrap()[0].label.as_deref(),
        Some("concurrent writer committed")
    );
    result
}

fn reservation_rows(table: &str) -> i64 {
    connect()
        .unwrap()
        .query_row(
            &format!("SELECT COUNT(*) FROM {table} WHERE card_id='delete-target'"),
            [],
            |row| row.get(0),
        )
        .unwrap()
}

#[test]
fn delete_waits_for_wal_writer_then_atomically_removes_record_and_key() {
    let _home = setup_temp_db();
    let (root, key) = fixture();
    delete_after_competing_writer(false).unwrap();
    assert!(get_store_model("delete-target").unwrap().is_none());
    assert!(find_api_key(&key).unwrap().is_none());
    assert_eq!(reservation_rows("model_versions"), 0);
    assert_eq!(reservation_rows("model_download_reservations"), 0);
    assert_eq!(reservation_rows("model_download_retained_roots"), 1);
    assert_eq!(
        fs::read(root.join("weights")).unwrap(),
        b"preserve shared model data"
    );
}

#[test]
fn delete_guard_sees_retained_version_committed_by_preceding_wal_writer() {
    let _home = setup_temp_db();
    let (root, key) = fixture();
    let before = serde_json::to_value(get_store_model("delete-target").unwrap()).unwrap();
    assert!(matches!(
        delete_after_competing_writer(true),
        Err(DeleteStoreModelError::RetainedVersions)
    ));
    assert_eq!(
        serde_json::to_value(get_store_model("delete-target").unwrap()).unwrap(),
        before
    );
    assert!(find_api_key(&key).unwrap().is_some());
    assert_eq!(reservation_rows("model_versions"), 2);
    assert_eq!(reservation_rows("model_download_reservations"), 1);
    assert_eq!(reservation_rows("model_download_retained_roots"), 0);
    assert_eq!(
        fs::read(root.join("weights")).unwrap(),
        b"preserve shared model data"
    );
}

#[test]
fn null_active_retains_versions_with_a_domain_error_and_no_record_or_key_changes() {
    let _home = setup_temp_db();
    let (root, key) = fixture();
    let conn = connect().unwrap();
    conn.execute(
        "UPDATE store_models SET active_version_id=NULL WHERE card_id='delete-target'",
        [],
    )
    .unwrap();
    let rec = get_store_model("delete-target").unwrap().unwrap();
    let derived = crate::features::model_store::platform::linux::version_store::active_id(&rec);
    conn.execute(
        "UPDATE model_versions SET version_id=?1 WHERE card_id='delete-target' AND version_id='active'",
        [derived],
    ).unwrap();
    let before = serde_json::to_value(&rec).unwrap();
    let error = delete_store_model("delete-target").unwrap_err();
    assert!(matches!(error, DeleteStoreModelError::RetainedVersions));
    assert!(error.to_string().contains("先在历史版本中清理保留版本"));
    assert!(!error.to_string().contains("Query is not read-only"));
    assert_eq!(
        serde_json::to_value(get_store_model("delete-target").unwrap().unwrap()).unwrap(),
        before
    );
    assert!(find_api_key(&key).unwrap().is_some());
    assert_eq!(reservation_rows("model_versions"), 1);
    assert_eq!(reservation_rows("model_download_reservations"), 1);
    assert_eq!(reservation_rows("model_download_retained_roots"), 0);
    assert_eq!(
        fs::read(root.join("weights")).unwrap(),
        b"preserve shared model data"
    );
}

#[test]
fn delete_rolls_back_retirement_and_record_when_key_delete_fails() {
    let _home = setup_temp_db();
    let (root, key) = fixture();
    connect()
        .unwrap()
        .execute_batch(
            "CREATE TRIGGER reject_key_delete BEFORE DELETE ON api_keys
         WHEN old.card_id='delete-target' BEGIN SELECT RAISE(ABORT,'injected failure'); END;",
        )
        .unwrap();
    assert!(delete_store_model("delete-target").is_err());
    assert!(get_store_model("delete-target").unwrap().is_some());
    assert!(find_api_key(&key).unwrap().is_some());
    assert_eq!(reservation_rows("model_versions"), 1);
    assert_eq!(reservation_rows("model_download_reservations"), 1);
    assert_eq!(reservation_rows("model_download_retained_roots"), 0);
    assert_eq!(
        fs::read(root.join("weights")).unwrap(),
        b"preserve shared model data"
    );
}
