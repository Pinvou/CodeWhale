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
