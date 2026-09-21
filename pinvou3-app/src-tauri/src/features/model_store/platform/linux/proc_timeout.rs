//! 带超时的外部命令执行。
//!
//! # 为什么需要
//!
//! `Command::output()` 是**无限期阻塞**的。本模块要挡的不是「命令跑得慢」，而是
//! 「命令永远不返回」——docker daemon 卡死（存储驱动挂起、containerd 失联）时
//! `docker ps` 就是这个表现，不报错也不退出。
//!
//! 踩点在 `reconcile_running_status()`：它在 `library()` 里被调用，而 `library()`
//! 是打开模型商店页面的第一个请求。daemon 一卡，用户看到的是**整个模型商店永久
//! 转圈**，而不是「拿不到运行状态」。
//!
//! 同一份代码里 ollama 那条分支早就设了 5 秒超时
//! （`ollama_runtime_lines` 的 `reqwest ... .timeout(5s)`），docker 和 nvidia-smi
//! 这两条却没有——标准不一致本身就是个信号：不是不需要，是漏了。
//!
//! # 为什么不用 `timeout(1)` 命令
//!
//! 那要求目标机器装了 coreutils 的 `timeout`，而且多起一个进程。这里只用标准库：
//! 子进程的 stdout 在一个线程里读完，主线程按 deadline 等它。
//!
//! # 超时之后
//!
//! **杀掉子进程**再返回 None。不杀的话每次打开商店页面都会留下一个卡住的
//! `docker ps`，几十分钟后系统里就是一堆僵尸——这个应用已经因为「残留进程占着
//! 文件锁」出过一次 16 小时的事故（见 `orphans.rs`），同类问题不该再来一次。

use std::io::Read;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

/// 查询类命令的默认时限。
///
/// 取 5 秒是为了跟 `ollama_runtime_lines` 那条已有的 `reqwest` 超时对齐——这三处
/// 都是「拿一下状态给界面看」的查询，没有理由各设各的阈值，不一致只会让排查变难。
///
/// 对正常的 `docker ps` 来说 5 秒宽得离谱（毫秒级），所以不会误伤慢机器；它只在
/// daemon 真的挂了的时候生效。
pub const QUERY_TIMEOUT: Duration = Duration::from_secs(5);

/// 跑一个命令并拿它的 stdout，超时就杀掉并返回 None。
///
/// 返回 None 的三种情况（调用方都按「拿不到」处理，不区分）：命令起不来、
/// 退出码非 0、超时。区分它们对调用方没有意义——界面上都是「拿不到状态」。
pub fn output_within(program: &str, args: &[&str], limit: Duration) -> Option<String> {
    let mut child = Command::new(program)
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;

    // stdout 必须在**另一个线程**里读干净。
    //
    // 只在主线程 try_wait 而不读管道的话，输出超过管道缓冲区（Linux 默认 64KB）
    // 时子进程会阻塞在 write 上永远退不出来——那就把「防卡死」变成了「制造卡死」。
    // `docker ps` 在装了几十个容器的机器上很容易过 64KB。
    let mut stdout = child.stdout.take()?;
    let reader = std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = stdout.read_to_end(&mut buf);
        buf
    });

    let deadline = Instant::now() + limit;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                let buf = reader.join().unwrap_or_default();
                if !status.success() {
                    return None;
                }
                return Some(String::from_utf8_lossy(&buf).into_owned());
            }
            Ok(None) => {}
            Err(_) => return None,
        }
        if Instant::now() >= deadline {
            // 杀掉，否则它会一直挂在那儿占着 fd 和进程表。
            let _ = child.kill();
            // 收尸也交给另一个线程：kill 之后 wait 通常立刻返回，但如果进程处在
            // 不可中断睡眠（D 状态，卡在 I/O 上的 daemon 子进程正是这样），wait
            // 会一直等下去，那又把调用方堵住了。
            std::thread::spawn(move || {
                let _ = child.wait();
            });
            eprintln!(
                "[model_store] `{program}` 超过 {:?} 没返回，已放弃并杀掉",
                limit
            );
            return None;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// `output_within` 的常用形态：用默认查询时限。
pub fn query(program: &str, args: &[&str]) -> Option<String> {
    output_within(program, args, QUERY_TIMEOUT)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 正常命令拿得到输出() {
        let out = query("echo", &["hello"]).expect("echo 应该成功");
        assert_eq!(out.trim(), "hello");
    }

    #[test]
    fn 退出码非零回_none() {
        assert!(query("false", &[]).is_none());
    }

    #[test]
    fn 命令不存在回_none不panic() {
        assert!(query("pinvou3-no-such-binary-for-tests", &[]).is_none());
    }

    #[test]
    fn 超时会返回并且不会等满整个睡眠时间() {
        let started = Instant::now();
        let out = output_within("sleep", &["30"], Duration::from_millis(300));
        let elapsed = started.elapsed();
        assert!(out.is_none(), "超时必须回 None");
        // 关键是「按 deadline 返回」而不是「等 sleep 自己结束」。放宽到 5 秒是给
        // 慢机器留余量，但它离 30 秒足够远，能证明确实提前返回了。
        assert!(
            elapsed < Duration::from_secs(5),
            "应该在 deadline 附近返回，实际等了 {elapsed:?}"
        );
    }

    /// 输出超过管道缓冲区时不能死锁——这是「只 try_wait 不读管道」的经典翻车点。
    #[test]
    fn 大输出不会死锁() {
        // yes 会一直输出，head 截断到远超 64KB 的量。
        let out = output_within(
            "sh",
            &["-c", "yes 0123456789abcdef | head -c 400000"],
            Duration::from_secs(10),
        )
        .expect("应该读完而不是卡住");
        assert_eq!(out.len(), 400_000);
    }
}
