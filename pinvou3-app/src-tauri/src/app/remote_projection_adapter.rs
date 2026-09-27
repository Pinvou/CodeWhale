//! DEV-30 application-composition slot for remote session projection adapters.
//!
//! Reserved for the owner implementing `RemoteSessionProjectionPort` and
//! `ApplicationEventPort` at the app boundary. Projection behavior remains in
//! its feature modules; this slot only wires those ports without creating a
//! sessions-to-remote-control dependency cycle.
