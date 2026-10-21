//! Fence only a maintained uninstall script; reuse the existing PTY executor.
//! A revoked execution ticket prevents a late child from starting after recovery.
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

#[derive(Clone)]
pub struct ExecutionFence {
    pub directory: PathBuf,
}

const WRAPPER: &str = r#"#!/bin/bash
set -euo pipefail
umask 077
exec flock -x "$LMM_EXEC_LOCK" /bin/bash -c '
  set -euo pipefail
  [ "$(cat "$LMM_EXEC_TICKET")" = "armed" ] || exit 77
  stat=$(cat /proc/$$/stat)
  rest=${stat##*)}
  fields=($rest)
  [ "${#fields[@]}" -ge 20 ] || exit 78
  boot=$(cat /proc/sys/kernel/random/boot_id)
  printf "{\"pid\":%s,\"pgid\":%s,\"starttime\":%s,\"boot_id\":\"%s\"}\n" \
    "$$" "${fields[2]}" "${fields[19]}" "$boot" > "$LMM_EXEC_IDENTITY.tmp"
  mv -- "$LMM_EXEC_IDENTITY.tmp" "$LMM_EXEC_IDENTITY"
  exec /bin/bash "$LMM_EXEC_SCRIPT"
'
"#;

pub(super) fn private_write(path: &Path, bytes: &[u8]) -> Result<(), String> {
    let parent = path.parent().ok_or("invalid_private_path")?;
    fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    if fs::symlink_metadata(parent)
        .map_err(|e| e.to_string())?
        .file_type()
        .is_symlink()
    {
        return Err("unsafe_private_directory".into());
    }
    fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).map_err(|e| e.to_string())?;
    let temporary = parent.join(format!(".write-{}", uuid::Uuid::new_v4().simple()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&temporary)
            .map_err(|e| e.to_string())?;
        file.write_all(bytes).map_err(|e| e.to_string())?;
        file.sync_all().map_err(|e| e.to_string())?;
        fs::rename(&temporary, path).map_err(|e| e.to_string())?;
        File::open(parent)
            .and_then(|f| f.sync_all())
            .map_err(|e| e.to_string())
    })();
    let _ = fs::remove_file(temporary);
    result
}

pub(super) fn open_lock(path: &Path) -> Result<File, String> {
    let parent = path.parent().ok_or("invalid_lock_path")?;
    fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).map_err(|e| e.to_string())?;
    if fs::symlink_metadata(path).is_ok_and(|m| m.file_type().is_symlink()) {
        return Err("unsafe_lock_path".into());
    }
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)
        .map_err(|e| e.to_string())?;
    file.set_permissions(fs::Permissions::from_mode(0o600))
        .map_err(|e| e.to_string())?;
    Ok(file)
}

impl ExecutionFence {
    pub fn prepare(directory: PathBuf) -> Result<Self, String> {
        let fence = Self { directory };
        private_write(&fence.directory.join("wrapper.sh"), WRAPPER.as_bytes())?;
        fs::set_permissions(fence.wrapper(), fs::Permissions::from_mode(0o700))
            .map_err(|e| e.to_string())?;
        private_write(&fence.directory.join("ticket"), b"armed")?;
        let _ = open_lock(&fence.directory.join("script.lock"))?;
        Ok(fence)
    }

    pub fn wrapper(&self) -> PathBuf {
        self.directory.join("wrapper.sh")
    }

    pub fn environment(&self, script: &Path) -> Vec<(&'static str, String)> {
        vec![
            (
                "LMM_EXEC_LOCK",
                self.directory
                    .join("script.lock")
                    .to_string_lossy()
                    .into_owned(),
            ),
            (
                "LMM_EXEC_TICKET",
                self.directory.join("ticket").to_string_lossy().into_owned(),
            ),
            (
                "LMM_EXEC_IDENTITY",
                self.directory
                    .join("process.json")
                    .to_string_lossy()
                    .into_owned(),
            ),
            ("LMM_EXEC_SCRIPT", script.to_string_lossy().into_owned()),
        ]
    }

    /// Hold the script lock through release; reject live writers and revoke
    /// any child that was spawned but has not acquired its execution lock yet.
    pub fn quiesce(&self) -> Result<File, String> {
        let lock = open_lock(&self.directory.join("script.lock"))?;
        lock.try_lock()
            .map_err(|_| "maintenance_script_active: 卸载脚本仍在执行，不能解除维护".to_string())?;
        private_write(&self.directory.join("ticket"), b"revoked")?;
        let path = self.directory.join("process.json");
        let bytes = match fs::read(path) {
            Ok(bytes) => bytes,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(lock),
            Err(e) => return Err(format!("maintenance_process_unknown: {e}")),
        };
        let identity: Identity = serde_json::from_slice(&bytes)
            .map_err(|_| "maintenance_process_unknown: 脚本进程记录损坏".to_string())?;
        let boot =
            fs::read_to_string("/proc/sys/kernel/random/boot_id").map_err(|e| e.to_string())?;
        if boot.trim() != identity.boot_id {
            return Ok(lock);
        }
        for entry in fs::read_dir("/proc").map_err(|e| e.to_string())? {
            let entry = entry.map_err(|e| e.to_string())?;
            if entry.file_name().to_string_lossy().parse::<u32>().is_err() {
                continue;
            }
            let stat = match fs::read_to_string(entry.path().join("stat")) {
                Ok(stat) => stat,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
                Err(e) => return Err(format!("maintenance_process_unknown: {e}")),
            };
            let Some(rest) = stat.rsplit_once(')').map(|(_, rest)| rest) else {
                return Err("maintenance_process_unknown: 无法核实进程组".into());
            };
            let fields: Vec<_> = rest.split_whitespace().collect();
            if fields.get(2).and_then(|v| v.parse::<i32>().ok()) == Some(identity.pgid)
                && fields
                    .get(19)
                    .and_then(|v| v.parse::<u64>().ok())
                    .is_some_and(|v| v >= identity.starttime)
                && fields.first().is_none_or(|v| *v != "Z" && *v != "X")
            {
                return Err(
                    "maintenance_script_active: 卸载脚本的进程组仍有活动进程，保留维护".into(),
                );
            }
        }
        Ok(lock)
    }
}

#[derive(serde::Deserialize)]
struct Identity {
    pgid: i32,
    starttime: u64,
    boot_id: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::process::CommandExt;
    use std::process::{Child, Command, Stdio};
    use std::time::{Duration, Instant};

    fn child(fence: &ExecutionFence, script: &Path) -> Child {
        Command::new("/bin/bash")
            .arg(fence.wrapper())
            .envs(fence.environment(script))
            .env_remove("BASH_ENV")
            .env_remove("ENV")
            .process_group(0)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap()
    }

    #[test]
    fn late_child_cannot_execute_a_revoked_ticket() {
        let tmp = tempfile::tempdir().unwrap();
        let script = tmp.path().join("uninstall.sh");
        let marker = tmp.path().join("deleted");
        fs::write(&script, format!("touch '{}'", marker.display())).unwrap();
        let fence = ExecutionFence::prepare(tmp.path().join("fence")).unwrap();
        let guard = fence.quiesce().unwrap();
        let mut process = child(&fence, &script);
        drop(guard);
        assert_eq!(process.wait().unwrap().code(), Some(77));
        assert!(!marker.exists());
    }

    #[test]
    fn running_script_prevents_release_and_writes_identity_before_work() {
        let tmp = tempfile::tempdir().unwrap();
        let script = tmp.path().join("uninstall.sh");
        let ready = tmp.path().join("ready");
        fs::write(&script, format!("touch '{}'; sleep 60", ready.display())).unwrap();
        let fence = ExecutionFence::prepare(tmp.path().join("fence")).unwrap();
        let mut process = child(&fence, &script);
        let deadline = Instant::now() + Duration::from_secs(3);
        while !ready.exists() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(ready.exists());
        assert!(fence.directory.join("process.json").is_file());
        assert!(fence.quiesce().is_err());
        let _ = nix::sys::signal::killpg(
            nix::unistd::Pid::from_raw(process.id() as i32),
            nix::sys::signal::Signal::SIGKILL,
        );
        process.wait().unwrap();
        let deadline = Instant::now() + Duration::from_secs(3);
        loop {
            if fence.quiesce().is_ok() {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "terminated script group must become quiescent"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn detached_child_without_inherited_lock_still_prevents_release() {
        let tmp = tempfile::tempdir().unwrap();
        let script = tmp.path().join("uninstall.sh");
        fs::write(&script, "python3 -c 'import subprocess; subprocess.Popen([\"sleep\",\"60\"], close_fds=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)'\n").unwrap();
        let fence = ExecutionFence::prepare(tmp.path().join("fence")).unwrap();
        let mut process = child(&fence, &script);
        let pgid = process.id() as i32;
        assert!(process.wait().unwrap().success());
        assert!(fence.quiesce().unwrap_err().contains("进程组"));
        let _ = nix::sys::signal::killpg(
            nix::unistd::Pid::from_raw(pgid),
            nix::sys::signal::Signal::SIGKILL,
        );
        let deadline = Instant::now() + Duration::from_secs(3);
        while fence.quiesce().is_err() {
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn corrupt_process_evidence_is_not_treated_as_quiescent() {
        let tmp = tempfile::tempdir().unwrap();
        let fence = ExecutionFence::prepare(tmp.path().join("fence")).unwrap();
        private_write(&fence.directory.join("process.json"), b"{").unwrap();
        assert!(fence.quiesce().is_err());
    }
}
