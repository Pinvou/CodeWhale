use crate::docker::fixtures as fx;
use crate::store::{ModelStatus, TaskStatus};
use std::path::PathBuf;
use std::sync::Arc;

fn fixture(tag: &str, actual_image: &str) -> (PathBuf, Arc<crate::app::App>, u16) {
    let root = fx::tmpdir(tag);
    let app = fx::make_app(&root, 128, 100);
    let port = fx::serve_models_json("Declared-Model");
    let dir = fx::write_docker_card(
        &app.config.cards_dir,
        "selected",
        "selected-ctr",
        port,
        Some("0.01"),
    );
    let weights = root.join("declared-weights");
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::write(dir.join("common.sh"),format!("IMAGE=fixture/img:1.0\nCONTAINER_NAME=selected-ctr\nMODEL_ID=Declared-Model\nPORT={port}\n")).unwrap();
    std::fs::write(
        dir.join("vendor/.deploy-env"),
        format!(
            "IMAGE=fixture/img:1.0\nCONTAINER_NAME=selected-ctr\nPORT={port}\nMODEL_DIR={}\n",
            weights.display()
        ),
    )
    .unwrap();
    fx::set_container(
        &root,
        "selected-ctr",
        actual_image,
        true,
        "always",
        0,
        false,
        4321,
        port,
        8000,
        "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
    );
    (root, app, port)
}
fn own(app: &Arc<crate::app::App>, port: u16, active: bool) {
    crate::ops::upsert_model_state(app, "selected", |m| {
        m.lifecycle_owned = true;
        m.status = ModelStatus::Running;
        m.main_pid = Some(4321);
        m.port = port;
    });
    if active {
        let mut s = app.store.lock().unwrap();
        s.state.active_model = Some("selected".into());
        s.mark_dirty();
    }
}
#[test]
fn explicit_enable_must_check_declared_weight_mount() {
    let (root, app, _port) = fixture("explicit-enable-unbound-weights", "fixture/img:1.0");
    fx::set_model_mount(&root, "selected-ctr", &root.join("unrelated-weights"));
    std::fs::create_dir_all(root.join("unrelated-weights")).unwrap();
    let card = crate::ops::find_card(&app, "selected").unwrap();
    let matching = app
        .docker
        .inspect("selected-ctr")
        .unwrap()
        .unwrap()
        .model_mount_matches(&card.docker_deploy.as_ref().unwrap().model_dir)
        .unwrap();
    assert!(
        !matching,
        "the fixture container really has no declared weight mount"
    );
    let task = crate::ops::enable(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    let owned = app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get("selected")
        .is_some_and(|model| model.lifecycle_owned);
    eprintln!(
        "ENABLE mount_matches={matching} status={:?} owned={owned} reality={} calls={:?}",
        done.status,
        fx::state_line(&root, "selected-ctr").unwrap(),
        fx::docker_calls(&root)
    );
    assert_eq!(
        done.status,
        TaskStatus::Failed,
        "a container without this card's model mount cannot be enrolled or fenced"
    );
    assert!(!owned);
}
#[test]
fn idempotent_switch_must_check_container_image() {
    let (root, app, port) = fixture("idempotent-switch-foreign-image", "foreign/image:latest");
    own(&app, port, true);
    let card = crate::ops::find_card(&app, "selected").unwrap();
    let reality = app.docker.inspect("selected-ctr").unwrap().unwrap();
    assert!(
        crate::ops::validate_container_config(card.docker_deploy.as_ref().unwrap(), &reality)
            .is_err()
    );
    let task = crate::ops::switch_to(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    eprintln!(
        "SWITCH status={:?} result={:?} image={} calls={:?}",
        done.status,
        done.result,
        reality.image,
        fx::docker_calls(&root)
    );
    assert_eq!(
        done.status,
        TaskStatus::Failed,
        "switch must not claim a healthy foreign image is the selected deployment"
    );
}
#[test]
fn set_default_must_check_container_image() {
    let (root, app, port) = fixture("set-default-foreign-image", "foreign/image:latest");
    own(&app, port, false);
    let result = crate::ops::set_default(&app, "selected");
    eprintln!(
        "DEFAULT result={result:?} active={:?} calls={:?}",
        app.store.lock().unwrap().state.active_model,
        fx::docker_calls(&root)
    );
    assert!(
        result.is_err(),
        "set-default should not route the declared deployment to a foreign image"
    );
}
#[test]
fn recovery_must_not_fence_replaced_container() {
    let (root, app, port) = fixture("recovery-foreign-image", "fixture/img:1.0");
    own(&app, port, false);
    crate::recovery::arm(&app, "selected", false).unwrap();
    fx::set_container(
        &root,
        "selected-ctr",
        "foreign/image:latest",
        true,
        "always",
        0,
        false,
        8765,
        port,
        8000,
        "--served-model-name,Foreign-Model",
    );
    std::fs::write(root.join("docker.calls"), "").unwrap();
    crate::recovery::tick(&app);
    let m = app.store.lock().unwrap().state.models["selected"].clone();
    let line = fx::state_line(&root, "selected-ctr").unwrap();
    eprintln!(
        "RECOVERY status={:?} pid={:?} reason={:?} reality={line} calls={:?}",
        m.status,
        m.main_pid,
        m.degraded_reason,
        fx::docker_calls(&root)
    );
    assert!(line.contains("^always^"),"recovery changed an unrelated replacement's restart policy before proving deployment identity");
}

#[test]
fn disable_must_preserve_replaced_foreign_image() {
    let (root, app, port) = fixture("disable-foreign-image", "foreign/image:latest");
    own(&app, port, true);
    let task = crate::ops::disable(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    let line = fx::state_line(&root, "selected-ctr").unwrap();
    eprintln!(
        "DISABLE status={:?} reality={line} calls={:?}",
        done.status,
        fx::docker_calls(&root)
    );
    assert_eq!(
        done.status,
        TaskStatus::Failed,
        "saved image disagrees: stopping the replacement is not authorized by old ownership"
    );
    assert!(line.contains("^true^always^"));
}

#[test]
fn purge_preserves_foreign_replacement_and_registration() {
    let (root, app, port) = fixture("purge-foreign-image", "foreign/image:latest");
    own(&app, port, true);
    let task = crate::ops::purge(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    assert_eq!(done.status, TaskStatus::Failed);
    assert!(app.store.lock().unwrap().state.models["selected"].lifecycle_owned);
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^true^always^"));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("stop ") || call.starts_with("update ")));
}
#[test]
fn stop_saved_reference_survives_card_deletion() {
    let (root, app, port) = fixture("saved-stop-without-card", "fixture/img:1.0");
    own(&app, port, true);
    std::fs::remove_dir_all(app.config.cards_dir.join("selected")).unwrap();
    let task = crate::ops::disable(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    assert_eq!(done.status, TaskStatus::Succeeded);
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^false^no^"));
}
#[test]
fn boot_intent_does_not_authorize_foreign_policy_changes() {
    let (root, app, port) = fixture("boot-foreign-image", "fixture/img:1.0");
    own(&app, port, false);
    crate::recovery::arm(&app, "selected", false).unwrap();
    fx::set_container(
        &root,
        "selected-ctr",
        "foreign/image:latest",
        true,
        "always",
        0,
        false,
        8765,
        port,
        8000,
        "--served-model-name,Foreign-Model",
    );
    std::fs::write(root.join("docker.calls"), "").unwrap();
    crate::recovery::set_boot(&app, "selected", false).unwrap();
    assert!(crate::recovery::set_boot(&app, "selected", true).is_err());
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^true^always^"));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("stop ") || call.starts_with("update ")));
}
#[test]
fn equivalent_recreation_is_still_managed_without_id_binding() {
    let (root, app, port) = fixture("equivalent-recreation", "fixture/img:1.0");
    own(&app, port, false);
    crate::recovery::arm(&app, "selected", false).unwrap();
    fx::set_container(
        &root,
        "selected-ctr",
        "fixture/img:1.0",
        true,
        "always",
        0,
        false,
        8765,
        port,
        8000,
        "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
    );
    crate::recovery::tick(&app);
    let model = app.store.lock().unwrap().state.models["selected"].clone();
    assert_eq!(model.main_pid, Some(8765));
    assert!(model.degraded_reason.is_none());
    assert!(crate::container_runtime::routable(&app, &model));
    crate::ops::set_default(&app, "selected").unwrap();
    let task = crate::ops::switch_to(&app, "selected").unwrap();
    assert_eq!(
        fx::wait_terminal(&app, &task, 10).status,
        TaskStatus::Succeeded
    );
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^true^no^"));
}

#[test]
fn malformed_scalars_never_take_control_or_change_existing_model_facts() {
    for (label, args) in [
        ("missing-fraction", "--served-model-name,Declared-Model"),
        ("invalid-fraction", "--mem-fraction-static,invalid"),
        ("nan-fraction", "--mem-fraction-static,NaN"),
        (
            "duplicate-fraction",
            "--mem-fraction-static,0.01,--mem-fraction-static=0.90",
        ),
        (
            "duplicate-port",
            "--mem-fraction-static,0.01,--port,8000,--port=8001",
        ),
        ("missing-port", "--mem-fraction-static,0.01,--port"),
    ] {
        let (root, app, port) = fixture(&format!("container-scalar-{label}"), "fixture/img:1.0");
        crate::ops::upsert_model_state(&app, "selected", |model| {
            model.status = ModelStatus::Stopped;
            model.measured_footprint_gib = Some(7.0);
        });
        let before = app.store.lock().unwrap().state.models["selected"].clone();
        fx::set_container(
            &root,
            "selected-ctr",
            "fixture/img:1.0",
            true,
            "always",
            0,
            false,
            4321,
            port,
            8000,
            args,
        );
        std::fs::write(root.join("docker.calls"), "").unwrap();
        let task = crate::ops::enable(&app, "selected").unwrap();
        let done = fx::wait_terminal(&app, &task, 10);
        assert_eq!(done.status, TaskStatus::Failed, "{label}: {:?}", done.error);
        let after = app.store.lock().unwrap().state.models["selected"].clone();
        assert!(!after.lifecycle_owned);
        assert_eq!(after.status, before.status);
        assert_eq!(after.image, before.image);
        assert_eq!(after.container_name, before.container_name);
        assert_eq!(after.measured_footprint_gib, before.measured_footprint_gib);
        assert!(fx::state_line(&root, "selected-ctr")
            .unwrap()
            .contains("^true^always^"));
        assert!(
            !fx::docker_calls(&root)
                .iter()
                .any(|call| call.starts_with("start ")
                    || call.starts_with("stop ")
                    || call.starts_with("update ")),
            "{label}"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[test]
fn saved_reference_requires_a_nonempty_matching_image() {
    let (_root, app, port) = fixture("saved-image-proof", "fixture/img:1.0");
    own(&app, port, false);
    let original = app.store.lock().unwrap().state.models["selected"].clone();
    let mut container = app.docker.inspect("selected-ctr").unwrap().unwrap();
    let mut unverified = Vec::new();
    for (label, saved, actual) in [
        ("missing", None, "fixture/img:1.0"),
        ("empty-matching-empty", Some(""), ""),
        ("empty", Some(""), "fixture/img:1.0"),
    ] {
        let mut model = original.clone();
        model.image = saved.map(str::to_owned);
        container.image = actual.into();
        let result = crate::container_runtime::saved_reference(&model, &container);
        eprintln!("SAVED_IMAGE {label} result={result:?}");
        if result.is_ok() {
            unverified.push(label);
        }
    }
    assert!(
        unverified.is_empty(),
        "missing image proof accepted: {unverified:?}"
    );
    container.image = "fixture/img:1.0".into();
    assert!(crate::container_runtime::saved_reference(&original, &container).is_ok());
}
#[test]
fn missing_saved_image_never_authorizes_disable_or_purge() {
    for action in ["disable", "purge"] {
        for (label, saved, actual) in [
            ("missing", None, "foreign/image:latest"),
            ("empty", Some(""), ""),
        ] {
            let (root, app, port) = fixture(&format!("saved-image-{action}-{label}"), actual);
            own(&app, port, true);
            app.store
                .lock()
                .unwrap()
                .state
                .models
                .get_mut("selected")
                .unwrap()
                .image = saved.map(str::to_owned);
            let before = app.store.lock().unwrap().state.models["selected"].clone();
            std::fs::write(root.join("docker.calls"), "").unwrap();
            let task = if action == "disable" {
                crate::ops::disable(&app, "selected")
            } else {
                crate::ops::purge(&app, "selected")
            }
            .unwrap();
            let done = fx::wait_terminal(&app, &task, 10);
            eprintln!(
                "SAVED_IMAGE_ACTION {action}/{label} status={:?} calls={:?}",
                done.status,
                fx::docker_calls(&root)
            );
            assert_eq!(done.status, TaskStatus::Failed, "{action}/{label}");
            let after = app.store.lock().unwrap().state.models["selected"].clone();
            assert_eq!(after.image, before.image);
            assert_eq!(after.container_name, before.container_name);
            assert_eq!(after.status, before.status);
            assert_eq!(after.main_pid, before.main_pid);
            assert!(after.lifecycle_owned);
            assert!(fx::state_line(&root, "selected-ctr")
                .unwrap()
                .contains("^true^always^"));
            assert!(!fx::docker_calls(&root)
                .iter()
                .any(|call| call.starts_with("stop ") || call.starts_with("update ")));
            std::fs::remove_dir_all(root).unwrap();
        }
    }
}

#[test]
fn bridge_without_published_endpoint_cannot_claim_unrelated_listener() {
    let (root, app, port) = fixture("missing-bridge-binding", "fixture/img:1.0");
    fx::set_container(
        &root,
        "selected-ctr",
        "fixture/img:1.0",
        true,
        "always",
        0,
        false,
        4321,
        0,
        8000,
        "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
    );
    let reality = app.docker.inspect("selected-ctr").unwrap().unwrap();
    assert_eq!(reality.host_port, None);
    let card = crate::ops::find_card(&app, "selected").unwrap();
    assert!(card.docker_deploy.as_ref().unwrap().host_bind.is_none());
    eprintln!("NETWORK_PROOF reality host_port={:?} card.port={} source_match={} unrelated_host_listener={}",reality.host_port,port,
        reality.model_mount_matches(&card.docker_deploy.as_ref().unwrap().model_dir).unwrap(),port);
    let task = crate::ops::enable(&app, "selected").unwrap();
    let done = fx::wait_terminal(&app, &task, 10);
    let model = app.store.lock().unwrap().state.models["selected"].clone();
    eprintln!(
        "NETWORK_PROOF task={:?} owned={} status={:?} model_id={} routable={} calls={:?}",
        done.status,
        model.lifecycle_owned,
        model.status,
        model.model_id,
        crate::container_runtime::routable(&app, &model),
        fx::docker_calls(&root)
    );
    assert_eq!(
        done.status,
        TaskStatus::Failed,
        "missing published port must not authorize the unrelated host listener"
    );
    assert!(!model.lifecycle_owned);
    assert!(!crate::container_runtime::routable(&app, &model));
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^true^always^"));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("update ")
            || call.starts_with("stop ")
            || call.starts_with("start ")));
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn container_network_proof_distinguishes_runtime_and_startup_bindings() {
    let (root, app, port) = fixture("network-matrix", "fixture/img:1.0");
    let base = crate::ops::find_card(&app, "selected").unwrap();
    let weights = base.docker_deploy.as_ref().unwrap().model_dir.clone();
    let bind = |ip: &str, p: u16| serde_json::json!([{"HostIp":ip,"HostPort":p.to_string()}]);
    let map = |ip: &str, p: u16| serde_json::json!({"8000/tcp":bind(ip,p)});
    let mut failed = Vec::new();
    for (label, host, mode, running, config_ports, active_ports, extra, expected) in [
        (
            "bridge-no-binding",
            false,
            "bridge",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![],
            false,
        ),
        (
            "bridge-nonloopback-binding",
            false,
            "bridge",
            true,
            map("192.0.2.5", port),
            map("192.0.2.5", port),
            vec![],
            false,
        ),
        (
            "bridge-declared-port-is-second",
            false,
            "bridge",
            true,
            serde_json::json!({"7000/tcp":bind("0.0.0.0",port-1),"8000/tcp":bind("127.0.0.1",port)}),
            serde_json::json!({"7000/tcp":bind("0.0.0.0",port-1),"8000/tcp":bind("127.0.0.1",port)}),
            vec![],
            true,
        ),
        (
            "bridge-configured-without-active-binding",
            false,
            "bridge",
            true,
            map("0.0.0.0", port),
            serde_json::json!({"8000/tcp":null}),
            vec![],
            false,
        ),
        (
            "compose-host-declaration-bridge-runtime",
            true,
            "bridge",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![
                "--port".to_owned(),
                port.to_string(),
                "--host".into(),
                "127.0.0.1".into(),
            ],
            false,
        ),
        (
            "compose-host-bind-drift",
            true,
            "host",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![
                "--port".to_owned(),
                port.to_string(),
                "--host".into(),
                "192.0.2.5".into(),
            ],
            false,
        ),
        (
            "valid-bridge-loopback",
            false,
            "bridge",
            true,
            map("127.0.0.1", port),
            map("127.0.0.1", port),
            vec![],
            true,
        ),
        (
            "valid-bridge-wildcard",
            false,
            "bridge",
            true,
            map("0.0.0.0", port),
            map("0.0.0.0", port),
            vec![],
            true,
        ),
        (
            "valid-bridge-default-address",
            false,
            "default",
            true,
            map("", port),
            map("", port),
            vec![],
            true,
        ),
        (
            "valid-bridge-second-address",
            false,
            "bridge",
            true,
            serde_json::json!({"8000/tcp":[{"HostIp":"192.0.2.5","HostPort":port.to_string()},{"HostIp":"127.0.0.1","HostPort":port.to_string()}]}),
            serde_json::json!({"8000/tcp":[{"HostIp":"192.0.2.5","HostPort":port.to_string()},{"HostIp":"127.0.0.1","HostPort":port.to_string()}]}),
            vec![],
            true,
        ),
        (
            "bridge-another-loopback-address",
            false,
            "bridge",
            true,
            map("127.0.0.2", port),
            map("127.0.0.2", port),
            vec![],
            false,
        ),
        (
            "bridge-ipv6-only-wildcard",
            false,
            "bridge",
            true,
            map("::", port),
            map("::", port),
            vec![],
            false,
        ),
        (
            "bridge-wrong-active-port",
            false,
            "bridge",
            true,
            map("127.0.0.1", port),
            map("127.0.0.1", port - 1),
            vec![],
            false,
        ),
        (
            "bridge-unknown-active-bindings",
            false,
            "bridge",
            true,
            map("127.0.0.1", port),
            serde_json::Value::Null,
            vec![],
            false,
        ),
        (
            "bridge-malformed-active-bindings",
            false,
            "bridge",
            true,
            map("127.0.0.1", port),
            serde_json::json!({"8000/tcp":[{"HostIp":"127.0.0.1","HostPort":"invalid"}]}),
            vec![],
            false,
        ),
        (
            "bridge-unrelated-udp-before-tcp",
            false,
            "bridge",
            true,
            serde_json::json!({"7000/udp":bind("0.0.0.0",port-1),"8000/tcp":bind("127.0.0.1",port)}),
            serde_json::json!({"7000/udp":bind("0.0.0.0",port-1),"8000/tcp":bind("127.0.0.1",port)}),
            vec![],
            true,
        ),
        (
            "bridge-process-port-drift",
            false,
            "bridge",
            true,
            map("127.0.0.1", port),
            map("127.0.0.1", port),
            vec!["--port=8001".into()],
            false,
        ),
        (
            "bridge-declaration-host-runtime",
            false,
            "host",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![],
            false,
        ),
        (
            "stopped-bridge-nonloopback",
            false,
            "bridge",
            false,
            map("192.0.2.5", port),
            serde_json::json!({}),
            vec![],
            false,
        ),
        (
            "compose-host-missing-host-argument",
            true,
            "host",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec!["--port".to_owned(), port.to_string()],
            false,
        ),
        (
            "compose-host-duplicate-host-argument",
            true,
            "host",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![
                "--port".to_owned(),
                port.to_string(),
                "--host=127.0.0.1".into(),
                "--host=0.0.0.0".into(),
            ],
            false,
        ),
        (
            "valid-stopped-bridge-config",
            false,
            "bridge",
            false,
            map("127.0.0.1", port),
            serde_json::json!({}),
            vec![],
            true,
        ),
        (
            "valid-compose-host",
            true,
            "host",
            true,
            serde_json::json!({}),
            serde_json::json!({}),
            vec![
                "--port".to_owned(),
                port.to_string(),
                "--host".into(),
                "127.0.0.1".into(),
            ],
            true,
        ),
    ] {
        let mut card = base.clone();
        card.docker_deploy.as_mut().unwrap().host_bind = host.then(|| "127.0.0.1".into());
        let mut args = vec!["--mem-fraction-static".to_string(), "0.01".to_string()];
        args.extend(extra);
        let raw = serde_json::json!([{"Name":"/selected-ctr","Path":"entry","Args":args,
            "State":{"Status":if running{"running"}else{"exited"},"Running":running,"Pid":if running{4321}else{0},"ExitCode":0},
            "Config":{"Image":"fixture/img:1.0"},"Mounts":[{"Source":weights}],
            "HostConfig":{"NetworkMode":mode,"RestartPolicy":{"Name":"always"},"PortBindings":config_ports},
            "NetworkSettings":{"Ports":active_ports}}]);
        let container = crate::docker::parse_inspect(&raw.to_string())
            .unwrap()
            .remove(0);
        let actual = crate::container_runtime::check(&card, None, &container).is_ok();
        eprintln!(
            "NETWORK_PROOF {label}: accepted={actual} expected={expected} selected_host_port={:?}",
            container.host_port
        );
        if actual != expected {
            failed.push(label);
        }
    }
    assert!(
        failed.is_empty(),
        "network proof cases disagreed: {failed:?}"
    );
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn unpublished_owned_runtime_cannot_route_or_change_recovery_policy() {
    let (root, app, port) = fixture("unpublished-owned-policy", "fixture/img:1.0");
    own(&app, port, true);
    crate::recovery::arm(&app, "selected", false).unwrap();
    fx::set_container(
        &root,
        "selected-ctr",
        "fixture/img:1.0",
        true,
        "always",
        0,
        false,
        4321,
        0,
        8000,
        "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
    );
    std::fs::write(root.join("docker.calls"), "").unwrap();
    crate::recovery::tick(&app);
    assert!(crate::ops::set_default(&app, "selected").is_err());
    crate::recovery::set_boot(&app, "selected", false).unwrap();
    assert!(crate::recovery::set_boot(&app, "selected", true).is_err());
    let task = crate::ops::switch_to(&app, "selected").unwrap();
    assert_eq!(
        fx::wait_terminal(&app, &task, 10).status,
        TaskStatus::Failed
    );
    let model = app.store.lock().unwrap().state.models["selected"].clone();
    assert!(!crate::container_runtime::routable(&app, &model));
    assert!(fx::state_line(&root, "selected-ctr")
        .unwrap()
        .contains("^true^always^"));
    assert!(!fx::docker_calls(&root)
        .iter()
        .any(|call| call.starts_with("update ")
            || call.starts_with("stop ")
            || call.starts_with("start ")));
    std::fs::remove_dir_all(root).unwrap();
}
