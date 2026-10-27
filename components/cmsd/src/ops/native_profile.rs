//! Effective native launch parameters and process identity; never infer a
//! deployment from a shared model name or HTTP port.
use super::*;
use std::collections::BTreeMap;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};

#[derive(Clone, Debug)]
pub struct Profile {
    pub context: u32,
    pub concurrency: u32,
    pub device_slots: u32,
    pub host_slots: u32,
    pub extras: String,
    pub environment: BTreeMap<String, String>,
    pub revision: String,
}

/// Literal long options only, with one optional value per option. Both the
/// launch recipe and observed argv use this grammar, so duplicates and stray
/// positional arguments cannot disappear during identity comparison.
fn options(tokens: &[String]) -> Result<BTreeMap<String, Option<String>>, &'static str> {
    let mut result = BTreeMap::new();
    let mut i = 0;
    while i < tokens.len() {
        let (name, inline) = tokens[i]
            .split_once('=')
            .map_or((tokens[i].as_str(), None), |(name, value)| {
                (name, Some(value))
            });
        let key = name
            .strip_prefix("--")
            .ok_or("unconsumed positional argument")?;
        if key.is_empty()
            || !key
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        {
            return Err("invalid long option");
        }
        let value = match inline {
            Some(value) => Some(value.to_owned()),
            None => {
                let value = tokens
                    .get(i + 1)
                    .filter(|value| !value.starts_with("--"))
                    .cloned();
                if value.is_some() {
                    i += 1;
                }
                value
            }
        };
        if result.insert(name.to_owned(), value).is_some() {
            return Err("duplicate long option");
        }
        i += 1;
    }
    Ok(result)
}

pub fn resolve(app: &App, card: &Card) -> Result<Profile, String> {
    let deploy = card.deploy.as_ref().ok_or("native_profile_not_deployed")?;
    let flags = resolve_serve_flags(app, card);
    if flags.chars().any(|c| {
        c.is_control()
            || (c.is_whitespace() && c != ' ')
            || matches!(c, '$' | '`' | '\'' | '"' | '\\' | '*' | '?' | '[' | ']')
    }) {
        return Err(
            "native_runtime_profile_unsupported: 请使用字面量运行参数，不执行shell表达式".into(),
        );
    }
    let mut values = BTreeMap::<String, String>::new();
    let mut extras = Vec::new();
    let tokens: Vec<String> = flags.split_whitespace().map(str::to_owned).collect();
    options(&tokens).map_err(|reason| format!("native_runtime_profile_unsupported: {reason}"))?;
    let managed = [
        "--max-context",
        "--max-concurrency",
        "--device-state-slots",
        "--host-state-slots",
        "--host",
        "--port",
        "--model-id",
    ];
    let mut i = 0;
    while i < tokens.len() {
        let (name, inline) = tokens[i]
            .split_once('=')
            .map(|(k, v)| (k, Some(v)))
            .unwrap_or((tokens[i].as_str(), None));
        if managed.contains(&name) {
            let value = if let Some(value) = inline {
                value.to_owned()
            } else {
                i += 1;
                tokens
                    .get(i)
                    .filter(|v| !v.starts_with("--"))
                    .cloned()
                    .ok_or_else(|| format!("native_runtime_profile_unsupported: {name} 缺值"))?
            };
            if values.insert(name.into(), value).is_some() {
                return Err(format!(
                    "native_runtime_profile_unsupported: 重复参数 {name}"
                ));
            }
        } else {
            extras.push(tokens[i].clone());
        }
        i += 1;
    }
    let number = |name: &str, default: u32, zero: bool| -> Result<u32, String> {
        values
            .get(name)
            .map(|v| {
                v.parse::<u32>()
                    .ok()
                    .filter(|n| zero || *n > 0)
                    .ok_or_else(|| format!("native_runtime_profile_unsupported: {name} 无效"))
            })
            .unwrap_or(Ok(default))
    };
    let context = number("--max-context", MAX_CONTEXT, false)?;
    let concurrency = number("--max-concurrency", deploy.max_concurrency, false)?;
    let default_slots = concurrency
        .checked_mul(2)
        .ok_or("native_runtime_profile_unsupported: 并发溢出")?
        .max(16);
    let device_slots = number("--device-state-slots", default_slots, true)?;
    let host_slots = number("--host-state-slots", default_slots, true)?;
    for (name, expected) in [
        ("--host", deploy.host.clone()),
        ("--port", deploy.port.to_string()),
        ("--model-id", deploy.model_id.clone()),
    ] {
        if values.get(name).is_some_and(|value| value != &expected) {
            return Err(format!(
                "native_runtime_profile_unsupported: {name} 与部署身份冲突"
            ));
        }
    }
    let extras = extras.join(" ");
    let environment = crate::cards::native_environment(&app.config.cards_dir.join(&card.name))?;
    let serialized = serde_jcs::to_vec(&serde_json::json!({"engine":deploy.engine_dir,
        "weights":card.weights_path(),"model":deploy.model_id,"context":context,
        "concurrency":concurrency,"device_slots":device_slots,"host_slots":host_slots,
        "extras":extras,"environment":environment}))
    .map_err(|e| e.to_string())?;
    let revision = ring::digest::digest(&ring::digest::SHA256, &serialized)
        .as_ref()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    Ok(Profile {
        context,
        concurrency,
        device_slots,
        host_slots,
        extras,
        environment,
        revision,
    })
}

pub fn matches(app: &App, card: &Card, port: u16, pid: u32, profile: &Profile) -> bool {
    matches_profile(app, card, port, pid, profile, true)
}

/// The read-only adoption probe proves cgroup/unit provenance independently,
/// before it may persist a binding. Share the complete deployment identity check
/// without requiring the binding that this check is authorizing.
pub(super) fn matches_deployment(
    app: &App,
    card: &Card,
    port: u16,
    pid: u32,
    profile: &Profile,
) -> bool {
    matches_profile(app, card, port, pid, profile, false)
}

fn matches_profile(
    app: &App,
    card: &Card,
    port: u16,
    pid: u32,
    profile: &Profile,
    require_owned_origin: bool,
) -> bool {
    if pid == 0 {
        return false;
    }
    let result = (|| -> Result<bool, std::io::Error> {
        let root = app.config.proc_root.join(pid.to_string());
        let engine = card
            .engine_bin()
            .ok_or(std::io::ErrorKind::NotFound)?
            .canonicalize()?;
        let weights = card
            .weights_path()
            .ok_or(std::io::ErrorKind::NotFound)?
            .canonicalize()?;
        let loaded = std::fs::metadata(root.join("exe"))?;
        let expected = std::fs::metadata(&engine)?;
        if loaded.dev() != expected.dev()
            || loaded.ino() != expected.ino()
            || std::fs::metadata(&root)?.uid()
                != app.systemd.model_run_uid().map_err(std::io::Error::other)?
        {
            return Ok(false);
        }
        let raw = std::fs::read(root.join("cmdline"))?;
        if raw.len() > 1024 * 1024 {
            return Ok(false);
        }
        let Some(arguments) = raw.strip_suffix(&[0]) else {
            return Ok(false);
        };
        let args: Vec<String> = arguments
            .split(|b| *b == 0)
            .map(|value| String::from_utf8(value.to_vec()))
            .collect::<Result<_, _>>()
            .map_err(|_| std::io::ErrorKind::InvalidData)?;
        if args.len() < 2 {
            return Ok(false);
        }
        if args
            .get(1)
            .and_then(|arg| Path::new(arg).canonicalize().ok())
            .as_ref()
            != Some(&weights)
        {
            return Ok(false);
        }
        let adopted = crate::store_adoption::native_state(app, &card.name)
            .ok()
            .flatten()
            .is_some_and(|state| state.is_active() && state.main_pid == pid);
        if require_owned_origin && !adopted {
            let unit = app.systemd.unit_name_for(&card.name);
            let group = std::fs::read_to_string(root.join("cgroup"))?;
            if !group
                .lines()
                .any(|line| line.rsplit('/').next() == Some(unit.as_str()))
            {
                return Ok(false);
            }
        }
        if !owns_listener(&app.config.proc_root, pid, port)? {
            return Ok(false);
        }
        if !environment_matches(&root, &profile.environment)? {
            return Ok(false);
        }
        let actual = options(&args[2..]).map_err(std::io::Error::other)?;
        let deploy = card.deploy.as_ref().ok_or(std::io::ErrorKind::NotFound)?;
        let mut wanted = options(
            &profile
                .extras
                .split_whitespace()
                .map(str::to_owned)
                .collect::<Vec<_>>(),
        )
        .map_err(std::io::Error::other)?;
        for (name, value) in [
            ("--model-id", deploy.model_id.clone()),
            ("--port", port.to_string()),
            ("--host", deploy.host.clone()),
            ("--max-context", profile.context.to_string()),
            ("--max-concurrency", profile.concurrency.to_string()),
            ("--device-state-slots", profile.device_slots.to_string()),
            ("--host-state-slots", profile.host_slots.to_string()),
        ] {
            wanted.insert(name.into(), Some(value));
        }
        Ok(actual == wanted)
    })();
    result.unwrap_or(false)
}

const ENVIRON_LIMIT: u64 = 1024 * 1024;

/// Compare only the declared tuning keys. Other process environment fields may
/// contain credentials: never decode, return or log them. Absence is meaningful;
/// do not infer engine defaults for an undeclared key.
fn environment_matches(
    process: &Path,
    expected: &BTreeMap<String, String>,
) -> std::io::Result<bool> {
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(process.join("environ"))?;
    if !file.metadata()?.is_file() {
        return Ok(false);
    }
    let mut bytes = Vec::new();
    file.take(ENVIRON_LIMIT + 1).read_to_end(&mut bytes)?;
    if bytes.len() as u64 > ENVIRON_LIMIT || (!bytes.is_empty() && bytes.last() != Some(&0)) {
        return Ok(false);
    }
    let mut actual = BTreeMap::new();
    for field in bytes
        .split(|byte| *byte == 0)
        .filter(|field| !field.is_empty())
    {
        let separator = field
            .iter()
            .position(|byte| *byte == b'=')
            .unwrap_or(field.len());
        let (key, value) = field.split_at(separator);
        let key = match key {
            b"TACHYON_PROJ_W4A4" => "TACHYON_PROJ_W4A4",
            b"TACHYON_W4A4_SCHED" => "TACHYON_W4A4_SCHED",
            _ => continue,
        };
        let Some(value) = value.strip_prefix(b"=") else {
            return Ok(false);
        };
        let Ok(value) = std::str::from_utf8(value) else {
            return Ok(false);
        };
        if actual.insert(key.to_owned(), value.to_owned()).is_some() {
            return Ok(false);
        }
    }
    Ok(&actual == expected)
}

fn owns_listener(proc_root: &Path, pid: u32, port: u16) -> std::io::Result<bool> {
    let mut listening = std::collections::BTreeSet::new();
    for name in ["tcp", "tcp6"] {
        let text = match std::fs::read_to_string(proc_root.join("net").join(name)) {
            Ok(text) => text,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error),
        };
        for line in text.lines().skip(1) {
            let fields: Vec<_> = line.split_whitespace().collect();
            if fields.len() > 9
                && fields[3] == "0A"
                && fields[1]
                    .rsplit_once(':')
                    .and_then(|(_, p)| u16::from_str_radix(p, 16).ok())
                    == Some(port)
            {
                listening.insert(format!("socket:[{}]", fields[9]));
            }
        }
    }
    if listening.is_empty() {
        return Ok(false);
    }
    let owned: std::collections::BTreeSet<_> =
        std::fs::read_dir(proc_root.join(pid.to_string()).join("fd"))?
            .filter_map(Result::ok)
            .filter_map(|entry| std::fs::read_link(entry.path()).ok())
            .map(|value| value.to_string_lossy().into_owned())
            .collect();
    Ok(listening.is_subset(&owned))
}

fn record_path(app: &App, card: &Card) -> PathBuf {
    app.config
        .state_dir
        .join("native-profiles")
        .join(format!("{}.json", card.name))
}

pub fn recorded(app: &App, card: &Card, profile: &Profile) -> bool {
    crate::private_storage::read_limit(&record_path(app, card), 8192)
        .ok()
        .flatten()
        .and_then(|raw| serde_json::from_slice::<serde_json::Value>(&raw).ok())
        .is_some_and(|value| value["revision"].as_str() == Some(&profile.revision))
}

pub fn record(app: &App, card: &Card, profile: &Profile) -> Result<(), String> {
    let path = record_path(app, card);
    crate::private_storage::directory(path.parent().unwrap(), true)?;
    let raw = serde_json::to_vec(&serde_json::json!({"revision":profile.revision,
        "context":profile.context,"concurrency":profile.concurrency,
        "device_slots":profile.device_slots,"host_slots":profile.host_slots}))
    .map_err(|e| e.to_string())?;
    crate::private_storage::publish(&path, &raw, true)
}

#[cfg(test)]
mod tests;
