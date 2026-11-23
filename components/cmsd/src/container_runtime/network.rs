//! A successful HTTP probe belongs to this container only after its inspected
//! network configuration proves the exact IPv4 loopback endpoint we route to.
use crate::cards::DockerDeploy;
use crate::docker::{unique_scalar, ContainerInfo};
use std::net::{IpAddr, Ipv4Addr};

fn loopback_reachable(host: &str) -> bool {
    // An IPv6-only wildcard or another 127/8 address does not prove that the
    // gateway's exact 127.0.0.1 endpoint is served. Docker's empty bind is IPv4 *.
    host.is_empty()
        || matches!(host.parse::<IpAddr>(), Ok(IpAddr::V4(ip))
            if ip.is_unspecified() || ip == Ipv4Addr::LOCALHOST)
}

pub(super) fn check(deploy: &DockerDeploy, container: &ContainerInfo) -> Result<(), String> {
    let invalid =
        || "configuration_changed: 容器网络未证明当前部署的回环端口，保留原实例及策略".to_owned();
    if deploy.port == 0 {
        return Err(invalid());
    }
    if let Some(host) = &deploy.host_bind {
        let actual_host = unique_scalar(&container.args, "--host").map_err(|_| invalid())?;
        let port = unique_scalar(&container.args, "--port").map_err(|_| invalid())?;
        if container.network_mode != "host"
            || host.is_empty()
            || !loopback_reachable(host)
            || host.parse::<IpAddr>().ok().is_none()
            || actual_host.and_then(|host| host.parse::<IpAddr>().ok())
                != host.parse::<IpAddr>().ok()
            || port.and_then(|value| value.parse::<u16>().ok()) != Some(deploy.port)
        {
            return Err(invalid());
        }
        return Ok(());
    }
    if container.network_mode.is_empty()
        || matches!(container.network_mode.as_str(), "host" | "none")
        || container.network_mode.starts_with("container:")
    {
        return Err(invalid());
    }
    // Running facts authorize health/routing; a stopped runtime has no active
    // Ports and instead needs the durable declaration before docker start.
    let bindings = if container.running {
        &container.active_tcp_bindings
    } else {
        &container.configured_tcp_bindings
    };
    let process_port = unique_scalar(&container.args, "--port")
        .map_err(|_| invalid())?
        .map(|value| value.parse::<u16>().map_err(|_| invalid()))
        .transpose()?;
    if !bindings.as_ref().is_some_and(|bindings| {
        bindings.iter().any(|binding| {
            binding.host_port == deploy.port
                && loopback_reachable(&binding.host_ip)
                && process_port.is_none_or(|port| port == binding.container_port)
        })
    }) {
        return Err(invalid());
    }
    Ok(())
}
