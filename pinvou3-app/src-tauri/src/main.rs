// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    // 模型商店开机自启恢复器（systemd user 服务调用的 headless 入口）：
    #![cfg_attr(not(target_os = "linux"), allow(unused))]
    let args: Vec<String> = std::env::args().collect();
    #[cfg(target_os = "linux")]
    if args.len() == 2 && args[1] == "model-store-autostart" {
        pinvou3_lib::run_model_store_autostart();
        return;
    }
    pinvou3_lib::run();
}
