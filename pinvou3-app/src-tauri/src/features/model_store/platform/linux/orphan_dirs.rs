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
//!     models_dir 之外或更深一级的路径，一律拒绝。库记录只有共享根、缺少根或
//!     根无法核实时，整根暂停列举和删除；命名启发与单个 MODEL_DIR 都不能证明
//!     脚本全部输出的归属。误删权重不可逆，漏报只是暂时保留磁盘占用。

use std::collections::HashSet;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Deserializer};
use serde_json::{Value, json};

use super::{config, db};

/// 单次列举的目录数上限。扫描由「扫描」按钮显式触发，不该变成一次无限时的
/// 全盘枚举；真攒到这个量级说明有别的问题，先截断并如实报告 partial。
const MAX_ENTRIES: usize = 512;

/// 「不准碰」的目录集合：库记录派生的权重目录（含其 models_dir 一级祖先）+
/// 进行中下载/版本任务的目标目录。
///
/// `unresolved` 是想排除却推导失败的进行中任务个数（物化卡片缺失或解析失败）。
/// 扫描对它只提示「列表可能不完整」；**删除对它整题拒绝**——删错不可逆。
/// `unresolved_library` 无法证明已入库权重的范围，扫描与删除均整根暂停。
struct Blocked {
    dirs: HashSet<PathBuf>,
    unresolved: usize,
    unresolved_library: Vec<String>,
}

impl Blocked {
    fn protect_library_path(
        &mut self,
        models_dir: &Path,
        scan_root: Option<&Path>,
        card_id: &str,
        root: Option<&str>,
    ) {
        if !library_root_is_separate(root, scan_root) {
            self.unresolved_library.push(card_id.to_owned());
        }
        if let Some(root) = root {
            self.insert(models_dir, PathBuf::from(root));
        }
    }

    fn contains(&self, path: &Path) -> bool {
        if self.dirs.contains(path) {
            return true;
        }
        // 原始形式对不上再比规范化形式：记录里的路径可能是带冗余分量的写法，
        // 目录项是 join 出来的干净形式，硬比较会漏判。
        let Ok(canonical) = path.canonicalize() else {
            return false;
        };
        if self.dirs.contains(&canonical) {
            return true;
        }
        // 祖先链兜底：记录里的隔离权重根也要保护其子目录。
        canonical
            .ancestors()
            .skip(1)
            .any(|ancestor| self.dirs.contains(ancestor))
    }

    fn insert(&mut self, models_dir: &Path, path: PathBuf) {
        let canonical = path.canonicalize().ok();
        // All sources, including the legacy runtime fallback, can return the
        // shared root. It is not an exclusive model directory.
        if path == models_dir
            || canonical
                .as_ref()
                .zip(models_dir.canonicalize().ok().as_ref())
                .is_some_and(|(path, root)| path == root)
        {
            return;
        }
        self.insert_exact(path.clone());
        if let Some(c) = &canonical {
            self.insert_exact(c.clone());
        }
        // 一级祖先也要拦：库记录可能指向**深层**目录（版本化权重在
        // models_dir/.versions/<card>/<revision>，记录的 models_root 指向
        // revision 层）。精确匹配拦不住它的祖先 .versions——而不拦祖先，
        // 整棵 .versions 会被当成一个"无主目录"列出来、然后被连根删掉。
        //
        // 原始形式与规范化形式各推一遍：models_dir 本身可能含符号链接分量
        // （用户可配置），此时 legacy/规范化的路径对原始 models_dir 的
        // strip_prefix 会失败，只推一遍就漏掉那个祖先。
        self.insert_first_level_ancestor(models_dir, &path);
        if let (Some(c), Ok(canon_models)) = (&canonical, models_dir.canonicalize()) {
            self.insert_first_level_ancestor(&canon_models, c);
        }
    }

    fn insert_first_level_ancestor(&mut self, root: &Path, path: &Path) {
        if let Ok(relative) = path.strip_prefix(root) {
            if let Some(first) = relative.components().next() {
                self.insert_exact(root.join(first));
            }
        }
    }

    fn insert_exact(&mut self, path: PathBuf) {
        self.dirs.insert(path);
    }
}

/// 安装类回执的 kind 白名单：这些任务还没写库（或写到库记录推导不到的深目录），
/// 目标目录必须从卡片元数据反推并排除。version_stage 也算——它把新版本权重
/// 下载进 models_dir/.versions，同样没有任何"已入库目录"可拦。
const IN_FLIGHT_KINDS: [&str; 3] = ["download", "install_local", "version_stage"];

pub(super) fn is_in_flight_kind(kind: &str) -> bool {
    IN_FLIGHT_KINDS.contains(&kind)
}

/// Only activity metadata is needed here. In particular, historical log lines
/// must be skipped by the streaming deserializer, not allocated as a Value tree.
#[derive(Deserialize)]
struct InstallActivity {
    card_id: Option<String>,
    kind: Option<String>,
    #[serde(default, rename = "outcome", deserialize_with = "pending_outcome")]
    pending: bool,
}

fn pending_outcome<'de, D: Deserializer<'de>>(deserializer: D) -> Result<bool, D::Error> {
    // A missing outcome defaults to false, while an explicit null denotes an
    // unfinished receipt. Non-null result payloads need no materialization.
    Option::<serde::de::IgnoredAny>::deserialize(deserializer).map(|value| value.is_none())
}

/// 未终结的安装类回执的卡片集合（card_id -> kind）。
fn non_terminal_install_cards(out: &mut std::collections::HashMap<String, String>) -> usize {
    let runs_dir = config::data_dir().join("runs");
    let entries = match std::fs::read_dir(&runs_dir) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return 0,
        Err(_) => return 1,
    };
    let mut unresolved = 0;
    for entry in entries {
        let Ok(entry) = entry else {
            unresolved += 1;
            continue;
        };
        let path = entry.path();
        if path.extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let Ok(file) = std::fs::File::open(&path) else {
            unresolved += 1;
            continue;
        };
        let Ok(receipt) =
            serde_json::from_reader::<_, InstallActivity>(std::io::BufReader::new(file))
        else {
            unresolved += 1;
            continue;
        };
        let kind = receipt.kind.as_deref().unwrap_or("");
        if is_in_flight_kind(kind) && receipt.pending {
            if let Some(card) = receipt.card_id {
                out.insert(card, kind.to_string());
            } else {
                unresolved += 1;
            }
        }
    }
    unresolved
}

/// A recorded download root protects its entire subtree. If it covers the scan
/// root, names cannot tell which children belong to the model. Missing or
/// unreadable roots are likewise unknown; a single deployed MODEL_DIR is not a
/// complete download-output manifest and must not reopen this cleanup gate.
fn library_root_is_separate(root: Option<&str>, scan_root: Option<&Path>) -> bool {
    let Some(root) = root.map(Path::new).filter(|root| root.is_absolute()) else {
        return false;
    };
    match (root.canonicalize(), scan_root) {
        (Ok(root), Some(scan_root)) if root.is_dir() => !scan_root.starts_with(root),
        _ => false,
    }
}

/// 汇总「不准碰」的目录。库记录来自 sqlite；进行中下载来自盘上未终结的安装
/// 回执（下载成功才写库，进行中的下载只有回执）和内存里的活跃运行登记。
/// 根目录沿用调用方固定的快照；设置可并发保存，不能把旧根的扫描/删除目标
/// 与新根推导的归属排除集混用。缺少库记录根时仍整根暂停。
fn blocked_dirs(
    service: Option<&super::service::ModelStoreService>,
    models_dir: &Path,
) -> Result<Blocked, String> {
    let mut blocked = Blocked {
        dirs: HashSet::new(),
        unresolved: 0,
        unresolved_library: Vec::new(),
    };

    let scan_root = models_dir.canonicalize().ok();
    let records = db::list_store_models().map_err(|error| error.to_string())?;
    for rec in &records {
        // 普通下载只登记共享根，不能把名字猜测当成权重归属证明。上面的
        // 未核实记录会暂停整根清理；独立/版本化根继续完整保护其子树。
        blocked.protect_library_path(
            models_dir,
            scan_root.as_deref(),
            &rec.card_id,
            rec.models_root.as_deref(),
        );
        // 旧记录的权威实际目录：部署脚本写在卡片 .deploy-env 里的 MODEL_DIR，
        // 与 candidate_paths 同一份真相，不能在扫描里再猜一遍。
        if let Some(actual) = super::lifecycle::legacy_resource_dir_for_scan(rec, models_dir) {
            blocked.insert(models_dir, actual);
        }
        // 名字和仓库名只是额外保守排除，不能证明脚本的实际输出目录。
        // 末段必须过 safe_path_component——"x/.." 的末段就是 ".."（
        // lifecycle::candidate_paths 里踩过的坑）。
        if let Some(repo) = rec
            .source_repo_id
            .as_deref()
            .and_then(|repo| repo.rsplit('/').next())
        {
            if let Ok(safe) = config::safe_path_component(repo) {
                blocked.insert(models_dir, models_dir.join(safe));
            }
        }
        if let Ok(safe) = config::safe_path_component(&rec.model_name) {
            blocked.insert(models_dir, models_dir.join(safe));
        }
    }

    // Switching moves the store row to the new managed root, but the old
    // rollback weights remain referenced only by model_versions. Protect every
    // state, including incomplete/deleting rows, until version cleanup removes
    // the reference. Neither resource_dir nor a saved deployment root may be
    // replaced by naming heuristics. Unverifiable references pause cleanup.
    for version in super::version_store::list(None)? {
        for root in [
            version.models_root.as_deref(),
            version.resource_dir.as_deref(),
        ] {
            blocked.protect_library_path(models_dir, scan_root.as_deref(), &version.card_id, root);
        }
        if let Some(record) = &version.record {
            blocked.protect_library_path(
                models_dir,
                scan_root.as_deref(),
                &version.card_id,
                record.models_root.as_deref(),
            );
        }
    }

    // 进行中任务的卡片集合（card_id -> kind）。version_stage 的写入点不走
    // 卡片名/HF 仓库名启发，单独推导。
    let mut cards: std::collections::HashMap<String, String> = std::collections::HashMap::new();
    if let Some(service) = service {
        for run in service.active_installs() {
            cards.insert(run.card_id, run.kind.to_string());
        }
    }
    blocked.unresolved += non_terminal_install_cards(&mut cards);

    for (card, kind) in &cards {
        let dirs = if kind == "version_stage" {
            // 版本暂存写进 models_dir/.versions/<card>/<revision>。按名字/仓库
            // 启发推导只会遮住一个可能无关的一级目录；它的真实目标在这里精确
            // 排除（.versions 本身另有点开头规则兜底）。
            config::safe_path_component(card)
                .ok()
                .map(|safe| vec![models_dir.join(".versions").join(safe)])
        } else {
            card_weight_dirs(card, models_dir)
        };
        match dirs {
            Some(dirs) => {
                for dir in dirs {
                    blocked.insert(models_dir, dir);
                }
            }
            // 推导不出目标目录就不能假装它不存在：扫描里少排一个目录是漏报，
            // 删除时放行就可能是把正在写的权重目录整棵删掉。
            None => blocked.unresolved += 1,
        }
    }
    // cmsd can accept an install before its receiver creates a local run or
    // takes the resource lock. Its target is not yet proven by local receipts;
    // retain the same fail-closed exclusion as incomplete-install dismissal.
    let (pending, pending_error) = super::delegated_run::pending(None);
    blocked.unresolved += pending.len() + usize::from(pending_error.is_some());
    blocked.unresolved_library.sort();
    blocked.unresolved_library.dedup();
    Ok(blocked)
}

/// 从物化卡片的 card.json 反推这张卡可能写入的权重目录。
///
/// 与 lifecycle::candidate_paths 同一套启发（HF 仓库名 + 卡片名小写化），
/// 小写化直接复用 lifecycle::lower_model_name，两处不会推出不同的名字。
/// 推不出任何候选返回 None——调用方据此把这次排除记为 unresolved。
fn card_weight_dirs(card_id: &str, models_dir: &Path) -> Option<Vec<PathBuf>> {
    let safe = config::safe_path_component(card_id).ok()?;
    let text = std::fs::read_to_string(
        config::data_dir()
            .join("cards")
            .join(safe)
            .join("card.json"),
    )
    .ok()?;
    let value: Value = serde_json::from_str(&text).ok()?;
    // 商店卡的元数据有的带 summary 包裹、有的平铺（本地卡），两处都看一眼。
    let summary = value
        .get("summary")
        .filter(|value| value.is_object())
        .unwrap_or(&value);
    let mut dirs = Vec::new();
    if let Some(repo) = summary
        .get("source_repo_id")
        .and_then(Value::as_str)
        .and_then(|repo| repo.rsplit('/').next())
    {
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
        // 空名回退与 suggest_model_name 对齐（它回退卡片 id）：名字整体是
        // 非法字符时目录名就是 card_id，不回退会让这类卡的进行中目录漏排。
        let candidate = if lowered.is_empty() {
            card_id.to_string()
        } else {
            lowered
        };
        if let Ok(safe) = config::safe_path_component(&candidate) {
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
    // SAFETY: geteuid has no preconditions. Bind ownership to this process,
    // never to a caller-supplied API field or the configured shared root.
    scan_for_user(service, unsafe { libc::geteuid() })
}

fn scan_for_user(
    service: Option<&super::service::ModelStoreService>,
    owner_uid: u32,
) -> Result<Value, String> {
    let settings = config::load_settings();
    let models_dir = settings.models_path();
    let blocked = blocked_dirs(service, &models_dir)?;
    if !blocked.unresolved_library.is_empty() {
        // Do not label unproven library weights as leftovers, even read-only.
        // remove() independently repeats this check under the resource lock.
        return Ok(json!({
            "entries": [], "partial": false,
            "unresolvedInFlight": blocked.unresolved,
            "unresolvedLibraryCards": blocked.unresolved_library,
        }));
    }
    let mut entries = Vec::new();
    let mut partial = false;
    let read = std::fs::read_dir(&models_dir).map_err(|_| "orphan_scan_unavailable".to_string())?;
    for entry in read {
        let Ok(entry) = entry else {
            partial = true;
            continue;
        };
        let Ok(file_type) = entry.file_type() else {
            partial = true;
            continue;
        };
        if file_type.is_symlink() || !file_type.is_dir() {
            continue;
        }
        // 点开头的目录是商店的内部结构（如版本化的 .versions），不是任何卡片
        // 脚本的权重目录命名形态，整类不出现在列表里——它们由库记录的一级
        // 祖先规则保护（见 Blocked::insert），这里再兜一层。
        let name = entry.file_name();
        let Some(name) = name.to_str() else {
            // The public command accepts UTF-8 paths; never offer a lossy path
            // that cannot identify the same directory when deletion is confirmed.
            partial = true;
            continue;
        };
        if name.starts_with('.') {
            continue;
        }
        let path = entry.path();
        let Ok(meta) = std::fs::symlink_metadata(&path) else {
            partial = true;
            continue;
        };
        if meta.is_symlink() || !meta.is_dir() {
            continue;
        }
        if meta.uid() != owner_uid {
            partial = true;
            continue;
        }
        if blocked.contains(&path) {
            continue;
        }
        if entries.len() >= MAX_ENTRIES {
            partial = true;
            break;
        }
        entries.push(json!({
            "name": name,
            "path": path.to_string_lossy(),
            "bytes": super::disk::dir_size_bytes(&path),
        }));
    }
    Ok(json!({
        "entries": entries,
        "partial": partial,
        "unresolvedInFlight": blocked.unresolved,
        "unresolvedLibraryCards": blocked.unresolved_library,
    }))
}

/// 删除一个残留目录。删除前**当场**复验全部条件（TOCTOU：扫描结果到手之后
/// 世界可能已经变了——用户可能装了模型、可能又点了一次下载）。
pub(super) fn remove(
    service: Option<&super::service::ModelStoreService>,
    path_text: &str,
) -> Result<Value, String> {
    // SAFETY: geteuid has no preconditions; API callers cannot choose this UID.
    remove_for_user(service, path_text, unsafe { libc::geteuid() })
}

fn remove_for_user(
    service: Option<&super::service::ModelStoreService>,
    path_text: &str,
    owner_uid: u32,
) -> Result<Value, String> {
    remove_for_user_with_recheck(service, path_text, owner_uid, || {})
}

// Private injection point for the directory-replacement regression. Production
// always supplies a no-op; the same validation/removal path runs in both cases.
fn remove_for_user_with_recheck(
    service: Option<&super::service::ModelStoreService>,
    path_text: &str,
    owner_uid: u32,
    before_recheck: impl FnOnce(),
) -> Result<Value, String> {
    let settings = config::load_settings();
    let models_dir = settings.models_path();
    let target = PathBuf::from(path_text);

    // 1) 只许删 models_dir 的直接子目录：上级路径对不上、路径分量不安全
    //    （".."、绝对路径逃逸）一律拒绝。点开头的名字同样拒绝——那是商店
    //    的内部目录（.versions 等），不属于"残留"，删了会端掉全部版本化权重。
    let Some(name) = target.file_name().and_then(|value| value.to_str()) else {
        return Err("orphan_path_unsafe".into());
    };
    if name.starts_with('.') {
        return Err("orphan_path_unsafe".into());
    }
    if target
        .parent()
        .map(|parent| parent != models_dir)
        .unwrap_or(true)
    {
        return Err("orphan_path_unsafe".into());
    }
    // 2) 必须是真实目录，软链接绝对不删——remove_dir_all 会穿过链接删掉目标。
    let meta = std::fs::symlink_metadata(&target).map_err(|_| "orphan_path_unsafe".to_string())?;
    if meta.is_symlink() || !meta.is_dir() {
        return Err("orphan_path_unsafe".into());
    }
    if meta.uid() != owner_uid {
        return Err("orphan_directory_other_user".into());
    }

    // 与下载、本地安装及版本暂存共用资源锁；从归属核查到删除结束始终排他，
    // 避免终验后又有写入方启动。已有写入方持共享锁时立即拒绝，供用户稍后重试。
    let _resources = super::version_store::resource_operation_lock(true)
        .map_err(|error| format!("orphan_resources_busy: {error}"))?;

    // 3) 不是库记录的目录、不是进行中下载的目标目录。进行中下载有推导失败的
    //    情形时整题拒绝：删错不可逆，等它结束再删只是晚几分钟。
    let blocked = blocked_dirs(service, &models_dir)?;
    if !blocked.unresolved_library.is_empty() {
        return Err(format!(
            "orphan_library_ownership_unresolved: {}",
            blocked.unresolved_library.join(", ")
        ));
    }
    if blocked.contains(&target) {
        return Err("orphan_directory_owned".into());
    }
    if blocked.unresolved > 0 {
        return Err("orphan_activity_unresolved".into());
    }

    // 4) 全量核查安装器进程，不依赖回执可读性或 run_id 关联；崩溃后即使
    //    回执已损坏/丢失，也不能跳过仍在写权重的脚本进程。
    super::orphans::ensure_host_quiet()?;

    // Keep measurement inside the exclusion window: supported resource writers
    // cannot change the tree between ownership validation and unlink. A cold
    // tree with many small files can hold this lock for minutes or hours; other
    // resource operations wait cancellably. This is an explicit safety cost.
    let bytes = super::disk::dir_size_bytes(&target);
    before_recheck();
    // Size traversal may take time. Recheck ownership and directory identity
    // immediately before deletion, including changes by another profile.
    let current =
        std::fs::symlink_metadata(&target).map_err(|_| "orphan_path_unsafe".to_string())?;
    if current.uid() != owner_uid {
        return Err("orphan_directory_other_user".into());
    }
    if current.is_symlink()
        || !current.is_dir()
        || current.dev() != meta.dev()
        || current.ino() != meta.ino()
    {
        return Err("orphan_path_unsafe".into());
    }
    std::fs::remove_dir_all(&target).map_err(|error| format!("orphan_remove_failed: {error}"))?;
    Ok(json!({ "freedBytes": bytes }))
}

#[cfg(test)]
mod tests {
    use super::super::service::ModelStoreService;
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
        std::fs::write(
            orphan.join(".cache/huggingface/download/weights.incomplete"),
            "partial",
        )
        .unwrap();
        let owned = models.join("owned-model");
        std::fs::create_dir_all(&owned).unwrap();
        std::fs::write(owned.join("weights.safetensors"), "weights").unwrap();
        (orphan, owned)
    }

    fn seed_record(model_name: &str) {
        let root = config::load_settings().models_path().join(model_name);
        std::fs::create_dir_all(&root).unwrap();
        seed_record_at_root(model_name, Some(&root));
    }

    fn seed_record_at_root(model_name: &str, root: Option<&Path>) {
        db::init_db().unwrap();
        let params = db::AddStoreModelParams {
            card_id: "owned-card",
            model_name,
            card_name: "Owned Model",
            org: None,
            description: None,
            engine_name: "test-engine",
            engine_version: None,
            size_bytes: 1,
            quantization: None,
            source_repo_id: Some("example/owned-repo"),
            source_filename: None,
            scripts: &Default::default(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: Default::default(),
            card_source: "store",
        };
        if let Some(root) = root {
            db::add_store_model_with_root(params, &root.to_string_lossy()).unwrap();
        } else {
            db::add_store_model(params).unwrap();
        }
    }

    fn seed_version(
        root: Option<&Path>,
        resource: Option<&Path>,
    ) -> super::super::version_store::Version {
        let version = super::super::version_store::Version {
            card_id: "archived-card".into(),
            version_id: "old-revision".into(),
            state: "history".into(),
            record: None,
            models_root: root.map(|path| path.to_string_lossy().into_owned()),
            runtime_card_id: "old-runtime".into(),
            resource_dir: resource.map(|path| path.to_string_lossy().into_owned()),
            managed: false,
            created_at: "2026-10-18T00:00:00Z".into(),
            error: None,
            planned_bytes: 0,
            measured_bytes: None,
        };
        super::super::version_store::put(&version).unwrap();
        version
    }

    #[test]
    fn every_version_state_protects_recorded_roots_resources_and_their_ancestors() {
        let _home = config::TempHome::new();
        let (orphan, _) = seed_models_dir();
        seed_record("owned-model");
        let models = config::load_settings().models_path();
        let root = models.join("archived-root/nested");
        let resource = models.join("Qwen-Custom-Old-Weights/nested");
        for dir in [&root, &resource] {
            std::fs::create_dir_all(dir).unwrap();
            std::fs::write(dir.join("weights"), b"rollback weights").unwrap();
        }
        let mut version = seed_version(Some(&root), Some(&resource));
        for state in [
            "history",
            "current",
            "ready",
            "downloading",
            "failed",
            "interrupted",
            "deleting",
            "future-state",
        ] {
            version.state = state.into();
            super::super::version_store::put(&version).unwrap();
            let report = scan(None).unwrap();
            assert_eq!(report["unresolvedLibraryCards"], json!([]));
            assert_eq!(
                report["entries"].as_array().unwrap().len(),
                1,
                "{state}: {report}"
            );
            assert_eq!(
                report["entries"][0]["path"],
                orphan.to_string_lossy().as_ref()
            );
            for dir in [root.parent().unwrap(), resource.parent().unwrap()] {
                assert_eq!(
                    remove(None, dir.to_str().unwrap()).unwrap_err(),
                    "orphan_directory_owned",
                    "{state}"
                );
            }
        }
        remove(None, orphan.to_str().unwrap()).unwrap();
        for dir in [&root, &resource] {
            assert_eq!(
                std::fs::read(dir.join("weights")).unwrap(),
                b"rollback weights"
            );
        }
    }

    #[test]
    fn unverified_version_paths_pause_cleanup_including_saved_record_roots() {
        for field in ["models_root", "resource_dir", "record_root"] {
            for invalid in ["missing", "shared", "relative", "absent", "file"] {
                let _home = config::TempHome::new();
                let (orphan, _) = seed_models_dir();
                seed_record("owned-model");
                let models = config::load_settings().models_path();
                let valid = models.join("old-weights");
                std::fs::create_dir_all(&valid).unwrap();
                let file = models.join("not-a-directory");
                std::fs::write(&file, b"keep").unwrap();
                let bad = match invalid {
                    "missing" => None,
                    "shared" => Some(models.to_string_lossy().into_owned()),
                    "relative" => Some("old-weights".to_owned()),
                    "absent" => Some(models.join("absent").to_string_lossy().into_owned()),
                    "file" => Some(file.to_string_lossy().into_owned()),
                    _ => unreachable!(),
                };
                let mut version = seed_version(Some(&valid), Some(&valid));
                match field {
                    "models_root" => version.models_root = bad,
                    "resource_dir" => version.resource_dir = bad,
                    "record_root" => {
                        let mut record = db::get_store_model("owned-card").unwrap().unwrap();
                        record.models_root = bad;
                        version.record = Some(record);
                    }
                    _ => unreachable!(),
                }
                super::super::version_store::put(&version).unwrap();
                let report = scan(None).unwrap();
                assert!(
                    report["entries"].as_array().unwrap().is_empty(),
                    "{field}/{invalid}"
                );
                assert_eq!(report["unresolvedLibraryCards"], json!(["archived-card"]));
                assert!(
                    remove(None, orphan.to_str().unwrap())
                        .unwrap_err()
                        .starts_with("orphan_library_ownership_unresolved:")
                );
                assert!(orphan.join("config.json").exists());
            }
        }
    }

    #[test]
    fn version_references_added_after_scanning_block_removal_and_corruption_fails_closed() {
        let _home = config::TempHome::new();
        let (orphan, _) = seed_models_dir();
        seed_record("owned-model");
        assert_eq!(scan(None).unwrap()["entries"].as_array().unwrap().len(), 1);
        let root = config::data_dir().join("old-independent-root");
        std::fs::create_dir_all(&root).unwrap();
        seed_version(Some(&root), Some(&orphan));
        assert_eq!(
            remove(None, orphan.to_str().unwrap()).unwrap_err(),
            "orphan_directory_owned"
        );
        db::connect()
            .unwrap()
            .execute("UPDATE model_versions SET record_json='{'", [])
            .unwrap();
        assert!(scan(None).is_err());
        assert!(remove(None, orphan.to_str().unwrap()).is_err());
        assert!(orphan.join("config.json").exists());
    }

    #[test]
    fn replacing_a_directory_during_measurement_cannot_delete_the_replacement() {
        for symlink in [false, true] {
            let _home = config::TempHome::new();
            db::init_db().unwrap();
            let (candidate, _) = seed_models_dir();
            let retained = candidate.with_file_name("original-retained");
            let unrelated = candidate.with_file_name("replacement-source");
            std::fs::create_dir_all(&unrelated).unwrap();
            std::fs::write(unrelated.join("sentinel"), b"unrelated files").unwrap();
            let error = remove_for_user_with_recheck(
                None,
                candidate.to_str().unwrap(),
                unsafe { libc::geteuid() },
                || {
                    std::fs::rename(&candidate, &retained).unwrap();
                    if symlink {
                        std::os::unix::fs::symlink(&unrelated, &candidate).unwrap();
                    } else {
                        std::fs::create_dir(&candidate).unwrap();
                        std::fs::write(candidate.join("sentinel"), b"replacement files").unwrap();
                    }
                },
            )
            .unwrap_err();
            assert_eq!(error, "orphan_path_unsafe");
            assert_eq!(std::fs::read(retained.join("config.json")).unwrap(), b"{}");
            assert_eq!(
                std::fs::read(unrelated.join("sentinel")).unwrap(),
                b"unrelated files"
            );
            if !symlink {
                assert_eq!(
                    std::fs::read(candidate.join("sentinel")).unwrap(),
                    b"replacement files"
                );
            }
        }
    }

    #[test]
    fn root_snapshot_keeps_ownership_and_active_paths_stable_after_settings_change() {
        for recorded_root in ["shared", "missing", "separate"] {
            let _home = config::TempHome::new();
            let (orphan, owned) = seed_models_dir();
            let models = config::load_settings().models_path();
            let root = match recorded_root {
                "shared" => Some(models.as_path()),
                "missing" => None,
                _ => Some(owned.as_path()),
            };
            seed_record_at_root("owned-model", root);
            let card = config::data_dir().join("cards/active-card");
            std::fs::create_dir_all(&card).unwrap();
            std::fs::write(
                card.join("card.json"),
                r#"{"name":"Active Model","source_repo_id":"org/active-repo"}"#,
            )
            .unwrap();
            write_receipt(
                "run-before-storage-change",
                &json!({"card_id":"active-card","kind":"download","outcome":null}),
            );

            // Simulate a settings save after scan/remove has fixed its root,
            // before the exclusion set reads library and activity metadata.
            let next_root = config::data_dir().join("new-models");
            super::super::service::set_storage_settings(
                Some(next_root.to_str().unwrap().to_owned()),
                None,
                None,
            )
            .unwrap();
            let blocked = blocked_dirs(None, &models).unwrap();
            assert_eq!(
                blocked.unresolved_library,
                if recorded_root == "separate" {
                    Vec::<String>::new()
                } else {
                    vec!["owned-card".to_string()]
                }
            );
            assert!(blocked.contains(&owned));
            assert!(blocked.contains(&models.join("active-repo")));
            assert!(blocked.contains(&models.join("active-model")));
            assert!(!blocked.contains(&next_root.join("active-repo")));
            assert!(orphan.join("config.json").exists());
            assert_eq!(
                std::fs::read(owned.join("weights.safetensors")).unwrap(),
                b"weights"
            );
        }
    }

    #[test]
    fn shared_download_root_pauses_cleanup_for_real_card_directory_mismatches() {
        for linked_root in [false, true] {
            let _home = config::TempHome::new();
            if linked_root {
                let real = config::data_dir().join("real-models");
                std::fs::create_dir_all(&real).unwrap();
                std::os::unix::fs::symlink(&real, config::data_dir().join("models")).unwrap();
            }
            let (orphan, owned) = seed_models_dir();
            let models = config::load_settings().models_path();
            let repo_owned = models.join("owned-repo");
            std::fs::create_dir_all(&repo_owned).unwrap();
            // Real scripts choose raw case, quantization and extra suffixes;
            // none of these matches this record's model/repository heuristics.
            let actual = models.join("Qwen3.8-27B-NVFP4-unsloth");
            std::fs::create_dir_all(&actual).unwrap();
            std::fs::write(actual.join("weights.bin"), b"registered weights").unwrap();
            // 与普通下载成功的落库路径一致：共享根以 canonical 形式登记。
            seed_record_at_root("owned-model", Some(&models.canonicalize().unwrap()));

            let report = scan(None).unwrap();
            let entries = report["entries"].as_array().unwrap();
            assert!(entries.is_empty(), "归属未核实时不得生成残留候选：{report}");
            assert_eq!(report["unresolvedLibraryCards"], json!(["owned-card"]));
            assert_eq!(report["unresolvedInFlight"], 0);
            for protected in [&owned, &repo_owned, &actual, &orphan] {
                let error = remove(None, &protected.to_string_lossy()).unwrap_err();
                assert!(error.starts_with("orphan_library_ownership_unresolved:"));
                assert!(protected.exists());
            }
            assert_eq!(
                std::fs::read(actual.join("weights.bin")).unwrap(),
                b"registered weights"
            );
            assert_eq!(
                db::get_store_model("owned-card").unwrap().unwrap().status,
                "downloaded"
            );
        }
    }

    #[test]
    fn legacy_runtime_shared_root_fallback_does_not_establish_ownership() {
        for linked in [false, true] {
            let _home = config::TempHome::new();
            if linked {
                let real = config::data_dir().join("real-models");
                std::fs::create_dir_all(&real).unwrap();
                std::os::unix::fs::symlink(real, config::data_dir().join("models")).unwrap();
            }
            let (orphan, owned) = seed_models_dir();
            let models = config::load_settings().models_path();
            seed_record_at_root("owned-model", Some(&models.canonicalize().unwrap()));
            db::connect().unwrap().execute(
                "UPDATE store_models SET runtime_card_id='legacy-runtime' WHERE card_id='owned-card'", [],
            ).unwrap();
            let record = db::get_store_model("owned-card").unwrap().unwrap();
            assert_eq!(
                super::super::lifecycle::legacy_resource_dir_for_scan(&record, &models),
                Some(models.canonicalize().unwrap()),
                "fixture must exercise the legacy shared-root fallback"
            );
            let report = scan(None).unwrap();
            assert!(report["entries"].as_array().unwrap().is_empty());
            assert_eq!(report["unresolvedLibraryCards"], json!(["owned-card"]));
            assert!(
                remove(None, &orphan.to_string_lossy())
                    .unwrap_err()
                    .starts_with("orphan_library_ownership_unresolved:")
            );
            assert!(orphan.exists());
            assert!(owned.exists());
        }
    }

    #[test]
    fn one_deployed_directory_is_not_a_complete_shared_root_manifest() {
        let _home = config::TempHome::new();
        let (orphan, _) = seed_models_dir();
        let models = config::load_settings().models_path();
        seed_record_at_root("owned-model", Some(&models.canonicalize().unwrap()));
        db::connect().unwrap().execute(
            "UPDATE store_models SET runtime_card_id='legacy-runtime' WHERE card_id='owned-card'", [],
        ).unwrap();
        let actual = models.join("custom-legacy-root").join("revision");
        std::fs::create_dir_all(&actual).unwrap();
        std::fs::write(actual.join("weights.bin"), b"registered weights").unwrap();
        let runtime = config::data_dir().join("cards/legacy-runtime");
        std::fs::create_dir_all(&runtime).unwrap();
        std::fs::write(
            runtime.join(".deploy-env"),
            format!("MODEL_DIR=\"{}\"\n", actual.display()),
        )
        .unwrap();

        let report = scan(None).unwrap();
        assert!(report["entries"].as_array().unwrap().is_empty());
        assert_eq!(report["unresolvedLibraryCards"], json!(["owned-card"]));
        assert!(
            blocked_dirs(None, &models)
                .unwrap()
                .contains(&models.join("custom-legacy-root"))
        );
        assert!(
            remove(None, &models.join("custom-legacy-root").to_string_lossy())
                .unwrap_err()
                .starts_with("orphan_library_ownership_unresolved:")
        );
        assert_eq!(
            std::fs::read(actual.join("weights.bin")).unwrap(),
            b"registered weights"
        );
        assert!(remove(None, &orphan.to_string_lossy()).is_err());
        assert!(orphan.exists());
    }

    #[test]
    fn absent_unreadable_or_ancestor_library_roots_pause_cleanup() {
        for root_kind in ["absent", "missing", "file", "relative", "ancestor"] {
            let _home = config::TempHome::new();
            let (orphan, _) = seed_models_dir();
            let models = config::load_settings().models_path();
            let root = match root_kind {
                "absent" => None,
                "missing" => Some(models.join("no-longer-readable-root")),
                "file" => {
                    let file = models.join("invalid-root.bin");
                    std::fs::write(&file, b"not a directory").unwrap();
                    Some(file)
                }
                "relative" => Some(PathBuf::from("relative-root")),
                _ => Some(models.parent().unwrap().to_path_buf()),
            };
            seed_record_at_root("unrelated-name", root.as_deref());
            let report = scan(None).unwrap();
            assert!(
                report["entries"].as_array().unwrap().is_empty(),
                "{root_kind}"
            );
            assert_eq!(report["unresolvedLibraryCards"], json!(["owned-card"]));
            assert!(
                remove(None, &orphan.to_string_lossy())
                    .unwrap_err()
                    .starts_with("orphan_library_ownership_unresolved:")
            );
            assert!(orphan.exists());
        }
    }

    #[test]
    fn separate_library_roots_protect_their_subtree_and_allow_other_leftovers() {
        for outside in [false, true] {
            let _home = config::TempHome::new();
            let (orphan, _) = seed_models_dir();
            let models = config::load_settings().models_path();
            let root = if outside {
                config::data_dir().join("separate-library-root")
            } else {
                models.join("Actual-Weights-With-A-Custom-Name")
            };
            std::fs::create_dir_all(root.join("extra-repo")).unwrap();
            std::fs::write(root.join("extra-repo/weights.bin"), b"owned").unwrap();
            seed_record_at_root("owned-model", Some(&root));
            let report = scan(None).unwrap();
            assert_eq!(report["unresolvedLibraryCards"], json!([]));
            assert_eq!(report["entries"].as_array().unwrap().len(), 1);
            assert_eq!(
                report["entries"][0]["path"],
                orphan.to_string_lossy().as_ref()
            );
            if !outside {
                assert_eq!(
                    remove(None, &root.to_string_lossy()).unwrap_err(),
                    "orphan_directory_owned"
                );
            }
            remove(None, &orphan.to_string_lossy()).unwrap();
            assert!(!orphan.exists());
            assert_eq!(
                std::fs::read(root.join("extra-repo/weights.bin")).unwrap(),
                b"owned"
            );
        }
    }

    #[test]
    fn library_ownership_is_rechecked_when_deleting_a_stale_scan_candidate() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (candidate, _) = seed_models_dir();
        let report = scan(None).unwrap();
        assert!(
            report["entries"]
                .as_array()
                .unwrap()
                .iter()
                .any(|entry| entry["path"] == candidate.to_string_lossy().as_ref())
        );
        let models = config::load_settings().models_path();
        seed_record_at_root("newly-downloaded-model", Some(&models));
        assert!(
            remove(None, &candidate.to_string_lossy())
                .unwrap_err()
                .starts_with("orphan_library_ownership_unresolved:")
        );
        assert!(candidate.join("config.json").exists());
    }

    #[test]
    fn unmaterialized_delegated_installs_block_removal_until_a_terminal_observation() {
        use std::io::{BufRead, BufReader, Write};
        use std::os::unix::net::UnixListener;
        use std::time::{Duration, Instant};

        for status in [
            "queued",
            "running",
            "succeeded",
            "missing",
            "failed",
            "cancelled",
        ] {
            let _home = config::TempHome::new();
            db::init_db().unwrap();
            let (orphan, _) = seed_models_dir();
            let socket = config::data_dir().join("orphan-parent.sock");
            let listener = UnixListener::bind(&socket).unwrap();
            listener.set_nonblocking(true).unwrap();
            let client = crate::features::megacube::Client::at_socket(socket).unwrap();
            super::super::delegated_run::save(
                &client,
                "parent",
                "retry-card",
                "download",
                "device-fixture",
            )
            .unwrap();
            let binding = config::data_dir().join("delegated-runs/cmsd-parent.json");
            let before = std::fs::read(&binding).unwrap();
            let worker = std::thread::spawn(move || {
                let tasks = if status == "missing" {
                    json!([])
                } else {
                    json!([{"id":"parent", "card":"retry-card", "kind":"install",
                        "store_run_id":"cmsd-parent", "status":status}])
                };
                // Both scan and remove must independently observe the device.
                for _ in 0..2 {
                    for response in [
                        json!({"device_id":"device-fixture"}),
                        json!({"tasks":tasks}),
                    ] {
                        let deadline = Instant::now() + Duration::from_secs(3);
                        let mut stream = loop {
                            match listener.accept() {
                                Ok((stream, _)) => break stream,
                                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                                    assert!(Instant::now() < deadline);
                                    std::thread::sleep(Duration::from_millis(5));
                                }
                                Err(e) => panic!("{e}"),
                            }
                        };
                        stream
                            .set_read_timeout(Some(Duration::from_secs(2)))
                            .unwrap();
                        let mut reader = BufReader::new(stream.try_clone().unwrap());
                        let mut line = String::new();
                        loop {
                            line.clear();
                            assert!(reader.read_line(&mut line).unwrap() > 0);
                            if line == "\r\n" {
                                break;
                            }
                        }
                        let body = response.to_string();
                        write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
                    }
                }
            });
            let unresolved = !matches!(status, "failed" | "cancelled");
            let report = scan(None).unwrap();
            assert_eq!(
                report["unresolvedInFlight"].as_u64().unwrap() > 0,
                unresolved,
                "{status}"
            );
            let removed = remove(None, &orphan.to_string_lossy());
            worker.join().unwrap();
            if unresolved {
                assert_eq!(
                    removed.unwrap_err(),
                    "orphan_activity_unresolved",
                    "{status}"
                );
                assert_eq!(std::fs::read(orphan.join("config.json")).unwrap(), b"{}");
                // Losing the parent connection is not a terminal observation.
                assert_eq!(
                    remove(None, &orphan.to_string_lossy()).unwrap_err(),
                    "orphan_activity_unresolved"
                );
                assert!(orphan.exists());
            } else {
                assert!(removed.unwrap()["freedBytes"].as_u64().unwrap() > 0);
                assert!(!orphan.exists());
            }
            assert_eq!(
                std::fs::read(binding).unwrap(),
                before,
                "binding must remain a durable fact"
            );
        }
    }

    #[test]
    fn utf8_directory_names_are_removable_and_lossy_names_are_not_offered() {
        use std::os::unix::ffi::OsStringExt;
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let models = config::load_settings().models_path();
        std::fs::create_dir_all(&models).unwrap();
        let invalid = models.join(std::ffi::OsString::from_vec(vec![b'b', 0xff]));
        std::fs::create_dir(&invalid).unwrap();
        for name in ["weights with spaces", "模型残留"] {
            let path = models.join(name);
            std::fs::create_dir(&path).unwrap();
            std::fs::write(path.join("partial"), b"keep until confirmed").unwrap();
        }
        let report = scan(None).unwrap();
        assert_eq!(report["partial"], true);
        assert_eq!(report["entries"].as_array().unwrap().len(), 2);
        for name in ["weights with spaces", "模型残留"] {
            let path = models.join(name);
            assert!(
                report["entries"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|item| item["name"] == name)
            );
            assert!(
                remove(None, &path.to_string_lossy()).unwrap()["freedBytes"]
                    .as_u64()
                    .unwrap()
                    > 0
            );
            assert!(!path.exists());
        }
        assert!(invalid.exists());
    }

    #[test]
    fn directories_owned_by_another_profile_are_not_listed_or_deleted() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _) = seed_models_dir();
        // Exercise the full scan/removal paths with another caller identity;
        // production entry points bind it to geteuid, never an API parameter.
        let other_uid = std::fs::metadata(&orphan).unwrap().uid().wrapping_add(1);
        let report = scan_for_user(None, other_uid).unwrap();
        assert!(report["entries"].as_array().unwrap().is_empty());
        assert_eq!(report["partial"], true);
        assert_eq!(
            remove_for_user(None, &orphan.to_string_lossy(), other_uid).unwrap_err(),
            "orphan_directory_other_user"
        );
        assert!(orphan.join("config.json").exists());
        assert!(
            !scan(None).unwrap()["entries"]
                .as_array()
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn terminal_installation_failures_do_not_permanently_protect_partial_files() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _) = seed_models_dir();
        for cancelled in [false, true] {
            write_receipt(
                "terminal",
                &json!({"card_id":"unknown-card", "kind":"download", "outcome":{"Err":"failed"}, "cancelled":cancelled}),
            );
            let report = scan(None).unwrap();
            assert_eq!(report["unresolvedInFlight"], 0);
            assert!(
                report["entries"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|item| item["path"] == orphan.to_string_lossy().as_ref())
            );
        }
        remove(None, &orphan.to_string_lossy()).unwrap();
        assert!(!orphan.exists());
    }

    #[test]
    fn activity_reader_skips_large_history_and_distinguishes_missing_and_null_outcomes() {
        let _home = config::TempHome::new();
        for (outcome, active) in [
            (None, false),
            (Some(Value::Null), true),
            (Some(json!({"Err":"stopped"})), false),
        ] {
            let mut receipt = json!({"card_id":"history-card", "kind":"download",
                "lines":["history ".repeat(256 * 1024)]});
            if let Some(outcome) = outcome {
                receipt["outcome"] = outcome;
            }
            write_receipt("history", &receipt);
            let mut cards = std::collections::HashMap::new();
            assert_eq!(non_terminal_install_cards(&mut cards), 0);
            assert_eq!(cards.contains_key("history-card"), active);
        }
    }

    #[test]
    fn skipped_log_fields_still_require_valid_json() {
        let _home = config::TempHome::new();
        super::super::execution_fence::private_write(
            &config::data_dir().join("runs/broken.json"),
            br#"{"card_id":"card","kind":"download","outcome":null,"lines":[invalid]}"#,
        )
        .unwrap();
        let mut cards = std::collections::HashMap::new();
        assert_eq!(non_terminal_install_cards(&mut cards), 1);
        assert!(cards.is_empty());
    }

    #[test]
    fn remove_refuses_while_a_writer_holds_the_shared_resource_lock() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _) = seed_models_dir();
        let writer = super::super::version_store::resource_operation_lock(false).unwrap();
        assert_eq!(
            remove(None, &orphan.to_string_lossy())
                .unwrap_err()
                .split(':')
                .next()
                .unwrap(),
            "orphan_resources_busy"
        );
        assert!(orphan.exists());
        drop(writer);
        remove(None, &orphan.to_string_lossy()).unwrap();
        assert!(!orphan.exists());
    }

    #[test]
    fn corrupt_receipts_are_unresolved_and_block_removal() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _) = seed_models_dir();
        let receipt = config::data_dir().join("runs/broken.json");
        super::super::execution_fence::private_write(&receipt, b"{broken").unwrap();
        assert_eq!(scan(None).unwrap()["unresolvedInFlight"], 1);
        assert_eq!(
            remove(None, &orphan.to_string_lossy()).unwrap_err(),
            "orphan_activity_unresolved"
        );
        assert!(orphan.exists());
    }

    #[test]
    fn live_installer_without_a_receipt_still_blocks_removal() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _) = seed_models_dir();
        let _installer = super::super::orphans::RunGuard::register_with_run_id(
            std::process::id() as i32,
            false,
            "orphan deletion regression",
            Some("missing-receipt"),
        );
        assert_eq!(
            remove(None, &orphan.to_string_lossy()).unwrap_err(),
            "installation_process_still_alive"
        );
        assert!(orphan.exists());
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
        assert_eq!(
            report["entries"].as_array().unwrap().len(),
            1,
            "扫描保持尽力而为"
        );

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
        for bad in [
            models.clone(),
            models.parent().unwrap().to_path_buf(),
            PathBuf::from("/tmp"),
        ] {
            assert!(
                remove(None, &bad.to_string_lossy()).is_err(),
                "{bad:?} 不该被放行"
            );
        }
    }

    /// 版本化权重根 models_dir/.versions 绝不能出现在可删列表，也绝不能被删：
    /// 它装着**所有**已入库模型的版本化权重，而库记录的 models_root 指向的是
    /// 它下面的深层 revision 目录——只按精确路径匹配拦不住这个祖先。
    #[test]
    fn versioned_weight_root_is_never_listed_or_removable() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let models = config::load_settings().models_path();
        let revisions = models
            .join(".versions")
            .join("versioned-card")
            .join("rev-1");
        std::fs::create_dir_all(&revisions).unwrap();
        std::fs::write(revisions.join("weights.safetensors"), "weights").unwrap();
        // 记录的 models_root 指向深层 revision 目录。
        let record = db::add_store_model_with_root(
            db::AddStoreModelParams {
                card_id: "versioned-card",
                model_name: "versioned-model",
                card_name: "Versioned Model",
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
            },
            &revisions.to_string_lossy(),
        )
        .unwrap();
        assert!(record.models_root.is_some(), "测试前提：记录带 models_root");

        let report = scan(None).unwrap();
        let names: Vec<&str> = report["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|entry| entry["name"].as_str().unwrap())
            .collect();
        assert!(
            names.iter().all(|name| *name != ".versions"),
            "版本化权重根不该被列出：{names:?}"
        );

        let error = remove(None, &models.join(".versions").to_string_lossy())
            .err()
            .unwrap();
        assert!(
            error.contains("unsafe") || error.contains("owned"),
            "点开头的内部目录必须被拒绝：{error}"
        );
        assert!(models.join(".versions").exists(), ".versions 必须原样保留");
    }

    /// 库记录的深层路径要连它所在的 models_dir 一级祖先一起拦——这是上一条
    /// 测试的通用形态（不限于 .versions 这个名字）。
    #[test]
    fn deep_library_paths_block_their_first_level_ancestor() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let models = config::load_settings().models_path();
        let deep = models.join("some-root").join("inner").join("weights");
        std::fs::create_dir_all(&deep).unwrap();
        std::fs::write(deep.join("w.bin"), "w").unwrap();
        db::add_store_model_with_root(
            db::AddStoreModelParams {
                card_id: "deep-card",
                model_name: "deep-model",
                card_name: "Deep Model",
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
            },
            &deep.to_string_lossy(),
        )
        .unwrap();

        let report = scan(None).unwrap();
        let names: Vec<&str> = report["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|entry| entry["name"].as_str().unwrap())
            .collect();
        assert!(
            names.iter().all(|name| *name != "some-root"),
            "库记录深层路径的一级祖先不该被列出：{names:?}"
        );
        let error = remove(None, &models.join("some-root").to_string_lossy())
            .err()
            .unwrap();
        assert!(error.contains("owned"), "实际错误：{error}");
    }

    /// 进行中的版本暂存（version_stage）：目标目录 .versions/<card> 被精确
    /// 排除（.versions 整体另有点开头规则兜底），而**无关**残留目录的删除
    /// 不受牵连——排除的目的是防误删，不是把整个商店锁死。
    #[test]
    fn in_flight_version_stage_excludes_its_target_but_not_unrelated_orphans() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let models = config::load_settings().models_path();
        let (orphan, _owned) = seed_models_dir();
        write_receipt(
            "staging",
            &json!({"card_id":"versioned-card","kind":"version_stage","created_at":"2026-10-22T00:00:00Z","outcome":null,"cancelled":false,"lines":[]}),
        );
        // kind 白名单必须覆盖 version_stage：回执进入目标排除与进程核查。
        let mut cards = std::collections::HashMap::new();
        assert_eq!(non_terminal_install_cards(&mut cards), 0);
        assert_eq!(
            cards.get("versioned-card").map(String::as_str),
            Some("version_stage")
        );
        // 暂存目标精确进禁删集合（它自身的删除先被点开头规则拦下）。
        let staging_dir = models.join(".versions").join("versioned-card");
        std::fs::create_dir_all(&staging_dir).unwrap();
        assert!(blocked_dirs(None, &models).unwrap().contains(&staging_dir));
        // 无关残留不牵连：版本任务进行中照样可以删别的残留。
        remove(None, &orphan.to_string_lossy()).unwrap();
        assert!(!orphan.exists(), "无关残留应可正常删除");
    }

    /// models_dir 本身含符号链接分量时的祖先拦截：库记录/legacy 路径是
    /// canonicalize 过的形式，对原始 models_dir 的 strip_prefix 会失败——
    /// 只推一遍祖先就漏掉那个一级目录，整棵可被列出并删除。
    #[test]
    fn canonical_ancestor_is_blocked_when_models_dir_contains_a_symlink() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        // 默认 models_dir 是 data_dir/models；把它变成指向 real 的符号链接。
        let store_root = config::data_dir();
        let real = store_root.join("real-models");
        std::fs::create_dir_all(&real).unwrap();
        let link = store_root.join("models");
        std::os::unix::fs::symlink(&real, &link).unwrap();
        let deep = real.join("outer").join("inner").join("weights");
        std::fs::create_dir_all(&deep).unwrap();
        std::fs::write(deep.join("w.bin"), "w").unwrap();
        db::add_store_model_with_root(
            db::AddStoreModelParams {
                card_id: "deep-card",
                model_name: "deep-model",
                card_name: "Deep Model",
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
            },
            &deep.to_string_lossy(),
        )
        .unwrap();

        // 列举走原始 models_dir（符号链接形式）；记录路径的 canonical 形式
        // 对它做 strip_prefix 必然失败。修复后 canonical 祖先仍会被拦。
        let report = scan(None).unwrap();
        let names: Vec<&str> = report["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|entry| entry["name"].as_str().unwrap())
            .collect();
        assert!(
            names.iter().all(|name| *name != "outer"),
            "canonical 深层路径的一级祖先不该被列出：{names:?}"
        );
        let error = remove(None, &link.join("outer").to_string_lossy())
            .err()
            .unwrap();
        assert!(error.contains("owned"), "实际错误：{error}");
        assert!(link.join("outer").exists(), "有主目录必须原样保留");
    }

    /// 回执落盘到 RunGuard 登记之间存在窗口，那一刻「正在写权重」的唯一守卫
    /// 是内存注册表（active_installs）：这条路径必须能独立驱动到，不能只在
    /// 回执路径上有测试。
    #[test]
    fn active_registry_blocks_its_target_without_any_receipt() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let (orphan, _owned) = seed_models_dir();
        // 卡片物化了，但盘上没有任何回执：排除依据只剩注册表。
        let card_id = "qwen-half";
        let card_dir = config::data_dir().join("cards").join(card_id);
        std::fs::create_dir_all(&card_dir).unwrap();
        std::fs::write(
            card_dir.join("card.json"),
            r#"{"name":"Qwen","quantization":"NVFP4","source_repo_id":"Qwen/Half-Downloaded-Model"}"#,
        )
        .unwrap();
        let service = ModelStoreService::new();
        service
            .active_registry_for_tests()
            .try_begin(
                card_id,
                super::super::run_state::OP_DOWNLOAD,
                Some("run-registry"),
            )
            .unwrap();

        let report = scan(Some(&service)).unwrap();
        let orphan_text = orphan.to_string_lossy().into_owned();
        assert!(
            report["entries"]
                .as_array()
                .unwrap()
                .iter()
                .all(|entry| entry["path"] != orphan_text),
            "注册表在跑的下载目标不该被列出：{report:?}"
        );
        assert!(
            remove(Some(&service), &orphan.to_string_lossy()).is_err(),
            "注册表在跑的下载期间不允许删除残留"
        );
    }
}
