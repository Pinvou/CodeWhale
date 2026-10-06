// Compile the production ledger and budgets, not the later-wave file adapters.
// Keep both parent-module boundaries identical to the product graph.
#[path = "../../../src/features/remote_control/controller/transfer/core.rs"]
pub(crate) mod core;

#[path = "../../../src/features/remote_control/controller/transfer/limits.rs"]
mod limits;

pub(crate) use core::{TransferDirection, TransferRegistry};

#[path = "../../../src/features/remote_control/controller/transfer/upload.rs"]
pub(crate) mod upload;

#[path = "../../../src/features/remote_control/controller/transfer/download.rs"]
pub(crate) mod download;

#[path = "../../../src/features/remote_control/controller/transfer/staging.rs"]
pub(crate) mod staging;
