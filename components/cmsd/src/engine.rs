//! 引擎进程发现与健康判定。
//!
//! 进程发现走 /proc：exe 符号链接锚定内容寻址引擎目录（卡片范式：路径即身份，
//! 不会误伤同名进程）。健康判定沿用驭光卡口径：GET /v1/models 应答且
//! 模型 id 与期望一致。

use crate::http;
use std::path::Path;
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct ProcInfo {
    pub pid: u32,
    /// exe 路径里解析出的引擎 sha8（内容寻址目录名）。
    pub sha8: String,
    pub port: Option<u16>,
    pub model_id: Option<String>,
    /// /proc/<pid>/stat 的启动时刻（jiffies），pid 复用校验用（孤儿检测思想）。
    pub start_time: u64,
}

/// 从命令行参数取 `--port N` / `--model-id X`。
fn arg_value(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

/// 扫描 /proc，找 exe 位于 engine_root 下的 tachyon-serve 进程。
pub fn scan_engine_procs(proc_root: &Path, engine_root: &Path) -> Vec<ProcInfo> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir(proc_root) else {
        return out;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(name_str) = name.to_str() else {
            continue;
        };
        let Ok(pid) = name_str.parse::<u32>() else {
            continue;
        };
        let exe_link = entry.path().join("exe");
        let Ok(exe) = std::fs::read_link(&exe_link) else {
            continue;
        };
        if exe.file_name().map(|f| f.to_string_lossy()) != Some("tachyon-serve".into()) {
            continue;
        }
        if !exe.starts_with(engine_root) {
            continue;
        }
        // exe = <engine_root>/<sha8>/tachyon-serve
        let Some(sha8) = exe
            .parent()
            .and_then(|p| p.file_name())
            .map(|f| f.to_string_lossy().into_owned())
        else {
            continue;
        };
        let cmdline = std::fs::read(entry.path().join("cmdline")).unwrap_or_default();
        let args: Vec<String> = cmdline
            .split(|b| *b == 0)
            .filter(|s| !s.is_empty())
            .map(|s| String::from_utf8_lossy(s).into_owned())
            .collect();
        let start_time = std::fs::read_to_string(entry.path().join("stat"))
            .ok()
            .and_then(|s| {
                // 第 22 字段是 starttime（comm 可能含空格/括号，从最后一个 ')' 后数）
                s.rfind(')')
                    .map(|pos| s[pos + 2..].to_string())
                    .and_then(|rest| rest.split_whitespace().nth(19).map(|v| v.to_string()))
            })
            .and_then(|v| v.parse::<u64>().ok())
            .unwrap_or(0);
        out.push(ProcInfo {
            pid,
            sha8,
            port: arg_value(&args, "--port").and_then(|v| v.parse().ok()),
            model_id: arg_value(&args, "--model-id"),
            start_time,
        });
    }
    out.sort_by_key(|p| p.pid);
    out
}

#[derive(Debug, Clone)]
pub struct Health {
    pub ok: bool,
    pub model_id: Option<String>,
    /// 引擎自报的上下文窗口（/v1/models 的 max_model_len）：接入行
    /// context_window 的事实来源——没有它接入行落到保守兜底窗口，
    /// 系统提示词就超过输入地板，每条消息都误触发紧急压缩（真机实测）。
    pub model_len: Option<u64>,
    pub checked_at: u64,
    pub detail: String,
}

/// 健康判定：/v1/models 应答且（若给期望）id 一致。
/// 空串期望按无期望处理（W6 容器卡收编后可能还没问到真名）。
pub fn check_health(port: u16, expect_model_id: Option<&str>) -> Health {
    let expect_model_id = expect_model_id.filter(|e| !e.is_empty());
    let now = crate::timeutil::now_epoch();
    match http::client_get("127.0.0.1", port, "/v1/models", Duration::from_secs(3)) {
        Ok(resp) if resp.status == 200 => {
            let id = String::from_utf8_lossy(&resp.body).to_string();
            let parsed = crate::json::Value::parse(&id).ok();
            let model_entry = parsed
                .as_ref()
                .and_then(|v| v.get("data"))
                .and_then(|d| d.as_arr())
                .and_then(|a| a.first().cloned());
            let model_id = model_entry
                .as_ref()
                .and_then(|m| m.get("id"))
                .and_then(|i| i.as_str())
                .map(|s| s.to_string());
            let model_len = model_entry
                .as_ref()
                .and_then(|m| m.get("max_model_len"))
                .and_then(|l| l.as_u64());
            let mismatch = match (expect_model_id, model_id.as_deref()) {
                (Some(expect), Some(actual)) if expect != actual => {
                    Some(format!("模型 id 不符：期望 {expect}，实际 {actual}"))
                }
                _ => None,
            };
            match mismatch {
                Some(detail) => Health {
                    ok: false,
                    model_id,
                    model_len,
                    checked_at: now,
                    detail,
                },
                None if model_id.is_some() => Health {
                    ok: true,
                    model_id,
                    model_len,
                    checked_at: now,
                    detail: "ok".to_string(),
                },
                None => Health {
                    ok: false,
                    model_id: None,
                    model_len: None,
                    checked_at: now,
                    detail: "/v1/models 应答里没有模型 id".to_string(),
                },
            }
        }
        Ok(resp) => Health {
            ok: false,
            model_id: None,
            model_len: None,
            checked_at: now,
            detail: format!("HTTP {}", resp.status),
        },
        Err(e) => Health {
            ok: false,
            model_id: None,
            model_len: None,
            checked_at: now,
            detail: format!("连接失败：{e}"),
        },
    }
}

/// 进程 VmRSS（GiB）。统一内存平台上驱动分配不完全计入 RSS，仅供参考。
pub fn proc_rss_gib(proc_root: &Path, pid: u32) -> Option<f64> {
    let text = std::fs::read_to_string(proc_root.join(pid.to_string()).join("status")).ok()?;
    let kb: u64 = text
        .lines()
        .find(|l| l.starts_with("VmRSS:"))?
        .split_whitespace()
        .nth(1)?
        .parse()
        .ok()?;
    Some(kb as f64 / 1048576.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 健康探测顺手带回引擎自报的上下文窗口（接入行 context_window 的
    /// 事实来源；缺失时接入行落保守兜底窗口，每条消息都误触发紧急压缩）。
    #[test]
    fn health_captures_max_model_len() {
        let port = crate::docker::fixtures::serve_models_json_with_len("Fixture-Model", 65536);
        let h = check_health(port, None);
        assert!(h.ok, "detail={}", h.detail);
        assert_eq!(h.model_id.as_deref(), Some("Fixture-Model"));
        assert_eq!(h.model_len, Some(65536));
    }

    #[test]
    fn arg_extraction() {
        let args = vec![
            "tachyon-serve".to_string(),
            "--port".to_string(),
            "18099".to_string(),
            "--model-id".to_string(),
            "M".to_string(),
        ];
        assert_eq!(arg_value(&args, "--port").as_deref(), Some("18099"));
        assert_eq!(arg_value(&args, "--model-id").as_deref(), Some("M"));
        assert!(arg_value(&args, "--missing").is_none());
    }

    #[test]
    fn scan_with_fake_proc() {
        // 假 /proc：一个 exe 指向引擎目录的进程 + 一个指向别处的进程
        let root = std::env::temp_dir().join(format!("cmsd-proc-test-{}", std::process::id()));
        let engine_root = root.join("engines/tachyon-serve");
        let bin_dir = engine_root.join("0f8a4a7c");
        std::fs::create_dir_all(&bin_dir).unwrap();
        std::fs::write(bin_dir.join("tachyon-serve"), b"bin").unwrap();
        let p100 = root.join("proc/100");
        let p200 = root.join("proc/200");
        std::fs::create_dir_all(&p100).unwrap();
        std::fs::create_dir_all(&p200).unwrap();
        std::os::unix::fs::symlink(bin_dir.join("tachyon-serve"), p100.join("exe")).unwrap();
        std::fs::write(
            p100.join("cmdline"),
            b"tachyon-serve\0/m/x.tachyon\0--port\x0018099\0--model-id\0Wild\0",
        )
        .unwrap();
        // stat 字段 22 = starttime：comm 之后 state+17 个字段才是它
        std::fs::write(
            p100.join("stat"),
            "100 (tachyon-serve) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456 20",
        )
        .unwrap();
        std::os::unix::fs::symlink("/usr/bin/bash", p200.join("exe")).unwrap();
        let procs = scan_engine_procs(&root.join("proc"), &engine_root);
        assert_eq!(procs.len(), 1);
        assert_eq!(procs[0].pid, 100);
        assert_eq!(procs[0].sha8, "0f8a4a7c");
        assert_eq!(procs[0].port, Some(18099));
        assert_eq!(procs[0].model_id.as_deref(), Some("Wild"));
        assert_eq!(procs[0].start_time, 123456);
        std::fs::remove_dir_all(&root).ok();
    }
}
