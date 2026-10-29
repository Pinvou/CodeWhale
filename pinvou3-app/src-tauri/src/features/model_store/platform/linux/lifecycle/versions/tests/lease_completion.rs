//! A runner exit does not finish staging while a descendant still holds its OFD.
use super::*;
use std::time::{Duration, Instant};

struct Writer(i32);

impl Drop for Writer {
    fn drop(&mut self) {
        if let Ok(group) = nix::unistd::getpgid(Some(nix::unistd::Pid::from_raw(self.0))) {
            assert_ne!(group, nix::unistd::getpgrp());
            let _ = nix::sys::signal::killpg(group, nix::sys::signal::Signal::SIGKILL);
        }
    }
}

#[test]
fn staging_keeps_plan_for_background_success_or_error_and_does_not_retry_a_live_writer() {
    for code in [0, 1] {
        let (_home, manager, current, mut card, mut package, _) = fixture();
        let body = format!(
            r#"#!/bin/sh
mkdir -p "$LMM_MODELS_DIR/new"
printf new-weights > "$LMM_MODELS_DIR/new/new.tachyon"
printf x >> "$LMM_MODELS_DIR/attempts"
sh -c 'trap "" HUP; i=0; while [ "$i" -lt 600 ]; do printf x >> "$LMM_MODELS_DIR/partial"; i=$((i+1)); sleep 0.05; done' >/dev/null 2>&1 &
printf '%s\n' "$!" > "$LMM_MODELS_DIR/background.pid"
# Establish child readiness before parent exit can deliver PTY SIGHUP.
# Further writes after the runner exits remain asserted by the Rust test.
ready_waits=0
while [ ! -s "$LMM_MODELS_DIR/partial" ]; do
  ready_waits=$((ready_waits+1))
  if [ "$ready_waits" -ge 500 ]; then
    printf '%s\n' 'fixture background writer readiness timeout' >&2
    exit 97
  fi
  sleep 0.01
done
exit {code}
"#
        );
        package.scripts.insert("download_model".into(), blob(&body));
        card.script_sha256
            .as_mut()
            .unwrap()
            .insert("download_model".into(), script_runner::sha256_hex(&body));
        let revision = card_update::revision(&card_update::remote_manifest(&card));
        let error = manager
            .stage_verified(
                &current,
                &card,
                &revision,
                package,
                &mut |_| {},
                Default::default(),
            )
            .unwrap_err();
        let version = version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap();
        let root = Path::new(version.models_root.as_deref().unwrap());
        let writer = Writer(
            fs::read_to_string(root.join("background.pid"))
                .unwrap()
                .trim()
                .parse()
                .unwrap(),
        );
        assert!(error.to_string().contains("下载脚本运行"), "{error}");
        assert_eq!(version.state, "downloading");
        assert!(version.planned_bytes >= card.summary.size_bytes);
        assert_eq!(fs::read(root.join("attempts")).unwrap(), b"x");
        assert_eq!(
            serde_json::to_value(db::get_store_model(&current.card_id).unwrap().unwrap()).unwrap(),
            serde_json::to_value(&current).unwrap()
        );
        let before = fs::metadata(root.join("partial")).unwrap().len();
        let deadline = Instant::now() + Duration::from_secs(5);
        while fs::metadata(root.join("partial")).unwrap().len() <= before {
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
        version_store::recover_interrupted(&current.card_id).unwrap();
        assert_eq!(
            version_store::get(&current.card_id, &revision)
                .unwrap()
                .unwrap()
                .state,
            "downloading"
        );
        drop(writer);
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if version_store::try_download_lease(&current.card_id)
                .unwrap()
                .is_some()
            {
                break;
            }
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
        version_store::recover_interrupted(&current.card_id).unwrap();
        let recovered = version_store::get(&current.card_id, &revision)
            .unwrap()
            .unwrap();
        assert_eq!(recovered.state, "failed");
        assert_eq!(recovered.planned_bytes, 0);
        assert_eq!(
            fs::read(root.join("new/new.tachyon")).unwrap(),
            b"new-weights"
        );
    }
}
