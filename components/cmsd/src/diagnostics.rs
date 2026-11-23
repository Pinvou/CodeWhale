//! Bounded read-only rescue client. No private capabilities, state-file reads,
//! repair, subprocesses, lifecycle requests, or alternative transport fallback.
use crate::{config::Config, http};
use serde_json::{json, Value};
use std::io::{self, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::{Duration, Instant};

const BODY_LIMIT: usize = 256 * 1024;
const REQUEST_BUDGET: Duration = Duration::from_secs(1);
const ENDPOINTS: [(&str, &str); 4] = [
    ("device", "/v1/device"),
    ("daemon", "/v1/status"),
    ("plans", "/v1/control/status"),
    ("remote", "/v1/remote/status"),
];

struct Deadline {
    stream: UnixStream,
    until: Instant,
}
impl Deadline {
    fn remaining(&self) -> io::Result<Duration> {
        self.until
            .checked_duration_since(Instant::now())
            .filter(|value| !value.is_zero())
            .ok_or_else(|| io::Error::new(io::ErrorKind::TimedOut, "diagnostic deadline"))
    }
}
impl Read for Deadline {
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        self.stream.set_read_timeout(Some(self.remaining()?))?;
        self.stream.read(bytes)
    }
}
impl Write for Deadline {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.stream.set_write_timeout(Some(self.remaining()?))?;
        self.stream.write(bytes)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.remaining()?;
        self.stream.flush()
    }
}

fn identifier(value: &Value) -> Option<&str> {
    value.as_str().filter(|text| {
        !text.is_empty()
            && text.len() <= 128
            && text
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"-_.:".contains(&byte))
    })
}
fn fault(code: &'static str) -> Value {
    json!({"state":"unavailable","code":code,"facts":null})
}
fn classify(id: &str, value: &Value) -> Value {
    if !value.is_object() {
        return fault("invalid_response");
    }
    match id {
        "device" => match (
            identifier(&value["device_id"]),
            identifier(&value["boot_id"]),
        ) {
            (Some(device), Some(boot)) if value["source"] == "cmsd" => {
                json!({"state":"available","code":null,"facts":{"device_id":device,"boot_id":boot,
                    "identity_scope":identifier(&value["identity_scope"]),"signature_verified":false}})
            }
            _ => fault("invalid_response"),
        },
        "daemon" => {
            let Some(models) = value["models"].as_array().filter(|rows| rows.len() <= 4096) else {
                return fault("invalid_response");
            };
            if value["service"] != "cmsd" {
                return fault("invalid_response");
            }
            if !value["device"].is_object()
                || identifier(&value["device"]["device_id"]).is_none()
                || identifier(&value["device"]["boot_id"]).is_none()
            {
                return fault("identity_unconfirmed");
            }
            if models.iter().any(|model| !model.is_object()) {
                return fault("invalid_response");
            }
            let managed = models
                .iter()
                .filter(|model| model["managed"] == true)
                .count();
            let observed = models
                .iter()
                .filter(|model| model["managed"] == false)
                .count();
            let degraded = models
                .iter()
                .filter(|model| model["status"] == "degraded")
                .count();
            let status_unknown = models
                .iter()
                .filter(|model| {
                    !matches!(
                        model["status"].as_str(),
                        Some("running" | "starting" | "stopped" | "degraded")
                    )
                })
                .count();
            let gateway = &value["gateway_status"];
            let gateway_known = gateway["ready"].is_boolean();
            let lifecycle_known = value["lifecycle_version_ready"].is_boolean();
            let maintenance = value["component_maintenance"].as_bool();
            let attention = !gateway_known
                || !lifecycle_known
                || gateway["ready"] != true
                || value["lifecycle_version_ready"] != true
                || maintenance != Some(false)
                || degraded > 0
                || status_unknown > 0;
            json!({"state":if attention{"degraded"}else{"available"},"code":if attention{Some("daemon_attention_required")}else{None},
                "facts":{"device_id":value["device"]["device_id"],"boot_id":value["device"]["boot_id"],
                    "lifecycle_version_ready":value["lifecycle_version_ready"].as_bool(),
                    "component_maintenance":maintenance,"registered_models":models.len(),"managed_models":managed,
                    "observation_only_models":observed,"ownership_unknown_models":models.len()-managed-observed,
                    "degraded_models":degraded,"status_unknown_models":status_unknown,"running_reported":models.iter().filter(|m|m["status"]=="running").count(),
                    "active_selection_present":value.get("active_model").and_then(|v|if v.is_null(){Some(false)}else{v.as_str().map(|_|true)}),
                    "gateway_listener_ready":gateway["ready"].as_bool(),"inference_verified":false}})
        }
        "plans" => {
            let Some(enabled) = value["enabled"].as_bool() else {
                return fault("invalid_response");
            };
            let ready = value["journal_ready"].as_bool();
            let version = value["lifecycle_version_ready"].as_bool();
            let kinds = value["supported_plan_kinds"]
                .as_array()
                .filter(|rows| rows.len() <= 32);
            if ready.is_none()
                || version.is_none()
                || kinds.is_none_or(|rows| rows.iter().any(|kind| identifier(kind).is_none()))
            {
                return fault("invalid_response");
            }
            let okay = enabled && ready == Some(true) && version == Some(true);
            json!({"state":if okay{"available"}else{"degraded"},"code":if okay{None}else{Some("plan_authority_unavailable")},
                "facts":{"enabled":enabled,"journal_ready":ready,"lifecycle_version_ready":version,
                    "supported_plan_kinds":kinds,"active_workers":value["active_workers"].as_u64(),
                    "group_coordination":value["group_coordination"].as_bool(),"remote_model_writes":value["remote_model_writes"].as_bool()}})
        }
        "remote" => {
            let Some(configured) = value["configured"].as_bool() else {
                return fault("invalid_response");
            };
            let Some(enabled) = value["enabled"].as_bool() else {
                return fault("invalid_response");
            };
            if !configured && enabled {
                return fault("invalid_response");
            }
            let registry = value["registry_ready"].as_bool();
            let tls = value["server_certificate"]["configuration_ready"].as_bool();
            // Older daemons lack split registry/TLS facts: absence is unknown,
            // even when their combined enabled bit happens to be true.
            let okay = !configured || (enabled && registry == Some(true) && tls == Some(true));
            json!({"state":if !configured{"not_configured"}else if okay{"available"}else{"degraded"},
                "code":if okay{None}else{Some("remote_authority_unconfirmed")},
                "facts":{"configured":configured,"enabled":enabled,"registry_ready":registry,
                    "tls_configuration_ready":tls,"network_health_verified":false,"remote_model_writes":value["remote_model_writes"].as_bool()}})
        }
        _ => fault("invalid_response"),
    }
}

fn request(
    path: &Path,
    route: &str,
    uid: u32,
    pid: &mut Option<u32>,
    budget: Duration,
) -> Result<Value, &'static str> {
    let started = Instant::now();
    let stream = http::connect_unix(path, budget).map_err(io_code)?;
    let peer = crate::peer::LocalPeer::from_stream(&stream).map_err(|_| "peer_unconfirmed")?;
    if peer.uid != uid {
        return Err("peer_uid_mismatch");
    }
    if pid.is_some_and(|old| old != peer.pid) {
        return Err("peer_changed");
    }
    *pid = Some(peer.pid);
    let mut stream = Deadline {
        stream,
        until: started + budget,
    };
    stream
        .write_all(
            format!("GET {route} HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\n\r\n").as_bytes(),
        )
        .map_err(io_code)?;
    let response =
        http::read_response_limited(&mut BufReader::new(stream), BODY_LIMIT).map_err(io_code)?;
    match response.status {
        200 => (),
        404 => return Err("endpoint_unsupported"),
        401 | 403 => return Err("access_denied"),
        429 | 503 => return Err("service_unavailable"),
        _ => return Err("http_error"),
    }
    let text = std::str::from_utf8(&response.body).map_err(|_| "invalid_response")?;
    // Existing bounded parser rejects duplicate keys and excessive JSON depth.
    crate::json::Value::parse(text).map_err(|_| "invalid_response")?;
    serde_json::from_str(text).map_err(|_| "invalid_response")
}
fn io_code(error: io::Error) -> &'static str {
    match error.kind() {
        io::ErrorKind::NotFound => "socket_missing",
        io::ErrorKind::ConnectionRefused => "connection_refused",
        io::ErrorKind::PermissionDenied => "access_denied",
        io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut => "request_timeout",
        io::ErrorKind::InvalidInput | io::ErrorKind::InvalidData | io::ErrorKind::UnexpectedEof => {
            "invalid_response"
        }
        _ => "transport_unconfirmed",
    }
}

fn observe(
    path: &Path,
    id: &str,
    route: &str,
    uid: u32,
    peer: &mut Option<u32>,
    budget: Duration,
) -> Value {
    let started = Instant::now();
    let mut result = fault("request_timeout");
    let mut attempts = 0;
    let mut first_error = None;
    for _ in 0..2 {
        let remaining = budget.saturating_sub(started.elapsed());
        if remaining.is_zero() {
            break;
        }
        attempts += 1;
        result = match request(path, route, uid, peer, remaining) {
            Ok(value) => classify(id, &value),
            Err("endpoint_unsupported") if id == "plans" || id == "remote" => {
                json!({"state":"unsupported","code":"endpoint_unsupported","facts":null})
            }
            Err(code) => fault(code),
        };
        // The status endpoint may omit device facts while another local read
        // owns its nonblocking identity lock. Read once more, within the SAME
        // endpoint budget. Never repair, reuse a stale snapshot, or retry a
        // malformed body, denied peer, or locked authority.
        if attempts == 1
            && matches!(
                result["code"].as_str(),
                Some("service_unavailable" | "identity_unconfirmed")
            )
            && budget.saturating_sub(started.elapsed()) > Duration::from_millis(20)
        {
            first_error = result["code"].as_str().map(str::to_owned);
            std::thread::sleep(Duration::from_millis(20));
            continue;
        }
        break;
    }
    result["attempts"] = json!(attempts);
    result["first_observation_error"] = json!(first_error);
    result
}

pub fn run(config: &Config) -> (Value, i32) {
    // SAFETY: geteuid has no preconditions.
    let uid = if config.systemd_mode == crate::config::SystemdMode::System {
        0
    } else {
        unsafe { libc::geteuid() }
    };
    let started = Instant::now();
    let mut peer = None;
    let mut checks = Vec::new();
    for (id, path) in ENDPOINTS {
        let before = Instant::now();
        let mut result = observe(
            &config.socket_path,
            id,
            path,
            uid,
            &mut peer,
            REQUEST_BUDGET,
        );
        result["id"] = json!(id);
        result["elapsed_ms"] = json!(before.elapsed().as_millis() as u64);
        checks.push(result);
    }
    let identity_changed = checks[0]["facts"].is_object()
        && checks[1]["facts"].is_object()
        && (checks[0]["facts"]["device_id"] != checks[1]["facts"]["device_id"]
            || checks[0]["facts"]["boot_id"] != checks[1]["facts"]["boot_id"]);
    let peer_changed = checks.iter().any(|check| check["code"] == "peer_changed");
    if identity_changed || peer_changed {
        // No mixed-generation facts, including earlier successful counts.
        for check in &mut checks {
            if check["facts"].is_object() {
                check["state"] = json!("unconfirmed");
                check["code"] = json!("snapshot_invalidated");
            }
            check["facts"] = Value::Null;
        }
    }
    let unavailable = identity_changed
        || peer_changed
        || checks[..2]
            .iter()
            .any(|check| check["state"] == "unavailable");
    let attention = checks.iter().any(|check| {
        !matches!(
            check["state"].as_str(),
            Some("available" | "not_configured" | "unsupported")
        )
    });
    let outcome = if unavailable {
        "unavailable"
    } else if attention {
        "degraded"
    } else {
        "available"
    };
    (
        json!({"format":"megacube-cmsd-diagnostic-v1","read_only":true,"observed_at":crate::timeutil::iso8601(crate::timeutil::now_epoch()),
        "outcome":outcome,"error":if identity_changed{Some("identity_changed")}else if peer_changed{Some("peer_changed")}else{None},
        "elapsed_ms":started.elapsed().as_millis() as u64,"server_peer":{"uid_expected":uid,"pid":peer,
            "unit_binding_verified":false,"signature_verified":false},"checks":checks,
        "limits":{"endpoints":4,"requests_max":8,"attempts_per_endpoint_max":2,"per_endpoint_ms":1000,"retry_delay_ms":20,"response_body_bytes":BODY_LIMIT},
        "interpretation":{"available_means":"fixed GET observations obtained; not complete feature or inference acceptance",
            "unknown_is_empty":false,"inference_verified":false,"physical_multinode_verified":false,
            "optional_kits_required":false,"separate_read_observations":true},
        "recovery":{"automatic_actions":false,"legacy_fallback":false,
            "next":"Inspect the configured instance in Pinvou sidebar > Device management (Status on ordinary devices) > MegaCube > Cube Manage Software or its exact systemd unit/logs; retain state, original operation IDs and current authority; refresh after matching-instance recovery"}}),
        if unavailable {
            4
        } else if attention {
            3
        } else {
            0
        },
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn transient_read_retries_once_with_the_original_total_budget() {
        let root = crate::docker::fixtures::tmpdir("diagnostic-retry");
        let path = root.join("api.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let server = std::thread::spawn(move || {
            for index in 0..4 {
                let (mut stream, _) = listener.accept().unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut raw = Vec::new();
                while !raw.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    raw.push(byte[0]);
                    assert!(raw.len() < 256);
                }
                if index % 2 == 0 {
                    stream
                        .write_all(b"HTTP/1.1 503 Unavailable\r\nContent-Length: 2\r\n\r\n{}")
                        .unwrap();
                } else if index == 1 {
                    let body = json!({"device_id":"d","boot_id":"b","source":"cmsd"}).to_string();
                    stream
                        .write_all(
                            format!(
                                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
                                body.len()
                            )
                            .as_bytes(),
                        )
                        .unwrap();
                } else {
                    stream
                        .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n")
                        .unwrap();
                    std::thread::sleep(Duration::from_millis(160));
                }
            }
        });
        // SAFETY: geteuid has no preconditions.
        let uid = unsafe { libc::geteuid() };
        let mut peer = None;
        let first = observe(
            &path,
            "device",
            "/v1/device",
            uid,
            &mut peer,
            Duration::from_millis(100),
        );
        assert_eq!(first["state"], "available");
        assert_eq!(first["attempts"], 2);
        assert_eq!(first["first_observation_error"], "service_unavailable");
        let started = Instant::now();
        let second = observe(
            &path,
            "device",
            "/v1/device",
            uid,
            &mut peer,
            Duration::from_millis(70),
        );
        assert_eq!(second["code"], "request_timeout");
        assert_eq!(second["attempts"], 2);
        assert!(started.elapsed() < Duration::from_millis(140));
        server.join().unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn changed_identity_discards_all_prior_facts() {
        let root = crate::docker::fixtures::tmpdir("diagnostic-identity");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let config = app.config.clone();
        let listener = std::os::unix::net::UnixListener::bind(&config.socket_path).unwrap();
        let server = std::thread::spawn(move || {
            let values = [
                json!({"device_id":"d","boot_id":"boot-old","source":"cmsd"}),
                json!({"service":"cmsd","device":{"device_id":"d","boot_id":"boot-new"},"models":[],"lifecycle_version_ready":true,
                    "component_maintenance":false,"gateway_status":{"ready":true}}),
                json!({"enabled":true,"journal_ready":true,"lifecycle_version_ready":true,"supported_plan_kinds":[]}),
                json!({"configured":false,"enabled":false}),
            ];
            for ((_, path), value) in ENDPOINTS.into_iter().zip(values) {
                let (mut stream, _) = listener.accept().unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut raw = Vec::new();
                while !raw.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    raw.push(byte[0]);
                    assert!(raw.len() < 256);
                }
                assert!(raw.starts_with(format!("GET {path} HTTP/1.1\r\n").as_bytes()));
                let body = value.to_string();
                stream
                    .write_all(
                        format!(
                            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
                            body.len()
                        )
                        .as_bytes(),
                    )
                    .unwrap();
            }
        });
        let (report, code) = run(&config);
        assert_eq!(code, 4);
        assert_eq!(report["error"], "identity_changed");
        assert!(report["checks"]
            .as_array()
            .unwrap()
            .iter()
            .all(|check| check["facts"].is_null() && check["state"] == "unconfirmed"));
        server.join().unwrap();
        drop(app);
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn changed_peer_is_rejected_before_any_get() {
        let root = crate::docker::fixtures::tmpdir("diagnostic-peer");
        let path = root.join("api.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(1)))
                .unwrap();
            assert_eq!(stream.read(&mut [0; 256]).unwrap(), 0);
        });
        // SAFETY: geteuid has no preconditions.
        let uid = unsafe { libc::geteuid() };
        let mut pid = Some(std::process::id().wrapping_add(1));
        assert_eq!(
            request(&path, "/v1/device", uid, &mut pid, Duration::from_secs(1)).unwrap_err(),
            "peer_changed"
        );
        server.join().unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn unknowns_and_corrupt_authority_are_never_empty_success() {
        assert_eq!(classify("daemon", &json!({}))["state"], "unavailable");
        assert_eq!(
            classify(
                "device",
                &json!({"device_id":"d","boot_id":"b","source":"other"})
            )["state"],
            "unavailable"
        );
        let missing = classify(
            "plans",
            &json!({"enabled":true,"journal_ready":true,"lifecycle_version_ready":true}),
        );
        assert_eq!(missing["facts"], Value::Null);
        let failed = classify(
            "plans",
            &json!({"enabled":true,"journal_ready":false,"lifecycle_version_ready":true,"supported_plan_kinds":["service.set_default"]}),
        );
        assert_eq!(failed["state"], "degraded");
        assert_eq!(failed["facts"]["journal_ready"], false);
    }
    #[test]
    fn remote_optional_split_unknown_and_secret_projection() {
        assert_eq!(
            classify("remote", &json!({"configured":false,"enabled":false}))["state"],
            "not_configured"
        );
        let old = classify(
            "remote",
            &json!({"configured":true,"enabled":true,"error":"private-SENTINEL","ca_certificate":"private-SENTINEL"}),
        );
        assert_eq!(old["state"], "degraded");
        assert_eq!(old["facts"]["registry_ready"], Value::Null);
        assert!(!old.to_string().contains("private-SENTINEL"));
        let broken = classify(
            "remote",
            &json!({"configured":true,"enabled":false,"registry_ready":true,"server_certificate":{"configuration_ready":false}}),
        );
        assert_eq!(broken["facts"]["registry_ready"], true);
        assert_eq!(broken["facts"]["tls_configuration_ready"], false);
    }
    #[test]
    fn listener_and_process_counts_never_prove_inference_or_ownership() {
        let base = json!({"service":"cmsd","device":{"device_id":"d","boot_id":"b"},"models":[{"managed":true,"status":"running"},{"managed":false,"status":"running"},{"status":"stopped"}],
            "lifecycle_version_ready":true,"component_maintenance":false,"gateway_status":{"ready":true},"error":"private-SENTINEL"});
        let report = classify("daemon", &base);
        assert_eq!(report["state"], "available");
        assert_eq!(report["facts"]["inference_verified"], false);
        assert_eq!(report["facts"]["ownership_unknown_models"], 1);
        assert_eq!(report["facts"]["observation_only_models"], 1);
        assert!(!report.to_string().contains("private-SENTINEL"));
        let mut older = base;
        older.as_object_mut().unwrap().remove("gateway_status");
        assert_eq!(classify("daemon", &older)["state"], "degraded");
    }
    #[test]
    fn fixed_get_transport_rejects_large_duplicate_and_slow_responses() {
        let root = crate::docker::fixtures::tmpdir("diagnostic-http");
        let path = root.join("api.sock");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let server = std::thread::spawn(move || {
            for response in [
                b"HTTP/1.1 200 OK\r\nContent-Length: 300000\r\n\r\n".as_slice(),
                b"HTTP/1.1 200 OK\r\nContent-Length: 13\r\n\r\n{\"x\":1,\"x\":2}",
                b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n",
            ] {
                let (mut stream, _) = listener.accept().unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut raw = Vec::new();
                let mut byte = [0];
                while !raw.ends_with(b"\r\n\r\n") {
                    stream.read_exact(&mut byte).unwrap();
                    raw.push(byte[0]);
                    assert!(raw.len() < 256);
                }
                assert_eq!(
                    std::str::from_utf8(&raw).unwrap(),
                    "GET /v1/device HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\n\r\n"
                );
                stream.write_all(response).unwrap();
                if response.ends_with(b"2\r\n\r\n") {
                    std::thread::sleep(Duration::from_millis(160));
                }
            }
        });
        let mut pid = None;
        // SAFETY: geteuid has no preconditions.
        let uid = unsafe { libc::geteuid() };
        assert_eq!(
            request(
                &path,
                "/v1/device",
                uid,
                &mut pid,
                Duration::from_millis(70)
            )
            .unwrap_err(),
            "invalid_response"
        );
        assert_eq!(
            request(
                &path,
                "/v1/device",
                uid,
                &mut pid,
                Duration::from_millis(70)
            )
            .unwrap_err(),
            "invalid_response"
        );
        let before = Instant::now();
        assert_eq!(
            request(
                &path,
                "/v1/device",
                uid,
                &mut pid,
                Duration::from_millis(70)
            )
            .unwrap_err(),
            "request_timeout"
        );
        assert!(before.elapsed() < Duration::from_millis(500));
        server.join().unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }
}
