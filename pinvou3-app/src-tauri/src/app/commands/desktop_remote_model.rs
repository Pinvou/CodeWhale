//! Private temporary-model lifecycle state. A07 intentionally has no renderer
//! model descriptor/install/release commands. A08 must resolve OS credentials
//! into SensitiveString inside the authenticated Rust sender, never via JS.

use crate::features::remote_control::controller::temporary_model::{
    SharedTemporaryModelStore, TemporaryModelStore,
};
use std::sync::Arc;

pub(crate) fn shared_model_store() -> SharedTemporaryModelStore {
    Arc::new(TemporaryModelStore::default())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn shared_store_is_empty_on_creation() {
        let store = shared_model_store();
        assert!(store.is_empty());
        assert!(store.active_task().is_none());
    }
}
