//! Mount boundaries belong to orphan-cleanup safety, not model ownership.
//! Device numbers alone miss same-filesystem bind mounts (including files).
//! Read the calling thread's namespace; unavailable or malformed evidence must
//! never authorize recursive deletion. The configured root itself may be mounted.

use std::os::unix::ffi::OsStringExt;
use std::path::{Component, Path, PathBuf};

pub(super) struct Mounts(Vec<PathBuf>);

impl Mounts {
    pub(super) fn read() -> Result<Self, String> {
        let bytes = std::fs::read("/proc/thread-self/mountinfo")
            .map_err(|_| "orphan_mount_state_unavailable".to_string())?;
        Self::parse(&bytes)
    }

    pub(super) fn parse(bytes: &[u8]) -> Result<Self, String> {
        let invalid = || "orphan_mount_state_unavailable".to_string();
        let mut points = Vec::new();
        // Format: docs.kernel.org/filesystems/proc.html#proc-pid-mountinfo
        for line in bytes
            .split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
        {
            let fields: Vec<_> = line.split(|byte| *byte == b' ').collect();
            let separator = fields
                .iter()
                .position(|field| *field == b"-")
                .ok_or_else(invalid)?;
            if separator < 6
                || fields.len() != separator + 4
                || fields.iter().any(|field| field.is_empty())
                || !fields[0].iter().all(u8::is_ascii_digit)
                || !fields[1].iter().all(u8::is_ascii_digit)
            {
                return Err(invalid());
            }
            let device: Vec<_> = fields[2].split(|byte| *byte == b':').collect();
            if device.len() != 2
                || device
                    .iter()
                    .any(|part| part.is_empty() || !part.iter().all(u8::is_ascii_digit))
            {
                return Err(invalid());
            }
            let mut decoded = Vec::new();
            let mut rest = fields[4];
            while let Some((&byte, tail)) = rest.split_first() {
                if byte == b'\\' {
                    let Some(escape) = rest.get(1..4) else {
                        return Err(invalid());
                    };
                    decoded.push(match escape {
                        b"040" => b' ',
                        b"011" => b'\t',
                        b"012" => b'\n',
                        b"134" => b'\\',
                        _ => return Err(invalid()),
                    });
                    rest = &rest[4..];
                } else {
                    if byte == 0 {
                        return Err(invalid());
                    }
                    decoded.push(byte);
                    rest = tail;
                }
            }
            let point = PathBuf::from(std::ffi::OsString::from_vec(decoded));
            if !point.is_absolute()
                || point
                    .components()
                    .any(|part| matches!(part, Component::ParentDir | Component::CurDir))
            {
                return Err(invalid());
            }
            points.push(point);
        }
        if points.is_empty() {
            return Err(invalid());
        }
        Ok(Self(points))
    }

    /// `target` must be canonical. A mount at or inside it excludes the whole
    /// candidate, while an ancestor mount (e.g. a separate models disk) is fine.
    pub(super) fn intersects(&self, target: &Path) -> bool {
        self.0.iter().any(|point| point.starts_with(target))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn same_device_bind_mounts_and_nested_file_mounts_exclude_the_candidate() {
        let mounts = Mounts::parse(b"1 1 8:1 / / rw - ext4 /dev/test rw\n2 1 8:1 /source /models/Bound rw shared:9 future:tag - ext4 /dev/test rw\n3 1 8:1 /file /models/Nested/weights.bin rw - ext4 /dev/test rw\n").unwrap();
        for path in ["/models/Bound", "/models/Nested"] {
            assert!(mounts.intersects(Path::new(path)));
        }
        for path in ["/models/Normal", "/models/Bound-extra", "/elsewhere"] {
            assert!(!mounts.intersects(Path::new(path)));
        }
    }

    #[test]
    fn mount_paths_preserve_kernel_escaping_and_non_utf8_bytes() {
        let mounts = Mounts::parse(
            b"1 1 8:1 / /models/a\\040b\\011c\\012d\\134e\xff rw - ext4 /dev/test rw\n",
        )
        .unwrap();
        let target = PathBuf::from(std::ffi::OsString::from_vec(
            b"/models/a b\tc\nd\\e\xff".to_vec(),
        ));
        assert!(mounts.intersects(&target));
    }

    #[test]
    fn unreadable_or_malformed_mount_evidence_cannot_mean_no_mounts() {
        for bytes in [
            b"".as_slice(),
            b"truncated",
            b"1 1 8:1 / relative rw - ext4 none rw\n",
            b"1 1 8:1 / /bad\\123 rw - ext4 none rw\n",
            b"1 1 8:1 / /bad\0 rw - ext4 none rw\n",
            b"1 1 8:1 / /bad rw - ext4 none\n",
        ] {
            assert!(
                matches!(Mounts::parse(bytes), Err(error) if error == "orphan_mount_state_unavailable")
            );
        }
    }

    #[test]
    fn configured_root_may_be_a_mount_without_excluding_ordinary_children() {
        let mounts = Mounts::parse(b"1 1 8:1 / /models rw - ext4 /dev/test rw\n").unwrap();
        assert!(!mounts.intersects(Path::new("/models/leftover")));
    }
}
