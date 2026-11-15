//! Test-only real SQLite/settings fixture for app-owned command composition.

#[cfg(target_os = "linux")]
pub(crate) use linux::{CARD_ID, Fixture, MODEL_ID, OTHER_MODEL_ID};

#[cfg(target_os = "linux")]
mod linux {
    use super::super::platform::linux::{config, db, script_runner, store_client};
    use crate::platform::prefs::{SavedModel, UserPrefs};
    use std::collections::BTreeMap;

    pub(crate) const CARD_ID: &str = "qwen38-27b-nvfp4-tachyon";
    pub(crate) const MODEL_ID: &str = "ipc-linked-model";
    pub(crate) const OTHER_MODEL_ID: &str = "ipc-unrelated-model";

    pub(crate) struct Fixture {
        // Hold the existing environment lock until every app job has finished.
        _home: config::TempHome,
    }

    impl Fixture {
        pub(crate) fn new(base_url: &str) -> Self {
            let fixture = Self {
                _home: config::TempHome::new(),
            };
            db::init_db().unwrap();
            db::add_store_model(db::AddStoreModelParams {
                card_id: CARD_ID,
                model_name: "wire",
                card_name: "Isolated IPC fixture",
                org: None,
                description: None,
                engine_name: "native",
                engine_version: None,
                size_bytes: 1,
                quantization: None,
                source_repo_id: None,
                source_filename: None,
                scripts: &BTreeMap::new(),
                status: "deployed",
                profiles: &[],
                deploy_topology: store_client::DeployTopology::Single,
                card_source: "store",
            })
            .unwrap();
            db::set_endpoint_info(CARD_ID, Some(base_url), Some("wire")).unwrap();
            let script = format!(
                "#!/bin/sh\necho LMM_ENDPOINT_URL={base_url}/chat/completions\necho LMM_ENDPOINT_MODEL=wire\n"
            );
            db::set_scripts_content(
                CARD_ID,
                &BTreeMap::from([(
                    "enable".into(),
                    db::ScriptBlob {
                        sha256: script_runner::sha256_hex(&script),
                        content: script,
                    },
                )]),
                &BTreeMap::new(),
            )
            .unwrap();
            let linked: SavedModel = serde_json::from_value(serde_json::json!({
                "id": MODEL_ID, "name": "Local model", "alias": "Keep this alias",
                "preset": "openai_compatible", "provider_kind": "custom",
                "model": "wire", "base_url": base_url, "model_store_card_id": CARD_ID,
                "context_window_tokens": 32768, "max_output_tokens": 4096,
                "reasoning_effort": "high"
            }))
            .unwrap();
            let mut unrelated = linked.clone();
            unrelated.id = OTHER_MODEL_ID.into();
            unrelated.model_store_card_id = None;
            unrelated.base_url = "http://127.0.0.1:1/v1".into();
            unrelated.context_window_tokens = Some(131072);
            UserPrefs::update_transaction(|prefs| {
                prefs.advanced.saved_models = vec![linked, unrelated];
                prefs.advanced.active_model_id = Some(OTHER_MODEL_ID.into());
                Ok(())
            })
            .unwrap();
            fixture
        }
    }
}
