// Use a real parent module file: an inline module's virtual directory need not
// exist in a clean POSIX checkout, so #[path] cannot safely traverse through it.
pub(crate) use crate::legacy_protocol as protocol;

#[path = "../../../src/features/remote_control/relay_client.rs"]
pub(crate) mod relay_client;
