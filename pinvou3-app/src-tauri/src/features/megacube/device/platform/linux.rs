//! Fresh, bounded observations. A failed/partial query never means no workload.
use std::io::Read;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use serde_json::{Value, json};

fn small(path: &Path, limit: u64) -> Result<Vec<u8>, String> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)
        .and_then(|file| file.take(limit + 1).read_to_end(&mut bytes))
        .map_err(|_| "observation_unavailable")?;
    if bytes.len() as u64 > limit {
        return Err("observation_limit".into());
    }
    Ok(bytes)
}

pub(super) fn compatible_board() -> bool {
    [
        "/sys/firmware/devicetree/base/model",
        "/sys/class/dmi/id/product_name",
    ]
    .iter()
    .filter_map(|path| small(Path::new(path), 4096).ok())
    .any(|raw| board_hint(&String::from_utf8_lossy(&raw)))
}

fn board_hint(value: &str) -> bool {
    let value = value.to_ascii_lowercase().replace(['_', '-'], " ");
    value.split_whitespace().any(|word| word == "gb10") || value.contains("dgx spark")
}

fn smi(args: &[&str]) -> Result<String, String> {
    let binary = ["/usr/bin/nvidia-smi", "/bin/nvidia-smi"]
        .into_iter()
        .find(|path| Path::new(path).is_file())
        .ok_or("nvidia_driver_unavailable")?;
    let mut command = Command::new(binary);
    command.args(args).env("LC_ALL", "C");
    let output = crate::platform::process::output_with_timeout(command, Duration::from_secs(3))
        .map_err(|_| "gpu_query_failed")?;
    if !output.status.success() || output.stdout.len() > 64 * 1024 {
        return Err("gpu_query_failed".into());
    }
    String::from_utf8(output.stdout).map_err(|_| "gpu_query_failed".into())
}

fn kind(args: &[&str]) -> Option<&'static str> {
    let executable = args
        .first()
        .and_then(|arg| Path::new(arg).file_name())?
        .to_str()?;
    match executable {
        "tachyon-serve" | "llama-server" | "llama-cpp-server" | "ollama" => {
            Some(match executable {
                "tachyon-serve" => "tachyon",
                "ollama" => "ollama",
                _ => "llama.cpp",
            })
        }
        value if value.starts_with("python") => args.iter().find_map(|arg| {
            if *arg == "vllm" || arg.starts_with("vllm.") || arg.contains("/vllm/") {
                Some("vllm")
            } else if *arg == "sglang" || arg.starts_with("sglang.") || arg.contains("/sglang/") {
                Some("sglang")
            } else {
                None
            }
        }),
        "vllm" => Some("vllm"),
        _ => None,
    }
}

fn native_models(proc_root: &Path) -> (Vec<Value>, bool) {
    let start = Instant::now();
    let Ok(entries) = std::fs::read_dir(proc_root) else {
        return (vec![], false);
    };
    let mut models = Vec::new();
    let mut complete = true;
    let mut count = 0;
    for entry in entries {
        let Ok(entry) = entry else {
            complete = false;
            continue;
        };
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|name| name.parse::<u32>().ok())
        else {
            continue;
        };
        count += 1;
        if count > 4096 || start.elapsed() > Duration::from_secs(2) {
            complete = false;
            break;
        }
        let command = entry.path().join("cmdline");
        match small(&command, 16 * 1024) {
            Ok(raw) => {
                let Ok(text) = std::str::from_utf8(&raw) else {
                    complete = false;
                    continue;
                };
                let args: Vec<_> = text.split('\0').filter(|arg| !arg.is_empty()).collect();
                if let Some(kind) = kind(&args) {
                    models.push(json!({"pid":pid,"kind":kind}));
                }
            }
            Err(_) if !entry.path().exists() => {} // A process exited during observation.
            Err(_) => complete = false,
        }
    }
    (models, complete)
}

fn gpu_processes(raw: &str) -> Result<Vec<Value>, String> {
    let mut result = Vec::new();
    for line in raw.lines().filter(|line| !line.trim().is_empty()) {
        let (pid, name) = line.split_once(',').ok_or("gpu_query_invalid")?;
        let pid = pid.trim().parse::<u32>().map_err(|_| "gpu_query_invalid")?;
        if pid == 0 {
            return Err("gpu_query_invalid".into());
        }
        let name = Path::new(name.trim())
            .file_name()
            .and_then(|name| name.to_str())
            .filter(|name| name.len() <= 256 && !name.chars().any(char::is_control))
            .ok_or("gpu_query_invalid")?;
        result.push(json!({"pid":pid,"name":name}));
    }
    Ok(result)
}

fn gpu_names(raw: &str) -> Result<Vec<String>, String> {
    let mut names = Vec::new();
    for line in raw.lines().filter(|line| !line.trim().is_empty()) {
        let (name, driver) = line.split_once(',').ok_or("gpu_query_invalid")?;
        let (name, driver) = (name.trim(), driver.trim());
        if name.is_empty()
            || matches!(name, "N/A" | "[Not Supported]")
            || name.len() > 256
            || name.chars().any(char::is_control)
            || driver.is_empty()
            || driver.len() > 80
            || driver
                .split('.')
                .any(|part| part.is_empty() || !part.bytes().all(|byte| byte.is_ascii_digit()))
        {
            return Err("gpu_query_invalid".into());
        }
        names.push(name.to_owned());
    }
    if names.is_empty() {
        return Err("gpu_query_invalid".into());
    }
    Ok(names)
}

pub(super) fn inspect() -> (Value, Value) {
    let gpu = smi(&[
        "--query-gpu=name,driver_version",
        "--format=csv,noheader,nounits",
    ])
    .and_then(|raw| gpu_names(&raw));
    let os = small(Path::new("/etc/os-release"), 16 * 1024)
        .ok()
        .and_then(|raw| String::from_utf8(raw).ok());
    let platform = std::env::consts::ARCH == "aarch64"
        && os.as_deref().is_some_and(|value| {
            value
                .lines()
                .any(|line| matches!(line, "ID=ubuntu" | "ID=\"ubuntu\""))
                && value
                    .lines()
                    .any(|line| matches!(line, "VERSION_ID=24.04" | "VERSION_ID=\"24.04\""))
        });
    let hardware = match &gpu {
        Ok(value)
            if platform
                && value
                    .iter()
                    .any(|name| name.split_whitespace().any(|word| word == "GB10")) =>
        {
            json!({"state":"compatible","label":"NVIDIA GB10 compatible device","rebootRequired":Path::new("/var/run/reboot-required").exists()})
        }
        Ok(_) => {
            json!({"state":"mismatch","reason":"device_hardware_mismatch","rebootRequired":Path::new("/var/run/reboot-required").exists()})
        }
        Err(reason) => {
            json!({"state":"unknown","reason":reason,"rebootRequired":Path::new("/var/run/reboot-required").exists()})
        }
    };
    let (models, complete) = native_models(Path::new("/proc"));
    let gpu_processes = smi(&[
        "--query-compute-apps=pid,process_name",
        "--format=csv,noheader,nounits",
    ])
    .and_then(|raw| gpu_processes(&raw));
    let state = if !models.is_empty()
        || gpu_processes
            .as_ref()
            .is_ok_and(|processes| !processes.is_empty())
    {
        "running"
    } else if complete && gpu.is_ok() && gpu_processes.is_ok() {
        "idle"
    } else {
        "unknown"
    };
    let reason = if !complete {
        Some("process_observation_incomplete".to_string())
    } else {
        gpu_processes
            .as_ref()
            .err()
            .cloned()
            .or_else(|| gpu.as_ref().err().cloned())
    };
    (
        hardware,
        json!({"state":state,"observedAt":chrono::Utc::now().to_rfc3339(),"models":models,"gpuProcesses":gpu_processes.unwrap_or_default(),"reason":reason}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn observation_uses_arguments_not_arbitrary_model_name_substrings() {
        assert!(board_hint("NVIDIA_DGX_Spark\0"));
        assert!(board_hint("NVIDIA DGX Spark"));
        assert!(board_hint("NVIDIA GB10 Development Kit"));
        assert!(!board_hint("Generic Ubuntu Workstation"));
        assert!(!board_hint("NVIDIA GB100"));
        assert_eq!(
            kind(&["/opt/tachyon-serve", "--model", "x"]),
            Some("tachyon")
        );
        assert_eq!(
            kind(&["python3", "-m", "sglang.launch_server"]),
            Some("sglang")
        );
        assert_eq!(kind(&["bash", "echo", "vllm.launch"]), None);
        assert_eq!(kind(&["python3", "analysis.py", "my-vllm-report"]), None);
        assert!(gpu_processes("N/A, N/A").is_err());
        assert!(gpu_names("").is_err());
        assert!(gpu_names("NVIDIA GB10").is_err());
        assert!(gpu_names("NVIDIA GB10, N/A").is_err());
        assert_eq!(
            gpu_names("NVIDIA GB10, 580.95.05\n").unwrap(),
            vec!["NVIDIA GB10"]
        );
        assert!(gpu_processes("").unwrap().is_empty());
        assert_eq!(
            gpu_processes("12, /opt/engine/tachyon-serve\n").unwrap()[0]["pid"],
            12
        );
    }
    #[test]
    fn inaccessible_or_truncated_process_observation_is_not_idle() {
        let root = tempfile::tempdir().unwrap();
        let pid = root.path().join("123");
        std::fs::create_dir(&pid).unwrap();
        assert!(!native_models(root.path()).1);
        std::fs::write(pid.join("cmdline"), b"tachyon-serve\0--model\0private\0").unwrap();
        let (models, complete) = native_models(root.path());
        assert!(complete);
        assert_eq!(models, vec![json!({"pid":123,"kind":"tachyon"})]);
        assert!(!models[0].to_string().contains("private"));
    }
}

pub fn status(inspect_running: bool) -> Result<Value, String> {
    let detected = compatible_board();
    let (hardware, running) = if detected && inspect_running {
        inspect()
    } else {
        (
            json!({"state":"not_checked"}),
            json!({"state":"not_checked","models":[],"gpuProcesses":[]}),
        )
    };
    Ok(json!({"supported":true,"detected":detected,"hardware":hardware,"running":running}))
}
