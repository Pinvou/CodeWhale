//! Ranked settings resolution.
//!
//! One rank table for every setting: the highest-ranked layer that names a
//! scalar wins outright.
//!
//! Layers with no producer today (managed policy files, project settings)
//! simply contribute no entries; the rank is the contract, and an empty
//! layer abstains rather than vetoing.

/// Where a setting value came from, highest rank first.
///
/// The order is the product rule: policy beats the CLI flag, the flag beats
/// a session override, overrides beat files, and files beat the default.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Layer {
    /// Organization / managed policy. No file producer yet.
    ManagedPolicy,
    /// Per-run `-c key=value` CLI flag (lands with the config CLI slice).
    CliFlag,
    /// Session override (`/set`, `/config <key> <value>` without `--save`).
    SessionOverride,
    /// Project settings. No file producer yet.
    ProjectConfig,
    /// `settings.toml` (user config).
    UserConfig,
    /// The schema default.
    Default,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rank_order_is_policy_over_cli_over_session_over_files_over_default() {
        let layers = [
            Layer::ManagedPolicy,
            Layer::CliFlag,
            Layer::SessionOverride,
            Layer::ProjectConfig,
            Layer::UserConfig,
            Layer::Default,
        ];
        let mut sorted = layers;
        sorted.sort();
        assert_eq!(sorted, layers);
    }
}
