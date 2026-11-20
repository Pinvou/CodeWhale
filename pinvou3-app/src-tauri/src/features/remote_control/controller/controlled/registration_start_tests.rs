//! Real operation admission, including the lock spanning its startup commit.
use super::*;
use std::sync::{Arc, mpsc};
use std::thread::JoinHandle;
use std::time::Duration;

#[test]
fn admission_rejects_retired_identity_refresh_intent_and_nonregistering_phase() {
    let manager = ControlledDeviceManager::default();
    let old = manager.start(None, Vec::new()).unwrap();
    let mut foreign_intent = old.clone();
    foreign_intent.intent += 1;
    let mut foreign_generation = old.clone();
    foreign_generation.generation = "different-generation".into();
    for foreign in [foreign_intent, foreign_generation] {
        assert_eq!(
            manager.with_registration_start(&foreign, || -> std::result::Result<(), String> {
                panic!("a foreign operation reached startup")
            }),
            Err("desktop_remote_stale_result".to_owned())
        );
        assert_eq!(manager.current_operation(), Some(old.clone()));
    }
    let retiring = manager.begin_refresh().unwrap();
    assert_eq!(retiring.generation(), old.generation());
    assert_ne!(retiring.intent, old.intent);
    assert_eq!(
        manager.with_registration_start(&old, || Ok(())),
        Err("desktop_remote_stale_result".to_owned())
    );
    assert_eq!(
        manager.with_registration_start(&retiring, || Ok(())),
        Err("desktop_remote_phase_invalid".to_owned())
    );
    let next = manager.complete_refresh_revoked(retiring).unwrap();
    assert_ne!(next.generation(), old.generation());
    assert_eq!(manager.with_registration_start(&next, || Ok(7)), Ok(7));
    super::tests::register(&manager, &next);
    assert_eq!(
        manager.with_registration_start(&next, || Ok(())),
        Err("desktop_remote_phase_invalid".to_owned())
    );
    manager.stop();
    assert_eq!(manager.status().kind(), ControlledStatusKind::Stopped);
    assert_eq!(
        manager.with_registration_start(&next, || Ok(())),
        Err("desktop_remote_stale_result".to_owned())
    );
}

struct AdmissionAndStop {
    release: Option<mpsc::Sender<()>>,
    admission: Option<JoinHandle<std::result::Result<(), String>>>,
    stop: Option<JoinHandle<ControlledStatus>>,
}

impl AdmissionAndStop {
    fn finish(mut self) -> ControlledStatus {
        if let Some(release) = self.release.take() {
            let _ = release.send(());
        }
        self.admission.take().unwrap().join().unwrap().unwrap();
        self.stop.take().unwrap().join().unwrap()
    }
}

impl Drop for AdmissionAndStop {
    fn drop(&mut self) {
        // A failed assertion must release the exact admission before joining
        // the Stop thread that may legitimately be waiting for its guard.
        if let Some(release) = self.release.take() {
            let _ = release.send(());
        }
        if let Some(admission) = self.admission.take() {
            let _ = admission.join();
        }
        if let Some(stop) = self.stop.take() {
            let _ = stop.join();
        }
    }
}

#[test]
fn admission_holds_the_operation_guard_through_the_real_commit_callback() {
    let manager = Arc::new(ControlledDeviceManager::default());
    let operation = manager.start(None, Vec::new()).unwrap();
    let admitted = manager.clone();
    let start_operation = operation.clone();
    let (entered, observed) = mpsc::channel();
    let (release, released) = mpsc::channel();
    let admission = std::thread::spawn(move || {
        admitted.with_registration_start(&start_operation, || {
            entered.send(()).unwrap();
            released.recv_timeout(Duration::from_secs(10)).unwrap();
            Ok(())
        })
    });
    let mut pending = AdmissionAndStop {
        release: Some(release),
        admission: Some(admission),
        stop: None,
    };
    observed.recv_timeout(Duration::from_secs(10)).unwrap();
    let probe = manager.clone();
    let could_enter =
        std::thread::spawn(move || probe.registration_start_guard_available_for_test())
            .join()
            .unwrap();
    let stopping = manager.clone();
    pending.stop = Some(std::thread::spawn(move || stopping.stop()));
    assert!(
        !could_enter,
        "the registration commit must exclude concurrent operation retirement"
    );
    assert_eq!(pending.finish().kind(), ControlledStatusKind::Stopped);
    assert_eq!(
        manager.with_registration_start(&operation, || Ok(())),
        Err("desktop_remote_stale_result".to_owned())
    );
    let next = manager.start(None, Vec::new()).unwrap();
    assert_eq!(manager.with_registration_start(&next, || Ok(())), Ok(()));
}

#[test]
fn admission_preserves_startup_results_and_releases_the_guard_on_error() {
    let manager = ControlledDeviceManager::default();
    let operation = manager.start(None, Vec::new()).unwrap();
    assert_eq!(
        manager.with_registration_start(&operation, || Ok(42)),
        Ok(42)
    );
    assert_eq!(
        manager.with_registration_start(&operation, || {
            Err::<(), _>("desktop_controller_relay_busy".to_owned())
        }),
        Err("desktop_controller_relay_busy".to_owned())
    );
    assert!(manager.inner.try_lock().is_some());
    assert_eq!(manager.stop().kind(), ControlledStatusKind::Stopped);
    let next = manager.start(None, Vec::new()).unwrap();
    assert_eq!(manager.with_registration_start(&next, || Ok(())), Ok(()));
}
