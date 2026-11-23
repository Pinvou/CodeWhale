//! First creation must also start with Docker's independent restart disabled.
//! This adapter is for trusted store scripts, not a sandbox for arbitrary code.
use crate::json::Value;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::Command;

fn rewrite_create(args: &[String], expected: &str) -> Result<Vec<String>, String> {
    let mut out = vec![args[0].clone(), "--restart=no".into()];
    let mut name = None;
    let mut image = false;
    let mut i = 1;
    const VALUES: &[&str] = &[
        "--name",
        "--restart",
        "--runtime",
        "--gpus",
        "--network",
        "--net",
        "--ipc",
        "--shm-size",
        "--ulimit",
        "-p",
        "--publish",
        "-v",
        "--volume",
        "-e",
        "--env",
        "--entrypoint",
        "--workdir",
        "--user",
        "--cpus",
        "--memory",
        "--memory-swap",
        "--mount",
        "--label",
        "-l",
        "-w",
        "-u",
        "--hostname",
        "-h",
        "--device",
        "--cap-add",
        "--security-opt",
        "--env-file",
        "--group-add",
        "--add-host",
        "--pid",
    ];
    const FLAGS: &[&str] = &[
        "--rm",
        "-d",
        "--detach",
        "-it",
        "-i",
        "-t",
        "--init",
        "--privileged",
        "--read-only",
    ];
    while i < args.len() {
        let arg = &args[i];
        if !arg.starts_with('-') {
            out.extend_from_slice(&args[i..]);
            image = true;
            break;
        }
        let (key, inline) = arg
            .split_once('=')
            .map_or((arg.as_str(), None), |(k, v)| (k, Some(v)));
        if VALUES.contains(&key) {
            let value = match inline {
                Some(v) => v,
                None => {
                    i += 1;
                    args.get(i).ok_or("Docker option missing value")?
                }
            };
            if key == "--name" && name.replace(value.to_owned()).is_some() {
                return Err("duplicate container name".into());
            }
            if key != "--restart" {
                out.push(key.into());
                out.push(value.to_owned());
            }
        } else if FLAGS.contains(&key) {
            out.push(arg.clone());
        } else {
            return Err(format!("unsupported first-install Docker option {key}"));
        }
        i += 1;
    }
    if name.as_deref() != Some(expected) || !image {
        return Err("first install must use its declared container and image".into());
    }
    Ok(out)
}

pub fn prepare(
    command: &mut Command,
    app: &crate::app::App,
    task: &str,
    name: &str,
) -> Result<(), String> {
    if name.is_empty()
        || name.len() > 128
        || !name.as_bytes()[0].is_ascii_alphanumeric()
        || !name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
    {
        return Err("invalid declared container name".into());
    }
    let directory = app.config.state_dir.join("docker-guard").join(task);
    std::fs::create_dir_all(&directory).map_err(|e| e.to_string())?;
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())?;
    let proxy = directory.join("docker");
    std::fs::write(
        &proxy,
        "#!/bin/sh\nexec \"$CMSD_GUARD_BINARY\" --docker-recovery-guard \"$@\"\n",
    )
    .map_err(|e| e.to_string())?;
    std::fs::set_permissions(&proxy, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| e.to_string())?;
    let real = if Path::new(&app.config.docker_cli).is_absolute() {
        app.config.docker_cli.clone()
    } else {
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default())
            .map(|p| p.join(&app.config.docker_cli))
            .find(|p| p.is_file())
            .ok_or("Docker CLI not found")?
            .to_string_lossy()
            .into_owned()
    };
    let pid = std::process::id();
    let search = std::env::join_paths(std::iter::once(directory.clone()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .map_err(|e| e.to_string())?;
    command
        .env("PATH", search)
        .env(
            "CMSD_GUARD_BINARY",
            std::env::current_exe().map_err(|e| e.to_string())?,
        )
        .env("CMSD_GUARD_REAL_DOCKER", real)
        .env("CMSD_GUARD_CONTAINER", name)
        .env("CMSD_GUARD_PID", pid.to_string())
        .env(
            "CMSD_GUARD_TICKS",
            crate::peer::start_ticks(pid).ok_or("daemon process identity unavailable")?,
        )
        .env("CMSD_GUARD_BOOT", crate::peer::boot_id())
        .env("CMSD_GUARD_DIR", directory);
    Ok(())
}

fn compose(args: &[String], real: &str, expected: &str) -> Result<Vec<String>, String> {
    let index = args
        .iter()
        .position(|v| {
            [
                "up", "create", "start", "restart", "down", "stop", "config", "ps", "logs", "pull",
            ]
            .contains(&v.as_str())
        })
        .ok_or("unsupported first-install compose action")?;
    let action = &args[index];
    if ["config", "ps", "logs", "pull"].contains(&action.as_str()) {
        return Ok(args.to_vec());
    }
    let mut prefix = args[..index].to_vec();
    if !prefix
        .iter()
        .any(|v| v == "-f" || v == "--file" || v.starts_with("--file="))
    {
        let file = [
            "compose.yaml",
            "compose.yml",
            "docker-compose.yaml",
            "docker-compose.yml",
        ]
        .iter()
        .find(|p| Path::new(p).is_file())
        .ok_or("compose file must be explicit")?;
        prefix.extend(["-f".into(), (*file).into()]);
    }
    let mut query = prefix.clone();
    query.extend(["config".into(), "--format".into(), "json".into()]);
    let out = crate::process::output(
        Command::new(real).args(&query),
        std::time::Duration::from_secs(10),
    )
    .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err("cannot verify compose container identity".into());
    }
    let config = Value::parse(std::str::from_utf8(&out.stdout).map_err(|e| e.to_string())?)
        .map_err(|e| e.to_string())?;
    let services = config
        .get("services")
        .and_then(Value::as_obj)
        .ok_or("compose services missing")?;
    if services.len() != 1 {
        return Err("multi-service compose requires a managed deployment recipe".into());
    }
    let (service, definition) = services.iter().next().unwrap();
    if definition.get("container_name").and_then(Value::as_str) != Some(expected) {
        return Err("compose container differs from declared card".into());
    }
    let mut policy = Value::obj();
    policy.set("restart", "no".into());
    let mut entries = Value::obj();
    entries.set(service, policy);
    let mut overlay = Value::obj();
    overlay.set("services", entries);
    let path =
        std::path::PathBuf::from(std::env::var("CMSD_GUARD_DIR").map_err(|e| e.to_string())?)
            .join("compose-recovery.json");
    std::fs::write(&path, overlay.to_string()).map_err(|e| e.to_string())?;
    prefix.extend(["-f".into(), path.to_string_lossy().into_owned()]);
    prefix.extend_from_slice(&args[index..]);
    Ok(prefix)
}

pub fn run(args: &[String]) -> Result<(), String> {
    ensure_owner_alive()?;
    execute(args)
}

fn ensure_owner_alive() -> Result<(), String> {
    let pid: u32 = std::env::var("CMSD_GUARD_PID")
        .map_err(|e| e.to_string())?
        .parse()
        .map_err(|_| "invalid daemon pid")?;
    if crate::peer::start_ticks(pid) != std::env::var("CMSD_GUARD_TICKS").ok()
        || std::env::var("CMSD_GUARD_BOOT").ok().as_deref() != Some(crate::peer::boot_id().as_str())
    {
        return Err("owning daemon exited; delayed script execution is fenced".into());
    }
    Ok(())
}

fn execute(args: &[String]) -> Result<(), String> {
    let real = std::env::var("CMSD_GUARD_REAL_DOCKER").map_err(|e| e.to_string())?;
    let expected = std::env::var("CMSD_GUARD_CONTAINER").map_err(|e| e.to_string())?;
    let action = args
        .first()
        .map(String::as_str)
        .ok_or("missing Docker action")?;
    let rewritten = match action {
        "run" | "create" => rewrite_create(args, &expected)?,
        "compose" => compose(args, &real, &expected)?,
        "start" | "restart" => {
            if args.len() != 2 || args[1] != expected { return Err("script attempted a different container".into()); }
            crate::docker::Docker::new(&real, "nvidia-smi").set_restart_policy(&expected, "no")?;
            args.to_vec()
        },
        "stop" | "kill" | "rm" => {
            if !args.iter().skip(1).any(|v| v == &expected) { return Err("script omitted declared container".into()); }
            // Known card scripts use only flag switches here, no other targets.
            if args.iter().skip(1).any(|v| !v.starts_with('-') && v != &expected) { return Err("unsupported container target/options".into()); }
            args.to_vec()
        },
        "inspect" | "ps" | "info" | "version" | "logs" | "images" | "pull" => args.to_vec(),
        _ => return Err(format!("unsupported Docker script action {action}; use the model store to prepare this runtime")),
    };
    ensure_owner_alive()?;
    Err(Command::new(real).args(rewritten).exec().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn create_never_inherits_independent_restart_or_another_container() {
        for args in [
            vec![
                "run",
                "--name",
                "owned",
                "--restart",
                "unless-stopped",
                "image",
            ],
            vec!["create", "--restart=always", "--name=owned", "image"],
        ] {
            let values: Vec<_> = args.into_iter().map(str::to_owned).collect();
            let guarded = rewrite_create(&values, "owned").unwrap();
            assert_eq!(
                guarded
                    .iter()
                    .filter(|v| v.starts_with("--restart"))
                    .collect::<Vec<_>>(),
                [&"--restart=no".to_owned()]
            );
            assert!(!guarded
                .iter()
                .any(|v| v == "always" || v == "unless-stopped"));
            assert!(rewrite_create(&values, "other").is_err());
        }
    }
}
