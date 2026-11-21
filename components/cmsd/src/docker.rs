//! docker 后端（W6）：容器发现、状态/策略查询、生命周期（start/stop/restart 策略）、
//! 显存归属。全部 shell-out docker CLI（设备出厂把登录用户放进 docker 组，免 sudo）。
//!
//! CLI 路径经 config.docker_cli / nvidia_smi_cli 注入（测试用 fixture 脚本顶替），
//! 本模块不感知真假 docker：只约定调用的子命令形状与输出解析。
//!
//! 显存归属口径（GB10 统一内存实测）：
//! - `nvidia-smi --query-compute-apps=pid,used_memory` 给的是**宿主 PID**；
//! - `docker top <name> -eo pid,comm` 列出的也是**宿主 PID**（容器命名空间已翻译）；
//! - 两者取交集求和即该容器的显存占用。整卡总量查询（memory.total）在 GB10 上
//!   返回 [N/A]（统一内存没有独立显存），视图层必须容忍缺省。

use crate::json::Value;
use std::process::Command;

/// 自启标记口径：这两种 restart 策略 = 「每次开机拉起」。
pub const AUTOSTART_POLICIES: [&str; 2] = ["unless-stopped", "always"];

#[derive(Debug, Clone)]
pub struct Docker {
    cli: String,
    nvidia_smi_cli: String,
}

#[derive(Debug, Clone, Default)]
pub struct ContainerInfo {
    pub name: String,
    pub image: String,
    /// .State.Status：running / exited / created / paused / dead…
    pub state: String,
    pub running: bool,
    pub exit_code: i64,
    pub oom_killed: bool,
    /// HostConfig.RestartPolicy.Name：no / always / unless-stopped / on-failure。
    pub restart_policy: String,
    /// First declared TCP mapping for observation only, never routing proof.
    pub host_port: Option<u16>,
    pub container_port: Option<u16>,
    /// All network facts belong to this inspect snapshot. Missing/malformed
    /// facts stay unknown while ordinary container observation remains usable.
    pub network_mode: String,
    pub configured_tcp_bindings: Option<Vec<TcpBinding>>,
    pub active_tcp_bindings: Option<Vec<TcpBinding>>,
    /// State.Pid（running 时 >0，宿主侧 PID）。
    pub pid: u32,
    /// 启动命令（Path + Args 拼接），预算解析 --mem-fraction-static 用。
    pub args: Vec<String>,
    /// Source mounts from this inspect snapshot; container destinations may vary.
    pub mount_sources: Vec<String>,
}

#[derive(Debug, Clone)]
pub struct TcpBinding {
    pub container_port: u16,
    pub host_port: u16,
    pub host_ip: String,
}

/// Preserve every binding; UDP/exposed-but-unpublished ports are not TCP
/// publication evidence. Invalid data must not turn into an empty success.
fn tcp_bindings(value: Option<&Value>) -> Option<Vec<TcpBinding>> {
    let mut result = Vec::new();
    for (name, bindings) in value?.as_obj()? {
        let Some(port) = name.strip_suffix("/tcp") else {
            continue;
        };
        let container_port = port.parse::<u16>().ok().filter(|port| *port > 0)?;
        if matches!(bindings, Value::Null) {
            continue;
        }
        for binding in bindings.as_arr()? {
            result.push(TcpBinding {
                container_port,
                host_port: binding
                    .get("HostPort")?
                    .as_str()?
                    .parse::<u16>()
                    .ok()
                    .filter(|port| *port > 0)?,
                host_ip: binding.get("HostIp")?.as_str()?.to_owned(),
            });
        }
    }
    Some(result)
}

impl ContainerInfo {
    /// Compare the declared source to this exact inspect result.
    pub fn model_mount_matches(&self, expected: &str) -> Result<bool, String> {
        let expected = std::path::Path::new(expected)
            .canonicalize()
            .map_err(|e| e.to_string())?;
        Ok(self.mount_sources.iter().any(|source| {
            std::path::Path::new(source).canonicalize().ok().as_ref() == Some(&expected)
        }))
    }

    pub fn is_autostart(&self) -> bool {
        AUTOSTART_POLICIES.contains(&self.restart_policy.as_str())
    }

    /// 从启动命令提取 `--served-model-name X`（容器服务的真名）。
    pub fn served_model_name(&self) -> Option<String> {
        arg_value(&self.args, "--served-model-name")
    }

    /// 从启动命令提取 `--mem-fraction-static F`（SGLang 静态预留比例）。
    pub fn mem_fraction_static(&self) -> Option<f64> {
        arg_value(&self.args, "--mem-fraction-static").and_then(|v| v.parse().ok())
    }
}

/// 命令行参数里取 `--flag value` 的值；compose 渲染的 `--flag=value` 连写形式也认。
pub fn arg_value(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1))
        .cloned()
        .or_else(|| {
            let prefix = format!("{name}=");
            args.iter()
                .find_map(|a| a.strip_prefix(&prefix).map(|v| v.to_string()))
        })
}

/// Read one known scalar without dropping duplicate or missing values. This
/// does not interpret unrelated options or reject repeatable list parameters.
pub fn unique_scalar(args: &[String], name: &str) -> Result<Option<String>, &'static str> {
    let mut found = None;
    let prefix = format!("{name}=");
    for (index, argument) in args.iter().enumerate() {
        let value = if argument == name {
            Some(
                args.get(index + 1)
                    .filter(|value| !value.starts_with("--"))
                    .ok_or("missing scalar value")?
                    .as_str(),
            )
        } else {
            argument.strip_prefix(&prefix)
        };
        if let Some(value) = value {
            if value.is_empty() || found.replace(value.to_owned()).is_some() {
                return Err("empty or repeated scalar option");
            }
        }
    }
    Ok(found)
}

/// exec 的瞬时 ETXTBSY 重试：高负载机上「刚写完的脚本立刻 exec」偶发
/// Text file busy（os error 26，内核侧写者引用未及时释放是瞬态的）。
/// 只对这个错误重试（5 × 20ms），别的错误原样上抛。
fn output_with_retry(cmd: &mut Command) -> std::io::Result<std::process::Output> {
    let mut attempt = 0;
    loop {
        let mutation = cmd
            .get_args()
            .next()
            .is_some_and(|arg| matches!(arg.to_str(), Some("start" | "stop" | "update")));
        match crate::process::output(
            cmd,
            std::time::Duration::from_secs(if mutation { 150 } else { 10 }),
        ) {
            Err(e) if e.raw_os_error() == Some(26) && attempt < 5 => {
                attempt += 1;
                std::thread::sleep(std::time::Duration::from_millis(20));
            }
            other => return other,
        }
    }
}

impl Docker {
    pub fn new(cli: &str, nvidia_smi_cli: &str) -> Docker {
        Docker {
            cli: cli.to_string(),
            nvidia_smi_cli: nvidia_smi_cli.to_string(),
        }
    }

    fn run(&self, args: &[&str]) -> Result<String, String> {
        let out = output_with_retry(
            Command::new(&self.cli)
                .args(args)
                .stdin(std::process::Stdio::null()),
        )
        .map_err(|e| format!("{} 调用失败：{e}", self.cli))?;
        if out.status.success() {
            Ok(String::from_utf8_lossy(&out.stdout).to_string())
        } else {
            Err(format!(
                "docker {} 失败（rc={:?}）：{}",
                args.join(" "),
                out.status.code(),
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
    }

    /// 全部容器名（docker ps -a）。daemon 连不上时返回 Err（调用方按「没数据」
    /// 降级，绝不能拿空列表去收敛状态——那是「没容器」而不是「查不到」）。
    pub fn list_names(&self) -> Result<Vec<String>, String> {
        let out = self.run(&["ps", "-a", "--format", "{{.Names}}"])?;
        Ok(out
            .lines()
            .map(|l| l.trim().to_string())
            .filter(|l| !l.is_empty())
            .collect())
    }

    /// inspect 单个容器。不存在返回 Ok(None)。
    pub fn inspect(&self, name: &str) -> Result<Option<ContainerInfo>, String> {
        match self.run(&["inspect", name]) {
            Ok(text) => parse_inspect(&text)
                .map(|mut v| v.drain(..).next())
                .ok_or_else(|| format!("docker inspect {name} 输出无法解析")),
            // 各版本 docker 文案不一：Error: No such container / error: no such object
            // （真机实测全小写形）——统一小写化再判，容器不存在不算 docker 故障
            Err(e)
                if {
                    let l = e.to_lowercase();
                    l.contains("no such container") || l.contains("no such object")
                } =>
            {
                Ok(None)
            }
            Err(e) => Err(e),
        }
    }

    /// 全量扫描：ps -a + 一次 inspect 全部。任何一个环节失败整体 Err（口径见 list_names）。
    pub fn scan(&self) -> Result<Vec<ContainerInfo>, String> {
        let names = self.list_names()?;
        if names.is_empty() {
            return Ok(Vec::new());
        }
        let mut args = vec!["inspect"];
        for n in &names {
            args.push(n);
        }
        let text = self.run(&args)?;
        parse_inspect(&text).ok_or_else(|| "docker inspect 批量输出无法解析".to_string())
    }

    /// 拉起已存在的容器。
    pub fn start(&self, name: &str) -> Result<(), String> {
        self.run(&["start", name]).map(|_| ())
    }

    /// 体面停止（与卡片 stop.sh 同口径：120s 宽限）。
    pub fn stop(&self, name: &str) -> Result<(), String> {
        self.run(&["stop", "-t", "120", name]).map(|_| ())
    }

    /// 挂/撤自启标记（语义规格 1/2/3 的唯一事实源：restart 策略即标记）。
    pub fn set_restart_policy(&self, name: &str, policy: &str) -> Result<(), String> {
        self.run(&["update", &format!("--restart={policy}"), name])
            .map(|_| ())
    }

    /// 容器内进程的宿主 PID 列表（显存归属用）。
    pub fn top_pids(&self, name: &str) -> Result<Vec<u32>, String> {
        let out = self.run(&["top", name, "-eo", "pid"])?;
        Ok(out
            .lines()
            .skip(1) // 表头 "PID"
            .filter_map(|l| l.trim().parse::<u32>().ok())
            .collect())
    }

    pub fn logs_tail(&self, name: &str, tail: u32) -> Result<String, String> {
        // docker logs 把应用输出同时写 stdout/stderr，合并取回
        let out = output_with_retry(
            Command::new(&self.cli)
                .args(["logs", "--tail", &tail.to_string(), name])
                .stdin(std::process::Stdio::null()),
        )
        .map_err(|e| format!("{} 调用失败：{e}", self.cli))?;
        if out.status.success() {
            let mut text = String::from_utf8_lossy(&out.stdout).to_string();
            if !out.stderr.is_empty() {
                if !text.is_empty() {
                    text.push('\n');
                }
                text.push_str(&String::from_utf8_lossy(&out.stderr));
            }
            Ok(text)
        } else {
            Err(format!(
                "docker logs 失败：{}",
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
    }

    /// 显存使用：宿主 PID → MiB（nvidia-smi query-compute-apps）。失败返回空
    /// （GB10 上这个查询可用；不可用时不阻塞视图，只是没有归属数据）。
    pub fn gpu_procs_mib(&self) -> Vec<(u32, u64)> {
        let out = output_with_retry(
            Command::new(&self.nvidia_smi_cli)
                .args([
                    "--query-compute-apps=pid,used_memory",
                    "--format=csv,noheader",
                ])
                .stdin(std::process::Stdio::null()),
        );
        let Ok(out) = out else { return Vec::new() };
        if !out.status.success() {
            return Vec::new();
        }
        String::from_utf8_lossy(&out.stdout)
            .lines()
            .filter_map(|l| {
                let (pid, mib) = l.split_once(',')?;
                Some((pid.trim().parse().ok()?, parse_mib(mib.trim())?))
            })
            .collect()
    }
}

/// "75010 MiB" / "[N/A]" → MiB 数字（GB10 的 memory.total 会回 [N/A]，须容忍）。
fn parse_mib(s: &str) -> Option<u64> {
    s.split_whitespace().next()?.parse().ok()
}

/// 解析 `docker inspect` 输出（JSON 数组，每个元素一个容器）。
pub fn parse_inspect(text: &str) -> Option<Vec<ContainerInfo>> {
    let v = Value::parse(text).ok()?;
    let arr = v.as_arr()?;
    let mut out = Vec::new();
    for item in arr {
        out.push(parse_inspect_one(item)?);
    }
    Some(out)
}

fn parse_inspect_one(v: &Value) -> Option<ContainerInfo> {
    let name = v
        .get("Name")
        .and_then(|x| x.as_str())
        .map(|s| s.trim_start_matches('/').to_string())?;
    let state = v.get("State")?;
    let host_config = v.get("HostConfig");
    // 端口映射：取第一个 TCP 绑定（卡片范式都是单端口服务）
    let (container_port, host_port) = host_config
        .and_then(|h| h.get("PortBindings"))
        .and_then(|p| p.as_obj())
        .and_then(|bindings| {
            bindings.iter().find_map(|(k, v)| {
                let cp: u16 = k.trim_end_matches("/tcp").parse().ok()?;
                let hp: u16 = v
                    .as_arr()?
                    .first()?
                    .get("HostPort")?
                    .as_str()?
                    .parse()
                    .ok()?;
                Some((cp, hp))
            })
        })
        .map(|(cp, hp)| (Some(cp), Some(hp)))
        .unwrap_or((None, None));
    let mut args: Vec<String> = Vec::new();
    if let Some(path) = v.get("Path").and_then(|x| x.as_str()) {
        args.push(path.to_string());
    }
    if let Some(arr) = v.get("Args").and_then(|x| x.as_arr()) {
        for a in arr {
            if let Some(s) = a.as_str() {
                args.push(s.to_string());
            }
        }
    }
    Some(ContainerInfo {
        name,
        image: v
            .get("Config")
            .and_then(|c| c.get("Image"))
            .and_then(|x| x.as_str())
            .unwrap_or_default()
            .to_string(),
        state: state
            .get("Status")
            .and_then(|x| x.as_str())
            .unwrap_or_default()
            .to_string(),
        running: state
            .get("Running")
            .and_then(|x| x.as_bool())
            .unwrap_or(false),
        exit_code: state.get("ExitCode").and_then(|x| x.as_i64()).unwrap_or(0),
        oom_killed: state
            .get("OOMKilled")
            .and_then(|x| x.as_bool())
            .unwrap_or(false),
        restart_policy: host_config
            .and_then(|h| h.get("RestartPolicy"))
            .and_then(|r| r.get("Name"))
            .and_then(|x| x.as_str())
            .unwrap_or("no")
            .to_string(),
        host_port,
        container_port,
        network_mode: host_config
            .and_then(|value| value.get("NetworkMode"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        configured_tcp_bindings: tcp_bindings(
            host_config.and_then(|value| value.get("PortBindings")),
        ),
        active_tcp_bindings: tcp_bindings(
            v.get("NetworkSettings")
                .and_then(|value| value.get("Ports")),
        ),
        pid: state
            .get("Pid")
            .and_then(|x| x.as_u64())
            .map(|p| p as u32)
            .unwrap_or(0),
        args,
        mount_sources: v
            .get("Mounts")
            .and_then(Value::as_arr)
            .into_iter()
            .flatten()
            .filter_map(|mount| {
                mount
                    .get("Source")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
            })
            .collect(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    // 真机 qwen38-sglang 的 inspect 形状（裁剪到解析关心的字段）
    const INSPECT_SAMPLE: &str = r#"[{
        "Name": "/qwen38-sglang",
        "Path": "/opt/nvidia/nvidia_entrypoint.sh",
        "Args": ["python3", "-m", "sglang.launch_server", "--served-model-name", "Qwen3.8-27B-NVFP4", "--port", "30000", "--mem-fraction-static", "0.60"],
        "State": {"Status": "running", "Running": true, "OOMKilled": false, "Pid": 1365029, "ExitCode": 0},
        "Config": {"Image": "lmsysorg/sglang:v0.5.19"},
        "HostConfig": {
            "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
            "PortBindings": {"30000/tcp": [{"HostIp": "", "HostPort": "30001"}]}
        }
    }]"#;

    #[test]
    fn parses_running_container() {
        let list = parse_inspect(INSPECT_SAMPLE).unwrap();
        assert_eq!(list.len(), 1);
        let c = &list[0];
        assert_eq!(c.name, "qwen38-sglang");
        assert_eq!(c.image, "lmsysorg/sglang:v0.5.19");
        assert_eq!(c.state, "running");
        assert!(c.running);
        assert_eq!(c.pid, 1365029);
        assert_eq!(c.restart_policy, "unless-stopped");
        assert!(c.is_autostart());
        assert_eq!(c.host_port, Some(30001));
        assert_eq!(c.container_port, Some(30000));
        assert_eq!(c.served_model_name().as_deref(), Some("Qwen3.8-27B-NVFP4"));
        assert_eq!(c.mem_fraction_static(), Some(0.60));
        assert!(!c.oom_killed);
        assert_eq!(c.exit_code, 0);
    }

    #[test]
    fn parses_stopped_container_without_ports() {
        let text = r#"[{
            "Name": "/fixture",
            "Path": "sleep",
            "Args": ["inf"],
            "State": {"Status": "exited", "Running": false, "OOMKilled": true, "Pid": 0, "ExitCode": 137},
            "Config": {"Image": "alpine:latest"},
            "HostConfig": {"RestartPolicy": {"Name": "no"}, "PortBindings": {}}
        }]"#;
        let c = parse_inspect(text).unwrap().remove(0);
        assert!(!c.running);
        assert!(!c.is_autostart());
        assert_eq!(c.host_port, None);
        assert!(c.oom_killed);
        assert_eq!(c.exit_code, 137);
    }

    #[test]
    fn parse_mib_tolerates_na() {
        assert_eq!(parse_mib("75010 MiB"), Some(75010));
        assert_eq!(parse_mib("[N/A]"), None);
    }

    #[test]
    fn arg_value_both_forms() {
        // compose.yaml 渲染的连写形式（mtp 一代卡）
        let args = vec![
            "python3".to_string(),
            "-m".to_string(),
            "sglang.launch_server".to_string(),
            "--model-path=/models/target".to_string(),
            "--host=127.0.0.1".to_string(),
            "--port=30000".to_string(),
            "--mem-fraction-static=0.70".to_string(),
        ];
        assert_eq!(
            arg_value(&args, "--mem-fraction-static").as_deref(),
            Some("0.70")
        );
        assert_eq!(arg_value(&args, "--port").as_deref(), Some("30000"));
        assert_eq!(arg_value(&args, "--host").as_deref(), Some("127.0.0.1"));
        // 空格分隔形式（sglang0519 一代）不回归
        let args2 = vec!["--mem-fraction-static".to_string(), "0.60".to_string()];
        assert_eq!(
            arg_value(&args2, "--mem-fraction-static").as_deref(),
            Some("0.60")
        );
        assert!(arg_value(&args, "--missing").is_none());
    }

    /// 真机 docker 的「容器不存在」文案是全小写 `error: no such object: x`
    /// （另一版本是 `Error: No such container`）——两种都必须按 Ok(None) 处理，
    /// 否则 enable 会把它误判成「docker 不可用」（真机实测 mtp 卡首启踩中）。
    #[test]
    fn inspect_missing_container_lowercase_no_such_object() {
        let root = fixtures::tmpdir("inspect-miss");
        let cli = root.join("docker");
        std::fs::write(
            &cli,
            "#!/bin/sh\necho 'error: no such object: nothing-here' >&2\nexit 1\n",
        )
        .unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o755)).unwrap();
        let d = Docker::new(&cli.display().to_string(), "nvidia-smi");
        let got = d.inspect("nothing-here").unwrap();
        assert!(
            got.is_none(),
            "全小写 no such object 也必须按容器不存在处理"
        );
        let _ = std::fs::remove_dir_all(&root);
    }
}

/// 测试夹具（ops/supervisor 的 W6 测试共用）：fake docker CLI（状态文件驱动）、
/// 容器卡生成、迷你 /v1/models 应答器、带 meminfo fixture 的 App。
#[cfg(test)]
pub(crate) mod fixtures {
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;

    static SEQ: AtomicU64 = AtomicU64::new(0);

    pub fn tmpdir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "cmsd-w6-{tag}-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// fake docker 脚本模板：@STATE@/@LOG@ 写入时替换（不碰进程环境变量，并行测试安全）。
    /// 状态行：name^image^running^policy^exitcode^oom^pid^hostport^containerport^args(逗号分隔)
    const FAKE_DOCKER_SH: &str = r#"#!/bin/sh
STATE="@STATE@"
LOG="@LOG@"
MOUNTS="@MOUNTS@"
echo "$*" >> "$LOG"
cmd="$1"; shift
get() { grep "^$1^" "$STATE" 2>/dev/null; }
case "$cmd" in
  ps)
    cut -d'^' -f1 "$STATE" 2>/dev/null
    ;;
  inspect)
    printf '['; sep=""; rc=0
    for name in "$@"; do
      line=$(get "$name")
      if [ -z "$line" ]; then echo "Error: No such container: $name" >&2; rc=1; continue; fi
      printf '%s' "$sep"; sep=","
      mount=$(grep "^$name^" "$MOUNTS" 2>/dev/null | cut -d'^' -f2-)
      echo "$line" | awk -F'^' -v mount="$mount" '{
        status = ($3=="true") ? "running" : "exited";
        n = split($10, a, ",");
        args = "[";
        for (i = 1; i <= n; i++) { if (a[i] != "") args = args (args == "[" ? "" : ", ") "\"" a[i] "\""; }
        args = args "]";
        network = ($8 == "" && $9 == "0") ? "host" : "bridge";
        pb = ($8 != "") ? sprintf("\"%s/tcp\":[{\"HostIp\":\"\",\"HostPort\":\"%s\"}]", $9, $8) : "";
        printf "{\"Name\":\"/%s\",\"Path\":\"entry\",\"Args\":%s,\"State\":{\"Status\":\"%s\",\"Running\":%s,\"OOMKilled\":%s,\"Pid\":%s,\"ExitCode\":%s},\"Config\":{\"Image\":\"%s\"},\"Mounts\":[{\"Source\":\"%s\"}],\"HostConfig\":{\"RestartPolicy\":{\"Name\":\"%s\"},\"NetworkMode\":\"%s\",\"PortBindings\":{%s}},\"NetworkSettings\":{\"Ports\":{%s}}}", $1, args, status, $3, $6, $7, $5, $2, mount, $4, network, pb, ($3=="true" ? pb : "")
      }'
    done
    printf ']'
    [ "$rc" = "0" ]
    ;;
  start)
    name="$1"
    get "$name" >/dev/null || { echo "Error: No such container: $name" >&2; exit 1; }
    awk -F'^' -v OFS='^' -v n="$name" '{ if ($1 == n) { $3 = "true"; $7 = "4321" } print }' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"
    ;;
  stop)
    name="$3"
    get "$name" >/dev/null || { echo "Error: No such container: $name" >&2; exit 1; }
    awk -F'^' -v OFS='^' -v n="$name" '{ if ($1 == n) { $3 = "false"; $5 = "143"; $7 = "0" } print }' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"
    ;;
  update)
    pol="${1#--restart=}"; name="$2"
    get "$name" >/dev/null || { echo "Error: No such container: $name" >&2; exit 1; }
    awk -F'^' -v OFS='^' -v n="$name" -v p="$pol" '{ if ($1 == n) { $4 = p } print }' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"
    ;;
  top)
    echo "PID"
    get "$1" | awk -F'^' '$3 == "true" { print "4321"; print "4322" }'
    ;;
  logs)
    echo "fake: 容器日志行"
    ;;
  *)
    echo "fake docker: 不认识的子命令 $cmd $*" >&2
    exit 2
    ;;
esac
"#;

    /// 写 fake docker CLI 与空状态文件，返回脚本路径（给 config.docker_cli）。
    pub fn write_fake_docker(dir: &Path) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let script = dir.join("docker");
        let text = FAKE_DOCKER_SH
            .replace("@STATE@", &dir.join("docker.state").display().to_string())
            .replace("@LOG@", &dir.join("docker.calls").display().to_string())
            .replace("@MOUNTS@", &dir.join("docker.mounts").display().to_string());
        // 写临时名 + 原子 rename：直接写目标路径后立刻 exec，偶发 ETXTBSY
        // （内核还持有上个写者/读者的引用），并行压测下能稳定复现
        let tmp = dir.join("docker.tmp");
        std::fs::write(&tmp, text).unwrap();
        std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o755)).unwrap();
        std::fs::rename(&tmp, &script).unwrap();
        std::fs::write(dir.join("docker.state"), "").unwrap();
        std::fs::write(dir.join("docker.mounts"), "").unwrap();
        script
    }

    /// 放/换一个容器状态行。
    #[allow(clippy::too_many_arguments)]
    pub fn set_container(
        dir: &Path,
        name: &str,
        image: &str,
        running: bool,
        policy: &str,
        exit_code: i64,
        oom: bool,
        pid: u32,
        host_port: u16,
        container_port: u16,
        args: &str,
    ) {
        let state = dir.join("docker.state");
        let old = std::fs::read_to_string(&state).unwrap_or_default();
        let kept: Vec<&str> = old
            .lines()
            .filter(|l| !l.starts_with(&format!("{name}^")))
            .collect();
        let hp = if host_port > 0 {
            host_port.to_string()
        } else {
            String::new()
        };
        let line = format!(
            "{name}^{image}^{running}^{policy}^{exit_code}^{oom}^{pid}^{hp}^{container_port}^{args}"
        );
        let mut text = kept.join("\n");
        if !text.is_empty() {
            text.push('\n');
        }
        text.push_str(&line);
        text.push('\n');
        std::fs::write(&state, text).unwrap();
        // Simulate the declared source mount at creation time. Later card edits
        // never rewrite this immutable container configuration.
        let mount_file = dir.join("docker.mounts");
        let mounts = std::fs::read_to_string(&mount_file).unwrap_or_default();
        if !mounts
            .lines()
            .any(|line| line.starts_with(&format!("{name}^")))
        {
            if let Some(deploy) = crate::cards::scan_cards(&dir.join("cards"))
                .into_iter()
                .filter_map(|card| card.docker_deploy)
                .find(|deploy| deploy.container_name == name)
            {
                let path = PathBuf::from(deploy.model_dir);
                if path.starts_with(dir) {
                    std::fs::create_dir_all(&path).unwrap();
                    set_model_mount(dir, name, &path);
                }
            }
        }
    }

    pub fn set_model_mount(dir: &Path, name: &str, source: &Path) {
        let path = dir.join("docker.mounts");
        let old = std::fs::read_to_string(&path).unwrap_or_default();
        let mut rows: Vec<_> = old
            .lines()
            .filter(|line| !line.starts_with(&format!("{name}^")))
            .map(str::to_owned)
            .collect();
        rows.push(format!("{name}^{}", source.display()));
        std::fs::write(path, rows.join("\n") + "\n").unwrap();
    }

    pub fn state_line(dir: &Path, name: &str) -> Option<String> {
        std::fs::read_to_string(dir.join("docker.state"))
            .ok()
            .and_then(|t| {
                t.lines()
                    .find(|l| l.starts_with(&format!("{name}^")))
                    .map(|l| l.to_string())
            })
    }

    /// fake docker 收到的全部调用（每行一条）。
    pub fn docker_calls(dir: &Path) -> Vec<String> {
        std::fs::read_to_string(dir.join("docker.calls"))
            .unwrap_or_default()
            .lines()
            .map(|l| l.to_string())
            .collect()
    }

    /// 造一张 compose 型容器卡（对齐 mtp 一代：无 common.sh；install_engine.sh 声明
    /// VENDOR_DIR 并内嵌 compose.yaml + .env.example heredoc；download_model.sh 内嵌
    /// sha256 清单；deploy.sh 把 .env.example 拷成 .env）。全部用绝对路径，不碰 $HOME。
    pub fn write_compose_card(
        cards_dir: &Path,
        name: &str,
        container: &str,
        vendor_dir: &Path,
        models_dir: &Path,
        port: u16,
        manifest_sha: &str,
    ) -> PathBuf {
        let dir = cards_dir.join(name);
        std::fs::create_dir_all(&dir).unwrap();
        let install_engine = format!(
            r#"#!/bin/sh
VENDOR_DIR="{vendor}"
if [ "$LMM_PROBE_ONLY" = "1" ]; then
    [ -f "$VENDOR_DIR/.engine-ready" ] && {{ echo "probe: 引擎已就绪"; exit 0; }}
    echo "probe: 引擎尚未准备" >&2
    exit 1
fi
mkdir -p "$VENDOR_DIR/profiles/benchmark" "$VENDOR_DIR/scripts"
cat > "$VENDOR_DIR/profiles/benchmark/compose.yaml" <<'YAML'
services:
  qwen38:
    image: ${{IMAGE}}
    container_name: ${{CONTAINER_NAME}}
    network_mode: host
    restart: unless-stopped
YAML
cat > "$VENDOR_DIR/profiles/benchmark/.env.example" <<'ENV'
IMAGE=fixture/sglang:v0.5.16
CONTAINER_NAME={container}
MODEL_PATH={models}/Qwen3.8-27B-NVFP4
CACHE_PATH={vendor}/cache
HOST_BIND=127.0.0.1
PORT={port}
MEM_FRACTION_STATIC=0.70
ENV
touch "$VENDOR_DIR/.engine-ready"
echo "引擎部署包已就绪"
"#,
            vendor = vendor_dir.display(),
            container = container,
            models = models_dir.display(),
            port = port
        );
        let download_model = format!(
            r#"#!/bin/sh
set -e
LMM_WEIGHTS_MANIFEST_SHA="{sha}"
lmm_write_manifest() {{
    cat > "$1" <<'LMM_WEIGHTS_EOF'
fbcdb5ba1cdda462b5f38592d071e772c4d398afea61a0aa9188b32d1a239a79  model-00001-of-00003.safetensors
db6146a5464fb0a891181b93c81593f0ca65c602eb14120a1c2b1b09bca11f85  model-00002-of-00003.safetensors
d3cfb92742e30c8b46564665791dbe0a86ed64cfc02b1275081530793c0c9581  model-00003-of-00003.safetensors
0997f410c57a1f4e53b09e4be8f4a172d90edd9564368fb0847030937229b9f3  tokenizer.json
LMM_WEIGHTS_EOF
}}
MODEL_PATH="{models}/Qwen3.8-27B-NVFP4"
mkdir -p "$MODEL_PATH"
echo "下载权重（断点续传）…"
head -c 16 /dev/zero > "$MODEL_PATH/model-00001-of-00003.safetensors"
echo "LMM_PROGRESS_BYTES=16/40"
head -c 8 /dev/zero > "$MODEL_PATH/model-00002-of-00003.safetensors"
head -c 8 /dev/zero > "$MODEL_PATH/model-00003-of-00003.safetensors"
echo '{{}}' > "$MODEL_PATH/config.json"
printf 'x' > "$MODEL_PATH/tokenizer.json"
echo "LMM_PROGRESS_BYTES=40/40"
printf '%s' "{sha}" > "$MODEL_PATH/.lmm-weights-verified"
echo "权重下载完成并校验通过"
"#,
            sha = manifest_sha,
            models = models_dir.display()
        );
        let deploy = format!(
            r#"#!/bin/sh
VENDOR_DIR="{vendor}"
cp "$VENDOR_DIR/profiles/benchmark/.env.example" "$VENDOR_DIR/.env"
echo "部署完成，可以点「启用」。"
"#,
            vendor = vendor_dir.display()
        );
        let enable = format!(
            r#"#!/bin/sh
PORT="$(grep '^PORT=' "{vendor}/.env" | cut -d= -f2)"
echo "LMM_ENDPOINT_URL=http://127.0.0.1:${{PORT}}/v1/chat/completions"
"#,
            vendor = vendor_dir.display()
        );
        for (fname, body) in [
            ("install_engine.sh", install_engine),
            ("download_model.sh", download_model),
            ("deploy.sh", deploy),
            ("enable.sh", enable),
            ("stop.sh", "#!/bin/sh\nexit 0\n".to_string()),
        ] {
            use std::os::unix::fs::PermissionsExt;
            let p = dir.join(fname);
            std::fs::write(&p, body).unwrap();
            std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        dir
    }

    /// 造一张容器卡（对齐 sglang 卡形状：common.sh + vendor/{.deploy-env,start.sh}）。
    pub fn write_docker_card(
        cards_dir: &Path,
        name: &str,
        container: &str,
        port: u16,
        mem_fraction: Option<&str>,
    ) -> PathBuf {
        let dir = cards_dir.join(name);
        std::fs::create_dir_all(dir.join("vendor")).unwrap();
        std::fs::write(
            dir.join("common.sh"),
            format!(
                "IMAGE=\"fixture/img:1.0\"\nCONTAINER_NAME=\"{container}\"\nexport PORT=\"${{PORT:-{port}}}\"\n"
            ),
        )
        .unwrap();
        std::fs::write(
            dir.join("vendor/.deploy-env"),
            format!(
                "IMAGE=\"fixture/img:1.0\"\nCONTAINER_NAME=\"{container}\"\nPORT=\"{port}\"\nMODEL_DIR=\"{}\"\n", dir.join("weights").display()
            ),
        )
        .unwrap();
        let fraction_line = mem_fraction
            .map(|f| format!("MEM_FRACTION_STATIC=\"${{MEM_FRACTION_STATIC:-{f}}}\"\n"))
            .unwrap_or_default();
        std::fs::write(dir.join("vendor/start.sh"), fraction_line).unwrap();
        dir
    }

    /// 迷你 /v1/models 应答器（真 TCP，健康探测走真 HTTP）：返回监听端口。
    pub fn serve_models_json(model_id: &str) -> u16 {
        serve_json(&format!("{{\"data\":[{{\"id\":\"{model_id}\"}}]}}"))
    }

    #[test]
    fn fixture_health_response_waits_for_complete_request_headers() {
        use std::io::{Read, Write};
        let port = serve_models_json("test-model");
        let mut stream = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
        stream
            .set_read_timeout(Some(std::time::Duration::from_millis(50)))
            .unwrap();
        stream.write_all(b"GET /v1/models HTTP/1.1\r\n").unwrap();
        let mut byte = [0];
        assert!(
            stream.read(&mut byte).is_err_and(|e| matches!(
                e.kind(),
                std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock
            )),
            "fixture replied and closed with unread HTTP headers"
        );
        stream
            .write_all(b"Host: local\r\nConnection: close\r\n\r\n")
            .unwrap();
        stream
            .set_read_timeout(Some(std::time::Duration::from_secs(1)))
            .unwrap();
        let mut response = Vec::new();
        stream.read_to_end(&mut response).unwrap();
        assert!(response.starts_with(b"HTTP/1.1 200 OK\r\n"));
    }

    /// /v1/models 应答器（带 max_model_len——健康探测的窗口事实）。
    pub fn serve_models_json_with_len(model_id: &str, max_model_len: u64) -> u16 {
        serve_json(&format!(
            "{{\"data\":[{{\"id\":\"{model_id}\",\"max_model_len\":{max_model_len}}}]}}"
        ))
    }

    fn serve_json(body: &str) -> u16 {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let body = body.to_string();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut s) = stream else { continue };
                use std::io::Write;
                let _ = s.set_read_timeout(Some(std::time::Duration::from_secs(2)));
                // A real client may send formatted headers in several writes.
                // Closing after a single read creates an unread-data RST and
                // falsely reports a healthy test service as unavailable.
                let complete = {
                    let mut reader = std::io::BufReader::new(&mut s);
                    matches!(crate::http::read_request(&mut reader), Ok(Some(_)))
                };
                if !complete {
                    continue;
                }
                let resp = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = s.write_all(resp.as_bytes());
            }
        });
        port
    }

    /// 造 App：临时目录全隔离 + fake docker + meminfo fixture（GiB 可指定）。
    pub fn make_app(root: &Path, mem_total_gib: u64, mem_avail_gib: u64) -> Arc<crate::app::App> {
        let meminfo = root.join("meminfo");
        std::fs::write(
            &meminfo,
            format!(
                "MemTotal:       {} kB\nMemFree:         {} kB\nMemAvailable:   {} kB\n",
                mem_total_gib * 1048576,
                mem_avail_gib * 1048576,
                mem_avail_gib * 1048576
            ),
        )
        .unwrap();
        let docker_cli = write_fake_docker(root);
        let cfg = crate::config::Config {
            socket_path: root.join("cmsd.sock"),
            state_dir: root.join("state"),
            gateway_listen: "127.0.0.1:0".to_string(),
            systemd_mode: crate::config::SystemdMode::User,
            native_unit_namespace: "legacy".into(),
            cards_dir: root.join("cards"),
            engine_root: root.join("engines"),
            unit_dir: root.join("units"),
            pool_start: 18080,
            pool_end: 18099,
            proc_root: PathBuf::from("/proc"),
            meminfo_path: meminfo,
            debug_available_mib: None,
            ready_timeout_secs: 1500,
            models_dir: root.join("models"),
            staging_dir: root.join("staging"),
            hf_endpoint: "https://hf-mirror.example".to_string(),
            docker_cli: docker_cli.display().to_string(),
            // 不存在：gpu_procs 查询失败按空归属降级（本组测试不碰显存）
            nvidia_smi_cli: root.join("nvidia-smi").display().to_string(),
            store_socket_path: root.join("store.sock"),
            store_poll_ms: 50,
            store_inventory_program: None,
            store_inventory_profile: None,
        };
        let store = crate::store::StateStore::load(&cfg.state_file());
        let systemd = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".to_string(),
        );
        systemd.write_template().unwrap();
        Arc::new(crate::app::App::new(cfg, store, systemd))
    }

    /// 等任务终结并取回记录（与 install 测试同款小轮子）。
    pub fn wait_terminal(
        app: &Arc<crate::app::App>,
        task_id: &str,
        timeout_secs: u64,
    ) -> crate::store::TaskRecord {
        let deadline = crate::timeutil::now_epoch() + timeout_secs;
        loop {
            {
                let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
                if let Some(t) = guard.state.tasks.get(task_id) {
                    if t.status.is_terminal() {
                        return t.clone();
                    }
                }
            }
            assert!(
                crate::timeutil::now_epoch() < deadline,
                "任务 {task_id} 在 {timeout_secs}s 内没有终结"
            );
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
    }
}
