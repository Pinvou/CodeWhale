//! Linux interface and raw-port evidence for the authenticated model proxy.
use super::lan_gateway::Interface;
use std::{
    ffi::CStr,
    net::{IpAddr, Ipv4Addr},
};

pub fn normalize_upstream(value: &str) -> Result<String, String> {
    let mut url = reqwest::Url::parse(value).map_err(|_| "lan_unsupported")?;
    if url.scheme() != "http"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("lan_unsupported".into());
    }
    let host = url
        .host_str()
        .ok_or("lan_unsupported")?
        .trim_start_matches('[')
        .trim_end_matches(']');
    if host == "localhost" || host == "localhost." {
        url.set_host(Some("127.0.0.1"))
            .map_err(|_| "lan_unsupported")?;
    } else if !host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback()) {
        return Err("lan_upstream_exposed".into());
    }
    if !matches!(
        url.path().trim_end_matches('/'),
        "" | "/v1" | "/v1/chat/completions"
    ) {
        return Err("lan_unsupported".into());
    }
    url.set_path("/v1");
    Ok(url.to_string().trim_end_matches('/').to_string())
}

pub fn interfaces() -> Result<Vec<Interface>, String> {
    let mut head: *mut libc::ifaddrs = std::ptr::null_mut();
    // SAFETY: getifaddrs initializes the linked list; guard frees it exactly once.
    if unsafe { libc::getifaddrs(&mut head) } != 0 {
        return Err("lan_address_unavailable".into());
    }
    struct Guard(*mut libc::ifaddrs);
    impl Drop for Guard {
        fn drop(&mut self) {
            unsafe {
                libc::freeifaddrs(self.0);
            }
        }
    }
    let _guard = Guard(head);
    let mut cursor = head;
    let mut result = Vec::new();
    while !cursor.is_null() {
        // SAFETY: nodes and sockaddr values remain valid for the guard's lifetime.
        let item = unsafe { &*cursor };
        if !item.ifa_addr.is_null()
            && !item.ifa_netmask.is_null()
            && unsafe { (*item.ifa_addr).sa_family } as i32 == libc::AF_INET
            && item.ifa_flags & libc::IFF_UP as u32 != 0
            && item.ifa_flags & (libc::IFF_LOOPBACK | libc::IFF_POINTOPOINT) as u32 == 0
        {
            let address = Ipv4Addr::from(
                unsafe {
                    (*(item.ifa_addr as *const libc::sockaddr_in))
                        .sin_addr
                        .s_addr
                }
                .to_ne_bytes(),
            );
            let mask = Ipv4Addr::from(
                unsafe {
                    (*(item.ifa_netmask as *const libc::sockaddr_in))
                        .sin_addr
                        .s_addr
                }
                .to_ne_bytes(),
            );
            let name = unsafe { CStr::from_ptr(item.ifa_name) }
                .to_string_lossy()
                .into_owned();
            // Container bridges are not user-facing LAN interfaces. VPN addresses
            // remain explicit choices; peers must still be in the chosen subnet.
            if address.is_private()
                && !name.starts_with("docker")
                && !name.starts_with("veth")
                && !name.starts_with("br-")
            {
                result.push(Interface {
                    address,
                    mask,
                    name,
                });
            }
        }
        cursor = item.ifa_next;
    }
    result.sort_by(|a, b| a.name.cmp(&b.name).then(a.address.cmp(&b.address)));
    result.dedup();
    Ok(result)
}

/// Reject a wildcard/private/public bind of the raw port in either family.
fn socket_evidence(text: &str, port: u16, ipv6: bool) -> Result<bool, String> {
    let mut found = false;
    for line in text.lines().skip(1) {
        let fields: Vec<_> = line.split_whitespace().collect();
        if fields.get(3) != Some(&"0A") {
            continue;
        }
        let Some((host, encoded_port)) = fields.get(1).and_then(|v| v.split_once(':')) else {
            continue;
        };
        if u16::from_str_radix(encoded_port, 16).ok() != Some(port) {
            continue;
        }
        let loopback = if ipv6 {
            host == "00000000000000000000000001000000"
        } else {
            u32::from_str_radix(host, 16)
                .is_ok_and(|ip| Ipv4Addr::from(ip.to_ne_bytes()).is_loopback())
        };
        if !loopback {
            return Err("lan_upstream_exposed".into());
        }
        found = true;
    }
    Ok(found)
}

fn docker_evidence(value: &serde_json::Value, port: u16) -> Result<bool, String> {
    let object = value
        .as_array()
        .and_then(|a| a.first())
        .ok_or("lan_isolation_unknown")?;
    if object["State"]["Running"] != true {
        return Err("lan_model_unavailable".into());
    }
    let mut found = false;
    if let Some(ports) = object["NetworkSettings"]["Ports"].as_object() {
        for mappings in ports.values().filter_map(|v| v.as_array()) {
            for mapping in mappings {
                let host = mapping["HostIp"].as_str().unwrap_or("");
                if !host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback()) {
                    return Err("lan_upstream_exposed".into());
                }
                if mapping["HostPort"]
                    .as_str()
                    .and_then(|v| v.parse::<u16>().ok())
                    == Some(port)
                {
                    found = true;
                }
            }
        }
    }
    Ok(found)
}
pub(super) fn isolation(
    port: u16,
    container: Option<&str>,
    inspect: impl Fn(&str) -> Result<serde_json::Value, String>,
) -> Result<(), String> {
    let tcp = std::fs::read_to_string("/proc/net/tcp").map_err(|_| "lan_isolation_unknown")?;
    let tcp6 = match std::fs::read_to_string("/proc/net/tcp6") {
        Ok(v) => v,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => String::new(),
        Err(_) => return Err("lan_isolation_unknown".into()),
    };
    let mut found = socket_evidence(&tcp, port, false)? | socket_evidence(&tcp6, port, true)?;
    if let Some(container) = container {
        if container.starts_with('-') || container.is_empty() {
            return Err("lan_isolation_unknown".into());
        }
        let value = inspect(container)?;
        found |= docker_evidence(&value, port)?;
    }
    if found {
        Ok(())
    } else {
        Err("lan_isolation_unknown".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn only_explicit_loopback_openai_targets() {
        for input in [
            "http://127.0.0.1:4000",
            "http://localhost:4000/v1/chat/completions",
            "http://[::1]:4000/v1/",
        ] {
            assert!(normalize_upstream(input).unwrap().ends_with("/v1"));
        }
        for input in [
            "http://0.0.0.0:4000/v1",
            "http://192.168.1.2/v1",
            "http://localhost.evil/v1",
            "http://127.0.0.1/generate",
            "http://127.0.0.1/v1?key=x",
            "http://user:secret@127.0.0.1/v1",
        ] {
            assert!(normalize_upstream(input).is_err());
        }
    }
    #[test]
    fn raw_socket_and_docker_publications_are_checked() {
        let header = "sl local_address rem_address st\n";
        assert!(
            socket_evidence(
                &format!("{header}0: 0100007F:1F90 00000000:0000 0A"),
                8080,
                false
            )
            .unwrap()
        );
        for ip in ["00000000", "0201A8C0"] {
            assert!(
                socket_evidence(
                    &format!("{header}0: {ip}:1F90 00000000:0000 0A"),
                    8080,
                    false
                )
                .is_err()
            );
        }
        assert!(
            socket_evidence(
                &format!("{header}0: 00000000000000000000000000000000:1F90 0:0 0A"),
                8080,
                true
            )
            .is_err()
        );
        for host in ["0.0.0.0", "::", "", "192.168.1.2"] {
            let value = serde_json::json!([{"State":{"Running":true},"NetworkSettings":{"Ports":{"8000/tcp":[{"HostIp":host,"HostPort":"8080"}]}}}]);
            assert!(docker_evidence(&value, 8080).is_err());
        }
        let value = serde_json::json!([{"State":{"Running":true},"NetworkSettings":{"Ports":{"8000/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}}}]);
        assert!(docker_evidence(&value, 8080).unwrap());
    }
    #[test]
    fn actual_raw_listener_must_be_loopback() {
        let local = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        assert!(isolation(local.local_addr().unwrap().port(), None, |_| unreachable!()).is_ok());
        let exposed = std::net::TcpListener::bind("0.0.0.0:0").unwrap();
        assert_eq!(
            isolation(
                exposed.local_addr().unwrap().port(),
                None,
                |_| unreachable!()
            )
            .unwrap_err(),
            "lan_upstream_exposed"
        );
        assert!(interfaces().is_ok());
    }
}
