//! models_dir 下「无库记录目录」的只读列举与显式删除。
//!
//! 取消/失败的下载不写库（写库发生在下载脚本成功返回之后），半截权重因此
//! 在任何界面上都不存在——这是下载管理视图「残留文件」分区的数据源。
//!
//! 两条铁律：
//!   * **删除永远显式**。断点续传是刻意设计（download_model.sh 失败分支原话：
//!     「已经下好的部分保留，再点一次下载会从断点接着传」），扫描只列举体积，
//!     删除必须用户逐条确认。
//!   * **宁可漏，不能错**。有库记录的目录、正在下载的目录、软链接、
//!     models_dir 之外或更深一级的路径，一律拒绝——误删几十 GB 权重不可逆，
//!     漏掉一个残留目录的代价只是它多躺一会儿。

use std::collections::HashSet;
use std::path::{Path, PathBuf};

use serde_json::{Value, json};

use super::{config, db};

/// 单次列举的目录数上限。扫描由「扫描」按钮显式触发，不该变成一次无限时的
/// 全盘枚举；真攒到这个量级说明有别的问题，先截断并如实报告 partial。
const MAX_ENTRIES: usize = 512;

/// 「不准碰」的目录集合：库记录派生的权重目录 + 进行中下载的目标目录。
///
/// `unresolved` 是想排除却推导失败的进行中下载个数（物化卡片缺失或解析失败）。
/// 扫描对它只提示「列表可能不完整」；**删除对它整题拒绝**——删错不可逆。
struct Blocked {
    dirs: HashSet<PathBuf>,
    unresolved: usize,
}

impl Blocked {
    fn contains(&self, path: &Path) -> bool {
        if self.dirs.contains(path) {
            return true;
        }
        // 原始形式对不上再比规范化形式：记录里的路径可能是带冗余分量的写法，
        // 目录项是 join 出来的干净形式，硬比较会漏判。
        path.canonicalize().map(|c| self.dirs.contains(&c)).unwrap_or(false)
    }

    fn insert(&mut self, path: PathBuf) {
        self.dirs.insert(path.clone());
        if let Ok(c) = path.canonicalize() {
            self.dirs.insert(c);
        }
    }
}

/// 一次运行回执的最小读取：只要 card_id/kind/outcome 三个字段。完整格式
/// （SavedRun）的 owner 在 service.rs；这里读它只为分辨「下载还没结束」，
/// 读不出来就当不存在——最坏后果是少排除一个目录（漏报），不是误删。
fn non_terminal_install_cards(out: &mut HashSet<String>) {
    let runs_dir = config::data_dir().join("runs");
    let Ok(entries) = std::fs::read_dir(&runs_dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if path.extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let Ok(bytes) = std::fs::read(&path) else { continue };
        let Ok(value) = serde_json::from_slice::<Value>(&bytes) else { continue };
        let kind = value.get("kind").and_then(Value::as_str).unwrap_or("");
        if matches!(kind, "download" | "install_local") && value.get("outcome").is_some_and(Value::is_null) {
            if let Some(card) = value.get("card_id").and_then(Value::as_str) {
                out.insert(card.to_string());
            }
        }
    }
}

/// 未终结的下载/本地安装回执的 run_id（run id 就是回执文件名）。
fn non_terminal_install_run_ids() -> Vec<String> {
    let mut ids = Vec::new();
    let runs_dir = config::data_dir().join("runs");
    let Ok(entries) = std::fs::read_dir(&runs_dir) else {
        return ids;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if path.extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let Ok(bytes) = std::fs::read(&path) else { continue };
        let Ok(value) = serde_json::from_slice::<Value>(&bytes) else { continue };
        let kind = value.get("kind").and_then(Value::as_str).unwrap_or("");
        if matches!(kind, "download" | "install_local") && value.get("outcome").is_some_and(Value::is_null) {
            if let Some(id) = path.file_stem().and_then(|value| value.to_str()) {
                ids.push(id.to_string());
            }
        }
    }
    ids
}

/// 汇总「不准碰」的目录。库记录来自 sqlite；进行中下载来自盘上未终结的安装
/// 回执（下载成功才写库，进行中的下载只有回执）和内存里的活跃运行登记。
fn blocked_dirs(service: Option<&super::service::ModelStoreService>) -> Result<Blocked, String> {
    let settings = config::load_settings();
    let models_dir = settings.models_path();
    let mut blocked = Blocked { dirs: HashSet::new(), unresolved: 0 };

    let records = db::list_store_models().map_err(|error| error.to_string())?;
    for rec in &records {
        // 原生（runtime）/版本化卡片的权重根指向实际隔离目录，原样收进集合；
        // 它可能不在 models_dir 下，只有落在 models_dir 下时才会被列举命中。
        if let Some(root) = &rec.models_root {
            blocked.insert(PathBuf::from(root));
        }
        // 商店卡片的权重目录：卡片脚本统一放 ${LMM_MODELS_DIR}/<HF 仓库名>。
        // 末段必须过 safe_path_component——"x/.." 的末段就是 ".."（
        // lifecycle::candidate_paths 里踩过的坑）。
        if let Some(repo) = rec.source_repo_id.as_deref().and_then(|repo| repo.rsplit('/').next()) {
            if let Ok(safe) = config::safe_path_component(repo) {
                blocked.insert(models_dir.join(safe));
            }
        }
        if let Ok(safe) = config::safe_path_component(&rec.model_name) {
            blocked.insert(models_dir.join(safe));
        }
    }

    // 进行中下载的卡片集合。
    let mut cards: HashSet<String> = HashSet::new();
    if let Some(service) = service {
        for run in service.active_installs() {
            cards.insert(run.card_id);
        }
    }
    non_terminal_install_cards(&mut cards);

    for card in &cards {
        match card_weight_dirs(card, &models_dir) {
            Some(dirs) => {
                for dir in dirs {
                    blocked.insert(dir);
                }
            }
            // 推导不出目标目录就不能假装它不存在：扫描里少排一个目录是漏报，
            // 删除时放行就可能是把正在写的权重目录整棵删掉。
            None => blocked.unresolved += 1,
        }
    }
    Ok(blocked)
}

/// 从物化卡片的 card.json 反推这张卡可能写入的权重目录。
///
/// 与 lifecycle::candidate_paths 同一套启发（HF 仓库名 + 卡片名小写化），
/// 小写化直接复用 lifecycle::lower_model_name，两处不会推出不同的名字。
/// 推不出任何候选返回 None——调用方据此把这次排除记为 unresolved。
fn card_weight_dirs(card_id: &str, models_dir: &Path) -> Option<Vec<PathBuf>> {
    let safe = config::safe_path_component(card_id).ok()?;
    let text = std::fs::read_to_string(config::data_dir().join("cards").join(safe).join("card.json")).ok()?;
    let value: Value = serde_json::from_str(&text).ok()?;
    // 商店卡的元数据有的带 summary 包裹、有的平铺（本地卡），两处都看一眼。
    let summary = value.get("summary").filter(|value| value.is_object()).unwrap_or(&value);
    let mut dirs = Vec::new();
    if let Some(repo) = summary.get("source_repo_id").and_then(Value::as_str).and_then(|repo| repo.rsplit('/').next()) {
        if let Ok(safe) = config::safe_path_component(repo) {
            dirs.push(models_dir.join(safe));
        }
    }
    if let Some(name) = summary.get("name").and_then(Value::as_str) {
        let base = match summary.get("quantization").and_then(Value::as_str) {
            Some(q) => format!("{name}-{q}"),
            None => name.to_string(),
        };
        let lowered = super::lifecycle::lower_model_name(&base);
        if let Ok(safe) = config::safe_path_component(&lowered) {
            dirs.push(models_dir.join(safe));
        }
    }
    if dirs.is_empty() { None } else { Some(dirs) }
}

/// 列举 models_dir 下无库记录的一级目录。只读，不自动触发、不落库。
///
/// 软链接整条跳过（不下钻也不列出）：`remove_dir_all` 穿过软链会删掉链接目标
/// 的内容，这类条目永远不该出现在「可删」列表里。
pub(super) fn scan(service: Option<&super::service::ModelStoreService>) -> Result<Value, String> {
    let settings = config::load_settings();
    let models_dir = settings.models_path();
    let blocked = blocked_dirs(service)?;
    let mut entries = Vec::new();
    let mut partial = false;
    let read = std::fs::read_dir(&models_dir).map_err(|_| "orphan_scan_unavailable".to_string())?;
    for entry in read.flatten() {
        let Ok(file_type) = entry.file_type() else { continue };
        if file_type.is_symlink() || !file_type.is_dir() {
            continue;
        }
        let path = entry.path();
        if blocked.contains(&path) {
            continue;
        }
        if entries.len() >= MAX_ENTRIES {
            partial = true;
            break;
        }
        entries.push(json!({
            "name": entry.file_name().to_string_lossy(),
            "path": path.to_string_lossy(),
            "bytes": super::disk::dir_size_bytes(&path),
        }));
    }
    Ok(json!({
        "entries": entries,
        "partial": partial,
        "unresolvedInFlight": blocked.unresolved,
    }))
}

/// 删除一个残留目录。删除前**当场**复验全部条件（TOCTOU：扫描结果到手之后
/// 世界可能已经变了——用户可能装了模型、可能又点了一次下载）。
pub(super) fn remove(
    service: Option<&super::service::ModelStoreService>,
    path_text: &str,
) -> Result<Value, String> {
    let settings = config::load_settings();
    let models_dir = settings.models_path();
    let target = PathBuf::from(path_text);

    // 1) 只许删 models_dir 的直接子目录：上级路径对不上、路径分量不安全
    //    （".."、绝对路径逃逸）一律拒绝。
    let Some(name) = target.file_name().and_then(|value| value.to_str()) else {
        return Err("orphan_path_unsafe".into());
    };
    config::safe_path_component(name)?;
    if target.parent().map(|parent| parent != models_dir).unwrap_or(true) {
        return Err("orphan_path_unsafe".into());
    }
    // 2) 必须是真实目录，软链接绝对不删——remove_dir_all 会穿过链接删掉目标。
    let meta = std::fs::symlink_metadata(&target).map_err(|_| "orphan_path_unsafe".to_string())?;
    if meta.is_symlink() || !meta.is_dir() {
        return Err("orphan_path_unsafe".into());
    }

    // 3) 不是库记录的目录、不是进行中下载的目标目录。进行中下载有推导失败的
    //    情形时整题拒绝：删错不可逆，等它结束再删只是晚几分钟。
    let blocked = blocked_dirs(service)?;
    if blocked.contains(&target) {
        return Err("orphan_directory_owned".into());
    }
    if blocked.unresolved > 0 {
        return Err("orphan_activity_unresolved".into());
    }

    // 4) 没有任何安装器进程还活着：对所有未终结的下载/本地安装回执逐个做
    //    进程核查。回执是每个下载开工就落盘的，"有回执且进程还活着"说明它
    //    正在写权重——写手不死，一个字节都不能动。
    for run_id in non_terminal_install_run_ids() {
        super::orphans::ensure_run_quiet(&run_id)?;
    }

    let bytes = super::disk::dir_size_bytes(&target);
    std::fs::remove_dir_all(&target)
        .map_err(|error| format!("orphan_remove_failed: {error}"))?;
    Ok(json!({ "freedBytes": bytes }))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 测试用的回执落盘：走 private_write 保持 0600/属主等信任属性，
    /// 否则后续读它的路径会把它当不可信文件。
    fn write_receipt(id: &str, value: &Value) {
        super::super::execution_fence::private_write(
            &config::data_dir().join("runs").join(format!("{id}.json")),
            &serde_json::to_vec(value).unwrap(),
        )
        .unwrap();
    }

    /// 造一个残留目录（无库记录）和一个「有主」目录（名字撞 model_name）。
    fn seed_models_dir() -> (PathBuf, PathBuf) {
        let models = config::load_settings().models_path();
        let orphan = models.join("Half-Downloaded-Model");
        std::fs::create_dir_all(orphan.join(".cache/huggingface/download")).unwrap();
        std::fs::write(orphan.join("config.json"), "{}").unwrap();
        std::fs::write(orphan.join(".cache/huggingface/download/weights.incomplete"), "partial").unwrap();
        let owned = models.join("owned-model");
        std::fs::create_dir_all(&owned).unwrap();
        std::fs::write(owned.join("weights.safetensors"), "weights").unwrap();
        (orphan, owned)
    }

    fn seed_record(model_name: &str) {
        db::init_db().unwrap();
        db::add_store_model(db::AddStoreModelParams {
            card_id: "owned-card",
            model_name,
            card_name: "Owned Model",
            org: None,
            description: None,
            engine_name: "test-engine",
            engine_version: None,
            size_bytes: 1,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: Default::default(),
            card_source: "store",
        })
        .unwrap();
    }

    #[test]
    fn scan_lists_orphans_and_skips_owned_directories_and_symlinks() {
        let _home = config::TempHome::new();
        seed_record("owned-model");
        let (orphan, _owned) = seed_models_dir();
        // 软链接目录：无论指向哪都不许出现在可删列表里。
        let models = config::load_settings().models_path();
        std::os::unix::fs::symlink(&orphan, models.join("linked-model")).unwrap();

        let report = scan(None).unwrap();
        let entries = report["entries"].as_array().unwrap();
        assert_eq!(entries.len(), 1, "只该有残留目录一条：{entries:?}");
        assert_eq!(entries[0]["name"], "Half-Downloaded-Model");
        assert!(entries[0]["bytes"].as_u64().unwrap() > 0);
    }

    #[test]
    fn scan_excludes_in_flight_download_target_from_its_card() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _owned) = seed_models_dir();
        // 一张进行中的下载：未终结回执 + 物化卡片。HF 仓库名 Qwen/Half-Downloaded-Model
        // 的末段正好是造出来的残留目录名——排除必须命中它。
        let card_id = "qwen-half";
        let card_dir = config::data_dir().join("cards").join(card_id);
        std::fs::create_dir_all(&card_dir).unwrap();
        std::fs::write(
            card_dir.join("card.json"),
            r#"{"name":"Qwen","quantization":"NVFP4","source_repo_id":"Qwen/Half-Downloaded-Model"}"#,
        )
        .unwrap();
        write_receipt(
            "in-flight",
            &json!({"card_id":card_id,"kind":"download","created_at":"2026-10-22T00:00:00Z","outcome":null,"cancelled":false,"lines":[]}),
        );

        let report = scan(None).unwrap();
        let entries = report["entries"].as_array().unwrap();
        let orphan_text = orphan.to_string_lossy().into_owned();
        assert!(
            entries.iter().all(|entry| entry["path"] != orphan_text),
            "进行中下载的目标目录不该被列出：{entries:?}"
        );

        // 删除它同样要被拒。
        let error = remove(None, &orphan.to_string_lossy()).err().unwrap();
        assert!(error.contains("owned"), "实际错误：{error}");
    }

    #[test]
    fn scan_reports_unresolved_in_flight_and_remove_refuses_everything() {
        let _home = config::TempHome::new();
        seed_record("owned-model");
        let (orphan, _owned) = seed_models_dir();
        // 进行中的下载，但卡片没物化：推导不出目标目录。
        write_receipt(
            "ghost",
            &json!({"card_id":"ghost-card","kind":"download","created_at":"2026-10-22T00:00:00Z","outcome":null,"cancelled":false,"lines":[]}),
        );

        let report = scan(None).unwrap();
        assert_eq!(report["unresolvedInFlight"], 1, "推导失败必须如实上报");
        assert_eq!(report["entries"].as_array().unwrap().len(), 1, "扫描保持尽力而为");

        // 删除是硬闸：只要有一笔推导不出的进行中下载，整题拒绝。
        let error = remove(None, &orphan.to_string_lossy()).err().unwrap();
        assert!(error.contains("unresolved"), "实际错误：{error}");
    }

    #[test]
    fn remove_deletes_orphan_and_reports_freed_bytes_but_refuses_escape_paths() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _owned) = seed_models_dir();

        let freed = remove(None, &orphan.to_string_lossy()).unwrap();
        assert!(freed["freedBytes"].as_u64().unwrap() > 0);
        assert!(!orphan.exists(), "残留目录应已被删除");

        // 路径逃逸三连：models_dir 本身、上一级、models_dir 之外——全拒绝。
        let models = config::load_settings().models_path();
        for bad in [models.clone(), models.parent().unwrap().to_path_buf(), PathBuf::from("/tmp")] {
            assert!(remove(None, &bad.to_string_lossy()).is_err(), "{bad:?} 不该被放行");
        }
    }
}
