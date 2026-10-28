//! 双机部署里跟具体卡片无关的"从机控制"能力。
//!
//! （2026-10-27 自旧 LMM `core/worker.rs` 移植，为双机卡的部署步骤提供
//! `$LMM_WORKER`。适配三处：config 路径换成品悟模型商店的根目录；密码持久化
//! 从 keyring 直依赖换成 codewhale_secrets::DefaultKeyringStore（service 与
//! slot 命名不变，LMM 时期存的密码可续读）；二进制入口从
//! `pinvou3-tauri model-store-worker` 换成 `pinvou3-tauri model-store-worker`。
//! 其余逐行保留——注释里的真机坑（askpass 主机密钥确认、无 tty 的 sudo、
//! 远端 PATH 引号、管理网流量带偏网口检测）一个都不能丢。）
//!
//! 为什么放进 LMM 而不是留在卡片脚本里：连从机、投公钥、远程提权、推文件、测直连
//! 网口——这些事每一张双机卡都要做，而且每一步都踩过真机上的坑（ssh-copy-id 的
//! 主机密钥确认会吃掉 askpass 的密码、无 tty 时 sudo 不会自己去用 SUDO_ASKPASS、
//! 远端 PATH 引号写错整个系统 PATH 就废了、管理网卡的 SSH 流量把网口检测带偏……）。
//! 让每张卡各抄一遍，就是让每张卡各踩一遍。只有"跑交付包自己的哪条命令"
//! （init --role worker、pair、deploy）才跟具体卡片有关，那部分留在卡片里。
//!
//! 形态：LMM 给双机卡片的脚本注入 `LMM_WORKER`——就是 LMM 自己的二进制（见
//! main.rs），卡片脚本调 `"$LMM_WORKER" <子命令>`，见 [`cli`]。
//!
//! **连接是按需的**：卡片确认要引导从机时才调 `connect`，而不是 LMM 在每次部署前都
//! 先连一遍。已经配对好的集群部署时只走直连线，根本用不到管理网——真机上遇到过
//! 主机的有线管理网掉了、只剩一个连不到从机的访客 WiFi，这时如果部署前硬要连管理网，
//! 本来零输入就能完成的部署也会被卡死。
//!
//! `connect` 要问用户时，走的是跟卡片脚本完全相同的 `LMM_PROMPT_FORM=` 通道：我们
//! 就跑在脚本的 pty 里，标记打到 stderr、答案从 stdin 读回来。

use std::fs;
use std::io::{BufRead, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use super::config;

/// 交给卡片脚本的环境变量：LMM 自己的二进制路径。
pub const ENV_CLI: &str = "LMM_WORKER";
/// 可选覆盖：设置了就用这里的从机，不读保存的。手动调试时用。
pub const ENV_HOST: &str = "LMM_WORKER_HOST";
pub const ENV_USER: &str = "LMM_WORKER_USER";
/// 可选覆盖：存放从机密码的 0600 文件。手动调试时用（SSH 登进设备时没有桌面会话，
/// 系统密钥环打不开）。
pub const ENV_SECRET: &str = "LMM_WORKER_SECRET";

const KEYRING_SERVICE: &str = "local-model-manager";
/// 表单最多重填几轮。填错地址、填错密码都会回到表单，但不能无限循环下去。
const MAX_ROUNDS: usize = 5;

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct WorkerAccess {
    /// 从机的**管理网**地址（WiFi / 普通有线网），不是直连线的地址——直连线要靠
    /// 从机上的交付包配起来，配之前根本不通。
    pub host: String,
    pub user: String,
}

impl WorkerAccess {
    pub fn target(&self) -> String {
        format!("{}@{}", self.user, self.host)
    }
}

#[derive(Debug)]
pub enum WorkerError {
    /// 管理地址连不上：多半是从机换了网络、DHCP 重新分了地址——让用户重填地址
    Unreachable(String),
    /// 密码不对（从机改过密码），或者这个用户没有 sudo 权限——让用户重填密码
    AuthFailed(String),
    /// 其它：缺环境、docker 组补不上……重填表单也没用，直接报出来
    Other(String),
}

// ---------------------------------------------------------------- 持久化

fn access_file() -> PathBuf {
    config::config_dir().join("worker.json")
}

/// 上次连通过的从机。只存地址和用户名（非敏感），密码走系统密钥环。
pub fn load_access() -> Option<WorkerAccess> {
    let text = fs::read_to_string(access_file()).ok()?;
    serde_json::from_str::<WorkerAccess>(&text)
        .ok()
        .filter(|a| !a.host.is_empty() && !a.user.is_empty())
}

pub fn save_access(a: &WorkerAccess) -> std::io::Result<()> {
    let json = serde_json::to_string_pretty(a).unwrap_or_default();
    fs::write(access_file(), json)
}

fn keyring_user(a: &WorkerAccess) -> String {
    format!("worker:{}", a.target())
}

/// 密钥环后端：与 HF Token 同一决策——经底座 `codewhale_secrets`，不直接依赖
/// `keyring`（见 config.rs 的长注释）。service 沿用 LMM 时期的
/// `local-model-manager`，LMM 时期已保存的从机密码无感续读。
fn keyring_store() -> codewhale_secrets::DefaultKeyringStore {
    codewhale_secrets::DefaultKeyringStore::new(KEYRING_SERVICE)
}

fn password_fallback_file(a: &WorkerAccess) -> PathBuf {
    let safe: String = a
        .target()
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '.' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect();
    config::config_dir().join(format!("worker-{safe}.secret"))
}

/// 读已保存的从机密码。跟 HF Token 一样：优先系统密钥环，不可用时退化为 0600 文件。
///
/// 为什么要存：用户明确要求"第二次部署零输入"，而配从机网络、补 docker 组都要
/// 从机的 sudo 密码。从机改了密码时 `connect` 会发现并请用户重填。
pub fn load_password(a: &WorkerAccess) -> Option<String> {
    use codewhale_secrets::KeyringStore as _;
    if let Ok(Some(pw)) = keyring_store().get(&keyring_user(a)) {
        if !pw.is_empty() {
            return Some(pw);
        }
    }
    fs::read_to_string(password_fallback_file(a))
        .ok()
        .map(|s| s.trim_end_matches(['\n', '\r']).to_string())
        .filter(|s| !s.is_empty())
}

pub fn save_password(a: &WorkerAccess, password: &str) {
    use codewhale_secrets::KeyringStore as _;
    if keyring_store().set(&keyring_user(a), password).is_ok() {
        let _ = fs::remove_file(password_fallback_file(a));
        return;
    }
    let f = password_fallback_file(a);
    if let Ok(mut file) = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&f)
    {
        let _ = file.write_all(password.as_bytes());
    }
    let _ = fs::set_permissions(&f, fs::Permissions::from_mode(0o600));
}

pub fn forget_password(a: &WorkerAccess) {
    use codewhale_secrets::KeyringStore as _;
    let _ = keyring_store().delete(&keyring_user(a));
    let _ = fs::remove_file(password_fallback_file(a));
}

/// 交给双机卡片脚本的环境变量：`LMM_WORKER` 指向一个 wrapper 脚本。
///
/// 卡片脚本按老 LMM 契约调 `"$LMM_WORKER" <子命令>`（quoted 调用，deploy.sh
/// 里 9 处）；本体的分发入口是 `pinvou3-tauri model-store-worker <子命令>`
/// ——中间差一层子命令。直接注入 exe 路径的话，`"$LMM_WORKER" connect` 会
/// 变成「以 connect 为首参启动整个 GUI」（.10 真机实锤：单实例插件让它静默
/// exit 0，info 输出空串，init 拦下「非法 worker 用户名」——10-28 双机验证
/// P0）。这里生成 `exec <exe> model-store-worker "$@"` 的 wrapper，卡片的
/// quoted 调用契约原样保留，一行都不用改。每次部署重写一遍（exe 可能随
/// 升级换路径），幂等。
pub fn script_env() -> Vec<(String, String)> {
    use std::io::Write as _;
    // 拿不到自身路径时不注入——deploy.sh 对 $LMM_WORKER 缺失有明确的
    // 「请升级 LMM」报错，比注入一个会起 GUI 的假路径好。
    let Ok(exe) = std::env::current_exe() else {
        return Vec::new();
    };
    let dir = config::config_dir().join("bin");
    if fs::create_dir_all(&dir).is_err() {
        return Vec::new();
    }
    let wrapper = dir.join("pinvou3-model-store-worker");
    let script = format!(
        "#!/bin/sh\nexec {} model-store-worker \"$@\"\n",
        shell_quote(&exe.to_string_lossy())
    );
    let written = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o755)
        .open(&wrapper)
        .and_then(|mut f| f.write_all(script.as_bytes()))
        .and_then(|_| fs::set_permissions(&wrapper, fs::Permissions::from_mode(0o755)));
    match written {
        Ok(()) => vec![(ENV_CLI.into(), wrapper.to_string_lossy().to_string())],
        Err(_) => Vec::new(),
    }
}

// ---------------------------------------------------------------- 基础动作

/// 把任意字符串变成远端 shell 里的一个单引号字面量。
pub fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

fn home() -> PathBuf {
    std::env::var("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/"))
}

const SSH_OPTS: &[&str] = &[
    "-o",
    "BatchMode=yes",
    "-o",
    "ConnectTimeout=8",
    "-o",
    "ServerAliveInterval=15",
    "-o",
    "ServerAliveCountMax=3",
];

/// `ssh <从机> <远端命令>`。远端命令是一整串交给远端 shell 解释的文本。
fn ssh(a: &WorkerAccess, remote_cmd: &str) -> Command {
    let mut c = Command::new("ssh");
    c.args(SSH_OPTS).arg(a.target()).arg(remote_cmd);
    c
}

/// 跑一条远端命令，stdin 喂进去（可为空），返回 (成功?, stdout, stderr)。
fn ssh_capture(a: &WorkerAccess, remote_cmd: &str, stdin: Option<&str>) -> (bool, String, String) {
    let mut c = ssh(a, remote_cmd);
    c.stdin(if stdin.is_some() {
        Stdio::piped()
    } else {
        Stdio::null()
    })
    .stdout(Stdio::piped())
    .stderr(Stdio::piped());
    let Ok(mut child) = c.spawn() else {
        return (false, String::new(), "无法启动 ssh".to_string());
    };
    if let (Some(data), Some(mut pipe)) = (stdin, child.stdin.take()) {
        let _ = pipe.write_all(data.as_bytes());
        // pipe 在这里 drop，远端读到 EOF
    }
    match child.wait_with_output() {
        Ok(o) => (
            o.status.success(),
            String::from_utf8_lossy(&o.stdout).to_string(),
            String::from_utf8_lossy(&o.stderr).to_string(),
        ),
        Err(e) => (false, String::new(), e.to_string()),
    }
}

/// 先把从机的主机密钥收进 known_hosts（TOFU）。
///
/// 不先收的话，ssh-copy-id 第一次连会问 "Are you sure you want to continue
/// connecting?"——而在 SSH_ASKPASS_REQUIRE=force 之下这个问题也走 askpass，拿到的
/// 是密码而不是 "yes"，直接失败。
fn trust_host_key(host: &str) -> Result<(), WorkerError> {
    let ssh_dir = home().join(".ssh");
    let _ = fs::create_dir_all(&ssh_dir);
    let _ = fs::set_permissions(&ssh_dir, fs::Permissions::from_mode(0o700));
    let kh = ssh_dir.join("known_hosts");
    if !kh.exists() {
        let _ = fs::write(&kh, "");
    }
    let _ = fs::set_permissions(&kh, fs::Permissions::from_mode(0o600));

    let known = Command::new("ssh-keygen")
        .args(["-F", host])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|s| s.success());
    if known {
        return Ok(());
    }
    let out = Command::new("ssh-keyscan")
        .args(["-T", "10", host])
        .stderr(Stdio::null())
        .output();
    match out {
        Ok(o) if o.status.success() && !o.stdout.is_empty() => {
            let mut f = fs::OpenOptions::new()
                .append(true)
                .open(&kh)
                .map_err(|e| WorkerError::Other(format!("写入 known_hosts 失败：{e}")))?;
            f.write_all(&o.stdout)
                .map_err(|e| WorkerError::Other(format!("写入 known_hosts 失败：{e}")))?;
            Ok(())
        }
        _ => Err(WorkerError::Unreachable(unreachable_hint(host))),
    }
}

fn unreachable_hint(host: &str) -> String {
    format!(
        "连不上从机的管理地址 {host}。请确认：① 这是从机的 WiFi / 普通有线网地址（不是直连线的地址）；\
         ② 两台机器的管理网是通的（本机只连着访客 WiFi 之类的隔离网络时到不了从机）；\
         ③ 从机的地址有没有变（换了网络、DHCP 重新分配）"
    )
}

/// 免密登录现在通不通。主机密钥对不上（从机重装过系统）单独报，否则会被当成
/// "密码错"让用户反复重填；已知主机但连不上的，按"地址不可达"处理。
fn key_login(a: &WorkerAccess) -> Result<bool, WorkerError> {
    let (ok, _, err) = ssh_capture(a, "true", None);
    if ok {
        return Ok(true);
    }
    if err.contains("REMOTE HOST IDENTIFICATION HAS CHANGED")
        || err.contains("Host key verification failed")
    {
        return Err(WorkerError::Other(format!(
            "从机 {} 的主机密钥变了（重装过系统，或者这个地址换了一台机器）。确认是同一台从机后，\
             在本机执行 ssh-keygen -R {} 再重新部署",
            a.host, a.host
        )));
    }
    if err.contains("Connection timed out")
        || err.contains("No route to host")
        || err.contains("Connection refused")
        || err.contains("Could not resolve")
        || err.contains("Network is unreachable")
    {
        return Err(WorkerError::Unreachable(unreachable_hint(&a.host)));
    }
    Ok(false)
}

/// 用一次密码把公钥投过去，之后全走密钥。
///
/// 密码通过 SSH_ASKPASS 交给 ssh-copy-id：写进 0600 临时文件、由 0700 助手 cat 出来，
/// 不出现在命令行、环境变量和日志里。不用 sshpass——它不一定装了，SSH_ASKPASS 是
/// OpenSSH 自带的（SSH_ASKPASS_REQUIRE=force 要 8.4+，Ubuntu 22.04 是 8.9）。
fn install_key(a: &WorkerAccess, password: &str) -> Result<(), WorkerError> {
    let key = home().join(".ssh/id_ed25519");
    if !key.exists() {
        let ok = Command::new("ssh-keygen")
            .args(["-t", "ed25519", "-N", "", "-q", "-f"])
            .arg(&key)
            .stdin(Stdio::null())
            .status()
            .is_ok_and(|s| s.success());
        if !ok {
            return Err(WorkerError::Other("生成本机 SSH 密钥失败".into()));
        }
    }
    let dir =
        tempfile::tempdir().map_err(|e| WorkerError::Other(format!("创建临时目录失败：{e}")))?;
    let pw_file = dir.path().join("pw");
    let helper = dir.path().join("askpass.sh");
    fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&pw_file)
        .and_then(|mut f| f.write_all(format!("{password}\n").as_bytes()))
        .map_err(|e| WorkerError::Other(format!("写临时文件失败：{e}")))?;
    fs::write(
        &helper,
        format!(
            "#!/bin/sh\ncat {}\n",
            shell_quote(&pw_file.to_string_lossy())
        ),
    )
    .and_then(|_| fs::set_permissions(&helper, fs::Permissions::from_mode(0o700)))
    .map_err(|e| WorkerError::Other(format!("写临时文件失败：{e}")))?;

    let _ = Command::new("ssh-copy-id")
        .args([
            "-o",
            "StrictHostKeyChecking=no",
            "-o",
            "ConnectTimeout=8",
            "-i",
        ])
        .arg(home().join(".ssh/id_ed25519.pub"))
        .arg(a.target())
        .env("SSH_ASKPASS", &helper)
        .env("SSH_ASKPASS_REQUIRE", "force")
        .env(
            "DISPLAY",
            std::env::var("DISPLAY").unwrap_or_else(|_| ":0".into()),
        )
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    drop(dir);

    if key_login(a)? {
        Ok(())
    } else {
        Err(WorkerError::AuthFailed(format!(
            "无法登录从机 {}：用户名或密码不正确（从机密码可能改过了）。\
             也可能是从机关闭了密码登录（sshd 的 PasswordAuthentication）",
            a.target()
        )))
    }
}

/// 核对密码能不能 sudo。配从机网络、补 docker 组都要它；这里不核对，等到交付包
/// 内部那几条 sudo nmcli 才失败，报错会难懂得多。
fn verify_sudo(a: &WorkerAccess, password: &str) -> Result<(), WorkerError> {
    let (ok, _, _) = ssh_capture(a, "sudo -S -p '' -v", Some(&format!("{password}\n")));
    if ok {
        Ok(())
    } else {
        Err(WorkerError::AuthFailed(format!(
            "从机 {} 的密码不正确（可能改过了），或者该用户没有 sudo 权限",
            a.target()
        )))
    }
}

/// 所有双机卡都离不开的基础命令。卡片特有的（nmcli、RDMA 驱动……）由卡片自己查。
const BASE_COMMANDS: &[&str] = &["docker", "rsync", "python3", "ip", "ping"];

fn check_basics(a: &WorkerAccess) -> Result<(), WorkerError> {
    let script = format!(
        "miss=''; for c in {}; do command -v \"$c\" >/dev/null 2>&1 || miss=\"$miss $c\"; done; printf '%s' \"$miss\"",
        BASE_COMMANDS.join(" ")
    );
    let (ok, out, err) = ssh_capture(a, &script, None);
    if !ok {
        return Err(WorkerError::Other(format!(
            "检查从机环境失败：{}",
            err.trim()
        )));
    }
    let missing = out.trim();
    if missing.is_empty() {
        Ok(())
    } else {
        Err(WorkerError::Other(format!(
            "从机缺少以下基础环境，装好之后再重试：{missing}"
        )))
    }
}

/// 从机登录用户要能以普通用户身份用 docker（交付包的 preflight 明确拒绝 root）。
/// 真机上缺过。手上正好有 sudo 密码，就顺手补上——但要在日志里说清楚，加入 docker
/// 组基本等于授予 root 权限，这不是可以悄悄做的事。
fn ensure_docker_group(
    a: &WorkerAccess,
    password: &str,
    log: &mut dyn FnMut(&str),
) -> Result<(), WorkerError> {
    if ssh_capture(a, "docker info >/dev/null 2>&1", None).0 {
        return Ok(());
    }
    log(&format!(
        "      从机的 {} 不在 docker 组，正在把它加入（这相当于授予该用户 root 级别的权限）…",
        a.user
    ));
    // 不加 -tt：那样密码会被远端伪终端回显回来、原样打进日志。
    let cmd = format!("sudo -S -p '' usermod -aG docker {}", shell_quote(&a.user));
    let _ = ssh_capture(a, &cmd, Some(&format!("{password}\n")));
    // 组成员身份要新开一次登录会话才生效——下面这次 ssh 就是新会话
    if ssh_capture(a, "docker info >/dev/null 2>&1", None).0 {
        log("      已加入 docker 组");
        Ok(())
    } else {
        Err(WorkerError::Other(format!(
            "从机的 {} 仍然无法使用 docker。请在从机上执行 sudo usermod -aG docker {} 后重试",
            a.user, a.user
        )))
    }
}

/// 把从机连到"卡片脚本可以直接用"的状态。每一步先查后做，重复调用是廉价的。
pub fn connect(
    a: &WorkerAccess,
    password: &str,
    log: &mut dyn FnMut(&str),
) -> Result<(), WorkerError> {
    log(&format!("[连接从机 1/4] 确认管理地址 {} 可达", a.host));
    trust_host_key(&a.host)?;
    log("[连接从机 2/4] 建立免密登录");
    if !key_login(a)? {
        install_key(a, password)?;
    }
    log("[连接从机 3/4] 核对从机管理员权限");
    verify_sudo(a, password)?;
    log("[连接从机 4/4] 检查从机基础环境");
    check_basics(a)?;
    ensure_docker_group(a, password, log)?;
    log(&format!("从机 {} 已就绪", a.target()));
    Ok(())
}

// ---------------------------------------------------------------- 表单

fn access_form(
    prev: Option<&WorkerAccess>,
    message: Option<&str>,
    has_pending_password: bool,
) -> String {
    let default_user = std::env::var("USER").unwrap_or_default();
    let pw_hint = if has_pending_password {
        "留空则沿用刚才填的密码"
    } else {
        "用于建立免密登录和配置从机网络。会保存在本机的系统密钥环里，从机改密码后会提示重新输入"
    };
    let mut spec = serde_json::json!({
        "title": "双机部署：连接从机",
        "fields": [
            {"id": "worker_host", "label": "从机管理 IP", "kind": "text",
             "value": prev.map(|a| a.host.clone()).unwrap_or_default(),
             "placeholder": "192.168.10.42",
             "hint": "从机的 WiFi 或普通有线网地址，不是直连线的地址——直连线要靠这一步才配得起来"},
            {"id": "worker_user", "label": "从机登录用户名", "kind": "text",
             "value": prev.map(|a| a.user.clone()).unwrap_or(default_user)},
            // 密码字段刻意不带 value：界面拿不到已保存的密码，也就不会把它显示或回传
            {"id": "worker_pw", "label": "从机登录密码", "kind": "password", "hint": pw_hint,
             "required": !has_pending_password},
        ],
    });
    if let Some(m) = message {
        spec["message"] = serde_json::Value::String(m.to_string());
    }
    spec.to_string()
}

fn parse_answer(
    answer: &str,
    pending_password: Option<&str>,
) -> Result<(WorkerAccess, String), String> {
    let v: serde_json::Value =
        serde_json::from_str(answer).map_err(|_| "表单答案格式不对".to_string())?;
    let get = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or("").to_string();
    let host = get("worker_host").trim().to_string();
    let user = get("worker_user").trim().to_string();
    let mut pw = get("worker_pw");
    if pw.is_empty() {
        pw = pending_password.unwrap_or("").to_string();
    }
    if host.is_empty() || user.is_empty() || pw.is_empty() {
        return Err("从机地址、用户名、密码都要填".to_string());
    }
    Ok((WorkerAccess { host, user }, pw))
}

// ---------------------------------------------------------------- 卡片脚本用的命令行

const USAGE: &str = "用法（LMM 部署双机卡片时注入 $LMM_WORKER，卡片脚本里调用）：
  $LMM_WORKER connect                      连接从机：用保存的信息，连不上时请用户填写（第一次必然会问）
  $LMM_WORKER info                         打印已连接的从机 <用户>@<地址>
  $LMM_WORKER exec <远端命令>              在从机上执行（走管理网、免密）
  $LMM_WORKER sudo <远端命令>              同上，但命令里的每一处 sudo 都能自动拿到密码
  $LMM_WORKER push <本地目录> <从机目录> [--exclude <模式>]...
  $LMM_WORKER detect-link <本机直连网卡> <从机直连地址> [从机候选网卡...]
  $LMM_WORKER diagnose-link <本机直连网卡> <从机直连地址>";

/// 这次要操作哪台从机：环境变量优先（手动调试时用），否则用上次 connect 成功保存的。
fn resolve_access() -> Option<WorkerAccess> {
    let host = std::env::var(ENV_HOST).ok().filter(|s| !s.is_empty());
    let user = std::env::var(ENV_USER).ok().filter(|s| !s.is_empty());
    match (host, user) {
        (Some(host), Some(user)) => Some(WorkerAccess { host, user }),
        _ => load_access(),
    }
}

fn exit_code(status: std::io::Result<std::process::ExitStatus>) -> i32 {
    match status {
        Ok(s) => s.code().unwrap_or(1),
        Err(e) => {
            eprintln!("ERROR: {e}");
            1
        }
    }
}

/// `pinvou3-tauri model-store-worker <子命令> ...` 的入口，返回进程退出码。
pub fn cli(args: &[String]) -> i32 {
    let Some(sub) = args.first().map(|s| s.as_str()) else {
        eprintln!("{USAGE}");
        return 2;
    };
    let rest = &args[1..];
    match sub {
        "connect" => return cli_connect(),
        // 纯本机检查，不需要从机信息
        "diagnose-link" => {
            return match (rest.first(), rest.get(1)) {
                (Some(iface), Some(peer)) => {
                    eprint!("{}", diagnose_link(iface, peer));
                    0
                }
                _ => {
                    eprintln!("{USAGE}");
                    2
                }
            };
        }
        _ => {}
    }
    let Some(a) = resolve_access() else {
        eprintln!("ERROR: 还没有连接过从机。请先调用 \"$LMM_WORKER\" connect");
        return 2;
    };
    match sub {
        "info" => {
            println!("{}", a.target());
            0
        }
        "exec" if !rest.is_empty() => exit_code(ssh(&a, &rest.join(" ")).status()),
        "sudo" if !rest.is_empty() => cli_sudo(&a, &rest.join(" ")),
        "push" if rest.len() >= 2 => cli_push(&a, rest),
        "detect-link" if rest.len() >= 2 => cli_detect_link(&a, &rest[0], &rest[1], &rest[2..]),
        _ => {
            eprintln!("{USAGE}");
            2
        }
    }
}

fn read_answer_line() -> Option<String> {
    let mut line = String::new();
    match std::io::stdin().lock().read_line(&mut line) {
        Ok(0) | Err(_) => None,
        Ok(_) => Some(line.trim_end_matches(['\n', '\r']).to_string()),
    }
}

/// 连接从机。第一次会弹一次表单；之后用保存的信息零输入。地址连不上（换了网络）、
/// 密码失效（从机改了密码）都回到表单并说明原因，而不是直接失败。
fn cli_connect() -> i32 {
    let mut access = load_access();
    let mut password = access.as_ref().and_then(load_password);
    let mut message: Option<String> = None;
    let mut log = |l: &str| println!("{l}");

    for _ in 0..MAX_ROUNDS {
        let (a, pw) = match (&access, &password, &message) {
            (Some(a), Some(pw), None) => {
                log(&format!("使用已保存的从机信息：{}", a.target()));
                (a.clone(), pw.clone())
            }
            _ => {
                let spec = access_form(access.as_ref(), message.as_deref(), password.is_some());
                // 标记走 stderr：卡片可能用 $( ) 调我们，stdout 会被捕获，LMM 就看不到了
                eprintln!("LMM_PROMPT_FORM={spec}");
                let Some(answer) = read_answer_line() else {
                    eprintln!("ERROR: 未收到从机信息，操作中止");
                    return 1;
                };
                match parse_answer(&answer, password.as_deref()) {
                    Ok(x) => x,
                    Err(m) => {
                        message = Some(m);
                        continue;
                    }
                }
            }
        };
        match connect(&a, &pw, &mut log) {
            Ok(()) => {
                let _ = save_access(&a);
                save_password(&a, &pw);
                return 0;
            }
            // 不以 "ERROR:" 开头：这不是最终结论，接下来会回到表单
            Err(WorkerError::Unreachable(m)) => {
                eprintln!("连接从机失败：{m}");
                message = Some(m);
                access = Some(a);
                password = Some(pw);
            }
            Err(WorkerError::AuthFailed(m)) => {
                eprintln!("连接从机失败：{m}");
                forget_password(&a);
                message = Some(m);
                access = Some(a);
                password = None;
            }
            Err(WorkerError::Other(m)) => {
                eprintln!("ERROR: {m}");
                return 1;
            }
        }
    }
    eprintln!("ERROR: 多次尝试仍未连上从机，请确认从机信息后重新部署");
    1
}

fn read_secret(a: &WorkerAccess) -> Option<String> {
    if let Ok(path) = std::env::var(ENV_SECRET) {
        if let Ok(s) = fs::read_to_string(path) {
            let s = s.trim_end_matches(['\n', '\r']).to_string();
            if !s.is_empty() {
                return Some(s);
            }
        }
    }
    load_password(a)
}

/// 在从机上跑一条需要 sudo 的命令。
///
/// 难点在于要密码的往往不是我们发的这条，而是交付包**内部**自己调的那几条
/// `sudo nmcli`，而三条常规路都堵着：
/// 1. 不能 `ssh -tt`：管道里的密码会被送进远端伪终端再回显回来，原样打进日志；
/// 2. `sudo -S` 只喂得到我们自己发的那一条，插不进包内部的 sudo；
/// 3. 无 tty 时 sudo **不会**自动去用 SUDO_ASKPASS（真机实测报 "a terminal is
///    required to read the password"），而且票据按 ppid 记，攒不下来。
///
/// 解法：在远端 PATH 最前面放一个 `sudo` 垫片，把 `-A` 补上。这样命令里每一处 sudo
/// 都会用我们的 askpass 助手，不需要 tty、不改从机任何系统配置。密码、助手、垫片
/// 全在一个 0700 的临时目录里，用完就删。
fn cli_sudo(a: &WorkerAccess, remote_cmd: &str) -> i32 {
    let Some(password) = read_secret(a) else {
        eprintln!("ERROR: 没有保存的从机密码，请先调用 \"$LMM_WORKER\" connect");
        return 1;
    };
    // 垫片里要用 sudo 的绝对路径，否则它会顺着 PATH 找回自己，无限递归
    let setup = r#"set -e
d="$(mktemp -d)"
( umask 077; cat > "$d/pw" )
printf '#!/bin/sh\ncat %s\n' "$d/pw" > "$d/askpass"
mkdir -p "$d/bin"
printf '#!/bin/sh\nexec %s -A "$@"\n' "$(command -v sudo)" > "$d/bin/sudo"
chmod 700 "$d/askpass" "$d/bin/sudo"
printf '%s' "$d""#;
    let (ok, rdir, err) = ssh_capture(a, setup, Some(&format!("{password}\n")));
    let rdir = rdir.trim().to_string();
    if !ok || rdir.is_empty() {
        eprintln!("ERROR: 无法在从机上准备提权助手：{}", err.trim());
        return 1;
    }
    // PATH 必须在**远端**展开：`$PATH` 放在远端 shell 的双引号里。写成单引号的话
    // PATH 会变成字面量，整个系统 PATH 就废了——`sudo -v` 还能过（垫片内部用的是
    // 绝对路径），紧接着的命令就找不到 bash 了。这个坑真机上踩过一次。
    let wrapped = format!(
        "export SUDO_ASKPASS={}; export PATH={}:\"$PATH\"; sudo -v && {{ {remote_cmd}\n}}",
        shell_quote(&format!("{rdir}/askpass")),
        shell_quote(&format!("{rdir}/bin")),
    );
    let code = exit_code(ssh(a, &wrapped).stdin(Stdio::null()).status());
    let _ = ssh_capture(a, &format!("rm -rf {}", shell_quote(&rdir)), None);
    code
}

fn cli_push(a: &WorkerAccess, rest: &[String]) -> i32 {
    let src = rest[0].trim_end_matches('/');
    let dst = rest[1].trim_end_matches('/');
    let mut excludes = Vec::new();
    let mut i = 2;
    while i < rest.len() {
        if rest[i] == "--exclude" && i + 1 < rest.len() {
            excludes.push(format!("--exclude={}", rest[i + 1]));
            i += 2;
        } else {
            eprintln!("{USAGE}");
            return 2;
        }
    }
    let (ok, _, err) = ssh_capture(a, &format!("mkdir -p {}", shell_quote(dst)), None);
    if !ok {
        eprintln!("ERROR: 在从机上创建目录 {dst} 失败：{}", err.trim());
        return 1;
    }
    let transport = format!("ssh {}", SSH_OPTS.join(" "));
    exit_code(
        Command::new("rsync")
            .arg("-a")
            // 10-28 双机验证 P1-②：远端路径不能再 shell_quote——argv 传进去的引号
            // 会被原样保留，远端收到的是带字面引号的相对路径
            // （'/home/gann/…'，mkdir ENOENT code 11，真机实锤）。argv 传参本就
            // 不经 shell，特殊字符靠 --protect-args（-s）保护到远端 shell 之外。
            .arg("-s")
            .args(&excludes)
            .args(["-e", &transport])
            .arg(format!("{src}/"))
            .arg(format!("{}:{}/", a.target(), dst))
            .status(),
    )
}

// ---------------------------------------------------------------- 测直连网口

/// 解析远端打印的 "<网口> <rx_packets>" 行。
fn parse_counters(text: &str) -> Vec<(String, u64)> {
    text.lines()
        .filter_map(|l| {
            let mut it = l.split_whitespace();
            let name = it.next()?;
            let n = it.next()?.parse().ok()?;
            Some((name.to_string(), n))
        })
        .collect()
}

/// 三次快照 → 哪个口在 ping 期间多收了包。
///
/// 只看"ping 期间涨了多少"不够：从机的其它有线口可能接着别的局域网，那边本来就
/// 一直有广播。所以先空等一个同样长的窗口量出各口的底噪，再用 ping 窗口的增量减掉
/// 底噪。阈值只要求 2 个：内核对解析不出来的地址会限速重发 ARP，真机上量到过
/// 20 个 ping 只出 6 个 ARP。
///
/// 命中多个不是失败：能收到我们广播的口一定跟发送口在同一个二层域里，在它上面配
/// 地址一定通。多个时优先跟主机同名的那个，图个对称。
fn pick_link_iface(
    t0: &[(String, u64)],
    t1: &[(String, u64)],
    t2: &[(String, u64)],
    head_iface: &str,
) -> Option<String> {
    let get = |v: &[(String, u64)], n: &str| v.iter().find(|(k, _)| k == n).map(|(_, c)| *c);
    let mut hits: Vec<&str> = t2
        .iter()
        .filter_map(|(name, c2)| {
            let c1 = get(t1, name)?;
            let c0 = get(t0, name).unwrap_or(c1);
            let noise = c1.saturating_sub(c0);
            let active = c2.saturating_sub(c1);
            (active.saturating_sub(noise) >= 2).then_some(name.as_str())
        })
        .collect();
    hits.sort();
    if hits.contains(&head_iface) {
        return Some(head_iface.to_string());
    }
    hits.first().map(|s| s.to_string())
}

/// 从机上的候选口：插着线、不是管理网那张（它一直在收我们自己的 SSH 流量，真机上
/// 量到过比真正的直连口涨得还多）、也不是各种虚拟网卡。卡片知道得更准的话（比如
/// 只看 RDMA 口），可以直接把候选列表传进来。
fn candidates_script(mgmt_host: &str, given: &[String]) -> String {
    let list = if given.is_empty() {
        format!(
            "mgmt=\"$(ip -o -4 addr show | awk -v h={} '{{split($4,a,\"/\"); if (a[1]==h) print $2}}')\"; \
             for p in /sys/class/net/*; do n=\"${{p##*/}}\"; \
               case \"$n\" in lo|docker*|veth*|br-*|virbr*|tun*|tap*|wg*|tailscale*) continue;; esac; \
               [ \"$n\" = \"$mgmt\" ] && continue; \
               [ \"$(cat \"$p/carrier\" 2>/dev/null)\" = 1 ] || continue; echo \"$n\"; done",
            shell_quote(mgmt_host)
        )
    } else {
        format!(
            "for n in {}; do echo \"$n\"; done",
            given
                .iter()
                .map(|g| shell_quote(g))
                .collect::<Vec<_>>()
                .join(" ")
        )
    };
    format!(
        "{{ {list}; }} | while read -r n; do \
           c=\"$(cat /sys/class/net/$n/statistics/rx_packets 2>/dev/null)\" && echo \"$n $c\"; done"
    )
}

fn cli_detect_link(a: &WorkerAccess, head_iface: &str, peer_ip: &str, given: &[String]) -> i32 {
    let script = candidates_script(&a.host, given);
    let snap = || -> Option<Vec<(String, u64)>> {
        let (ok, out, err) = ssh_capture(a, &script, None);
        if !ok {
            eprintln!("ERROR: 读取从机网口计数失败：{}", err.trim());
            return None;
        }
        Some(parse_counters(&out))
    };
    let Some(t0) = snap() else { return 1 };
    if t0.is_empty() {
        eprintln!("ERROR: 从机上没有插着线的候选网口");
        return 1;
    }
    // 底噪窗口，跟下面 ping 的时长一样（20 × 0.3 秒）
    std::thread::sleep(Duration::from_secs(6));
    let Some(t1) = snap() else { return 1 };
    // 从机那侧还没配地址时 ARP 解析不出来，内核会反复广播 ARP 请求——正是要的。
    // 目标必须在同一个直连子网里，否则没有路由，一个包都发不出去。
    let _ = Command::new("ping")
        .args([
            "-c", "20", "-i", "0.3", "-W", "1", "-I", head_iface, peer_ip,
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    let Some(t2) = snap() else { return 1 };
    match pick_link_iface(&t0, &t1, &t2, head_iface) {
        Some(iface) => {
            println!("{iface}");
            0
        }
        None => {
            eprintln!(
                "ERROR: 从机没有任何网口收到主机从 {head_iface} 发出的广播，说明这两台不在同一条链路上。\
                 请确认直连线确实接在这两台之间、从机对应的口已启用"
            );
            1
        }
    }
}

/// 连不上从机直连地址时，把「线的问题」和「对端的问题」分开说清楚。
///
/// 本机完全能自己判断线好不好：物理链路（carrier）是网卡跟对端协商出来的，跟对端
/// 配没配 IP 无关。之前只甩一句"请确认直连线已插好、从机已开机并完成 init"，三种
/// 原因并排列出等于一个都没排除——真机上就因为这句话让人去拔插了一遍好好的线。
pub fn diagnose_link(iface: &str, peer: &str) -> String {
    let mut out = format!("ERROR: 连不上从机 {peer}。以下是自检结果：\n");
    let net = Path::new("/sys/class/net").join(iface);
    if iface.is_empty() || !net.exists() {
        out += &format!("  ✗ 找不到直连网卡 {iface}（配置可能已失效，建议重新部署）\n");
        return out;
    }
    let read = |f: &str| {
        fs::read_to_string(net.join(f))
            .map(|s| s.trim().to_string())
            .unwrap_or_default()
    };
    if read("carrier") != "1" {
        out += &format!("  ✗ 本机 {iface} 没有物理链路\n");
        out += "  → 这是线路问题：请检查直连线缆/光模块两端是否插紧，以及从机对应的口是否启用。\n";
        return out;
    }
    let speed = read("speed");
    out += &format!("  ✓ 本机 {iface} 物理链路正常（{speed}Mb/s）——线本身没有问题\n");
    let pinged = Command::new("ping")
        .args(["-c", "2", "-W", "2", "-I", iface, peer])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|s| s.success());
    if pinged {
        out += &format!("  ✓ 能 ping 通 {peer}，但 SSH 连不上\n");
        out += "  → 请检查从机的 sshd 是否在运行、防火墙是否放行 22 端口。\n";
    } else {
        out += &format!("  ✗ ping 不通 {peer}，ARP 无应答——这条链路上没有设备认领这个地址\n");
        out += "  → 线是通的，问题在对端：从机上的直连网络还没配置好，或者配在了别的口上。\n";
        out += &format!("     在从机上执行 ip -br addr，应能看到 {peer}\n");
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn c(v: &[(&str, u64)]) -> Vec<(String, u64)> {
        v.iter().map(|(n, x)| (n.to_string(), *x)).collect()
    }

    /// 把配置目录切到临时 HOME 上（config::TempHome 自带 ENV_TEST_LOCK 串行与
    /// 离开时还原），并清掉从机相关的环境变量——这两个也记原值、离开时还原。
    ///
    /// 旧 LMM 版直接改 XDG_*，而品悟这边的 config 根是 HOME 制（见
    /// config::TempHome），所以隔离手段跟着换。edition 2024 起 set_var/
    /// remove_var 是 unsafe，全程持锁下包 unsafe 块。
    struct Isolated {
        _home: config::TempHome,
        prev_host: Option<std::ffi::OsString>,
        prev_user: Option<std::ffi::OsString>,
    }
    impl Drop for Isolated {
        fn drop(&mut self) {
            // SAFETY: 仍在 TempHome 持有的 ENV_TEST_LOCK 内（字段声明顺序保证
            // 先还原环境变量、后放锁/删目录）。
            unsafe {
                match self.prev_host.take() {
                    Some(v) => std::env::set_var(ENV_HOST, v),
                    None => std::env::remove_var(ENV_HOST),
                }
                match self.prev_user.take() {
                    Some(v) => std::env::set_var(ENV_USER, v),
                    None => std::env::remove_var(ENV_USER),
                }
            }
        }
    }
    fn isolated() -> Isolated {
        let home = config::TempHome::new();
        let prev_host = std::env::var_os(ENV_HOST);
        let prev_user = std::env::var_os(ENV_USER);
        // SAFETY: TempHome 已持有模型商店测试的 ENV_TEST_LOCK。
        unsafe {
            std::env::remove_var(ENV_HOST);
            std::env::remove_var(ENV_USER);
        }
        Isolated {
            _home: home,
            prev_host,
            prev_user,
        }
    }

    #[test]
    fn quoting_survives_single_quotes() {
        assert_eq!(shell_quote("a b"), "'a b'");
        assert_eq!(shell_quote("it's"), r"'it'\''s'");
    }

    /// 10-28 双机验证 P0：script_env 必须注入 wrapper 而非裸 exe——裸 exe 会让
    /// 卡片的 `"$LMM_WORKER" connect`（quoted 调用契约，deploy.sh 九处）变成
    /// 「以 connect 为首参启动整个 GUI」，再被单实例插件静默 exit 0。
    /// 这里钉住 wrapper 的内容与可执行位；幂等（重写不报错）一并钉。
    #[test]
    fn script_env_generates_a_wrapper_not_the_bare_exe() {
        let _iso = isolated();
        let env = script_env();
        assert_eq!(
            env.len(),
            1,
            "生成失败时应为空（脚本有明确报错），这里不该失败"
        );
        assert_eq!(env[0].0, ENV_CLI);
        let wrapper = std::path::Path::new(&env[0].1);
        assert_ne!(
            wrapper,
            std::env::current_exe().unwrap().as_path(),
            "注入的必须是 wrapper 路径，不能是裸 exe"
        );
        let content = fs::read_to_string(wrapper).unwrap();
        assert!(
            content.contains("model-store-worker \"$@\""),
            "wrapper 必须补上子命令层：{content}"
        );
        assert!(content.starts_with("#!/bin/sh"));
        use std::os::unix::fs::PermissionsExt as _;
        let mode = fs::metadata(wrapper).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o755, "卡片脚本要直接执行它，必须可执行");
        // 幂等：再生成一遍不报错、内容不变
        let again = script_env();
        assert_eq!(again[0].1, env[0].1);
        assert_eq!(fs::read_to_string(wrapper).unwrap(), content);
    }

    /// 接着别的局域网的口一直有底噪，不能因为它涨得多就选它。
    #[test]
    fn link_detection_subtracts_background_noise() {
        let t0 = c(&[("eth1", 100), ("rdma0", 10), ("rdma1", 10)]);
        let t1 = c(&[("eth1", 140), ("rdma0", 10), ("rdma1", 10)]); // eth1 底噪 +40
        let t2 = c(&[("eth1", 181), ("rdma0", 16), ("rdma1", 10)]); // eth1 +41，rdma0 +6
        assert_eq!(pick_link_iface(&t0, &t1, &t2, "enp1"), Some("rdma0".into()));
    }

    #[test]
    fn link_detection_prefers_the_same_name_and_reports_nothing_when_silent() {
        let t0 = c(&[("a", 0), ("b", 0)]);
        let t2 = c(&[("a", 5), ("b", 5)]);
        assert_eq!(pick_link_iface(&t0, &t0, &t2, "b"), Some("b".into()));
        assert_eq!(pick_link_iface(&t0, &t0, &t2, "zz"), Some("a".into()));
        assert_eq!(
            pick_link_iface(&t0, &t0, &c(&[("a", 1), ("b", 0)]), "a"),
            None
        );
    }

    #[test]
    fn counters_parse_and_skip_garbage() {
        assert_eq!(
            parse_counters("a 1\nbad\nb 2 x\n c notnum"),
            c(&[("a", 1), ("b", 2)])
        );
    }

    #[test]
    fn form_prefills_but_never_carries_a_password() {
        let prev = WorkerAccess {
            host: "10.0.0.2".into(),
            user: "sky".into(),
        };
        let spec: serde_json::Value =
            serde_json::from_str(&access_form(Some(&prev), Some("密码不对"), false)).unwrap();
        assert_eq!(spec["message"], "密码不对");
        assert_eq!(spec["fields"][0]["value"], "10.0.0.2");
        assert_eq!(spec["fields"][1]["value"], "sky");
        assert!(spec["fields"][2].get("value").is_none());
        assert_eq!(spec["fields"][2]["required"], true);
        // 地址填错回来重填时，密码可以留空沿用
        let spec: serde_json::Value =
            serde_json::from_str(&access_form(Some(&prev), None, true)).unwrap();
        assert_eq!(spec["fields"][2]["required"], false);
    }

    #[test]
    fn answer_parsing_trims_and_can_reuse_the_pending_password() {
        let (a, pw) = parse_answer(
            r#"{"worker_host":" 1.2.3.4 ","worker_user":"u","worker_pw":""}"#,
            Some("old"),
        )
        .unwrap();
        assert_eq!(
            a,
            WorkerAccess {
                host: "1.2.3.4".into(),
                user: "u".into()
            }
        );
        assert_eq!(pw, "old");
        assert!(
            parse_answer(
                r#"{"worker_host":"1.2.3.4","worker_user":"u","worker_pw":""}"#,
                None
            )
            .is_err()
        );
    }

    #[test]
    fn saved_access_round_trips_and_env_overrides_it() {
        let _iso = isolated();
        let saved = WorkerAccess {
            host: "192.168.10.42".into(),
            user: "sky".into(),
        };
        save_access(&saved).unwrap();
        assert_eq!(load_access(), Some(saved.clone()));
        assert_eq!(resolve_access(), Some(saved.clone()));

        // SAFETY: isolated() 持有 ENV_TEST_LOCK；改完在断言后立即还原。
        unsafe {
            std::env::set_var(ENV_HOST, "10.9.9.9");
            std::env::set_var(ENV_USER, "other");
        }
        assert_eq!(
            resolve_access(),
            Some(WorkerAccess {
                host: "10.9.9.9".into(),
                user: "other".into()
            })
        );
        unsafe {
            std::env::remove_var(ENV_HOST);
            std::env::remove_var(ENV_USER);
        }
        assert_eq!(resolve_access(), Some(saved));
    }

    #[test]
    fn cli_refuses_to_touch_a_worker_before_any_connect() {
        let _iso = isolated();
        assert_eq!(cli(&["exec".into(), "true".into()]), 2);
        assert_eq!(cli(&["info".into()]), 2);
        assert_eq!(cli(&[]), 2);
    }

    #[test]
    fn default_candidates_exclude_the_management_nic() {
        let s = candidates_script("192.168.10.42", &[]);
        assert!(s.contains("'192.168.10.42'"));
        assert!(s.contains("[ \"$n\" = \"$mgmt\" ] && continue"));
        let s = candidates_script("h", &["enP2p1s0f0np0".into()]);
        assert!(s.contains("for n in 'enP2p1s0f0np0'"));
    }
}
