//! Process-private Windows application journal consumers. No provider turn or
//! renderer is started. Resume phases use the existing domain transport fixture;
//! the separate reader regression covers signed Relay/A wire authentication.
use super::*;
use crate::features::remote_control::controller::{
    controlled::{
        ControlledDeviceManager, ControlledLease, ControlledRegistration, ControlledResumeStep,
        ControlledStatusKind,
    },
    encrypted_lane::ControlledLaneState,
    journal::{JournalKind, JournalOwner},
    permission::PermissionAuthority,
    protocol::LeaseContext,
    relay_runtime::{ControlledBusinessPort, ControlledOutboundState},
    task::{
        AdmissionOutcome, RemoteTaskLedger, RemoteTaskRequest, RemoteTaskTerminal,
        TaskLeaseBinding, task_input_digest_for_value,
    },
};
use parking_lot::Mutex;
use std::time::{Duration, Instant};
use tauri::Listener as _;

pub(in crate::app::commands) fn host(
    probe: Option<Arc<crate::features::remote_control::controller::relay_runtime::reader_resume_tests::journal_owner_tests::JournalReplayBarrier>>,
) -> (AppHandle, Arc<dyn ControlledBusinessPort>, tauri::App) {
    let mut context = crate::build_tauri_context();
    context.config_mut().app.windows.clear();
    let builder = tauri::Builder::default()
        .any_thread()
        .manage(ControlledDeviceManager::default())
        .manage(ControlledLaneState::default())
        .manage(ControlledOutboundState::default())
        .manage(Arc::new(PermissionAuthority::default()));
    let builder = if let Some(probe) = probe {
        builder.manage(probe)
    } else {
        builder
    };
    let app = builder.build(context).unwrap();
    let handle = app.handle().clone();
    let store = SessionStore::boot().unwrap();
    let pool = EnginePool::new_with_dependencies(
        handle.clone(),
        store.clone(),
        Arc::new(|_, _| Vec::new()),
        Arc::new(|_| Vec::new()),
    )
    .unwrap();
    let acp = AcpPool::new(handle.clone(), store.clone()).unwrap();
    let ledger = Arc::new(
        RemoteTaskLedger::open_recovered(
            crate::platform::paths::sessions_root().join("_remote_control_tasks.json"),
            0,
        )
        .unwrap(),
    );
    let tasks = DesktopRemoteTaskRuntime::new(
        handle.clone(),
        store,
        pool,
        acp,
        super::super::desktop_remote_model::shared_model_store(),
        ledger,
    );
    let history =
        super::super::desktop_remote_journal::DesktopRemoteJournalRuntime::from_controlled_manager(
            &handle.state::<ControlledDeviceManager>(),
        );
    tasks.hooks().set_journal(Arc::new(
        super::super::desktop_remote_journal::DesktopTaskEventSink::new(
            handle.clone(),
            history.journal().clone(),
            tasks.ledger(),
        ),
    ));
    handle.manage(tasks.ledger());
    handle.manage(tasks);
    handle.manage(history);
    handle
        .state::<ControlledDeviceManager>()
        .register_cleanup_sink(Arc::new(
            super::super::desktop_remote_controller_business::DesktopControlledCleanup::new(
                handle.clone(),
            ),
        ));
    let business: Arc<dyn ControlledBusinessPort> = Arc::new(
        super::super::desktop_remote_controller_business::DesktopControlledBusiness {
            app: handle.clone(),
        },
    );
    (handle, business, app)
}

fn admit_task(handle: &AppHandle, lease: &LeaseContext, remote: &str, index: usize) -> String {
    let runtime = handle.state::<Arc<DesktopRemoteTaskRuntime>>();
    let session = runtime
        .service()
        .ensure_unclaimed_remote_session(
            remote,
            "Phase regression",
            &lease.remote_control_generation,
        )
        .unwrap();
    let task = format!("journal-phase-task-{index}");
    let input = json!({"text":"accepted local work","attachment_ids":[],"skill_ids":[]});
    let request = RemoteTaskRequest {
        lease: TaskLeaseBinding::from_context(lease),
        task_id: task.clone(),
        client_request_id: format!("journal-phase-request-{index}"),
        session_id: session.clone(),
        capability_revision: "7".into(),
        mode: "chat".into(),
        workspace_id: None,
        model_source: "controlled_device".into(),
        input_digest: task_input_digest_for_value(&input).unwrap(),
        input,
    };
    assert!(matches!(
        runtime
            .ledger()
            .admit(lease, &request, 100 + index as u64)
            .unwrap(),
        AdmissionOutcome::Admitted(_)
    ));
    session
}

fn emit_admitted_task(handle: &AppHandle, owner: &JournalOwner, session: &str, index: usize) {
    let runtime = handle.state::<Arc<DesktopRemoteTaskRuntime>>();
    let task = format!("journal-phase-task-{index}");
    let manager = handle.state::<ControlledDeviceManager>();
    let journal = manager.journal();
    let before = journal.snapshot_for_owner(owner, session).unwrap();
    // This is the actual executor event adapter, with the real generation,
    // session map and admitted ledger record; no provider/tool is dispatched.
    let body = format!("body-phase-{index}");
    TaskEventBridge {
        app: handle.clone(),
    }
    .emit_journal(
        JournalKind::AssistantDelta,
        session,
        Some(&task),
        json!({"text":body}),
    );
    // Complete through the actual service cleanup -> hooks -> DesktopTaskEventSink.
    runtime
        .service()
        .complete(
            &task,
            RemoteTaskTerminal::Succeeded,
            None,
            200 + index as u64,
        )
        .unwrap();
    let after = journal.snapshot_for_owner(owner, session).unwrap();
    assert_eq!(after.epoch, before.epoch);
    assert!(after.sequence > before.sequence);
    assert_eq!(
        after
            .events
            .iter()
            .filter(|e| e.task_id.as_deref() == Some(task.as_str())
                && e.kind == "assistant_delta"
                && e.payload["text"] == body)
            .count(),
        1
    );
    assert_eq!(
        after
            .events
            .iter()
            .filter(|e| e.task_id.as_deref() == Some(task.as_str())
                && e.terminal_status.as_deref() == Some("succeeded"))
            .count(),
        1
    );
    assert!(after.verify_digest());
}

#[test]
fn journal_owner_app_records_body_and_terminal_through_same_generation_resume() {
    const CHILD: &str = "PINVOU_JOURNAL_PHASE_TEST_CHILD";
    if std::env::var(CHILD).as_deref() != Ok("1") {
        let filter = format!(
            "{}::journal_owner_app_records_body_and_terminal_through_same_generation_resume",
            module_path!().split_once("::").unwrap().1
        );
        let output =
            std::env::temp_dir().join(format!("journal-phase-{}.txt", uuid::Uuid::new_v4()));
        let file = std::fs::File::create(&output).unwrap();
        let mut child = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", &filter, "--nocapture"])
            .env(CHILD, "1")
            .stdout(file.try_clone().unwrap())
            .stderr(file)
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(45);
        let status = loop {
            if let Some(status) = child.try_wait().unwrap() {
                break status;
            }
            if Instant::now() >= deadline {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("journal phase child exceeded watchdog");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        let raw = std::fs::read_to_string(&output).unwrap();
        assert!(status.success(), "isolated phase regression failed: {raw}");
        assert!(
            raw.contains("test result: ok. 1 passed;"),
            "zero-match child is not proof: {raw}"
        );
        return;
    }
    let directory = tempfile::tempdir().unwrap();
    unsafe {
        std::env::set_var("PINVOU3_HOME", directory.path());
        std::env::set_var("CODEWHALE_HOME", directory.path().join("cw"));
    }
    let tokio = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    tokio.block_on(async {
        let (handle,_business,_app)=host(None);
        let bodies=Arc::new(Mutex::new(Vec::<Value>::new()));
        let captured=bodies.clone();
        let listener=handle.listen(TASK_CONTENT_EVENT,move |event| {
            captured.lock().push(serde_json::from_str(event.payload()).unwrap());
        });
        let manager=handle.state::<ControlledDeviceManager>();
        let operation=manager.start(None,Vec::new()).unwrap();
        manager.complete_registration(operation.clone(),ControlledRegistration {
            pairing_code:"2345678".into(),sealed_code_b64:"sealed".into(),expires_at_ms:30_000,
        },1_000).unwrap();
        manager.mark_pairing_reserved(operation.clone()).unwrap();
        manager.mark_handshake_pending(operation.clone()).unwrap();
        let lease=LeaseContext {
            connection_id:"connection-0001".into(),controller_lease_id:"lease-0001".into(),
            lease_epoch:"1".into(),relay_epoch:"relay-epoch-0001".into(),remote_control_generation:operation.generation().into(),
        };
        let capabilities=vec!["encrypted_frames_v1".into(),"pairing_v1".into(),"task_v1".into()];
        manager.complete_connected(operation.clone(),ControlledLease {
            context:lease.clone(),liveness_epoch_a:"live-a-1".into(),resume_deadline_b:30_000,
            resume_token:[1;32],transport_key_fingerprint:[2;32],controller_device_name:"Controller".into(),
            connected_at_b:3_000,negotiated_capabilities:capabilities.clone(),
        }).unwrap();
        // All work is durably admitted while Connected, before the transient
        // outage. Different conversations preserve the one-turn-per-session gate.
        let sessions: Vec<_>=(0..8).map(|i|admit_task(&handle,&lease,&format!("remote-phase-{i}"),i)).collect();
        let journal=manager.journal();
        let (_,owner)=manager.local_journal_context().unwrap();
        emit_admitted_task(&handle,&owner,&sessions[0],0);
        let baseline=journal.snapshot_for_owner(&owner,&sessions[0]).unwrap();
        let transport=crate::features::remote_control::controller::test_support::resume_reader_transport(
            crate::features::remote_control::controller::relay_transport::ControllerRole::ControlledB,lease.clone(),capabilities);
        manager.replace_authenticated_transport(operation.clone(),transport.clone()).unwrap();
        crate::features::remote_control::controller::relay_transport::disconnect_test_transport(&transport);
        assert_eq!(serde_json::to_value(manager.mark_transport_disconnected(5_000)).unwrap()["status"],json!(ControlledStatusKind::Reconnecting));
        emit_admitted_task(&handle,&owner,&sessions[1],1);
        manager.begin_resume(operation.clone(),6_000).unwrap();
        let activation="activation-phase-1";
        for (index,step,expected) in [
            (2,ControlledResumeStep::Provisional{activation_id:activation},ControlledStatusKind::ResumeProvisional),
            (3,ControlledResumeStep::Confirmed{activation_id:activation},ControlledStatusKind::ResumeProvisional),
            (4,ControlledResumeStep::ActivationAcknowledged{activation_id:activation},ControlledStatusKind::ResumeProvisional),
            (5,ControlledResumeStep::PostActivationLiveness{activation_id:activation,liveness_epoch_b:"live-b-2"},ControlledStatusKind::ResumeActivatedPendingLiveness),
            (6,ControlledResumeStep::ReadyAcknowledged{activation_id:activation},ControlledStatusKind::ResumeActivatedPendingLiveness),
        ] {
            assert_eq!(serde_json::to_value(manager.advance_resume(operation.clone(),step,7_000+index as u64*500).unwrap()).unwrap()["status"],json!(expected));
            emit_admitted_task(&handle,&owner,&sessions[index],index);
        }
        crate::features::remote_control::controller::relay_transport::dispatch_test_transport_frame(&transport,
            &crate::features::remote_control::controller::test_support::resume_final_frame(&lease,activation,"request-phase-final"));
        manager.complete_resumed(operation,activation,"request-phase-final",12_000,"live:a-2",[3;32],[4;32],1_700_000_030_000).unwrap();
        let (_,resumed_owner)=manager.local_journal_context().unwrap();
        emit_admitted_task(&handle,&resumed_owner,&sessions[7],7);
        assert_eq!(journal.snapshot_for_owner(&owner,&sessions[0]).unwrap(),baseline);
        for session in &sessions {
            let window=journal.snapshot_for_owner(&owner,session).unwrap();
            assert_eq!(window.epoch,baseline.epoch);
            assert_eq!(window.events.len(),2);
            assert_eq!(journal.snapshot_for_owner(&resumed_owner,session).unwrap(),window);
        }
        // A next legal admission in the first conversation must still work:
        // terminal cleanup has released its turn without rotating the journal.
        let next_session=admit_task(&handle,&lease,"remote-phase-0",8);
        assert_eq!(next_session,sessions[0]);
        emit_admitted_task(&handle,&resumed_owner,&next_session,8);
        let next_window=journal.snapshot_for_owner(&owner,&next_session).unwrap();
        assert_eq!(next_window.epoch,baseline.epoch);
        assert!(next_window.events.starts_with(&baseline.events));
        assert_eq!(next_window.events.len(),4);
        let bodies=bodies.lock();
        assert_eq!(bodies.len(),9);
        for (i,body) in bodies.iter().enumerate() {
            assert_eq!(body["sessionId"],sessions[if i==8 { 0 } else { i }]);
            assert_eq!(body["taskId"],format!("journal-phase-task-{i}"));
            assert_eq!(body["role"],"assistant");
            assert_eq!(body["text"],format!("body-phase-{i}"));
        }
        handle.unlisten(listener);
        manager.stop();
        assert!(journal.snapshot_for_owner(&owner,&sessions[0]).is_err());
    });
}
