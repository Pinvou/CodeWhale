//! Real AppEngine clone/custody tests with an op-mailbox/forwarder seam.
//! No model turn or external provider request is sent.
#![allow(clippy::await_holding_lock)]
use super::super::skill_materialization::{
    RemoteTurnOwner, RemoteTurnSkills, materialize_plain_remote_turn_skills,
};
use super::*;

struct TestHome {
    previous: Vec<(&'static str, Option<std::ffi::OsString>)>,
    directory: tempfile::TempDir,
}
impl TestHome {
    fn new() -> Self {
        let names = [
            "PINVOU3_HOME",
            "DEEPSEEK_API_KEY",
            "DEEPSEEK_BASE_URL",
            "DEEPSEEK_MODEL",
            "DEEPSEEK_PROVIDER",
        ];
        let previous = names
            .iter()
            .map(|name| (*name, std::env::var_os(name)))
            .collect();
        let directory = tempfile::tempdir().unwrap();
        // SAFETY: callers hold ENV_LOCK for the complete fixture.
        unsafe {
            std::env::set_var("PINVOU3_HOME", directory.path());
            for name in &names[1..] {
                std::env::remove_var(name);
            }
        }
        Self {
            previous,
            directory,
        }
    }
    fn snapshot(&self) -> Arc<RemoteTurnSkills> {
        Arc::new(
            materialize_plain_remote_turn_skills(
                "remote-custody",
                &[],
                Arc::new(RemoteTurnOwner::default()),
            )
            .unwrap(),
        )
    }
    async fn engine(
        &self,
        snapshot: Arc<RemoteTurnSkills>,
    ) -> (AppEngine, tokio::sync::mpsc::Receiver<Op>) {
        let mut bridge = Pinvou3Bridge::test_fixture(Some(SavedModel::builtin_llmapi()));
        bridge.workspace = self.directory.path().to_path_buf();
        bridge.runtime_model_credential_frozen = true;
        bridge.remote_turn_skills = Some(snapshot);
        let mut engine = AppEngine::spawn_headless(bridge).await.unwrap();
        // Shut down the real idle core. Replace only this wrapper's sender to
        // observe actual production custody/sync operations deterministically.
        engine.handle.send(Op::Shutdown).await.unwrap();
        let (sender, receiver) = tokio::sync::mpsc::channel(8);
        engine.handle.tx_op = sender;
        (engine, receiver)
    }
}
impl Drop for TestHome {
    fn drop(&mut self) {
        for (name, value) in &self.previous {
            // SAFETY: fixture drops before the caller releases ENV_LOCK.
            unsafe {
                match value {
                    Some(value) => std::env::set_var(name, value),
                    None => std::env::remove_var(name),
                }
            }
        }
    }
}
fn forwarder() -> (JoinHandle<()>, tokio::sync::oneshot::Receiver<()>) {
    let (sender, receiver) = tokio::sync::oneshot::channel();
    struct Finished(Option<tokio::sync::oneshot::Sender<()>>);
    impl Drop for Finished {
        fn drop(&mut self) {
            if let Some(sender) = self.0.take() {
                let _ = sender.send(());
            }
        }
    }
    let finished = Finished(Some(sender));
    let task = tokio::spawn(async move {
        let _finished = finished;
        std::future::pending::<()>().await;
    });
    (JoinHandle::Tokio(task), receiver)
}
fn entry(engine: AppEngine, forwarder: JoinHandle<()>, owner: Arc<RemoteTurnOwner>) -> EngineEntry {
    EngineEntry {
        engine,
        forwarder,
        runtime_model: PreparedRuntimeState::new(
            PreparedRuntimeModel::unchanged(SavedModel::builtin_llmapi()),
            true,
            0,
        ),
        remote_owner: Some(owner),
        mcp_config_revision: 0,
        spawned_at_ms: 0,
        steer_incarnation: 1,
        last_active_epoch_ms: AtomicU64::new(0),
    }
}
async fn assert_shutdown(receiver: &mut tokio::sync::mpsc::Receiver<Op>) {
    assert!(matches!(receiver.recv().await, Some(Op::CancelSubAgents)));
    assert!(matches!(receiver.recv().await, Some(Op::Shutdown)));
}

#[tokio::test(flavor = "current_thread")]
async fn remote_snapshot_bridge_and_engine_clones_hold_the_private_root() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let path = snapshot.path().to_owned();
    let (engine, _receiver) = home.engine(snapshot.clone()).await;
    let clone = engine.clone();
    let config = clone.bridge.build_engine_config_for_session_roots(
        "remote-custody",
        crate::features::sessions::session_roots_for("remote-custody", None),
    );
    assert_eq!(config.skills_dir, path);
    assert!(config.explicit_skills_root_only);
    drop(snapshot);
    drop(engine);
    assert!(path.is_dir());
    drop(clone);
    assert!(!path.exists());
}

#[tokio::test(flavor = "current_thread")]
async fn cancelled_cold_publication_queues_sync_then_reclaims_without_an_entry() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let owner = snapshot.owner().clone();
    let path = snapshot.path().to_owned();
    let (engine, mut receiver) = home.engine(snapshot).await;
    let (forwarder, finished) = forwarder();
    let custody = ColdEngineCustody::new(engine, forwarder);
    custody
        .resources
        .as_ref()
        .unwrap()
        .0
        .try_sync_new_session("remote-custody".into(), vec![])
        .unwrap();
    assert!(matches!(
        receiver.recv().await,
        Some(Op::SyncSession { .. })
    ));
    let entries = Mutex::new(HashMap::new());
    let held = entries.lock().await;
    let mut publishing = Box::pin(publish_cold_entry(
        &entries,
        "remote-custody",
        custody,
        |engine, forwarder| entry(engine, forwarder, owner),
    ));
    assert!(matches!(
        std::future::poll_fn(|cx| std::task::Poll::Ready(publishing.as_mut().poll(cx))).await,
        std::task::Poll::Pending
    ));
    drop(publishing);
    assert!(held.is_empty());
    drop(held);
    finished.await.unwrap();
    assert_shutdown(&mut receiver).await;
    assert!(!path.exists());
}

#[tokio::test(flavor = "current_thread")]
async fn failed_cold_sync_closes_forwarder_without_publishing_or_widening() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let path = snapshot.path().to_owned();
    let (engine, receiver) = home.engine(snapshot).await;
    drop(receiver);
    let (forwarder, finished) = forwarder();
    let custody = ColdEngineCustody::new(engine, forwarder);
    assert!(
        custody
            .resources
            .as_ref()
            .unwrap()
            .0
            .try_sync_new_session("remote-custody".into(), vec![])
            .is_err()
    );
    drop(custody);
    finished.await.unwrap();
    assert!(!path.exists());
}

#[tokio::test(flavor = "current_thread")]
async fn missing_remote_snapshot_never_self_heals_from_shared_enabled_skills() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let path = snapshot.path().to_owned();
    let (engine, _receiver) = home.engine(snapshot).await;
    let shared = crate::platform::paths::session_skills_dir("remote-custody");
    std::fs::create_dir_all(shared.join("broader-skill")).unwrap();
    // Exact fixture-owned private root; simulate its external deletion.
    assert!(path.starts_with(home.directory.path()));
    std::fs::remove_dir_all(&path).unwrap();
    let error = engine
        .bridge
        .build_send_message_op(
            "remote-custody",
            "probe".into(),
            AppMode::Agent,
            None,
            false,
        )
        .unwrap_err();
    assert_eq!(error.to_string(), "task_skill_unavailable");
    assert!(!path.exists());
    assert!(shared.join("broader-skill").exists());
}

#[test]
fn cold_custody_drop_with_full_mailbox_and_no_entered_runtime_does_not_panic() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let snapshot = home.snapshot();
    let path = snapshot.path().to_owned();
    let (custody, finished, _receiver) = runtime.block_on(async {
        let (engine, receiver) = home.engine(snapshot).await;
        for _ in 0..8 {
            engine.handle.tx_op.try_send(Op::Shutdown).unwrap();
        }
        let (forwarder, finished) = forwarder();
        (
            ColdEngineCustody::new(engine, forwarder),
            finished,
            receiver,
        )
    });
    assert!(tokio::runtime::Handle::try_current().is_err());
    drop(custody);
    runtime.block_on(finished).unwrap();
    assert!(!path.exists());
}

#[tokio::test(flavor = "current_thread")]
async fn cancelled_send_after_publication_keeps_root_until_exact_owner_cleanup() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let owner = snapshot.owner().clone();
    let path = snapshot.path().to_owned();
    let (engine, mut receiver) = home.engine(snapshot).await;
    let (forwarder, finished) = forwarder();
    let entries = Mutex::new(HashMap::new());
    let engine = publish_cold_entry(
        &entries,
        "remote-custody",
        ColdEngineCustody::new(engine, forwarder),
        |engine, forwarder| entry(engine, forwarder, owner.clone()),
    )
    .await;
    // The seam's mailbox is full: exercise the real reserved-send cancellation
    // suspension point without letting a model operation reach a provider.
    for _ in 0..8 {
        engine.handle.tx_op.try_send(Op::Shutdown).unwrap();
    }
    let mut sending = Box::pin(engine.send_headless_user_message(
        "private snapshot probe".into(),
        AppMode::Agent,
        None,
        false,
    ));
    assert!(matches!(
        std::future::poll_fn(|cx| std::task::Poll::Ready(sending.as_mut().poll(cx))).await,
        std::task::Poll::Pending
    ));
    assert!(path.is_dir());
    drop(sending);
    drop(engine);
    let removed =
        take_remote_owned_entry(&mut *entries.lock().await, "remote-custody", &owner).unwrap();
    assert!(
        path.is_dir(),
        "published entry retains the private root after send cancellation"
    );
    drop(ColdEngineCustody::new(removed.engine, removed.forwarder));
    finished.await.unwrap();
    // Full-channel fallback keeps only this old wrapper while bounded sends
    // deliver FIFO cleanup; no gate or newer entry is held.
    for _ in 0..8 {
        assert!(matches!(receiver.recv().await, Some(Op::Shutdown)));
    }
    assert_shutdown(&mut receiver).await;
    tokio::task::yield_now().await;
    assert!(!path.exists());
}

#[tokio::test(flavor = "current_thread")]
async fn old_remote_cleanup_cannot_take_new_owner_or_invalidate_its_reservation() {
    let _lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let home = TestHome::new();
    let snapshot = home.snapshot();
    let owner = snapshot.owner().clone();
    let stale = Arc::new(RemoteTurnOwner::default());
    let (engine, _receiver) = home.engine(snapshot).await;
    let lifecycle = Arc::new(TurnLifecycle::default());
    let reservation = lifecycle.reserve().unwrap();
    let (forwarder, finished) = forwarder();
    let mut entries = HashMap::from([(
        "remote-custody".to_owned(),
        entry(engine, forwarder, owner.clone()),
    )]);
    assert!(!remote_owner_matches(Some(&owner), None));
    assert!(!remote_owner_matches(None, Some(&owner)));
    assert!(!remote_owner_matches(Some(&owner), Some(&stale)));
    assert!(remote_owner_matches(Some(&owner), Some(&owner)));
    assert!(take_remote_owned_entry(&mut entries, "remote-custody", &stale).is_none());
    assert!(take_remote_owned_entry(&mut entries, "missing", &stale).is_none());
    assert!(reservation.ensure_active().is_ok());
    assert_eq!(entries.len(), 1);
    // A B-local replacement has no remote owner. Old cleanup must also be a
    // no-op in this case, not the ordinary evict(None) invalidation fallback.
    entries.get_mut("remote-custody").unwrap().remote_owner = None;
    assert!(take_remote_owned_entry(&mut entries, "remote-custody", &owner).is_none());
    assert!(reservation.ensure_active().is_ok());
    entries.get_mut("remote-custody").unwrap().remote_owner = Some(owner.clone());
    let removed = take_remote_owned_entry(&mut entries, "remote-custody", &owner).unwrap();
    removed.forwarder.abort();
    finished.await.unwrap();
}

#[tokio::test]
async fn fixed_remote_cancel_never_targets_a_successor_generation() {
    // Exercise the actual cancellation helper at all three lookup boundaries,
    // including a successor that exists BEFORE remote cancellation is invoked.
    for switch_at in 0..=2 {
        let locks = SessionTurnLocks::default();
        let lifecycles = SessionTurnLifecycles::default();
        let shell_tasks = SessionTurnShellTasks::default();
        let sid = "fixed-remote-cancel";
        let lifecycle = lifecycles.for_session(sid);
        let old = lifecycle.reserve().unwrap();
        let generation = old.generation();
        let successor = Arc::new(std::sync::Mutex::new(None));
        if switch_at == 0 {
            assert!(lifecycle.finish_unsubmitted_once());
            *successor.lock().unwrap() = Some(lifecycle.reserve().unwrap());
        }
        let lookups = std::sync::atomic::AtomicUsize::new(0);
        let cancels = std::sync::atomic::AtomicUsize::new(0);
        let cascades = std::sync::atomic::AtomicUsize::new(0);
        let claims = std::sync::atomic::AtomicUsize::new(0);
        let result = cancel_turn_with_gates(
            &locks,
            &lifecycles,
            &shell_tasks,
            sid,
            Some(generation),
            deepseek_tui::core::engine::CancelMode::StopDropInbox,
            || {
                let lookup = lookups.fetch_add(1, Ordering::SeqCst) + 1;
                let lifecycle = lifecycle.clone();
                let successor = successor.clone();
                async move {
                    // The lifecycle changes during the awaited engine lookup.
                    if lookup == switch_at {
                        assert!(lifecycle.finish_unsubmitted_once());
                        *successor.lock().unwrap() = Some(lifecycle.reserve().unwrap());
                    }
                    if switch_at == 2 && lookup == 1 {
                        None
                    } else {
                        Some(())
                    }
                }
            },
            |_: &(), _| {
                cancels.fetch_add(1, Ordering::SeqCst);
            },
            |_: &()| {
                cascades.fetch_add(1, Ordering::SeqCst);
                std::future::ready(())
            },
            |_, target| {
                assert_eq!(target, generation);
                claims.fetch_add(1, Ordering::SeqCst);
                // Keep the old turn alive to exercise phase two's await.
                false
            },
        )
        .await;
        assert_eq!(result, (Some(generation), false));
        assert_eq!(cancels.load(Ordering::SeqCst), 0, "boundary {switch_at}");
        assert_eq!(cascades.load(Ordering::SeqCst), 0, "boundary {switch_at}");
        assert_eq!(claims.load(Ordering::SeqCst), usize::from(switch_at == 2));
        let successor = successor.lock().unwrap().take().unwrap();
        assert_ne!(successor.generation(), generation);
        assert!(successor.ensure_active().is_ok());
        drop(old);
        assert!(successor.ensure_active().is_ok());
    }
}
