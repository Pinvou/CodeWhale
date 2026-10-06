// Compile the production ledger and budgets, not the later-wave file adapters.
// Keep both parent-module boundaries identical to the product graph.
#[path = "../../../src/features/remote_control/controller/transfer/core.rs"]
mod core;

#[path = "../../../src/features/remote_control/controller/transfer/limits.rs"]
mod limits;

pub(crate) use core::{TransferDirection, TransferRegistry};
pub(crate) use limits::TransferLimits;

#[path = "../../../src/features/remote_control/controller/transfer/upload.rs"]
mod upload;
