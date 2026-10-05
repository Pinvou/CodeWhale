//! Current-context model selection; authorization remains owned by the Gateway.
use super::{AccountPlatformConfig, AccountPlatformState, ApiRequest};
use crate::platform::prefs::{BUILTIN_ACCOUNT_PLATFORM_MODEL_ID, UserPrefs};
use serde::{Deserialize, Serialize};
use serde_json::Value;

const MAX_MODELS: usize = 4096;

#[derive(Debug, Serialize)]
pub struct BuiltinModelsView {
    pub available_models: Vec<String>,
    pub default_model: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Catalog {
    data: Vec<Entry>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Entry {
    id: String,
}

fn decode_catalog(body: Value) -> Result<Vec<String>, String> {
    let catalog: Catalog = serde_json::from_value(body)
        .map_err(|_| "account_platform_model_catalog_invalid".to_string())?;
    if catalog.data.len() > MAX_MODELS {
        return Err("account_platform_model_catalog_invalid".into());
    }
    let mut ids = std::collections::BTreeSet::new();
    for entry in catalog.data {
        if entry.id.trim().is_empty() {
            return Err("account_platform_model_catalog_invalid".into());
        }
        ids.insert(entry.id);
    }
    Ok(ids.into_iter().collect())
}

impl AccountPlatformState {
    pub(super) async fn model_catalog_with(
        &self,
        config: &AccountPlatformConfig,
    ) -> Result<Vec<String>, String> {
        let epoch = self.session_epoch.load(std::sync::atomic::Ordering::SeqCst);
        // The same verified account bootstrap as chat is required before the
        // current-context catalog can be resolved. Tokens never enter this DTO.
        self.chat_runtime_authorization_with(config).await?;
        let response = self
            .request_with(
                config,
                ApiRequest {
                    area: "token".into(),
                    method: "GET".into(),
                    resource: "/v1/models".into(),
                    body: None,
                    idempotency_key: None,
                },
            )
            .await?;
        if response.status != 200 {
            return Err("account_platform_model_catalog_unavailable".into());
        }
        self.require_session_epoch(epoch)?;
        decode_catalog(response.body)
    }

    pub async fn builtin_models(&self) -> Result<BuiltinModelsView, String> {
        let _context = self.model_context_guard.lock().await;
        let epoch = self.session_epoch.load(std::sync::atomic::Ordering::SeqCst);
        let available_models = self
            .model_catalog_with(&AccountPlatformConfig::from_env()?)
            .await?;
        let _refresh = self.refresh_guard.lock().await;
        self.require_session_epoch(epoch)?;
        let prefs = UserPrefs::load();
        let default_model = prefs
            .model_by_id(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .ok_or_else(|| "account_platform_model_configuration_invalid".to_string())?
            .model
            .clone();
        let default_model = if available_models.contains(&default_model) {
            default_model
        } else {
            String::new()
        };
        Ok(BuiltinModelsView {
            available_models,
            default_model,
        })
    }

    pub async fn set_builtin_default_model(
        &self,
        model: String,
    ) -> Result<BuiltinModelsView, String> {
        self.set_builtin_default_model_with(&AccountPlatformConfig::from_env()?, model)
            .await
    }

    pub(super) async fn set_builtin_default_model_with(
        &self,
        config: &AccountPlatformConfig,
        model: String,
    ) -> Result<BuiltinModelsView, String> {
        let _context = self.model_context_guard.lock().await;
        let epoch = self.session_epoch.load(std::sync::atomic::Ordering::SeqCst);
        let available_models = self.model_catalog_with(config).await?;
        self.require_session_epoch(epoch)?;
        if !available_models.contains(&model) {
            return Err("account_platform_model_not_available".into());
        }
        let _refresh = self.refresh_guard.lock().await;
        self.require_session_epoch(epoch)?;
        persist_default(&model)?;
        self.require_session_epoch(epoch)?;
        Ok(BuiltinModelsView {
            available_models,
            default_model: model,
        })
    }
}

fn persist_default(model: &str) -> Result<(), String> {
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.account_platform_default_model = Some(model.to_string());
        prefs.ensure_builtin_account_platform_model();
        Ok(())
    })
    .map(|_| ())
    .map_err(|_| "account_platform_model_configuration_failed".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn catalog_accepts_only_bounded_model_ids_and_no_provider_fields() {
        assert_eq!(
            decode_catalog(
                serde_json::json!({"data":[{"id":"model-b"},{"id":"model-a"},{"id":"model-a"}]})
            )
            .unwrap(),
            vec!["model-a", "model-b"]
        );
        let long_id = "x".repeat(201);
        assert_eq!(
            decode_catalog(serde_json::json!({"data":[{"id":long_id}]})).unwrap(),
            vec![long_id]
        );
        for body in [
            serde_json::json!(null),
            serde_json::json!({"data":[{"id":""}]}),
            serde_json::json!({"data":[{"id":"   "}]}),
            serde_json::json!({"data":[{"id":"x","secret":"dummy"}]}),
            serde_json::json!({"data":[],"owner":"dummy"}),
        ] {
            assert_eq!(
                decode_catalog(body).unwrap_err(),
                "account_platform_model_catalog_invalid"
            );
        }
        assert!(
            decode_catalog(
                serde_json::json!({"data":vec![serde_json::json!({"id":"x"});MAX_MODELS+1]})
            )
            .is_err()
        );
        assert_eq!(
            decode_catalog(serde_json::json!({"data":[]})).unwrap(),
            Vec::<String>::new()
        );
    }
}
