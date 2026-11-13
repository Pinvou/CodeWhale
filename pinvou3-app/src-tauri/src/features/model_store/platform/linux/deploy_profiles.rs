//! Deployment defaults are independent of mutable catalogue ordering.
use super::store_client::DeployProfile;

pub fn default_id<'a>(card_id: &str, profiles: &'a [DeployProfile]) -> Option<&'a str> {
    if card_id == "qwen38-27b-nvfp4-mtp" {
        if let Some(production) = profiles.iter().find(|p| p.id == "production-sampling") {
            return Some(&production.id);
        }
    }
    profiles.first().map(|p| p.id.as_str())
}

pub fn selected_id<'a>(
    card_id: &str,
    profiles: &'a [DeployProfile],
    saved: Option<&str>,
) -> Option<&'a str> {
    saved
        .and_then(|id| profiles.iter().find(|p| p.id == id))
        .map(|p| p.id.as_str())
        .or_else(|| default_id(card_id, profiles))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mtp_defaults_to_256k_independently_of_order_and_preserves_valid_user_choices() {
        let mut profiles: Vec<DeployProfile> = serde_json::from_value(serde_json::json!([
            {"id":"benchmark","name":"Benchmark"},
            {"id":"production-sampling","name":"Production"}
        ]))
        .unwrap();
        let card = "qwen38-27b-nvfp4-mtp";
        assert_eq!(default_id(card, &profiles), Some("production-sampling"));
        assert_eq!(
            selected_id(card, &profiles, Some("benchmark")),
            Some("benchmark")
        );
        assert_eq!(
            selected_id(card, &profiles, Some("removed")),
            Some("production-sampling")
        );
        profiles.reverse();
        assert_eq!(default_id(card, &profiles), Some("production-sampling"));
        profiles.retain(|p| p.id == "benchmark");
        assert_eq!(default_id(card, &profiles), Some("benchmark"));
        assert_eq!(default_id(card, &[]), None);
        assert_eq!(default_id("other-model", &profiles), Some("benchmark"));
    }
}
