//! 登记本应用派生出去的脚本进程，并在下次启动时清理上次残留下来的「孤儿」。
//!
//! 背景：下载/部署这类脚本跑在独立进程组里（见 script_runner 的 process_group），
//! 应用被强杀、崩溃或者被覆盖安装时，这些进程不会跟着退出，就变成没人跟踪进度、
//! 也不会回写数据库的孤儿。它们还会一直占着 huggingface_hub 的文件锁，把用户下
//! 一次点的下载堵在那儿干等——真机上就出现过一个跑了 16 小时、一个字节都没下的
//! 「僵持」进程对。
//!
//! 做法：每次派生进程时往一个 JSON 文件里登记一笔，正常结束时销号；下次启动读
//! 这个文件，把仍然活着的残留进程收掉。已经下好的权重一个字节都不动——hf
//! download 支持断点续传，用户重新点下载会接着上次的进度继续。
//!
//! 杀进程这件事必须格外小心 PID 复用：记录里的 pid 若已经被系统回收给了别的
//! 进程，照着记录去杀就是误杀无辜进程。这里叠了三道校验：
//!   1. boot_id 变了 => 机器重启过，记录里的 pid 全部作废，一个都不碰；
//!   2. 组长进程的 starttime 与登记时不一致 => pid 被复用了，整条记录作废；
//!   3. 逐个组员再比一次 starttime，早于组长启动的一律不碰。
//!
//! 宁可漏杀（用户手动清理一下），也绝不误杀。

use std::fs;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use nix::sys::signal::{Signal, kill};
use nix::unistd::Pid;
use serde::{Deserialize, Serialize};

use super::config;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RunRecord {
    pub pid: i32,
    /// 进程组号。非 root 脚本由 script_runner 建成独立进程组（pgid == pid），
    /// 脚本自己 fork 出来的孙子进程（curl/hf/docker…）都在这个组里。
    pub pgid: i32,
    pub boot_id: String,
    /// /proc/<pid>/stat 第 22 个字段：进程启动时刻（自开机起的时钟滴答数）。
    /// 与 pid 合起来才能唯一标识一个进程，用来识别 pid 复用。
    pub starttime: u64,
    pub label: String,
    pub need_root: bool,
    /// 派生这个脚本的**应用进程**自己的 pid + 启动时刻。
    ///
    /// 用来识别「另一个实例正管着它」：用户重复点桌面图标就会开出第二个实例，
    /// 第二个实例一启动就跑清理，那些记录在它眼里全是「上次残留」——校验也全
    /// 过（同一次开机、pid 没复用、pgid 正确），于是把第一个实例正在跑的下载
    /// 干掉了。记下属主后，属主还活着的记录一律不碰。
    ///
    /// serde(default) 是为了兼容上一版写下的、没有这两个字段的旧记录（读出来
    /// 是 0，走「属主未知」的分支）。
    #[serde(default)]
    pub app_pid: i32,
    #[serde(default)]
    pub app_starttime: u64,
}

/// 登记文件的读-改-写不是原子的，多个下载/部署同时跑时要互斥。
static FILE_LOCK: Mutex<()> = Mutex::new(());

fn record_file() -> PathBuf {
    config::data_dir().join("running-scripts.json")
}

fn read_boot_id() -> String {
    fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

/// 读 /proc/<pid>/stat 里的 pgid（字段 5）和 starttime（字段 22）。
///
/// comm（字段 2）被小括号包着，内容可能含空格甚至小括号（比如进程名本身就带
/// 括号），所以不能直接按空格切整行；标准做法是从**最后一个**右括号之后开始
/// 切，那之后的第 0 个 token 就是字段 3。
fn proc_stat(pid: i32) -> Option<(i32, u64)> {
    let text = fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let rest = &text[text.rfind(')')? + 1..];
    let fields: Vec<&str> = rest.split_whitespace().collect();
    let pgid = fields.get(5 - 3)?.parse().ok()?;
    let starttime = fields.get(22 - 3)?.parse().ok()?;
    Some((pgid, starttime))
}

fn load_unlocked() -> Vec<RunRecord> {
    fs::read_to_string(record_file())
        .ok()
        .and_then(|t| serde_json::from_str(&t).ok())
        .unwrap_or_default()
}

fn store_unlocked(records: &[RunRecord]) {
    let path = record_file();
    if records.is_empty() {
        let _ = fs::remove_file(&path);
        return;
    }
    let Ok(json) = serde_json::to_string_pretty(records) else {
        return;
    };
    // 先写临时文件再 rename：应用正好在这一刻被杀掉时，登记文件要么是旧的
    // 完整内容、要么是新的完整内容，不会留下半截 JSON 让下次启动读不出来。
    let tmp = path.with_extension("json.tmp");
    if fs::write(&tmp, json).is_ok() {
        let _ = fs::rename(&tmp, &path);
    }
}

fn lock() -> std::sync::MutexGuard<'static, ()> {
    // 别的线程 panic 时不该让登记功能整个瘫掉，中毒了照样把里面的值拿出来用。
    FILE_LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

/// 一次脚本运行的登记凭据：构造时登记，析构时销号。
///
/// 用 RAII 而不是手动配对 register/unregister，是因为 run_script 有取消、超时、
/// 非 0 退出等一大堆返回路径，漏掉任何一条都会在登记文件里留下永远销不掉的
/// 假记录，下次启动就会去杀一个早就退出了的 pid。
pub struct RunGuard {
    pid: i32,
}

impl RunGuard {
    pub fn register(pid: i32, need_root: bool, label: &str) -> Self {
        let _lk = lock();
        if let Some((pgid, starttime)) = proc_stat(pid) {
            let app_pid = std::process::id() as i32;
            let app_starttime = proc_stat(app_pid).map(|(_, st)| st).unwrap_or(0);
            let mut records = load_unlocked();
            records.retain(|r| r.pid != pid);
            records.push(RunRecord {
                pid,
                pgid,
                boot_id: read_boot_id(),
                starttime,
                label: label.to_string(),
                need_root,
                app_pid,
                app_starttime,
            });
            store_unlocked(&records);
        }
        Self { pid }
    }
}

impl Drop for RunGuard {
    fn drop(&mut self) {
        let _lk = lock();
        let mut records = load_unlocked();
        let before = records.len();
        records.retain(|r| r.pid != self.pid);
        if records.len() != before {
            store_unlocked(&records);
        }
    }
}

#[derive(Debug, Default, Clone, Serialize)]
pub struct OrphanCleanup {
    /// 已经收掉的残留进程（用登记时的人话标签，比如「下载 Ornith-1.5-35B-A3B」）
    pub killed: Vec<String>,
    /// 以 root 身份跑的残留进程：普通用户身份的应用没权限杀，只能报出来让用户知道
    pub skipped_root: Vec<String>,
}

impl OrphanCleanup {
    pub fn is_empty(&self) -> bool {
        self.killed.is_empty() && self.skipped_root.is_empty()
    }
}

/// 这条记录的属主应用是不是还活着（也就是「另一个实例正管着它」）。
///
/// 判断不了的时候一律当成「还活着」——宁可漏清理（用户手动收拾一下），也不能
/// 把别人正在跑的下载杀掉。
fn owner_still_running(record: &RunRecord, self_pid: i32) -> bool {
    if record.app_pid == 0 {
        // 上一版写的旧记录没记属主。那一版还没有多实例互杀的防护，能到这里
        // 说明应用确实重启过，按「属主已死」处理，行为跟上一版保持一致。
        return false;
    }
    if record.app_pid == self_pid {
        return false; // 就是我们自己，不是别的实例
    }
    match proc_stat(record.app_pid) {
        None => false, // 属主进程已经没了
        Some((_, starttime)) => {
            // 记录里没存到属主的启动时刻（读 /proc 失败）就没法排除 pid 复用，
            // 保守当成还活着。
            record.app_starttime == 0 || starttime == record.app_starttime
        }
    }
}

/// 找出某条记录对应的、现在还活着的进程组成员。
fn group_members(record: &RunRecord, self_pid: i32, self_pgid: i32) -> Vec<i32> {
    // 兜底自保：无论如何都不能把应用自己（或自己所在的进程组）算成清理目标。
    if record.pid == self_pid || record.pgid == self_pgid {
        return Vec::new();
    }
    // 只清理「自己亲手建的进程组」。script_runner 派生非 root 脚本时一律
    // process_group(0)，也就是子进程自己当组长（pgid == pid）；记录里对不上
    // 这条不变量，说明当时压根没建成独立进程组，那个 pgid 是应用自己所在的
    // 那棵进程树共用的——照着它去杀就会连累一大片无关进程。
    if record.pgid != record.pid {
        return Vec::new();
    }
    // 组长还在，但 starttime 跟登记时对不上 —— 这个 pid 已经被系统回收给别的
    // 进程用了，整条记录都不可信，直接放弃。
    if let Some((_, starttime)) = proc_stat(record.pid) {
        if starttime != record.starttime {
            return Vec::new();
        }
    }
    let mut members = Vec::new();
    let Ok(dir) = fs::read_dir("/proc") else {
        return members;
    };
    for entry in dir.flatten() {
        let Ok(pid) = entry.file_name().to_string_lossy().parse::<i32>() else {
            continue;
        };
        if pid == self_pid {
            continue;
        }
        let Some((pgid, starttime)) = proc_stat(pid) else {
            continue;
        };
        // 进程组号本身也可能被复用，再用「成员不可能比组长启动得更早」兜一层。
        if pgid == record.pgid && starttime >= record.starttime {
            members.push(pid);
        }
    }
    members
}

/// 启动时调用一次：收掉上次退出时残留的脚本进程，并清空登记文件。
pub fn cleanup_on_startup() -> OrphanCleanup {
    let _lk = lock();
    let mut report = OrphanCleanup::default();
    let records = load_unlocked();
    if records.is_empty() {
        return report;
    }

    let self_pid = std::process::id() as i32;
    let self_pgid = proc_stat(self_pid).map(|(pgid, _)| pgid).unwrap_or(-1);
    let boot_id = read_boot_id();

    let mut victims: Vec<(i32, String)> = Vec::new();
    let mut kept: Vec<RunRecord> = Vec::new();
    for record in &records {
        // 机器重启过：上次记下的 pid 全部失效，那些号早被别的进程用了，一个都不碰。
        if !boot_id.is_empty() && !record.boot_id.is_empty() && record.boot_id != boot_id {
            continue;
        }
        // 另一个应用实例正管着这个脚本（用户重复点了桌面图标）——不但不能杀，
        // 记录也得原样留着，否则那个实例结束时就销不了号，下次启动又会当成
        // 残留去处理。
        if owner_still_running(record, self_pid) {
            kept.push(record.clone());
            continue;
        }
        if record.need_root {
            // pkexec 起来的是 root 进程，非 root 的应用即便是它的父进程也没权限
            // 对它发信号——这是操作系统的权限边界，不是能绕过去的 bug。只登记
            // 一笔告诉用户，让用户自己决定要不要处理。
            if proc_stat(record.pid).map(|(_, st)| st) == Some(record.starttime) {
                report.skipped_root.push(record.label.clone());
            }
            continue;
        }
        for pid in group_members(record, self_pid, self_pgid) {
            victims.push((pid, record.label.clone()));
        }
    }

    if !victims.is_empty() {
        for (pid, _) in &victims {
            let _ = kill(Pid::from_raw(*pid), Signal::SIGTERM);
        }
        // 给一点时间让它们自己收尾（脚本 trap 里的清理、把手上的分片落盘等），
        // 到点还赖着不走的再强杀。
        let deadline = Instant::now() + Duration::from_secs(3);
        while Instant::now() < deadline {
            if victims.iter().all(|(pid, _)| proc_stat(*pid).is_none()) {
                break;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        for (pid, _) in &victims {
            if proc_stat(*pid).is_some() {
                let _ = kill(Pid::from_raw(*pid), Signal::SIGKILL);
            }
        }
        for (_, label) in victims {
            if !report.killed.contains(&label) {
                report.killed.push(label);
            }
        }
    }

    // 这一轮已经把每条记录都处理过了（杀掉/判定失效/没权限），除了留给别的
    // 实例的那些，其余全部销号。
    store_unlocked(&kept);
    report
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::Command;

    /// 登记文件放在 data_dir 下，而 data_dir 由 HOME 决定——这是个
    /// 进程级的环境变量，db 的测试也在改它。必须用同一把跨模块的锁串行化，
    /// 各锁各的等于没锁。
    fn with_temp_data_dir<T>(f: impl FnOnce() -> T) -> T {
        // TempHome 自带那把跨模块的锁，并在析构时把 HOME 还原回去——
        // 不还原会让 tempdir 删除后 HOME 悬空，污染同进程里读 HOME 的其他测试。
        let _home = config::TempHome::new();
        f()
    }

    /// 起一个自成进程组的长命子进程当「残留进程」。
    ///
    /// 这里用 pre_exec 里显式 setpgid，而不是 Command::process_group()：后者走
    /// posix_spawn 的属性位，在 QEMU 用户态模拟下不生效（实测子进程的 pgid 仍
    /// 是测试进程那一整棵树共用的），于是测试会拿着一个共用 pgid 去做清理，
    /// 顺手把同批跑的其它测试进程一起杀掉。setpgid 是子进程自己发的一条真
    /// 系统调用，模拟环境下同样可靠。
    fn spawn_sleeper() -> std::process::Child {
        use std::os::unix::process::CommandExt;
        let mut cmd = Command::new("sh");
        cmd.arg("-c").arg("sleep 300");
        unsafe {
            cmd.pre_exec(|| {
                nix::unistd::setpgid(Pid::from_raw(0), Pid::from_raw(0))
                    .map_err(std::io::Error::from)
            });
        }
        let child = cmd.spawn().unwrap();
        let pid = child.id() as i32;
        // 后面几个测试会照着记录里的 pgid 杀整个进程组。万一没建成独立进程组，
        // 那个 pgid 就是测试进程自己所在的组——宁可当场失败，也不能真的杀下去。
        let (pgid, _) = proc_stat(pid).expect("刚起来的子进程应该能读到 /proc");
        assert_eq!(
            pgid, pid,
            "子进程必须自成进程组，否则清理测试会误杀同批进程"
        );
        child
    }

    /// 校验 /proc/<pid>/stat 的字段位置有没有数错——数错一位就会拿着别人的
    /// 号去杀进程，后果很严重。
    ///
    /// 拿一个真子进程当样本，而不是测试进程自己：QEMU 用户态模拟会把自己那份
    /// /proc/<pid>/stat 伪造成除 pid/comm 外全是 0 的假数据（交叉编译后在 x86
    /// 上跑测试就会撞上），拿自己测等于测了个寂寞。真子进程是原生进程，读到的
    /// 是内核的真数据。
    ///
    /// pgid 用 getpgid 这条系统调用做独立比对，跟被测的 /proc 解析没有共用逻辑。
    #[test]
    fn proc_stat_matches_the_kernel() {
        let mut child = spawn_sleeper();
        let pid = child.id() as i32;

        let (pgid, starttime) = proc_stat(pid).expect("应该能读到子进程的 /proc 项");
        let expected = nix::unistd::getpgid(Some(Pid::from_raw(pid))).unwrap();
        assert_eq!(pgid, expected.as_raw(), "pgid 字段位置数错了");
        assert!(starttime > 0, "starttime 应该是自开机起的时钟滴答数");

        let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
        let _ = child.wait();
    }

    #[test]
    fn guard_registers_and_unregisters() {
        with_temp_data_dir(|| {
            let mut child = spawn_sleeper();
            let pid = child.id() as i32;
            {
                let _g = RunGuard::register(pid, false, "测试任务");
                let records = load_unlocked();
                assert_eq!(records.len(), 1);
                assert_eq!(records[0].pid, pid);
                assert_eq!(records[0].pgid, pid, "非 root 脚本应自成一个进程组");
            }
            // guard 析构后记录必须销掉，否则下次启动会去杀一个不存在的 pid
            assert!(load_unlocked().is_empty());
            let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
            let _ = child.wait();
        });
    }

    #[test]
    fn cleanup_kills_orphan_process_group() {
        with_temp_data_dir(|| {
            let mut child = spawn_sleeper();
            let pid = child.id() as i32;
            // 模拟「应用被强杀、没来得及销号」：登记之后把 guard forget 掉
            std::mem::forget(RunGuard::register(pid, false, "下载测试模型"));
            assert_eq!(load_unlocked().len(), 1);

            let report = cleanup_on_startup();
            assert_eq!(report.killed, vec!["下载测试模型".to_string()]);
            assert!(load_unlocked().is_empty(), "处理完必须清空登记文件");

            let _ = child.wait();
            assert!(proc_stat(pid).is_none(), "残留进程应已被收掉");
        });
    }

    #[test]
    fn cleanup_ignores_records_from_a_previous_boot() {
        with_temp_data_dir(|| {
            let mut child = spawn_sleeper();
            let pid = child.id() as i32;
            let (pgid, starttime) = proc_stat(pid).unwrap();
            // 机器重启过：boot_id 对不上，这个 pid 现在属于谁都不知道，绝不能杀
            store_unlocked(&[RunRecord {
                pid,
                pgid,
                boot_id: "00000000-0000-0000-0000-000000000000".to_string(),
                starttime,
                label: "上一次开机留下的下载".to_string(),
                need_root: false,
                app_pid: 0,
                app_starttime: 0,
            }]);

            let report = cleanup_on_startup();
            assert!(report.is_empty(), "跨重启的记录必须整条跳过");
            assert!(proc_stat(pid).is_some(), "无辜进程不能被误杀");

            let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
            let _ = child.wait();
        });
    }

    /// 用户重复点桌面图标开出第二个实例时，第二个实例不能把第一个实例正在跑
    /// 的下载杀掉——这是清理逻辑最容易误伤自己人的地方。
    #[test]
    fn cleanup_leaves_alone_downloads_owned_by_another_live_instance() {
        with_temp_data_dir(|| {
            let mut owner = spawn_sleeper(); // 冒充「另一个还活着的应用实例」
            let mut script = spawn_sleeper(); // 它正在跑的下载脚本
            let owner_pid = owner.id() as i32;
            let script_pid = script.id() as i32;
            let (pgid, starttime) = proc_stat(script_pid).unwrap();
            let (_, owner_starttime) = proc_stat(owner_pid).unwrap();

            store_unlocked(&[RunRecord {
                pid: script_pid,
                pgid,
                boot_id: read_boot_id(),
                starttime,
                label: "别的实例正在下的模型".to_string(),
                need_root: false,
                app_pid: owner_pid,
                app_starttime: owner_starttime,
            }]);

            let report = cleanup_on_startup();
            assert!(report.is_empty(), "不能去动别的实例管着的下载");
            assert!(proc_stat(script_pid).is_some(), "别的实例的下载被误杀了");
            assert_eq!(
                load_unlocked().len(),
                1,
                "记录要原样留着，否则那个实例结束时销不了号"
            );

            for pid in [script_pid, owner_pid] {
                let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
            }
            let _ = script.wait();
            let _ = owner.wait();
        });
    }

    #[test]
    fn cleanup_ignores_records_whose_pid_got_recycled() {
        with_temp_data_dir(|| {
            let mut child = spawn_sleeper();
            let pid = child.id() as i32;
            let (pgid, starttime) = proc_stat(pid).unwrap();
            // starttime 对不上 = 这个 pid 已经被回收给别的进程用了
            store_unlocked(&[RunRecord {
                pid,
                pgid,
                boot_id: read_boot_id(),
                starttime: starttime.saturating_sub(1),
                label: "pid 被复用的旧记录".to_string(),
                need_root: false,
                app_pid: 0,
                app_starttime: 0,
            }]);

            let report = cleanup_on_startup();
            assert!(report.is_empty(), "pid 复用的记录必须整条跳过");
            assert!(proc_stat(pid).is_some(), "无辜进程不能被误杀");

            let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
            let _ = child.wait();
        });
    }
}
