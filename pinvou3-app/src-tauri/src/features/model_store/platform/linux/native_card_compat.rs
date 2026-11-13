//! Narrow compatibility repair for an audited upstream process-receipt race.
//! Verify the published package first, then apply only this exact known fix.
use super::{db::ScriptBlob, script_runner};
use std::collections::BTreeMap;

pub const BROKEN: &str = "5cb4539ce50c34948ac22bc720d2fce2f08e840e615a963860453f9e1c321bc7";
pub const LAUNCH_FIXED: &str = "6b9968bf37c944d6a0ef6207000a88ebe36deb68bac400db206aafb39a55af24";
pub const FIXED: &str = "868605374b0f609f676bf826f72f314a4a1cdb9939464778842c4797368c7d01";
const PATH: &str = "native_engine_process.py";
const OLD: &str = r#"        # Popen waits for exec. Keep our child unreaped while recording its identity;
        # this PID cannot be reused by a different process in this interval.
        process = identity(child.pid, self.target)
        if process is None:
            child.poll()
            raise UnsafeState("引擎在建立运行记录前已退出；请查看诊断日志")
"#;
const NEW: &str = r#"        # Popen's exec handshake can finish before /proc exposes the new argv.
        # Only wait for this direct, unreaped child: discovery never authorizes
        # adoption, and this PID cannot be reused until poll observes its exit.
        deadline = time.monotonic() + 2
        while True:
            process = identity(child.pid, self.target)
            if process is not None:
                break
            if child.poll() is not None:
                raise UnsafeState("引擎在建立运行记录前已退出；请查看诊断日志")
            if time.monotonic() >= deadline:
                raise UnsafeState(f"引擎进程 {child.pid} 的身份尚未确认；已保留进程和日志，请联系管理员核对")
            time.sleep(0.01)
"#;
const IDENTITY_OLD: &str = r#"        if len(raw) > 65536 or not raw.endswith(b"\0"):
            raise UnsafeState("无法完整读取引擎身份")
"#;
const IDENTITY_NEW: &str = r#"        if len(raw) > 65536 or not raw.endswith(b"\0"):
            # An unrelated process can legitimately have a huge argv (rustc,
            # browsers, etc.). Exclude only identities proven different; a
            # truncated candidate engine must still block process operations.
            executable = root / "exe"
            if os.readlink(executable) != target["executable"]:
                return None
            metadata = executable.stat()
            if [metadata.st_dev, metadata.st_ino] != target["inode"]:
                return None
            prefix = raw.split(b"\0")[:-1]
            if len(prefix) >= 2:
                weights = Path(os.fsdecode(prefix[1]))
                if not weights.is_absolute():
                    weights = root / "cwd" / weights
                if str(weights.resolve()) != target["weights"]:
                    return None
            raise UnsafeState("无法完整读取引擎身份")
"#;

pub fn apply(assets: &mut BTreeMap<String, ScriptBlob>) -> Result<bool, String> {
    let Some(blob) = assets.get_mut(PATH) else {
        return Ok(false);
    };
    if ![BROKEN, LAUNCH_FIXED]
        .iter()
        .any(|hash| blob.sha256.eq_ignore_ascii_case(hash))
    {
        return Ok(false);
    }
    let original = blob.sha256.eq_ignore_ascii_case(BROKEN);
    if !script_runner::sha256_hex(&blob.content).eq_ignore_ascii_case(&blob.sha256)
        || (original && blob.content.matches(OLD).count() != 1)
        || (!original && blob.content.matches(NEW).count() != 1)
        || blob.content.matches(IDENTITY_OLD).count() != 1
    {
        return Err("控制器兼容修复的来源指纹不匹配，已保留原版本。".into());
    }
    let content = if original {
        blob.content.replacen(OLD, NEW, 1)
    } else {
        blob.content.clone()
    }
    .replacen(IDENTITY_OLD, IDENTITY_NEW, 1);
    if script_runner::sha256_hex(&content) != FIXED {
        return Err("控制器兼容修复的结果指纹不匹配，已保留原版本。".into());
    }
    blob.content = content;
    blob.sha256 = FIXED.into();
    Ok(true)
}

/// Compare against the publisher's revision while keeping the actual patched
/// file digest in storage. This prevents offering to restore the known bug.
pub fn upstream_digest(path: &str, digest: &str) -> String {
    if path == PATH
        && [LAUNCH_FIXED, FIXED]
            .iter()
            .any(|hash| digest.eq_ignore_ascii_case(hash))
    {
        BROKEN.into()
    } else {
        digest.to_ascii_lowercase()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unknown_controllers_are_untouched_and_bad_source_claims_are_rejected() {
        let mut assets = BTreeMap::from([(
            PATH.into(),
            ScriptBlob {
                content: "unknown".into(),
                sha256: "a".repeat(64),
            },
        )]);
        assert!(!apply(&mut assets).unwrap());
        assert_eq!(assets[PATH].content, "unknown");
        assets.get_mut(PATH).unwrap().sha256 = BROKEN.into();
        assert!(apply(&mut assets).is_err());
    }
    #[test]
    fn only_the_audited_controller_digest_is_compared_as_its_upstream_revision() {
        assert_eq!(upstream_digest(PATH, FIXED), BROKEN);
        assert_eq!(upstream_digest(PATH, LAUNCH_FIXED), BROKEN);
        assert_eq!(upstream_digest("other.py", FIXED), FIXED);
        assert_eq!(upstream_digest(PATH, &"c".repeat(64)), "c".repeat(64));
    }
}
