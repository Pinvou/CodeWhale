//! Storage-only RemoteProxy boundary regression. The wire fixture is accepted
//! by the real strict SessionStore loader, not a colliding local SavedSession.
//! No Engine, ACP, model, transport, or authenticated remote route is started.

mod common;

use pinvou3_lib::features::sessions::{SessionKind, SessionStore};

use common::{HomeGuard, Isolation, create_session_fixture, env_lock, expect_failed, run_json};

const REMOTE_ID: &str = "remote-cli-fixture";

fn protected_state(home: &HomeGuard, local_id: &str) -> Vec<Option<Vec<u8>>> {
    [
        "sessions/_remote_proxies.json".to_owned(),
        "sessions/_remote_proxies.archives.sqlite3".to_owned(),
        "sessions/_remote_proxies.archives.anchor.json".to_owned(),
        "sessions/_pinned_sessions.json".to_owned(),
        "sessions/_hidden_sessions.json".to_owned(),
        "projects/projects.json".to_owned(),
        format!("sessions/{local_id}.json"),
        format!("sessions/{REMOTE_ID}.json"),
        format!("sessions/{REMOTE_ID}/persona_equipped.json"),
        "session-agents.json".to_owned(),
    ]
    .into_iter()
    .map(|path| match std::fs::read(home.path().join(path)) {
        Ok(bytes) => Some(bytes),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
        Err(error) => panic!("read protected fixture state: {error}"),
    })
    .collect()
}

#[test]
fn remote_proxy_local_writes_are_refused_without_changing_proxy_or_local_state() {
    let _env_guard = env_lock();
    let home = HomeGuard::new("remote-sessions", "write-boundary", Isolation::ProductHome);
    let local_id = create_session_fixture();
    let project = run_json(&["pinvou", "projects", "create", "--name", "Local project"]);
    let project_id = project["id"].as_str().unwrap();
    run_json(&["pinvou", "projects", "move", &local_id, project_id]);
    run_json(&["pinvou", "sessions", "pin", &local_id]);
    std::fs::write(
        home.sessions_root().join("_remote_proxies.json"),
        include_bytes!("fixtures/remote_proxy.json"),
    )
    .unwrap();
    let store = SessionStore::boot().unwrap();
    assert_eq!(
        store.session_kind(REMOTE_ID).unwrap(),
        SessionKind::RemoteProxy
    );
    assert!(!home.session_record(REMOTE_ID).exists());
    drop(store);
    let before = protected_state(&home, &local_id);

    for args in [
        vec!["pinvou", "projects", "move", REMOTE_ID, project_id],
        vec!["pinvou", "projects", "move", REMOTE_ID, "--yes"],
    ] {
        let error = expect_failed(&args);
        assert!(
            error.to_string().contains("authenticated remote route"),
            "the semantic project guard must reject before load/assign: {error}"
        );
        assert_eq!(protected_state(&home, &local_id), before, "{args:?}");
    }
    // A valid proxy has no local JSON, but refusal must name remote authority
    // rather than relying on that missing file to stop a local mutation.
    for args in [
        vec!["pinvou", "sessions", "rename", REMOTE_ID, "new title"],
        vec!["pinvou", "sessions", "pin", REMOTE_ID],
        vec!["pinvou", "sessions", "unpin", REMOTE_ID],
        vec!["pinvou", "sessions", "archive", REMOTE_ID],
        vec!["pinvou", "sessions", "restore", REMOTE_ID],
        vec!["pinvou", "sessions", "delete", REMOTE_ID, "--yes"],
    ] {
        let error = expect_failed(&args);
        assert!(
            error.to_string().contains("authenticated remote route"),
            "a missing local JSON file is not a remote ownership guard: {error}"
        );
        assert_eq!(protected_state(&home, &local_id), before, "{args:?}");
        assert_eq!(
            SessionStore::boot()
                .unwrap()
                .session_kind(REMOTE_ID)
                .unwrap(),
            SessionKind::RemoteProxy
        );
    }
    for args in [
        vec!["pinvou", "sessions", "show", REMOTE_ID],
        vec![
            "pinvou", "sessions", "export", REMOTE_ID, "--format", "json",
        ],
        vec!["pinvou", "sessions", "timeline", REMOTE_ID],
        vec!["pinvou", "sessions", "subagents", REMOTE_ID],
        vec!["pinvou", "sessions", "folder", REMOTE_ID],
        vec!["pinvou", "code", "sessions", "info", REMOTE_ID],
        vec!["pinvou", "code", "sessions", "timeline", REMOTE_ID],
        vec!["pinvou", "code", "workspace", "list", REMOTE_ID],
        vec!["pinvou", "code", "checkpoints", "list", REMOTE_ID],
        vec!["pinvou", "personas", "equip", REMOTE_ID, "fixture-persona"],
        vec!["pinvou", "personas", "unequip", REMOTE_ID],
        vec!["pinvou", "personas", "active", REMOTE_ID],
    ] {
        let error = expect_failed(&args);
        assert!(
            error.to_string().contains("authenticated remote route"),
            "{args:?}: {error}"
        );
        assert_eq!(protected_state(&home, &local_id), before, "{args:?}");
    }
}

fn assert_remote_artifact_is_not_local(writable: bool) {
    let _env_guard = env_lock();
    let home = HomeGuard::new(
        "remote-sessions",
        "artifact-boundary",
        Isolation::ProductHome,
    );
    let local_id = create_session_fixture();
    std::fs::write(
        home.sessions_root().join("_remote_proxies.json"),
        include_bytes!("fixtures/remote_proxy.json"),
    )
    .unwrap();
    assert_eq!(
        SessionStore::boot()
            .unwrap()
            .session_kind(REMOTE_ID)
            .unwrap(),
        SessionKind::RemoteProxy
    );
    assert!(!home.session_record(REMOTE_ID).exists());
    let directory = home.sessions_root().join(REMOTE_ID).join("artifacts");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::create_dir_all(home.workspace_of(REMOTE_ID)).unwrap();
    let artifact = directory.join("note.md");
    std::fs::write(&artifact, "remote-owned evidence").unwrap();
    let input = home.path().join("replacement.md");
    std::fs::write(&input, "must not replace remote evidence").unwrap();
    let input = input.to_str().unwrap();
    let before = protected_state(&home, &local_id);
    let args = if writable {
        vec![
            "pinvou",
            "artifacts",
            "write",
            REMOTE_ID,
            "../artifacts/note.md",
            "--file",
            input,
        ]
    } else {
        vec![
            "pinvou",
            "artifacts",
            "read",
            REMOTE_ID,
            "../artifacts/note.md",
        ]
    };
    let error = expect_failed(&args);
    assert!(
        error.to_string().contains("authenticated remote route"),
        "{error}"
    );
    assert_eq!(std::fs::read(&artifact).unwrap(), b"remote-owned evidence");
    assert_eq!(protected_state(&home, &local_id), before);
}

#[test]
fn remote_proxy_existing_artifacts_cannot_be_read_as_local_files() {
    assert_remote_artifact_is_not_local(false);
}

#[test]
fn remote_proxy_existing_artifacts_cannot_be_overwritten_as_local_files() {
    assert_remote_artifact_is_not_local(true);
}

#[test]
fn ordinary_local_artifact_directory_without_session_json_stays_readable_and_writable() {
    let _env_guard = env_lock();
    let home = HomeGuard::new("remote-sessions", "local-artifact", Isolation::ProductHome);
    let id = "legacy-local-artifact";
    let directory = home.sessions_root().join(id).join("artifacts");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::create_dir_all(home.workspace_of(id)).unwrap();
    let artifact = directory.join("note.md");
    std::fs::write(&artifact, "local evidence").unwrap();
    assert!(!home.session_record(id).exists());
    let result = run_json(&["pinvou", "artifacts", "read", id, "../artifacts/note.md"]);
    assert_eq!(result["content"], "local evidence");
    let input = home.path().join("replacement.md");
    std::fs::write(&input, "local update").unwrap();
    run_json(&[
        "pinvou",
        "artifacts",
        "write",
        id,
        "../artifacts/note.md",
        "--file",
        input.to_str().unwrap(),
    ]);
    assert_eq!(std::fs::read(&artifact).unwrap(), b"local update");
}

#[test]
fn unavailable_remote_namespace_preserves_the_classification_failure() {
    let _env_guard = env_lock();
    let home = HomeGuard::new("remote-sessions", "unavailable", Isolation::ProductHome);
    let id = "remote-unavailable";
    let directory = home.sessions_root().join(id).join("artifacts");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::create_dir_all(home.workspace_of(id)).unwrap();
    let artifact = directory.join("note.md");
    std::fs::write(&artifact, "unavailable evidence").unwrap();
    let expected = format!(
        "{:#}",
        SessionStore::boot().unwrap().session_kind(id).unwrap_err()
    );
    for args in [
        vec!["pinvou", "artifacts", "read", id, "../artifacts/note.md"],
        vec!["pinvou", "projects", "move", id, "--yes"],
    ] {
        let error = expect_failed(&args);
        assert!(error.to_string().contains(&expected), "{args:?}: {error}");
        assert!(!error.to_string().contains("authenticated remote route"));
    }
    assert_eq!(std::fs::read(&artifact).unwrap(), b"unavailable evidence");
}

#[test]
fn uppercase_local_names_follow_the_host_filename_alias_boundary() {
    let _env_guard = env_lock();
    let home = HomeGuard::new("remote-sessions", "case-alias", Isolation::ProductHome);
    let id = "REMOTE-local-artifact";
    let directory = home.sessions_root().join(id).join("artifacts");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::create_dir_all(home.workspace_of(id)).unwrap();
    let artifact = directory.join("note.md");
    std::fs::write(&artifact, "case-sensitive local evidence").unwrap();
    let args = ["pinvou", "artifacts", "read", id, "../artifacts/note.md"];
    #[cfg(target_os = "windows")]
    expect_failed(&args);
    #[cfg(not(target_os = "windows"))]
    assert_eq!(run_json(&args)["content"], "case-sensitive local evidence");
    assert_eq!(
        std::fs::read(&artifact).unwrap(),
        b"case-sensitive local evidence"
    );
}
