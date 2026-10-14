//! Read-only discovery of native inference processes. Never expose command lines
//! (which may contain credentials) to the UI, and never infer identity from a PID
//! file alone. Mutating operations must separately revalidate process identity.

use std::fs;
use std::io::Read;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

#[derive(Debug, Clone)]
pub struct NativeProcess {
    pub pid: u32,
    pub start_ticks: u64,
    pub executable: PathBuf,
    pub model_id: Option<String>,
    pub port: Option<u16>,
}

fn argument(args: &[String], name: &str) -> Option<String> {
    args.iter().enumerate().find_map(|(index, arg)| {
        if arg == name {
            args.get(index + 1).cloned()
        } else {
            arg.strip_prefix(&format!("{name}=")).map(str::to_owned)
        }
    })
}

fn start_ticks(stat: &str) -> Option<u64> {
    // comm can itself contain spaces and ')' characters.
    stat.rsplit_once(") ")?
        .1
        .split_whitespace()
        .nth(19)?
        .parse()
        .ok()
}

fn inspect(root: &Path, pid: u32, uid: u32) -> Option<NativeProcess> {
    let dir = root.join(pid.to_string());
    if fs::metadata(&dir).ok()?.uid() != uid {
        return None;
    }
    let executable = fs::read_link(dir.join("exe")).ok()?;
    if executable.file_name()?.to_str()? != "tachyon-serve" {
        return None;
    }
    let before = start_ticks(&fs::read_to_string(dir.join("stat")).ok()?)?;
    let mut bytes = Vec::new();
    fs::File::open(dir.join("cmdline"))
        .ok()?
        .take(65537)
        .read_to_end(&mut bytes)
        .ok()?;
    if bytes.len() > 65536 {
        return None;
    }
    let args: Vec<String> = bytes
        .split(|byte| *byte == 0)
        .filter(|part| !part.is_empty())
        .map(|part| String::from_utf8(part.to_vec()))
        .collect::<Result<_, _>>()
        .ok()?;
    if start_ticks(&fs::read_to_string(dir.join("stat")).ok()?)? != before {
        return None;
    }
    Some(NativeProcess {
        pid,
        start_ticks: before,
        executable,
        model_id: argument(&args, "--model-id"),
        port: argument(&args, "--port").and_then(|value| value.parse().ok()),
    })
}

#[derive(Default)]
pub struct NativeSnapshot {
    pub processes: Vec<NativeProcess>,
    /// A time-limited/failed scan is not evidence that no workload is running.
    pub complete: bool,
}

pub fn discover() -> NativeSnapshot {
    let root = Path::new("/proc");
    let Ok(meta) = fs::metadata("/proc/self") else {
        return NativeSnapshot::default();
    };
    let Ok(entries) = fs::read_dir(root) else {
        return NativeSnapshot::default();
    };
    let deadline = Instant::now() + Duration::from_secs(2);
    let mut out = NativeSnapshot {
        processes: Vec::new(),
        complete: true,
    };
    for entry in entries {
        if Instant::now() >= deadline {
            out.complete = false;
            break;
        }
        let Ok(entry) = entry else {
            out.complete = false;
            continue;
        };
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|name| name.parse::<u32>().ok())
        else {
            continue;
        };
        if let Some(process) = inspect(root, pid, meta.uid()) {
            out.processes.push(process);
        }
    }
    out
}

/// The engine fingerprint is the content-addressed directory, not its marketing
/// version. Model ID and endpoint port further disambiguate shared engine builds.
pub fn matching_pid(
    processes: &[NativeProcess],
    fingerprint: &str,
    model_id: Option<&str>,
    port: Option<u16>,
) -> Option<u32> {
    if fingerprint.len() != 64 || !fingerprint.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return None;
    }
    let matches = processes
        .iter()
        .filter(|process| {
            process
                .executable
                .parent()
                .and_then(Path::file_name)
                .and_then(|name| name.to_str())
                == fingerprint.get(..8)
                && model_id.is_none_or(|id| process.model_id.as_deref() == Some(id))
                && port.is_none_or(|port| process.port == Some(port))
        })
        .map(|process| process.pid)
        .collect::<Vec<_>>();
    if matches.len() == 1 {
        Some(matches[0])
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn process_start_time_handles_parentheses_in_comm() {
        assert_eq!(
            start_ticks("17 (a ) b) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 20"),
            Some(4242)
        );
    }
    #[test]
    fn ambiguous_engine_is_not_assigned_to_a_model() {
        let process = NativeProcess {
            pid: 1,
            start_ticks: 4,
            executable: "/engines/12345678/tachyon-serve".into(),
            model_id: Some("one".into()),
            port: Some(18081),
        };
        let other = NativeProcess {
            pid: 2,
            model_id: Some("two".into()),
            port: Some(18082),
            ..process.clone()
        };
        let processes = vec![process, other];
        let fingerprint = format!("12345678{}", "a".repeat(56));
        assert_eq!(matching_pid(&processes, &fingerprint, None, None), None);
        assert_eq!(
            matching_pid(&processes, &fingerprint, Some("one"), Some(18081)),
            Some(1)
        );
        assert_eq!(
            matching_pid(&processes, &fingerprint, Some("one"), Some(18082)),
            None
        );
        assert_eq!(
            matching_pid(&processes, "12345678", Some("one"), Some(18081)),
            None
        );
    }

    #[test]
    fn same_model_name_on_another_port_is_a_separate_workload() {
        let process = NativeProcess {
            pid: 1,
            start_ticks: 4,
            executable: "/engines/12345678/tachyon-serve".into(),
            model_id: Some("one".into()),
            port: Some(18081),
        };
        let other = NativeProcess {
            pid: 2,
            port: Some(18082),
            ..process.clone()
        };
        let fingerprint = format!("12345678{}", "a".repeat(56));
        assert_eq!(
            matching_pid(
                &[process.clone(), other.clone()],
                &fingerprint,
                Some("one"),
                None
            ),
            None
        );
        assert_eq!(
            matching_pid(&[process, other], &fingerprint, Some("one"), Some(18082)),
            Some(2)
        );
    }

    #[test]
    fn supports_both_argument_forms_without_exposing_command_lines() {
        let args = vec![
            "tachyon-serve".into(),
            "--model-id=test".into(),
            "--port".into(),
            "18081".into(),
        ];
        assert_eq!(argument(&args, "--model-id").as_deref(), Some("test"));
        assert_eq!(argument(&args, "--port").as_deref(), Some("18081"));
        assert_eq!(argument(&args, "--missing"), None);
    }
}
