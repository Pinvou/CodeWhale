use super::*;
use crate::platform::credential_store::{CredentialReference, CredentialState};
use crate::platform::test_support::with_temp_home;

fn model(id: &str) -> SavedModel {
    serde_json::from_value(serde_json::json!({
        "id": id,
        "name": id,
        "preset": "openai_compatible",
        "provider_kind": "custom",
        "model": format!("{id}-wire"),
        "base_url": "http://127.0.0.1:8000/v1"
    }))
    .unwrap()
}

fn bridge(primary: SavedModel) -> Pinvou3Bridge {
    super::probed_facts_wiring_tests::wiring_bridge(primary)
}

fn local_fixture() -> (Pinvou3Bridge, PreparedRuntimeModel) {
    let mut primary = model("primary");
    primary.vision_model_id = Some("vision".into());
    let mut bridge = bridge(primary.clone());
    bridge.prefs.advanced.saved_models = vec![primary.clone(), model("vision"), model("other")];
    (bridge, PreparedRuntimeModel::unchanged(primary))
}

fn saved_vision(bridge: &mut Pinvou3Bridge) -> &mut SavedModel {
    bridge
        .prefs
        .advanced
        .saved_models
        .iter_mut()
        .find(|model| model.id == "vision")
        .unwrap()
}

fn state(
    prepared: &PreparedRuntimeModel,
    bridge: &Pinvou3Bridge,
    revisions: &ModelUpdateRevisions,
) -> PreparedRuntimeState {
    PreparedRuntimeState::from_bridge(prepared.clone(), bridge, revisions)
}

#[test]
fn local_vision_changes_invalidate_the_primary_and_keep_its_explicit_selection() {
    let (bridge, prepared) = local_fixture();
    let revisions = ModelUpdateRevisions::default();
    let previous = state(&prepared, &bridge, &revisions);
    let changes: [(&str, fn(&mut SavedModel)); 6] = [
        ("route", |model| {
            model.base_url = "http://127.0.0.1:8001/v1".into();
        }),
        ("wire model", |model| model.model = "upgraded-wire".into()),
        ("context", |model| {
            model.context_window_tokens = Some(262144)
        }),
        ("credential metadata", |model| {
            model.credential_ref = Some(CredentialReference::for_model("vision"));
            model.credential_state = CredentialState::Configured;
            model.has_secret = true;
        }),
        ("store association", |model| {
            model.model_store_card_id = Some("stable-card".into());
        }),
        ("detach", |model| model.model_store_link_detached = true),
    ];
    for (change, mutate) in changes {
        let mut latest_bridge = bridge.clone();
        mutate(saved_vision(&mut latest_bridge));
        let latest = state(&prepared, &latest_bridge, &revisions);
        assert!(
            !entry_is_fresh(latest.requires_rebuild_from(&previous), 0, 0),
            "a changed vision {change} must rebuild the dependent primary engine"
        );
        assert_eq!(
            latest.prepared.model.vision_model_id.as_deref(),
            Some("vision"),
            "rebuilding must preserve the user's explicit vision selection"
        );
    }
}

#[test]
fn store_reenable_revision_rebuilds_only_sessions_depending_on_that_vision_model() {
    let (mut bridge, prepared) = local_fixture();
    let revisions = ModelUpdateRevisions::default();
    let previous = state(&prepared, &bridge, &revisions);
    let mut unrelated = PreparedRuntimeModel::unchanged(model("unrelated-primary"));
    let unrelated_previous = state(&unrelated, &bridge, &revisions);
    revisions.bump("vision"); // The same notification as a same-route successful enable.
    assert!(state(&prepared, &bridge, &revisions).requires_rebuild_from(&previous));
    assert!(
        !state(&unrelated, &bridge, &revisions).requires_rebuild_from(&unrelated_previous),
        "a model that does not use this fallback must keep its engine"
    );

    unrelated.model.vision_model_id = Some("other".into());
    let other_previous = state(&unrelated, &bridge, &revisions);
    saved_vision(&mut bridge).model = "new-store-wire".into();
    revisions.bump("vision");
    assert!(!state(&unrelated, &bridge, &revisions).requires_rebuild_from(&other_previous));
}

#[test]
fn rebuilt_image_tool_and_reuse_snapshot_share_the_latest_selected_vision_route() {
    with_temp_home("vision-dependency-spawn-config", || {
        let (mut bridge, mut prepared) = local_fixture();
        // Runtime preparation may choose another fallback before finalize copies
        // the primary into the bridge. Snapshot the prepared choice, not the
        // bridge's older session_model.
        prepared.model.vision_model_id = Some("other".into());
        let other = bridge
            .prefs
            .advanced
            .saved_models
            .iter_mut()
            .find(|model| model.id == "other")
            .unwrap();
        other.base_url = "http://127.0.0.1:8002/v1".into();
        other.model = "selected-wire".into();
        let prepared_state = state(&prepared, &bridge, &ModelUpdateRevisions::default());
        let dependency = prepared_state.vision_dependency.as_ref().unwrap();
        assert_eq!(dependency.model.id, "other");
        apply_prepared_runtime_model(&mut bridge, &prepared);
        let spawned = bridge.build_engine_config().vision_config.unwrap();
        assert_eq!(spawned.model, dependency.model.model);
        assert_eq!(
            spawned.base_url.as_deref(),
            Some(dependency.model.base_url.as_str())
        );
        assert_eq!(
            bridge
                .session_model
                .as_ref()
                .unwrap()
                .vision_model_id
                .as_deref(),
            Some("other")
        );
    });
}

#[test]
fn deleting_an_explicit_saved_vision_dependency_invalidates_the_existing_image_tool() {
    let (mut bridge, prepared) = local_fixture();
    let revisions = ModelUpdateRevisions::default();
    let previous = state(&prepared, &bridge, &revisions);
    bridge
        .prefs
        .advanced
        .saved_models
        .retain(|model| model.id != "vision");
    let removed = state(&prepared, &bridge, &revisions);
    assert!(removed.vision_dependency.is_none());
    assert!(removed.requires_rebuild_from(&previous));
    assert_eq!(
        removed.prepared.model.vision_model_id.as_deref(),
        Some("vision")
    );
}

fn remote_fixture(with_vision: bool) -> (Pinvou3Bridge, PreparedRuntimeModel) {
    let (mut bridge, mut prepared) = local_fixture();
    prepared.model.api_key = "task-primary-fixture".into();
    if !with_vision {
        prepared.model.vision_model_id = None;
    }
    let mut vision = model("vision");
    vision.base_url = "http://127.0.0.1:9000/v1".into();
    vision.model = "task-vision-wire".into();
    vision.api_key = "task-vision-fixture".into();
    let bundle =
        RemoteRuntimeModelBundle::new(prepared.model, with_vision.then_some(vision)).unwrap();
    let prepared = PreparedRuntimeModel {
        model: bundle.primary,
        credential: Some(bundle.primary_credential),
        revision: Some("task-snapshot".into()),
    };
    bridge.runtime_route_locked = true;
    bridge.runtime_model_credential_frozen = true;
    bridge.runtime_vision_model = bundle.vision;
    bridge.runtime_vision_credential = bundle.vision_credential;
    apply_prepared_runtime_model(&mut bridge, &prepared);
    (bridge, prepared)
}

#[test]
fn task_vision_snapshot_and_credential_are_authoritative_over_same_id_local_changes() {
    with_temp_home("vision-dependency-task-snapshot", || {
        let (mut bridge, prepared) = remote_fixture(true);
        let revisions = ModelUpdateRevisions::default();
        let previous = state(&prepared, &bridge, &revisions);
        let dependency = previous.vision_dependency.as_ref().unwrap();
        assert_eq!(dependency.model.base_url, "http://127.0.0.1:9000/v1");
        assert!(dependency.model.api_key.is_empty());
        assert!(dependency.model_update_revision.is_none());
        saved_vision(&mut bridge).base_url = "http://127.0.0.1:8005/v1".into();
        saved_vision(&mut bridge).context_window_tokens = Some(32768);
        revisions.bump("vision");
        assert!(!state(&prepared, &bridge, &revisions).requires_rebuild_from(&previous));
        let spawned = bridge.build_engine_config().vision_config.unwrap();
        assert_eq!(spawned.model, dependency.model.model);
        assert_eq!(
            spawned.base_url.as_deref(),
            Some(dependency.model.base_url.as_str())
        );
        assert_eq!(spawned.api_key.as_deref(), Some("task-vision-fixture"));

        bridge.runtime_vision_model.as_mut().unwrap().model = "new-task-wire".into();
        assert!(state(&prepared, &bridge, &revisions).requires_rebuild_from(&previous));
        bridge.runtime_vision_model.as_mut().unwrap().model = "task-vision-wire".into();
        bridge.runtime_vision_credential =
            Some(RuntimeModelCredential::api_key("rotated-task-fixture").unwrap());
        assert!(state(&prepared, &bridge, &revisions).requires_rebuild_from(&previous));
        bridge.runtime_vision_credential = None;
        let absent = state(&prepared, &bridge, &revisions);
        assert!(
            absent
                .vision_dependency
                .as_ref()
                .unwrap()
                .credential
                .is_none()
        );
        assert!(absent.requires_rebuild_from(&previous));
        assert!(
            bridge.runtime_vision_credential.is_none(),
            "snapshotting must not refill frozen credentials"
        );
    });
}

#[test]
fn task_without_an_explicit_vision_choice_does_not_acquire_a_local_dependency() {
    let (mut bridge, mut prepared) = remote_fixture(false);
    prepared.credential = None;
    let revisions = ModelUpdateRevisions::default();
    let previous = state(&prepared, &bridge, &revisions);
    assert!(previous.vision_dependency.is_none());
    assert!(previous.credential_resolution == RuntimeCredentialResolution::FrozenAbsent);
    saved_vision(&mut bridge).model = "different-local-wire".into();
    revisions.bump("vision");
    let latest = state(&prepared, &bridge, &revisions);
    assert!(!latest.requires_rebuild_from(&previous));
    assert!(latest.prepared.credential.is_none());
    assert!(latest.prepared.model.vision_model_id.is_none());
    assert!(bridge.runtime_vision_model.is_none());
}

#[test]
fn unmatched_task_vision_keeps_the_existing_saved_fallback_priority() {
    let (mut bridge, prepared) = remote_fixture(true);
    bridge.runtime_vision_model.as_mut().unwrap().id = "different-task-vision".into();
    let revisions = ModelUpdateRevisions::default();
    let previous = state(&prepared, &bridge, &revisions);
    let dependency = previous.vision_dependency.as_ref().unwrap();
    assert_eq!(dependency.model.base_url, "http://127.0.0.1:8000/v1");
    assert_eq!(dependency.model_update_revision, Some(0));
    assert!(dependency.credential.is_none());
    revisions.bump("vision");
    assert!(state(&prepared, &bridge, &revisions).requires_rebuild_from(&previous));
}
