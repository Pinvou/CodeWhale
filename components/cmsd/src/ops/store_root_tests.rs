//! Store root is execution identity, not a new default from daemon settings.
use super::*;
use crate::docker::fixtures as fx;
use crate::store::TaskStatus;
use std::os::unix::fs::PermissionsExt;

struct Fixture {
    root: PathBuf,
    app: Arc<App>,
    inventory: PathBuf,
}
impl Fixture {
    fn new(tag: &str, store: bool) -> Self {
        let root = fx::tmpdir(tag);
        let mut app = fx::make_app(&root, 128, 100);
        let inventory = root.join("inventory.json");
        let config = &mut Arc::get_mut(&mut app).unwrap().config;
        config.models_dir = root.join("later-setting-B");
        std::fs::create_dir_all(&config.models_dir).unwrap();
        if store {
            let program = root.join("reader");
            std::fs::write(
                &program,
                format!("#!/bin/sh\ncat '{}'\n", inventory.display()),
            )
            .unwrap();
            std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o700)).unwrap();
            let profile = root.join("profile");
            std::fs::create_dir_all(profile.join("model-store")).unwrap();
            config.store_inventory_program = Some(program);
            config.store_inventory_profile = Some(profile);
        }
        Self {
            root,
            app,
            inventory,
        }
    }
    fn card(&self, physical: &str, fixed: &Path, port: u16) {
        std::fs::create_dir_all(fixed.join("weights")).unwrap();
        let name = format!("{physical}-ctr");
        let directory = fx::write_docker_card(
            &self.app.config.cards_dir,
            physical,
            &name,
            port,
            Some("0.01"),
        );
        std::fs::write(directory.join("vendor/.deploy-env"), format!("IMAGE=\"fixture/img:1.0\"\nCONTAINER_NAME=\"{name}\"\nPORT=\"{port}\"\nMODEL_DIR=\"{}\"\n", fixed.join("weights").display())).unwrap();
        // This fixture's ordinary recipe uses the same LMM root at download,
        // deploy and first creation. Its creation side effects are fake Docker
        // state only: no daemon, Docker command or real model is started.
        let script = directory.join("enable.sh");
        std::fs::write(&script, format!(r#"#!/bin/sh
printf '%s' "$LMM_MODELS_DIR" > '{observed}'
[ "$LMM_MODELS_DIR" = '{fixed}' ] || {{ echo 'wrong fixed root'; exit 41; }}
printf '%s\n' '{name}^fixture/img:1.0^true^no^0^false^4321^{port}^8000^--served-model-name,Fixed,--mem-fraction-static,0.01' >> '{state}'
printf '{name}^%s/weights\n' "$LMM_MODELS_DIR" >> '{mounts}'
echo 'created with the fixed root'
"#, observed=self.root.join(format!("{physical}.received-root")).display(), fixed=fixed.display(), state=self.root.join("docker.state").display(), mounts=self.root.join("docker.mounts").display())).unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o700)).unwrap();
    }
    fn select(&self, physical: &str, fixed: serde_json::Value) {
        let directory = self.app.config.cards_dir.join(physical);
        let files: serde_json::Map<_, _> = ["common.sh", "vendor/.deploy-env", "enable.sh"]
            .into_iter()
            .map(|name| {
                let raw = std::fs::read(directory.join(name)).unwrap();
                let hash: String = ring::digest::digest(&ring::digest::SHA256, &raw)
                    .as_ref()
                    .iter()
                    .map(|byte| format!("{byte:02x}"))
                    .collect();
                (name.to_owned(), hash.into())
            })
            .collect();
        let value = serde_json::json!({"format":"pinvou-installed-models-v1","selection_policy":"latest-installed-v1", "cards_root":self.app.config.cards_dir, "skipped":[], "selections":[{"logical_card":"logical","card":physical,"version_id":physical}], "archived_models":[], "models":[{"card":physical,"logical_card":"logical","version_id":physical,"origin":"store","store_status":"deployed","downloaded_at":"today","files":files,"models_root":fixed,"deployment_revision":format!("{physical}-profile")}]});
        std::fs::write(&self.inventory, value.to_string()).unwrap();
    }
    fn enable(&self, requested: &str) -> crate::store::TaskRecord {
        let task = super::enable(&self.app, requested).unwrap();
        fx::wait_terminal(&self.app, &task, 10)
    }
    fn observed(&self, physical: &str) -> PathBuf {
        self.root.join(format!("{physical}.received-root"))
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

#[test]
fn current_store_root_survives_settings_change_and_explicit_version_reselection() {
    let f = Fixture::new("fixed-store-root", true);
    let first = f.root.join("downloaded-A");
    let second = f.root.join("downloaded-C");
    let port_a = fx::serve_models_json("Fixed");
    let port_c = fx::serve_models_json("Fixed");
    f.card("physical-a", &first, port_a);
    f.card("physical-c", &second, port_c);
    f.select("physical-a", serde_json::json!(first));
    // An earlier enrollment copy must never override the current receipt.
    std::fs::create_dir_all(f.app.config.state_dir.join("store-installed")).unwrap();
    std::fs::write(
        f.app
            .config
            .state_dir
            .join("store-installed/physical-a.json"),
        serde_json::json!({"models_root":f.app.config.models_dir}).to_string(),
    )
    .unwrap();
    let done = f.enable("logical");
    assert_eq!(done.status, TaskStatus::Succeeded, "{:?}", done.error);
    assert_eq!(
        std::fs::read_to_string(f.observed("physical-a")).unwrap(),
        first.to_string_lossy()
    );
    f.select("physical-c", serde_json::json!(second));
    let done = f.enable("logical");
    assert_eq!(done.status, TaskStatus::Succeeded, "{:?}", done.error);
    assert_eq!(
        std::fs::read_to_string(f.observed("physical-c")).unwrap(),
        second.to_string_lossy()
    );
    assert!(fx::state_line(&f.root, "physical-a-ctr")
        .unwrap()
        .contains("^false^"));
    assert_eq!(
        f.app.store.lock().unwrap().state.active_model.as_deref(),
        Some("physical-c")
    );
}

#[test]
fn unknown_or_changed_store_root_refuses_before_creation_or_new_authority() {
    for (index, value) in [
        serde_json::Value::Null,
        serde_json::json!(42),
        serde_json::json!(""),
        serde_json::json!("relative/root"),
        serde_json::json!("/absent/store-root"),
        serde_json::json!("/tmp/nul\0root"),
    ]
    .into_iter()
    .enumerate()
    {
        let f = Fixture::new(&format!("unknown-store-root-{index}"), true);
        let fixed = f.root.join("downloaded-A");
        f.card("physical", &fixed, 18199);
        f.select("physical", value);
        let done = f.enable("logical");
        assert_eq!(done.status, TaskStatus::Failed);
        assert!(done.error.unwrap().starts_with("store_models_root_"));
        assert!(!f.observed("physical").exists());
        assert!(!f
            .app
            .store
            .lock()
            .unwrap()
            .state
            .models
            .get("physical")
            .is_some_and(|model| model.lifecycle_owned));
        assert!(!crate::recovery::managed(&f.app, "physical"));
        assert!(!fx::docker_calls(&f.root)
            .iter()
            .any(|call| call.starts_with("start ") || call.starts_with("update ")));
    }
    let f = Fixture::new("linked-store-root", true);
    let fixed = f.root.join("downloaded-A");
    f.card("physical", &fixed, 18199);
    let alias = f.root.join("replaced-link");
    std::os::unix::fs::symlink(&fixed, &alias).unwrap();
    f.select("physical", serde_json::json!(alias));
    let done = f.enable("logical");
    assert_eq!(done.status, TaskStatus::Failed);
    assert!(done
        .error
        .unwrap()
        .starts_with("store_models_root_changed:"));
    assert!(!f.observed("physical").exists());
}

#[test]
fn existing_store_container_needs_no_missing_legacy_root_or_script_replay() {
    for running in [true, false] {
        let f = Fixture::new(&format!("existing-store-root-unknown-{running}"), true);
        let fixed = f.root.join("downloaded-A");
        let port = fx::serve_models_json("Fixed");
        f.card("physical", &fixed, port);
        f.select("physical", serde_json::Value::Null);
        fx::set_container(
            &f.root,
            "physical-ctr",
            "fixture/img:1.0",
            running,
            "no",
            0,
            false,
            if running { 4321 } else { 0 },
            port,
            8000,
            "--served-model-name,Fixed,--mem-fraction-static,0.01",
        );
        let done = f.enable("logical");
        assert_eq!(done.status, TaskStatus::Succeeded, "{:?}", done.error);
        assert!(!f.observed("physical").exists());
        assert_eq!(
            fx::docker_calls(&f.root)
                .iter()
                .filter(|call| call.starts_with("start "))
                .count(),
            usize::from(!running)
        );
    }
}

#[test]
fn standalone_card_keeps_explicit_daemon_configuration_for_first_creation() {
    let f = Fixture::new("standalone-store-root", false);
    let port = fx::serve_models_json("Fixed");
    f.card("external", &f.app.config.models_dir, port);
    let done = f.enable("external");
    assert_eq!(done.status, TaskStatus::Succeeded, "{:?}", done.error);
    assert_eq!(
        std::fs::read_to_string(f.observed("external")).unwrap(),
        f.app.config.models_dir.to_string_lossy()
    );
}
