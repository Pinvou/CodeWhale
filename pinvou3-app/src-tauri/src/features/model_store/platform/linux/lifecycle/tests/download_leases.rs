//! Real runner children must retain quota protection after their app is killed.
use super::*;
use crate::features::model_store::platform::linux::{service, version_store};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::time::{Duration, Instant};

const MODE: &str = "PINVOU_DOWNLOAD_LEASE_TEST_MODE";
const ROOT: &str = "PINVOU_DOWNLOAD_LEASE_TEST_ROOT";
const OWNER: &str = "lease-owner";
const RESERVED: u64 = 2 * 1024 * 1024 * 1024;

const GROWING_DOWNLOAD: &str = r#"#!/bin/sh
trap '' HUP
printf '%s\n' "$$" > "$LMM_MODELS_DIR/writer.pid"
i=0
while [ "$i" -lt 600 ]; do
    printf x >> "$LMM_MODELS_DIR/partial"
    i=$((i+1))
    sleep 0.05
done
"#;

fn wait_for(mut condition: impl FnMut() -> bool, what: &str) {
    let deadline = Instant::now() + Duration::from_secs(8);
    while !condition() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn worker(root: &Path, mode: &str) {
    if mode.starts_with("owner-") {
        if mode.ends_with("closed-stdio") {
            // SAFETY: this isolated helper process never returns to normal test
            // output; only its runner and file markers are used before SIGKILL.
            unsafe {
                for fd in 0..=2 {
                    libc::close(fd);
                }
            }
        }
        let locks = config::data_dir().join("operation-locks");
        fs::create_dir_all(&locks).unwrap();
        let operation = crate::platform::filesystem::open_private_lock_file(
            &locks.join(format!("{OWNER}.lock")),
        )
        .unwrap();
        operation.try_lock().unwrap();
        let weights = root.join("owner-weights");
        fs::create_dir_all(&weights).unwrap();
        if mode.starts_with("owner-isolated") {
            version_store::put(&version_store::Version {
                measured_bytes: None,
                card_id: OWNER.into(),
                version_id: "e".repeat(64),
                state: "downloading".into(),
                record: None,
                models_root: Some(weights.to_string_lossy().into_owned()),
                runtime_card_id: "lease-owner-staged".into(),
                resource_dir: Some(weights.to_string_lossy().into_owned()),
                managed: true,
                created_at: "2026-10-28T00:00:00Z".into(),
                error: None,
                planned_bytes: RESERVED,
            })
            .unwrap();
            let mut lease = version_store::acquire_download_lease(OWNER, || false, || {}).unwrap();
            let entry = root.join("isolated-download.sh");
            fs::write(&entry, GROWING_DOWNLOAD).unwrap();
            fs::set_permissions(&entry, fs::Permissions::from_mode(0o700)).unwrap();
            let env = HashMap::from([
                ("LMM_CARD_ID".into(), OWNER.into()),
                (
                    "LMM_MODELS_DIR".into(),
                    weights.to_string_lossy().into_owned(),
                ),
            ]);
            script_runner::run_script(
                &entry,
                &env,
                RunScriptOptions {
                    download_lease: Some(lease.script_file().unwrap()),
                    ..Default::default()
                },
            )
            .unwrap();
        } else {
            let mut card = local_script_card(&root.join("owner-scripts"), None);
            card.summary.id = OWNER.into();
            card.summary.size_bytes = RESERVED;
            let entry = &card.scripts["download_model"];
            fs::write(entry, GROWING_DOWNLOAD).unwrap();
            card.script_sha256.as_mut().unwrap().insert(
                "download_model".into(),
                script_runner::sha256_hex(GROWING_DOWNLOAD),
            );
            let manager = manager_with_card(download_settings(&weights), card);
            manager
                .download_from_store(OWNER, &mut |_| {}, Default::default(), None)
                .unwrap();
        }
        panic!("owner must be killed while its actual download runner is active");
    }

    let weights = root.join("admission-weights");
    let mut card = local_script_card(&root.join("admission-scripts"), None);
    card.summary.id = "lease-admitted".into();
    let mut settings = download_settings(&weights);
    settings.storage_quota_gb = Some(1.0);
    let manager = manager_with_card(settings, card);
    let result =
        manager.download_from_store("lease-admitted", &mut |_| {}, Default::default(), None);
    if mode == "reject" {
        let error = result.unwrap_err();
        assert!(error.to_string().contains("超出存储配额"), "{error}");
        assert!(db::get_store_model("lease-admitted").unwrap().is_none());
        assert_eq!(ran_times(&weights.join("download-ran")), 0);
        assert!(version_store::retained_bytes().unwrap() >= RESERVED);
    } else {
        result.unwrap();
        assert_eq!(
            db::get_store_model("lease-admitted")
                .unwrap()
                .unwrap()
                .status,
            "downloaded"
        );
    }
}

fn command(root: &Path, mode: &str) -> Command {
    let module = module_path!().split_once("::").unwrap().1;
    let name = format!(
        "{module}::download_lease_survives_app_sigkill_and_protects_second_process_admission"
    );
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args(["--exact", &name, "--nocapture", "--test-threads=1"])
        .env(MODE, mode)
        .env(ROOT, root)
        .stdin(Stdio::null());
    command
}

struct Owner {
    app: Child,
    script: Option<i32>,
}

impl Owner {
    fn stop_script(&mut self) {
        if let Some(pid) = self.script.take() {
            // This PID came from our runner's own script. Check its independent
            // group before signalling so a failed fixture cannot kill the test.
            if nix::unistd::getpgid(Some(nix::unistd::Pid::from_raw(pid))).ok()
                == Some(nix::unistd::Pid::from_raw(pid))
            {
                let _ = nix::sys::signal::killpg(
                    nix::unistd::Pid::from_raw(pid),
                    nix::sys::signal::Signal::SIGKILL,
                );
            }
        }
    }
}

impl Drop for Owner {
    fn drop(&mut self) {
        if self.app.try_wait().ok().flatten().is_none() {
            let _ = self.app.kill();
            let _ = self.app.wait();
        }
        self.stop_script();
    }
}

#[test]
fn download_lease_survives_app_sigkill_and_protects_second_process_admission() {
    if let Ok(mode) = std::env::var(MODE) {
        worker(&PathBuf::from(std::env::var_os(ROOT).unwrap()), &mode);
        return;
    }
    for kind in ["isolated", "ordinary", "isolated-closed-stdio"] {
        let home = setup_temp_db();
        let root = home.path();
        let mut app = command(root, &format!("owner-{kind}"));
        app.stdout(Stdio::null());
        app.stderr(fs::File::create(root.join("owner-stderr.log")).unwrap());
        let mut owner = Owner {
            app: app.spawn().unwrap(),
            script: None,
        };
        let weights = root.join("owner-weights");
        let marker = weights.join("writer.pid");
        wait_for(|| marker.is_file(), "real download runner");
        owner.script = Some(fs::read_to_string(marker).unwrap().trim().parse().unwrap());
        wait_for(
            || fs::metadata(weights.join("partial")).is_ok_and(|m| m.len() >= 2),
            "download writes",
        );
        owner.app.kill().unwrap();
        assert_eq!(owner.app.wait().unwrap().signal(), Some(9));

        // The application/canonical lock is gone, but its actual runner child
        // keeps writing and retains the same download OFD across exec.
        let card = crate::platform::filesystem::open_private_lock_file(
            &config::data_dir()
                .join("operation-locks")
                .join(format!("{OWNER}.lock")),
        )
        .unwrap();
        card.try_lock().unwrap();
        drop(card);
        let before = fs::metadata(weights.join("partial")).unwrap().len();
        wait_for(
            || fs::metadata(weights.join("partial")).is_ok_and(|m| m.len() > before),
            "orphan writes after app SIGKILL",
        );
        assert!(version_store::try_download_lease(OWNER).unwrap().is_none());
        let same_card = service::start_download(
            &Arc::new(service::ModelStoreService::new()),
            OWNER.into(),
            false,
        )
        .unwrap_err();
        assert!(same_card.contains("下载脚本运行"), "{same_card}");
        let rejected = command(root, "reject").output().unwrap();
        assert!(
            rejected.status.success(),
            "{kind}: {}",
            String::from_utf8_lossy(&rejected.stderr)
        );
        if kind.starts_with("isolated") {
            let version = version_store::get(OWNER, &"e".repeat(64)).unwrap().unwrap();
            assert_eq!(version.state, "downloading");
            assert_eq!(version.planned_bytes, RESERVED);
        }

        owner.stop_script();
        wait_for(
            || version_store::try_download_lease(OWNER).unwrap().is_some(),
            "download descendants exit",
        );
        let admitted = command(root, "admit").output().unwrap();
        assert!(
            admitted.status.success(),
            "{kind}: {}",
            String::from_utf8_lossy(&admitted.stderr)
        );
        assert!(fs::metadata(weights.join("partial")).unwrap().len() > 0);
        if kind.starts_with("isolated") {
            let version = version_store::get(OWNER, &"e".repeat(64)).unwrap().unwrap();
            assert_eq!(version.state, "failed");
            assert_eq!(version.planned_bytes, 0);
        }
    }
}

fn background_download(exit_code: i32) -> String {
    format!(
        r#"#!/bin/sh
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
exit {exit_code}
"#
    )
}

struct BackgroundWriter(i32);

impl Drop for BackgroundWriter {
    fn drop(&mut self) {
        let pid = nix::unistd::Pid::from_raw(self.0);
        if let Ok(group) = nix::unistd::getpgid(Some(pid)) {
            assert_ne!(
                group,
                nix::unistd::getpgrp(),
                "fixture must not share the test process group"
            );
            let _ = nix::sys::signal::killpg(group, nix::sys::signal::Signal::SIGKILL);
        }
    }
}

#[test]
fn ordinary_download_retains_plan_after_background_success_or_error_and_never_retries_live_writer()
{
    for code in [0, 1] {
        let home = setup_temp_db();
        let root = home.path().join("models");
        let mut card = local_script_card(&home.path().join("scripts"), None);
        let body = background_download(code);
        fs::write(&card.scripts["download_model"], &body).unwrap();
        card.script_sha256
            .as_mut()
            .unwrap()
            .insert("download_model".into(), script_runner::sha256_hex(&body));
        let manager = manager_with_card(download_settings(&root), card);
        let error = manager
            .download_from_store("card-dl", &mut |_| {}, Default::default(), None)
            .unwrap_err();
        let writer = BackgroundWriter(
            fs::read_to_string(root.join("background.pid"))
                .unwrap()
                .trim()
                .parse()
                .unwrap(),
        );
        assert!(error.to_string().contains("下载脚本运行"), "{error}");
        assert_eq!(
            fs::read(root.join("attempts")).unwrap(),
            b"x",
            "never retry while an old writer owns the OFD"
        );
        assert!(db::get_store_model("card-dl").unwrap().is_none());
        assert!(
            version_store::retained_bytes().unwrap() >= 4096,
            "Drop must keep the full plan"
        );
        let before = fs::metadata(root.join("partial")).unwrap().len();
        wait_for(
            || fs::metadata(root.join("partial")).is_ok_and(|m| m.len() > before),
            "background download remains live",
        );
        drop(writer);
        wait_for(
            || {
                version_store::try_download_lease("card-dl")
                    .unwrap()
                    .is_some()
            },
            "background writer exits",
        );
        assert!(version_store::retained_bytes().unwrap() < 4096);
        assert!(root.join("partial").is_file());
    }
}
