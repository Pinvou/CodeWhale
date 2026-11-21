//! Read-only kernel network observations. No configuration or readiness claim.
use crate::app::{api_err, ApiErr, App};
use crate::json::Value;
use std::path::Path;
use std::process::Command;
use std::sync::{Mutex, TryLockError};
use std::time::{Duration, Instant};

pub struct Network {
    cache: Mutex<Option<(Instant, Value)>>,
}
impl Network {
    pub fn new() -> Self {
        Self {
            cache: Mutex::new(None),
        }
    }
    pub fn view(&self, app: &App) -> Result<Value, ApiErr> {
        // Single flight and one-second observation reuse prevent an inventory
        // reader from spawning unbounded ip processes. Never hold App.store.
        let mut cache = match self.cache.try_lock() {
            Ok(guard) => guard,
            Err(TryLockError::WouldBlock) => {
                return Err(api_err(
                    503,
                    "network_query_busy",
                    "网络事实正在采集，请稍后刷新",
                ))
            }
            Err(TryLockError::Poisoned(error)) => error.into_inner(),
        };
        if let Some((started, value)) = &*cache {
            if started.elapsed() < Duration::from_secs(1) {
                return Ok(value.clone());
            }
        }
        let started = Instant::now();
        let device = crate::device::record(app)?
            .get("device_id")
            .cloned()
            .ok_or_else(|| api_err(500, "identity_unavailable", "设备身份缺失"))?;
        let before = crate::peer::boot_id();
        let mut result = Value::obj();
        result.set("source", "cmsd".into());
        result.set("device_id", device);
        result.set("boot_id", before.clone().into());
        result.set("read_only", true.into());
        result.set(
            "observed_at",
            crate::timeutil::iso8601(crate::timeutil::now_epoch()).into(),
        );
        result.set("physical_topology_verified", false.into());
        result.set("network_ready", Value::Null);
        let mut queries = Value::obj();
        for (key, args, interfaces) in [
            ("interfaces", vec!["-j", "address", "show"], true),
            (
                "routes_ipv4",
                vec!["-j", "-4", "route", "show", "table", "all"],
                false,
            ),
            (
                "routes_ipv6",
                vec!["-j", "-6", "route", "show", "table", "all"],
                false,
            ),
        ] {
            let mut query = Value::obj();
            let budget = Duration::from_secs(3).checked_sub(started.elapsed());
            let value = budget
                .filter(|v| !v.is_zero())
                .ok_or("network observation deadline exceeded".to_owned())
                .and_then(|budget| run(&args, budget))
                .and_then(|value| normalize(value, interfaces));
            match value {
                Ok(value) => {
                    query.set("available", true.into());
                    result.set(key, value);
                }
                Err(message) => {
                    query.set("available", false.into());
                    let mut error = Value::obj();
                    error.set("code", "network_query_failed".into());
                    error.set("message", message.into());
                    query.set("error", error);
                    result.set(key, Value::Null);
                }
            }
            queries.set(key, query);
        }
        result.set("queries", queries);
        result.set("rdma", rdma(Path::new("/sys/class/infiniband")));
        if before != crate::peer::boot_id() {
            return Err(api_err(
                409,
                "observation_changed",
                "网络事实采集期间系统身份变化，请重新读取",
            ));
        }
        result.set(
            "observation_duration_ms",
            (started.elapsed().as_millis() as u64).into(),
        );
        *cache = Some((Instant::now(), result.clone()));
        Ok(result)
    }
}

fn run(args: &[&str], budget: Duration) -> Result<Value, String> {
    let output = crate::process::output(Command::new("ip").args(args).env("LC_ALL", "C"), budget)
        .map_err(|error| format!("ip observation unavailable: {error}"))?;
    if !output.status.success() {
        return Err("ip query failed; no configuration was changed".into());
    }
    if output.stdout.len() > 1024 * 1024 {
        return Err("network observation exceeds 1 MiB".into());
    }
    let text =
        std::str::from_utf8(&output.stdout).map_err(|_| "network observation UTF-8 invalid")?;
    Value::parse_limited(text, 65536).map_err(|_| "network observation JSON invalid".into())
}
fn name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 64
        && value != "."
        && value != ".."
        && !value
            .chars()
            .any(|c| c.is_control() || c == '/' || c == '\\')
}
fn text(path: &Path) -> Option<String> {
    use std::io::Read;
    let mut value = String::new();
    std::fs::File::open(path)
        .ok()?
        .take(1025)
        .read_to_string(&mut value)
        .ok()?;
    (value.len() <= 1024).then(|| value.trim().to_string())
}
fn normalize(value: Value, interfaces: bool) -> Result<Value, String> {
    let rows = value
        .as_arr()
        .ok_or("network observation must be an array")?;
    if rows.len() > 4096 {
        return Err(
            "too many network records; observation unavailable, not an empty network".into(),
        );
    }
    if !interfaces {
        if rows.iter().any(|row| row.as_obj().is_none()) {
            return Err("invalid route observation; no partial success claimed".into());
        }
        return Ok(value);
    }
    for row in rows {
        let valid = row.get("ifname").and_then(Value::as_str).is_some_and(name)
            && row
                .get("ifindex")
                .and_then(Value::as_u64)
                .is_some_and(|n| n > 0 && n <= u32::MAX as u64)
            && row
                .get("mtu")
                .and_then(Value::as_u64)
                .is_some_and(|n| n > 0 && n <= u32::MAX as u64)
            && row
                .get("addr_info")
                .and_then(Value::as_arr)
                .is_some_and(|addresses| {
                    addresses.iter().all(|address| {
                        let local = address
                            .get("local")
                            .and_then(Value::as_str)
                            .and_then(|s| s.parse::<std::net::IpAddr>().ok());
                        let prefix = address.get("prefixlen").and_then(Value::as_u64);
                        matches!(
                            (local, prefix),
                            (Some(std::net::IpAddr::V4(_)), Some(0..=32))
                                | (Some(std::net::IpAddr::V6(_)), Some(0..=128))
                        )
                    })
                });
        if !valid {
            return Err("invalid interface/address observation; no partial success claimed".into());
        }
    }
    Ok(value)
}
fn rdma(root: &Path) -> Value {
    let mut out = Value::obj();
    out.set("collective_validation", "not_run".into());
    let entries = match std::fs::read_dir(root) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            out.set("available", true.into());
            out.set("devices", Value::Arr(Vec::new()));
            return out;
        }
        Err(_) => {
            out.set("available", false.into());
            out.set("devices", Value::Null);
            return out;
        }
    };
    let mut devices = Vec::new();
    let mut complete = true;
    for entry in entries.take(65) {
        let Ok(entry) = entry else {
            complete = false;
            continue;
        };
        let Some(device) = entry
            .file_name()
            .to_str()
            .filter(|v| name(v))
            .map(str::to_string)
        else {
            complete = false;
            continue;
        };
        let mut record = Value::obj();
        record.set("name", device.into());
        let mut ports = Vec::new();
        match std::fs::read_dir(entry.path().join("ports")) {
            Ok(entries) => {
                for port in entries.take(65) {
                    let Ok(port) = port else {
                        complete = false;
                        continue;
                    };
                    let Some(index) = port
                        .file_name()
                        .to_str()
                        .and_then(|v| v.parse::<u32>().ok())
                    else {
                        complete = false;
                        continue;
                    };
                    let mut item = Value::obj();
                    item.set("port", (index as u64).into());
                    for field in ["state", "phys_state", "link_layer", "active_mtu", "rate"] {
                        let observed = text(&port.path().join(field));
                        if observed.is_none() {
                            complete = false;
                        }
                        item.set(field, observed.map(Value::from).unwrap_or(Value::Null));
                    }
                    ports.push(item);
                }
            }
            Err(_) => complete = false,
        }
        if ports.len() > 64 {
            complete = false;
            ports.truncate(64);
        }
        record.set("ports", Value::Arr(ports));
        devices.push(record);
    }
    if devices.len() > 64 {
        complete = false;
        devices.truncate(64);
    }
    devices.sort_by(|a, b| {
        a.get("name")
            .and_then(Value::as_str)
            .cmp(&b.get("name").and_then(Value::as_str))
    });
    out.set("available", complete.into());
    out.set("devices", Value::Arr(devices));
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn malformed_route_rows_are_unknown_instead_of_counted_as_real_routes() {
        for raw in [r#"[null]"#, r#"["route"]"#, "[1]", "[[]]"] {
            assert!(normalize(Value::parse(raw).unwrap(), false).is_err());
        }
        for raw in [
            "[]",
            r#"[{"dst":"default","dev":"eth0"}]"#,
            r#"[{"type":"unreachable","dst":"192.0.2.0/24"}]"#,
        ] {
            assert!(normalize(Value::parse(raw).unwrap(), false).is_ok());
        }
    }
    #[test]
    fn invalid_interfaces_never_become_an_empty_or_successful_snapshot() {
        let good = r#"[{"ifindex":2,"ifname":"eth0","mtu":1500,"addr_info":[{"local":"192.0.2.2","prefixlen":24}]}]"#;
        assert!(normalize(Value::parse(good).unwrap(), true).is_ok());
        for bad in [
            good.replace("eth0", "../eth0"),
            good.replace("192.0.2.2", "bad"),
            good.replace(":24", ":33"),
            good.replace("1500", "0"),
        ] {
            assert!(normalize(Value::parse(&bad).unwrap(), true).is_err());
        }
        assert!(normalize(Value::obj(), true).is_err());
    }
    #[test]
    fn rdma_presence_never_proves_collective_or_topology_readiness() {
        let root = crate::docker::fixtures::tmpdir("network-rdma");
        let missing = rdma(&root.join("absent"));
        assert_eq!(missing.get("available"), Some(&Value::Bool(true)));
        let port = root.join("mlx5_0/ports/1");
        std::fs::create_dir_all(&port).unwrap();
        for field in ["state", "phys_state", "link_layer", "active_mtu", "rate"] {
            std::fs::write(port.join(field), "observed\n").unwrap();
        }
        let value = rdma(&root);
        assert_eq!(value.get("available"), Some(&Value::Bool(true)));
        assert_eq!(
            value.get("collective_validation").and_then(Value::as_str),
            Some("not_run")
        );
        std::fs::remove_file(port.join("state")).unwrap();
        assert_eq!(rdma(&root).get("available"), Some(&Value::Bool(false)));
        std::fs::remove_dir_all(root).unwrap();
    }
}
