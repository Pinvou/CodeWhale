//! Shared controller transfer core (DEV-14).
//!
//! Attachment upload (DEV-17) and artifact download (DEV-18) share one
//! accounting core so both directions compete for the same connection budget
//! without ever consuming each other's handles. This module owns the frozen
//! contract: [`core`] (binding, handles, chunk accounting, cleanup) and
//! [`limits`] (byte, count and timeout budgets).
//!
//! `upload.rs` and `download.rs` are intentionally thin extension slots. DEV-17
//! only edits `upload.rs` and DEV-18 only edits `download.rs`; a change to the
//! shared accounting must come back to this core first.

pub(crate) mod core;
pub(crate) mod download;
pub(crate) mod limits;
pub(crate) mod upload;

// Re-exported for DEV-17/DEV-18, which only add their own slot file.
#[allow(unused_imports)]
pub(crate) use self::core::{
    ChunkOutcome, CompletedTransfer, TransferBinding, TransferContext, TransferDirection,
    TransferError, TransferHandle, TransferMetadata, TransferRegistry,
};
#[allow(unused_imports)]
pub(crate) use self::limits::TransferLimits;
