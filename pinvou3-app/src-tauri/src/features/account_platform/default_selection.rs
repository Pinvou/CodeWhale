use crate::platform::prefs::{ModelSelectionRef, UserPrefs};

const DEFAULT_OBSERVATION_OBSOLETE: &str = "model_default_observation_obsolete";

pub(crate) fn retire_catalog_default(
    catalog: &serde_json::Value,
    expected: Option<&ModelSelectionRef>,
    revision: &str,
) -> Result<bool, String> {
    let Some(selection @ ModelSelectionRef::AccountPlatform { model_id }) = expected else {
        return Ok(false);
    };
    // An absent item, incomplete/error response, or unknown status is not proof
    // that a model was retired. Never clear a different or newer local intent.
    if !catalog
        .get("catalog_version")
        .and_then(serde_json::Value::as_u64)
        .is_some_and(|version| version > 0)
    {
        return Ok(false);
    }
    let retired = catalog
        .get("items")
        .and_then(serde_json::Value::as_array)
        .and_then(|items| {
            items.iter().find(|item| {
                item.get("model_id").and_then(serde_json::Value::as_str) == Some(model_id)
            })
        })
        .and_then(|item| {
            item.get("lifecycle_status")
                .and_then(serde_json::Value::as_str)
        })
        .is_some_and(|status| matches!(status, "DEPRECATED" | "REMOVED"));
    if !retired {
        return Ok(false);
    };
    match UserPrefs::update_transaction(|prefs| {
        if prefs.retire_observed_platform_default(selection, revision) {
            Ok(())
        } else {
            Err(DEFAULT_OBSERVATION_OBSOLETE.to_string())
        }
    }) {
        Ok(_) => Ok(true),
        Err(error) if error == DEFAULT_OBSERVATION_OBSOLETE => Ok(false),
        Err(error) => Err(error),
    }
}
