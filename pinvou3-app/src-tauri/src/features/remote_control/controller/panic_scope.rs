//! Compatibility path for the controller; the shared execution primitive
//! lives in core so both the controller and its adapters use one panic scope.
pub(crate) use crate::core::panic_scope::{future, run};
