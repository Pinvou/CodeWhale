//! Resource-bounded test harness for the disconnected controller modules.
//!
//! This avoids linking the full Tauri/CodeWhale lib-test binary while DEV-05
//! remains deliberately outside all application routing.

#![allow(dead_code)]

#[path = "../../../src/features/remote_control/controller/protocol.rs"]
mod protocol;

#[path = "../../../src/features/remote_control/controller/crypto.rs"]
mod crypto;
