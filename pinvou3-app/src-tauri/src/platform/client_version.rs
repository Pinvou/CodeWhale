/// Stable wire identity attached to Account Platform requests from the exact
/// desktop artifact. Keeping this below feature modules prevents the model
/// runtime and account UI adapters from depending on each other.
pub(crate) const HEADER: &str = "X-Pinvou-Client-Version";
pub(crate) const CURRENT: &str = env!("CARGO_PKG_VERSION");
