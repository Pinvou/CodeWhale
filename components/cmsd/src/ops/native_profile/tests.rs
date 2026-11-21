use super::*;
use crate::docker::fixtures as fx;

fn fixture(tag: &str) -> (PathBuf, Arc<App>, Card) {
    let root = fx::tmpdir(tag);
    let mut app = fx::make_app(&root, 128, 100);
    let config = &mut Arc::get_mut(&mut app).unwrap().config;
    config.proc_root = root.join("proc");
    let dir = config.cards_dir.join("current");
    std::fs::create_dir_all(&dir).unwrap();
    let engine = root.join("engines/11111111");
    std::fs::create_dir_all(&engine).unwrap();
    std::fs::copy("/usr/bin/true", engine.join("tachyon-serve")).unwrap();
    let weights = root.join("weights");
    std::fs::create_dir_all(&weights).unwrap();
    std::fs::write(weights.join("latest.tachyon"), b"data").unwrap();
    std::fs::write(dir.join("common.sh"),format!("ENGINE_BIN_SHA256=\"{}\"\nMODEL_ID=\"SharedName\"\nARTIFACT=\"latest.tachyon\"\nTOTAL_BYTES=\"4\"\n","1".repeat(64))).unwrap();
    std::fs::write(dir.join(".deploy-env"),format!("PORT=\"18199\"\nHOST=\"127.0.0.1\"\nMODEL_DIR=\"{}\"\nENGINE_DIR=\"{}\"\nARTIFACT=\"latest.tachyon\"\nMODEL_ID=\"SharedName\"\nMAX_CONCURRENCY=\"8\"\n",weights.display(),engine.display())).unwrap();
    std::fs::write(dir.join("enable.sh"),"exec tachyon-serve weights --max-context 262144 --max-concurrency \"$MAX_CONCURRENCY\" --device-state-slots 16 --host-state-slots 32 --kv-capacity 262144 --kv-dtype fp8\n").unwrap();
    let card = crate::cards::scan_cards(&config.cards_dir)
        .into_iter()
        .find(|card| card.name == "current")
        .unwrap();
    (root, app, card)
}

#[test]
fn declared_256k_and_independent_slots_are_not_replaced_by_legacy_defaults() {
    let (_root, app, card) = fixture("native-current-profile");
    let profile = resolve(&app, &card).unwrap();
    assert_eq!(
        (
            profile.context,
            profile.concurrency,
            profile.device_slots,
            profile.host_slots
        ),
        (262144, 8, 16, 32)
    );
    assert!(!profile.extras.contains("max-context"));
    assert!(!profile.extras.contains("state-slots"));
    assert!(profile.extras.contains("--kv-capacity 262144"));
    assert!(!recorded(&app, &card, &profile));
    record(&app, &card, &profile).unwrap();
    assert!(recorded(&app, &card, &profile));
    let mut changed = card.clone();
    changed.serve_flags = Some("--max-context 32768".into());
    let changed = resolve(&app, &changed).unwrap();
    assert_ne!(profile.revision, changed.revision);
    assert!(!recorded(&app, &card, &changed));
}

#[test]
fn duplicate_or_conflicting_endpoint_flags_are_rejected_before_launch() {
    let (_root, app, mut card) = fixture("native-invalid-profile");
    for flags in [
        "--max-context 262144 --max-context 32768",
        "--port 18080",
        "--max-context nope",
        "--max-context $OLD",
        "--kv-dtype fp8 --kv-dtype fp16",
        "--kv-dtype=fp8 --kv-dtype fp8",
        "orphan",
        "--vision first second",
        "--",
        "-x",
        "--kv-capacity *",
    ] {
        card.serve_flags = Some(flags.into());
        assert!(resolve(&app, &card).is_err(), "{flags}");
    }
}

#[test]
fn shared_model_id_and_port_do_not_prove_weights_profile_process_or_listener_identity() {
    let (root, mut app, card) = fixture("native-current-identity");
    let profile = resolve(&app, &card).unwrap();
    let pid = 412;
    let process = app.config.proc_root.join(pid.to_string());
    std::fs::create_dir_all(process.join("fd")).unwrap();
    std::fs::write(process.join("environ"), b"").unwrap();
    std::os::unix::fs::symlink(card.engine_bin().unwrap(), process.join("exe")).unwrap();
    std::fs::write(
        process.join("cgroup"),
        format!("0::/user.slice/{}\n", app.systemd.unit_name_for(&card.name)),
    )
    .unwrap();
    std::fs::create_dir_all(app.config.proc_root.join("net")).unwrap();
    std::fs::write(
        app.config.proc_root.join("net/tcp"),
        format!(
            "header\n0: 0100007F:{:04X} 00000000:0000 0A 0:0 0:0 0 1000 0 45678\n",
            18199
        ),
    )
    .unwrap();
    std::os::unix::fs::symlink("socket:[45678]", process.join("fd/3")).unwrap();
    let args = vec![
        card.engine_bin().unwrap().to_string_lossy().into_owned(),
        card.weights_path().unwrap().to_string_lossy().into_owned(),
        "--host".into(),
        "127.0.0.1".into(),
        "--port".into(),
        "18199".into(),
        "--model-id".into(),
        "SharedName".into(),
        "--max-context".into(),
        "262144".into(),
        "--max-concurrency".into(),
        "8".into(),
        "--device-state-slots".into(),
        "16".into(),
        "--host-state-slots".into(),
        "32".into(),
        "--kv-capacity".into(),
        "262144".into(),
        "--kv-dtype".into(),
        "fp8".into(),
    ];
    let write =
        |args: &[String]| std::fs::write(process.join("cmdline"), args.join("\0") + "\0").unwrap();
    write(&args);
    assert!(matches(&app, &card, 18199, pid, &profile));
    let mut updated = card.clone();
    updated.serve_flags = Some("--max-context 262144 --max-concurrency 8 --device-state-slots 16 --host-state-slots 32 --kv-dtype fp8".into());
    let updated_profile = resolve(&app, &updated).unwrap();
    assert_ne!(profile.revision, updated_profile.revision);
    assert!(
        !matches(&app, &updated, 18199, pid, &updated_profile),
        "removing --kv-capacity cannot adopt the previous runtime"
    );
    for suffix in [
        vec!["--other-runtime-setting", "1"],
        vec!["--kv-capacity", "262144"],
        vec!["--kv-capacity=65536"],
        vec!["unconsumed-positional"],
        vec!["--vision", "1", "unconsumed-positional"],
        vec!["--"],
    ] {
        let mut invalid = args.clone();
        invalid.extend(suffix.into_iter().map(str::to_owned));
        write(&invalid);
        assert!(!matches(&app, &card, 18199, pid, &profile), "{invalid:?}");
    }
    let mut invalid_utf8 = args.join("\0").into_bytes();
    invalid_utf8[0] = 0xff;
    invalid_utf8.push(0);
    std::fs::write(process.join("cmdline"), invalid_utf8).unwrap();
    assert!(!matches(&app, &card, 18199, pid, &profile));
    std::fs::write(process.join("cmdline"), args.join("\0")).unwrap();
    assert!(!matches(&app, &card, 18199, pid, &profile));
    let mut reordered = args[..2].to_vec();
    let (pairs, remainder) = args[2..].as_chunks::<2>();
    assert!(remainder.is_empty());
    for pair in pairs.iter().rev() {
        reordered.push(format!("{}={}", pair[0], pair[1]));
    }
    write(&reordered);
    assert!(
        matches(&app, &card, 18199, pid, &profile),
        "ordering and inline value spelling do not change the effective profile"
    );
    let mut literal_inline = card.clone();
    literal_inline.serve_flags = Some("--kv-dtype=fp8 --host-state-slots=32 --device-state-slots=16 --max-context=262144 --max-concurrency=8 --kv-capacity=262144 --vision --no-thinking".into());
    let inline_profile = resolve(&app, &literal_inline).unwrap();
    reordered.extend(["--no-thinking".into(), "--vision".into()]);
    write(&reordered);
    assert!(matches(&app, &literal_inline, 18199, pid, &inline_profile));
    write(&args);
    {
        let systemd = &mut Arc::get_mut(&mut app).unwrap().systemd;
        systemd.mode = crate::config::SystemdMode::System;
        systemd.run_user = unsafe { libc::geteuid() }.to_string();
    }
    assert!(matches(&app, &card, 18199, pid, &profile));
    Arc::get_mut(&mut app).unwrap().systemd.run_user = "cmsd-nonexistent-execution-account".into();
    assert!(!matches(&app, &card, 18199, pid, &profile));
    if unsafe { libc::geteuid() } != 0 {
        Arc::get_mut(&mut app).unwrap().systemd.run_user = "root".into();
        assert!(!matches(&app, &card, 18199, pid, &profile));
    }
    Arc::get_mut(&mut app).unwrap().systemd.run_user = unsafe { libc::geteuid() }.to_string();
    let mut old = args.clone();
    old[9] = "32768".into();
    write(&old);
    assert!(!matches(&app, &card, 18199, pid, &profile));
    let wrong = root.join("old.tachyon");
    std::fs::write(&wrong, b"data").unwrap();
    let mut old = args.clone();
    old[1] = wrong.to_string_lossy().into_owned();
    write(&old);
    assert!(!matches(&app, &card, 18199, pid, &profile));
    write(&args);
    let common = app.config.cards_dir.join(&card.name).join("common.sh");
    let original = std::fs::read_to_string(&common).unwrap();
    std::fs::write(
        &common,
        format!("{original}export TACHYON_PROJ_W4A4=0 TACHYON_W4A4_SCHED=0\n"),
    )
    .unwrap();
    let zero = resolve(&app, &card).unwrap();
    std::fs::write(
        process.join("environ"),
        b"PRIVATE_TOKEN=do-not-report\0TACHYON_PROJ_W4A4=0\0TACHYON_W4A4_SCHED=0\0",
    )
    .unwrap();
    assert!(matches(&app, &card, 18199, pid, &zero));
    std::fs::write(
        &common,
        format!("{original}export TACHYON_PROJ_W4A4=1 TACHYON_W4A4_SCHED=3\n"),
    )
    .unwrap();
    let tuned = resolve(&app, &card).unwrap();
    assert_ne!(zero.revision, tuned.revision);
    assert!(
        !matches(&app, &card, 18199, pid, &tuned),
        "same PID with old tuning cannot satisfy a new profile"
    );
    std::fs::write(
        process.join("environ"),
        b"TACHYON_W4A4_SCHED=3\0PRIVATE_TOKEN=do-not-report\0TACHYON_PROJ_W4A4=1\0",
    )
    .unwrap();
    assert!(matches(&app, &card, 18199, pid, &tuned));
    assert!(
        !matches(&app, &card, 18199, pid, &zero),
        "changing back must also verify the actual environment"
    );
    std::fs::write(&common, &original).unwrap();
    let removed = resolve(&app, &card).unwrap();
    assert!(
        !matches(&app, &card, 18199, pid, &removed),
        "an empty declaration cannot adopt an instance that still has tuning"
    );
    std::fs::write(process.join("environ"), b"PRIVATE_TOKEN=do-not-report\0").unwrap();
    assert!(matches(&app, &card, 18199, pid, &removed));
    std::fs::remove_file(process.join("environ")).unwrap();
    assert!(!matches(&app, &card, 18199, pid, &removed));
    std::fs::write(process.join("environ"), b"").unwrap();
    std::fs::remove_file(process.join("fd/3")).unwrap();
    assert!(
        !matches(&app, &card, 18199, pid, &profile),
        "another process serving this port is not healthy evidence"
    );
}

#[test]
fn native_tuning_environment_is_bounded_and_unambiguous() {
    let root = fx::tmpdir("native-environment-boundary");
    let expected = BTreeMap::from([
        ("TACHYON_PROJ_W4A4".into(), "1".into()),
        ("TACHYON_W4A4_SCHED".into(), "3".into()),
    ]);
    for bytes in [
        b"TACHYON_PROJ_W4A4=1\0TACHYON_W4A4_SCHED=3\0".as_slice(),
        b"SECRET=\xff\0UNRELATED\0TACHYON_W4A4_SCHED=3\0TACHYON_PROJ_W4A4=1\0",
    ] {
        std::fs::write(root.join("environ"), bytes).unwrap();
        assert!(environment_matches(&root, &expected).unwrap());
    }
    for bytes in [
        b"TACHYON_PROJ_W4A4=1\0TACHYON_PROJ_W4A4=1\0TACHYON_W4A4_SCHED=3\0".as_slice(),
        b"TACHYON_PROJ_W4A4=1\0TACHYON_PROJ_W4A4=0\0TACHYON_W4A4_SCHED=3\0",
        b"TACHYON_PROJ_W4A4=1\0",
        b"TACHYON_PROJ_W4A4=1\0TACHYON_W4A4_SCHED=\xff\0",
        b"TACHYON_PROJ_W4A4=1\0TACHYON_W4A4_SCHED=3",
        b"TACHYON_PROJ_W4A4\0TACHYON_W4A4_SCHED=3\0",
    ] {
        std::fs::write(root.join("environ"), bytes).unwrap();
        assert!(!environment_matches(&root, &expected).unwrap());
    }
    let oversized = vec![b'x'; ENVIRON_LIMIT as usize + 1];
    std::fs::write(root.join("environ"), oversized).unwrap();
    assert!(!environment_matches(&root, &BTreeMap::new()).unwrap());
    std::fs::remove_file(root.join("environ")).unwrap();
    assert!(environment_matches(&root, &BTreeMap::new()).is_err());
    std::os::unix::fs::symlink("/dev/null", root.join("environ")).unwrap();
    assert!(environment_matches(&root, &BTreeMap::new()).is_err());
    std::fs::remove_file(root.join("environ")).unwrap();
    let fifo = std::ffi::CString::new(root.join("environ").as_os_str().as_encoded_bytes()).unwrap();
    // SAFETY: valid terminated fixture-only path; this never opens a real proc.
    assert_eq!(unsafe { libc::mkfifo(fifo.as_ptr(), 0o600) }, 0);
    assert!(!environment_matches(&root, &BTreeMap::new()).unwrap());
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
#[ignore = "Opt-in read-only check of the current machine card; does not start or stop a model"]
fn inspect_real_current_card_profile() {
    let path = PathBuf::from(std::env::var("CMSD_QA_CARD_DIR").expect("CMSD_QA_CARD_DIR required"));
    assert!(path.is_absolute());
    let root = fx::tmpdir("native-read-current");
    let mut app = fx::make_app(&root, 128, 100);
    Arc::get_mut(&mut app).unwrap().config.cards_dir = path.parent().unwrap().to_path_buf();
    let card = crate::cards::scan_cards(&app.config.cards_dir)
        .into_iter()
        .find(|card| path.file_name().unwrap() == card.name.as_str())
        .unwrap();
    let profile = resolve(&app, &card).unwrap();
    assert_eq!(profile.context, 262144);
    println!(
        "{}",
        serde_json::json!({"card":card.name,"context":profile.context,"concurrency":profile.concurrency,
        "device_slots":profile.device_slots,"host_slots":profile.host_slots,"profile_revision":profile.revision,
        "scope":"Read actual production card files through production parser; no model lifecycle"})
    );
}

#[test]
fn complete_options_reject_ambiguous_or_unconsumed_arguments() {
    let parse = |tokens: &[&str]| {
        options(
            &tokens
                .iter()
                .map(|value| (*value).to_owned())
                .collect::<Vec<_>>(),
        )
    };
    assert_eq!(
        parse(&["--alpha", "1", "--vision"]).unwrap(),
        parse(&["--vision", "--alpha=1"]).unwrap()
    );
    for tokens in [
        vec!["--alpha", "1", "--alpha=1"],
        vec!["--alpha=1", "--alpha=2"],
        vec!["--vision", "--vision"],
        vec!["--alpha", "1", "bare"],
        vec!["bare", "--alpha", "1"],
        vec!["--"],
        vec!["-x"],
        vec!["--bad_name", "1"],
    ] {
        assert!(parse(&tokens).is_err(), "{tokens:?}");
    }
}

#[test]
fn failed_incompatible_native_enable_preserves_foreign_unit() {
    use std::os::unix::fs::PermissionsExt;
    let (root, mut app, card) = fixture("native-failed-claim");
    let cli = root.join("systemctl");
    let calls = root.join("systemctl.calls");
    std::fs::write(&cli, format!(r#"#!/bin/sh
[ "$1" = --user ] && shift
printf '%s\n' "$*" >> '{}'
case "$1" in
 show)
  if [ "$3" = --property=Restart ]; then echo no; else
   printf 'ActiveState=active\nSubState=running\nMainPID=412\nResult=success\nLoadState=loaded\nUnitFileState=enabled\n'
  fi
  ;;
 is-enabled) echo enabled ;;
esac
exit 0
"#, calls.display())).unwrap();
    std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
    let config = app.config.clone();
    Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
        config.systemd_mode,
        config.unit_dir.clone(),
        config.env_dir(),
        "test".into(),
    )
    .with_test_cli(&cli);
    let process = app.config.proc_root.join("412");
    std::fs::create_dir_all(process.join("fd")).unwrap();
    std::fs::write(process.join("environ"), b"").unwrap();
    std::fs::write(
        process.join("stat"),
        format!("412 (tachyon-serve) {} 99", vec!["0"; 19].join(" ")),
    )
    .unwrap();
    std::os::unix::fs::symlink(card.engine_bin().unwrap(), process.join("exe")).unwrap();
    std::fs::write(
        process.join("cgroup"),
        format!("0::/user.slice/{}\n", app.systemd.unit_name_for(&card.name)),
    )
    .unwrap();
    std::fs::write(process.join("cmdline"),format!("tachyon-serve\0{}\0--host\0127.0.0.1\0--port\018199\0--model-id\0SharedName\0--max-context\032768\0--max-concurrency\08\0--device-state-slots\016\0--host-state-slots\032\0--kv-capacity\0262144\0--kv-dtype\0fp8\0",card.weights_path().unwrap().display())).unwrap();
    std::fs::create_dir_all(app.config.proc_root.join("net")).unwrap();
    std::fs::write(
        app.config.proc_root.join("net/tcp"),
        format!(
            "header\n0: 0100007F:{:04X} 00000000:0000 0A 0:0 0:0 0 1000 0 45678\n",
            18199
        ),
    )
    .unwrap();
    std::os::unix::fs::symlink("socket:[45678]", process.join("fd/3")).unwrap();
    let profile = resolve(&app, &card).unwrap();
    assert!(!matches(&app, &card, 18199, 412, &profile));
    let task = app.task_create("enable", &card.name);
    let result = super::super::enable_inner(&app, &task, &card, card.deploy.as_ref().unwrap());
    assert!(
        result
            .as_ref()
            .unwrap_err()
            .starts_with("native_instance_mismatch"),
        "{result:?}"
    );
    assert!(!app
        .store
        .lock()
        .unwrap()
        .state
        .models
        .get(&card.name)
        .is_some_and(|model| model.lifecycle_owned));
    assert!(crate::recovery::view(&app, &card.name).unwrap().is_none());
    let calls = std::fs::read_to_string(calls).unwrap();
    assert!(
        !calls
            .lines()
            .any(|line| line.starts_with("disable ") || line == "daemon-reload"),
        "{calls}"
    );
    assert!(!root
        .join("units/megacube-model@current.service.d/90-cmsd-recovery.conf")
        .exists());
    // A legitimate managed instance can retain an allocated port that differs
    // from the preferred card port; its ownership-origin proof is still needed.
    let actual = std::fs::read_to_string(process.join("cmdline"))
        .unwrap()
        .replace("32768", "262144")
        .replace("18199", "18200");
    std::fs::write(process.join("cmdline"), actual).unwrap();
    std::fs::write(
        app.config.proc_root.join("net/tcp"),
        format!(
            "header\n0: 0100007F:{:04X} 00000000:0000 0A 0:0 0:0 0 1000 0 45678\n",
            18200
        ),
    )
    .unwrap();
    super::super::upsert_model_state(&app, &card.name, |model| {
        model.lifecycle_owned = true;
        model.port = 18200;
    });
    super::super::verify_native_runtime(&app, &card, &profile).unwrap();
    std::fs::write(process.join("cgroup"), "0::/foreign.service\n").unwrap();
    assert!(super::super::verify_native_runtime(&app, &card, &profile).is_err());
    assert!(std::fs::read_to_string(root.join("systemctl.calls"))
        .unwrap()
        .lines()
        .all(|line| line.starts_with("show ")));
}
