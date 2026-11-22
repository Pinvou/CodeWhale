//! Small native test target for the production server, without Tauri/ARM64 builds.
#![allow(dead_code)]
mod db;
#[path = "../../pinvou3-app/src-tauri/src/features/model_store/platform/linux/lan_gateway.rs"]
mod lan_gateway;
#[path = "../../pinvou3-app/src-tauri/src/features/model_store/platform/linux/lan_network.rs"]
mod lan_network;
#[path = "../../pinvou3-app/src-tauri/src/features/model_store/platform/linux/lan_store.rs"]
mod lan_store;
#[cfg(test)]
mod network_tests;
mod service {
    #[derive(Default)]
    pub struct ModelStoreService {
        pub lan: super::lan_gateway::Gateway,
    }
}
mod platform {
    pub mod process {
        // Compile-time interface shim only: no Docker invocation in this harness.
        pub fn output_with_timeout(
            _: std::process::Command,
            _: std::time::Duration,
        ) -> Result<std::process::Output, String> {
            Err("container helper requires the full application".into())
        }
    }
}
