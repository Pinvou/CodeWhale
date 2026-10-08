use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::fs;
use std::io::{Read, Seek, SeekFrom, Write};
use std::path::{Component, Path, PathBuf};
use std::sync::{LazyLock, Mutex};
use std::time::UNIX_EPOCH;

use anyhow::{Context, Result, bail};
use base64::Engine;
use serde::{Deserialize, Serialize};
use walkdir::{DirEntry, WalkDir};

// `SEARCH_LIMIT`, `PREVIEW_LIMIT` and `DIFF_LIMIT` are `pub` because the
// headless CLI's `code` commands, which depend on this crate, apply the same
// caps and reference these constants so the two surfaces cannot drift apart.
const LIST_LIMIT: usize = 500;
pub const SEARCH_LIMIT: usize = 300;
const WALK_LIMIT: usize = 20_000;
pub const PREVIEW_LIMIT: usize = 512 * 1024;
const IMAGE_PREVIEW_LIMIT: u64 = 10 * 1024 * 1024;
pub const DIFF_LIMIT: usize = 1024 * 1024;

const IGNORED_DIRECTORIES: &[&str] = &[
    ".git",
    ".hg",
    ".svn",
    "node_modules",
    "target",
    "dist",
    "build",
    ".next",
    ".cache",
    "__pycache__",
    ".venv",
    "venv",
];

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceEntry {
    pub name: String,
    pub relative_path: String,
    pub kind: String,
    pub size: u64,
    pub modified: i64,
    pub has_children: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceListing {
    pub relative_path: String,
    pub entries: Vec<WorkspaceEntry>,
    pub truncated: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspacePreview {
    pub name: String,
    pub relative_path: String,
    pub kind: String,
    pub size: u64,
    pub modified: i64,
    pub text: Option<String>,
    pub data_url: Option<String>,
    pub truncated: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceChange {
    pub relative_path: String,
    pub status: String,
    pub staged: bool,
    /// `session` / `preexisting` / `preexisting_modified` / `unknown`
    pub origin: String,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceChanges {
    pub git: bool,
    pub branch: Option<String>,
    pub repository_url: Option<String>,
    pub commit_sha: Option<String>,
    pub baseline_available: bool,
    pub changes: Vec<WorkspaceChange>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceDiff {
    pub relative_path: String,
    pub text: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceBranches {
    pub git: bool,
    pub current: Option<String>,
    pub branches: Vec<String>,
    /// 未提交更改条目数（git status），供前端在切换前提示确认。
    pub dirty_count: usize,
}

#[derive(Debug, Clone)]
pub(super) struct WorkspacePromptReference {
    pub relative_path: String,
    pub absolute_path: PathBuf,
    pub size: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
struct FileFingerprint {
    size: u64,
    modified: i64,
    sha256: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct WorkspaceBaseline {
    workspace_path: String,
    git: bool,
    #[serde(default)]
    dirty_paths: BTreeSet<String>,
    entries: BTreeMap<String, FileFingerprint>,
}

pub fn list_workspace(root: &Path, relative_path: Option<&str>) -> Result<WorkspaceListing> {
    let root = canonical_workspace(root)?;
    let relative = normalize_relative_path(relative_path.unwrap_or_default())?;
    let directory = resolve_existing_path(&root, &relative, true)?;
    let mut entries = Vec::new();
    for entry in fs::read_dir(&directory)
        .with_context(|| format!("读取工作目录失败: {}", directory.display()))?
        .filter_map(|entry| entry.ok())
    {
        if let Some(entry) = workspace_entry(&root, entry.path())? {
            entries.push(entry);
        }
    }
    entries.sort_by(|left, right| {
        let left_dir = left.kind == "directory";
        let right_dir = right.kind == "directory";
        right_dir
            .cmp(&left_dir)
            .then_with(|| left.name.to_lowercase().cmp(&right.name.to_lowercase()))
    });
    let truncated = entries.len() > LIST_LIMIT;
    entries.truncate(LIST_LIMIT);
    Ok(WorkspaceListing {
        relative_path: relative,
        entries,
        truncated,
    })
}

pub fn search_workspace(root: &Path, query: &str) -> Result<Vec<WorkspaceEntry>> {
    let query = query.trim().to_lowercase();
    if query.is_empty() {
        return Ok(Vec::new());
    }
    let root = canonical_workspace(root)?;
    let mut results = Vec::new();
    for entry in WalkDir::new(&root)
        .follow_links(false)
        .into_iter()
        .filter_entry(should_walk)
        .filter_map(|entry| entry.ok())
        .take(WALK_LIMIT)
    {
        if entry.depth() == 0 || !entry.file_type().is_file() {
            continue;
        }
        let relative = relative_text(&root, entry.path())?;
        if relative.to_lowercase().contains(&query) {
            if let Some(item) = workspace_entry(&root, entry.path().to_path_buf())? {
                results.push(item);
            }
            if results.len() >= SEARCH_LIMIT {
                break;
            }
        }
    }
    results.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
    Ok(results)
}

pub fn preview_workspace_file(root: &Path, relative_path: &str) -> Result<WorkspacePreview> {
    let relative = normalize_relative_path(relative_path)?;
    let path = resolve_existing_path(root, &relative, false)?;
    let metadata = path
        .metadata()
        .with_context(|| format!("读取文件信息失败: {}", path.display()))?;
    if !metadata.is_file() {
        bail!("不是文件: {relative}");
    }
    let kind = file_kind(&path);
    let modified = modified_seconds(&metadata);
    let name = path
        .file_name()
        .and_then(|value| value.to_str())
        .unwrap_or(&relative)
        .to_string();

    if kind == "image" {
        if metadata.len() > IMAGE_PREVIEW_LIMIT {
            return Ok(WorkspacePreview {
                name,
                relative_path: relative,
                kind,
                size: metadata.len(),
                modified,
                text: None,
                data_url: None,
                truncated: true,
            });
        }
        let bytes = fs::read(&path).with_context(|| format!("读取图片失败: {}", path.display()))?;
        let mime = image_mime_type(&path);
        return Ok(WorkspacePreview {
            name,
            relative_path: relative,
            kind,
            size: metadata.len(),
            modified,
            text: None,
            data_url: Some(format!(
                "data:{mime};base64,{}",
                base64::engine::general_purpose::STANDARD.encode(bytes)
            )),
            truncated: false,
        });
    }

    if kind == "text" {
        let mut file =
            fs::File::open(&path).with_context(|| format!("打开文件失败: {}", path.display()))?;
        let mut bytes = Vec::with_capacity(PREVIEW_LIMIT.min(metadata.len() as usize));
        std::io::Read::by_ref(&mut file)
            .take(PREVIEW_LIMIT as u64 + 1)
            .read_to_end(&mut bytes)
            .with_context(|| format!("读取文件失败: {}", path.display()))?;
        let truncated = bytes.len() > PREVIEW_LIMIT;
        bytes.truncate(PREVIEW_LIMIT);
        let text = String::from_utf8_lossy(&bytes).into_owned();
        return Ok(WorkspacePreview {
            name,
            relative_path: relative,
            kind,
            size: metadata.len(),
            modified,
            text: Some(text),
            data_url: None,
            truncated,
        });
    }

    Ok(WorkspacePreview {
        name,
        relative_path: relative,
        kind,
        size: metadata.len(),
        modified,
        text: None,
        data_url: None,
        truncated: false,
    })
}

pub fn resolve_workspace_resource(root: &Path, resource_path: &str) -> Result<PathBuf> {
    let root = canonical_workspace(root)?;
    let path = resource_path_from_uri(resource_path)?;
    let candidate = if path.is_absolute() {
        path
    } else {
        root.join(path)
    };
    let canonical = fs::canonicalize(&candidate)
        .with_context(|| format!("工作区资源不存在: {resource_path}"))?;
    ensure_path_within_workspace(&root, &canonical)?;
    if !canonical.is_file() {
        bail!("工作区资源不是文件: {resource_path}");
    }
    Ok(canonical)
}

pub fn capture_baseline(session_id: &str, root: &Path) -> Result<()> {
    let root = canonical_workspace(root)?;
    let git = is_git_workspace(&root);
    let (dirty_paths, entries) = if git {
        let status = git_status_entries(&root)?;
        let mut entries = BTreeMap::new();
        let mut dirty_paths = BTreeSet::new();
        for change in status {
            dirty_paths.insert(change.relative_path.clone());
            let path = root.join(&change.relative_path);
            if let Some(fingerprint) = fingerprint(&path, true)? {
                entries.insert(change.relative_path, fingerprint);
            }
        }
        (dirty_paths, entries)
    } else {
        (BTreeSet::new(), snapshot_entries(&root)?)
    };
    let baseline = WorkspaceBaseline {
        workspace_path: root.to_string_lossy().into_owned(),
        git,
        dirty_paths,
        entries,
    };
    let path = baseline_path(session_id);
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("创建工作区基线目录失败: {}", parent.display()))?;
    }
    let payload = serde_json::to_vec_pretty(&baseline).context("序列化工作区基线失败")?;
    let temporary = path.with_extension("json.tmp");
    {
        let mut file = fs::File::create(&temporary)
            .with_context(|| format!("创建工作区基线失败: {}", temporary.display()))?;
        file.write_all(&payload)
            .with_context(|| format!("写入工作区基线失败: {}", temporary.display()))?;
        file.sync_all().ok();
    }
    fs::rename(&temporary, &path)
        .with_context(|| format!("保存工作区基线失败: {}", path.display()))?;
    Ok(())
}

pub fn workspace_changes(session_id: &str, root: &Path) -> Result<WorkspaceChanges> {
    let root = canonical_workspace(root)?;
    let git = is_git_workspace(&root);
    let baseline = load_baseline(session_id, &root)?;
    let baseline_available = baseline.is_some();

    let mut changes = if git {
        git_status_entries(&root)?
    } else {
        filesystem_changes(&root, baseline.as_ref())?
    };
    for change in &mut changes {
        change.origin = classify_origin(&root, baseline.as_ref(), &change.relative_path)?;
    }
    changes.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
    Ok(WorkspaceChanges {
        git,
        branch: git.then(|| git_branch(&root)).flatten(),
        repository_url: git.then(|| git_repository_url(&root)).flatten(),
        commit_sha: git.then(|| git_commit_sha(&root)).flatten(),
        baseline_available,
        changes,
    })
}

fn is_git_workspace(root: &Path) -> bool {
    git_root(root).is_some_and(|git_root| git_root == root)
}

pub fn workspace_branches(root: &Path) -> Result<WorkspaceBranches> {
    let root = canonical_workspace(root)?;
    if !is_git_workspace(&root) {
        return Ok(WorkspaceBranches {
            git: false,
            current: None,
            branches: Vec::new(),
            dirty_count: 0,
        });
    }
    let current = git_branch(&root);
    // 按最近提交时间倒序（committerdate），与 IDE 的分支列表习惯一致；
    // 不再按名称排序。
    let output = git_output(
        &root,
        &[
            "branch",
            "--sort=-committerdate",
            "--format=%(refname:short)",
        ],
    )?;
    let branches = output
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(str::to_string)
        .collect::<Vec<_>>();
    let dirty_count = git_status_entries(&root)?.len();
    Ok(WorkspaceBranches {
        git: true,
        current,
        branches,
        dirty_count,
    })
}

/// 脏工作区分支切换策略：carry = 直接 checkout（更改随工作区携带，冲突时
/// 整体失败）；stash = 先 `git stash -u` 暂存，切换后自动 pop 恢复；
/// commit = 先把全部更改（含未跟踪文件）提交到当前分支，再切换。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BranchSwitchMode {
    Carry,
    Stash,
    Commit,
}

impl BranchSwitchMode {
    pub fn parse(value: &str) -> Result<Self> {
        match value {
            "carry" => Ok(Self::Carry),
            "stash" => Ok(Self::Stash),
            "commit" => Ok(Self::Commit),
            _ => bail!("未知的分支切换模式: {value}"),
        }
    }
}

/// 进程级互斥：串行化分支切换的完整 git 序列。并发调用（双开窗口、连点）会
/// 把两次切换的 stash push/checkout/pop 交错成跨分支、跨 stash 条目的中间态，
/// 回滚与报错都无法对应到单次操作。
static CHECKOUT_LOCK: Mutex<()> = Mutex::new(());

/// refs/stash 当前指向；仓库没有任何 stash 条目时为 None。用于判断一次
/// stash push 是否真的新建了条目（见 checkout_workspace_branch 的 Stash 模式）。
fn stash_head(root: &Path) -> Result<Option<String>> {
    match git_output(root, &["rev-parse", "-q", "--verify", "refs/stash"]) {
        Ok(head) => Ok(Some(head.trim().to_string())),
        Err(_) => Ok(None),
    }
}

pub fn checkout_workspace_branch(
    root: &Path,
    branch: &str,
    mode: BranchSwitchMode,
    commit_message: Option<&str>,
) -> Result<WorkspaceBranches> {
    let _checkout_guard = CHECKOUT_LOCK.lock().unwrap_or_else(|p| p.into_inner());
    let root = canonical_workspace(root)?;
    if !is_git_workspace(&root) {
        bail!("当前工作区不是 Git 仓库");
    }
    let branch = branch.trim();
    // 拒绝以 '-' 开头的分支名，避免被 git 误解析为选项。
    if branch.is_empty() || branch.starts_with('-') {
        bail!("无效的分支名: {branch}");
    }
    // 只允许切换到已存在的本地分支。否则当传入名字恰好是工作区内的文件
    // 路径时，`git checkout <name>` 会按 pathspec 从索引还原该文件，静默
    // 丢弃未提交更改。
    let known = git_output(&root, &["branch", "--format=%(refname:short)"])?;
    if !known.lines().map(str::trim).any(|name| name == branch) {
        bail!("分支不存在: {branch}");
    }
    if git_status_entries(&root)?.is_empty() {
        git_output(&root, &["checkout", branch])?;
        return workspace_branches(&root);
    }
    match mode {
        BranchSwitchMode::Carry => {
            git_output(&root, &["checkout", branch])?;
        }
        BranchSwitchMode::Stash => {
            // 暂存（含未跟踪文件）→ 切换 → 恢复。checkout 失败时先 pop 恢复
            // 现场再报错；恢复也失败时必须说明更改仍完好保存在 stash 中，避免
            // 现场看似丢失。切换后的 pop 冲突时 git 会保留该 stash 条目；注意
            // 未跟踪文件可能仅存在于该条目中，必须先确认内容再清理，不能直接
            // 引导 drop。
            // push 可能不创建条目（status 非空但无可暂存内容，典型是仅子模块
            // 工作区内容变更）：此时 pop 会误弹用户已有的 stash 条目并把其内容
            // 应用到新分支。比对 push 前后 refs/stash 指向，只有新建了条目才 pop。
            let prior_stash = stash_head(&root)?;
            git_output(
                &root,
                &[
                    "stash",
                    "push",
                    "--include-untracked",
                    "-m",
                    "pinvou: branch switch",
                ],
            )?;
            let stash_created = stash_head(&root)? != prior_stash;
            if let Err(error) = git_output(&root, &["checkout", branch]) {
                if stash_created {
                    if let Err(pop_error) = git_output(&root, &["stash", "pop"]) {
                        bail!(
                            "切换到 {branch} 失败，且自动恢复暂存的更改也失败；你的更改完好\
                             保留在 stash 中（git stash list 查看，git stash pop 恢复）。\
                             切换错误: {error:#}；恢复错误: {pop_error:#}"
                        );
                    }
                }
                return Err(error);
            }
            if stash_created {
                if let Err(error) = git_output(&root, &["stash", "pop"]) {
                    bail!(
                        "已切换到 {branch}，但恢复暂存的更改失败：冲突内容已应用到工作区，\
                         该 stash 条目已保留。清理前先执行 git stash show -u stash@{{0}} 确认\
                         内容——未跟踪文件可能仅存在于该条目中，直接 git stash drop 会丢失\
                         它们: {error:#}"
                    );
                }
            }
        }
        BranchSwitchMode::Commit => {
            let message = commit_message.map(str::trim).unwrap_or("");
            if message.is_empty() {
                bail!("提交信息不能为空");
            }
            git_output(&root, &["add", "-A"])?;
            git_output(&root, &["commit", "-m", message])?;
            git_output(&root, &["checkout", branch])?;
        }
    }
    workspace_branches(&root)
}

/// diff 内存缓存：键 = (session_id, 相对路径)，值 = 文件指纹 + diff 文本。
/// 指纹覆盖工作区文件（size + mtime 纳秒 + 内容采样 hash——≤8KB 全量、更大头尾
/// 各 4KB，内容变而 size/mtime 未变（同尺寸覆盖写）也能失效）、`.git/index`
/// （git add/reset 等暂存区变化会更新 index）与 HEAD 指向的 ref 文件（reset
/// --soft / update-ref / commit 只移动 HEAD、可能不更新 index 或工作区——而
/// git diff --cached 依赖 HEAD），任一变化即失效；仅进程内存、不落盘，重启即清空。
const DIFF_CACHE_MAX_ENTRIES: usize = 64;

#[derive(Debug, Clone, PartialEq, Eq)]
struct DiffFingerprint {
    file_size: u64,
    file_modified: u128,
    head_tail_hash: u64,
    index_size: u64,
    index_modified: u128,
    index_hash: u64,
    head_ref_size: u64,
    head_ref_modified: u128,
    head_ref_hash: u64,
}

#[derive(Clone)]
struct CachedDiff {
    fingerprint: DiffFingerprint,
    text: String,
    truncated: bool,
}

static DIFF_CACHE: LazyLock<Mutex<HashMap<(String, String), CachedDiff>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

fn modified_nanos(metadata: &fs::Metadata) -> u128 {
    metadata
        .modified()
        .ok()
        .and_then(|value| value.duration_since(UNIX_EPOCH).ok())
        .map(|value| value.as_nanos())
        .unwrap_or_default()
}

// FNV-1a 内容采样：≤ 8KB 全量读取，不留采样盲区——中等规模仓库的 .git/index
// 恰落此区间，条目定长、同路径同 SHA 位数的重写尺寸不变，且可能落在同一 mtime
// 粒度内，只采样头部会漏掉后半段条目与校验和的改写；> 8KB 读头尾各 4KB，内容
// 变化（含同尺寸覆盖写）时大概率失效。每次开销恒定 ≤ 8KB 读取，远小于重跑 git diff。
fn sample_head_tail_hash(path: &Path, len: u64) -> u64 {
    let mut hash = 0xcbf2_9ce4_8422_2325u64;
    let mut visit = |bytes: &[u8]| {
        for byte in bytes {
            hash ^= u64::from(*byte);
            hash = hash.wrapping_mul(0x1_0000_0000_01b3);
        }
    };
    if let Ok(mut file) = fs::File::open(path) {
        if len <= 8192 {
            let mut full = vec![0u8; len as usize];
            if file.read_exact(&mut full).is_ok() {
                visit(&full);
            }
        } else {
            let mut head = vec![0u8; 4096];
            if file.read_exact(&mut head).is_ok() {
                visit(&head);
            }
            let mut tail = vec![0u8; 4096];
            if file.seek(SeekFrom::End(-4096)).is_ok() && file.read_exact(&mut tail).is_ok() {
                visit(&tail);
            }
        }
    }
    hash
}

fn diff_fingerprint(root: &Path, relative: &str) -> DiffFingerprint {
    let mut fingerprint = DiffFingerprint {
        file_size: 0,
        file_modified: 0,
        head_tail_hash: 0,
        index_size: 0,
        index_modified: 0,
        index_hash: 0,
        head_ref_size: 0,
        head_ref_modified: 0,
        head_ref_hash: 0,
    };
    if let Ok(metadata) = root.join(relative).metadata() {
        fingerprint.file_size = metadata.len();
        fingerprint.file_modified = modified_nanos(&metadata);
        fingerprint.head_tail_hash = sample_head_tail_hash(&root.join(relative), metadata.len());
    }
    // git 暂存区变化会重写 .git/index；非 git 工作区无此文件，字段保持 0。
    // index 条目定长、同路径同 SHA 位数，重写往往尺寸不变，且同样可能落在
    // 同一 mtime 粒度内——与 HEAD ref 一样加内容哈希兜底。
    // 路径经 git_path 解析：linked worktree 下 .git 是 gitfile，直接拼
    // root/.git/index 会读不到真实 index。
    let index_path = git_path(root, "index").unwrap_or_else(|| root.join(".git/index"));
    if let Ok(metadata) = index_path.metadata() {
        fingerprint.index_size = metadata.len();
        fingerprint.index_modified = modified_nanos(&metadata);
        fingerprint.index_hash = sample_head_tail_hash(&index_path, metadata.len());
    }
    // git diff --cached 比较 index 与 HEAD；reset --soft / update-ref / commit 只移动
    // HEAD（改写其指向的 ref 文件），可能不更新 .git/index 或工作区。解析 .git/HEAD
    // （多为 symref: ref: refs/heads/<branch>）后 stat 目标文件，使此类改动也能失效。
    if let Some(head_ref) = head_ref_path(root) {
        if let Ok(metadata) = head_ref.metadata() {
            fingerprint.head_ref_size = metadata.len();
            fingerprint.head_ref_modified = modified_nanos(&metadata);
            // 内容哈希兜底:reset --soft / update-ref 与上次指纹落在同一时间戳
            // 粒度内时 mtime 不变,只有内容(SHA 改写)才能正确失效缓存。
            fingerprint.head_ref_hash = sample_head_tail_hash(&head_ref, metadata.len());
        }
    }
    fingerprint
}

// 解析 HEAD 指向的 ref 文件：symref（`ref: refs/heads/<branch>`）→ 该 ref 文件路径；
// 分离头（直接存 commit SHA）→ HEAD 文件本身。reset --soft / update-ref 改写后者，
// stat 它即可让指纹失效。非 git 工作区返回 None。
// 路径经 git_path 解析：linked worktree 下 .git 是 gitfile，HEAD 与分支 ref 都在
// gitdir/commondir 下，直接拼 root/.git/... 会解析不到。
fn head_ref_path(root: &Path) -> Option<PathBuf> {
    let head_path = git_path(root, "HEAD").unwrap_or_else(|| root.join(".git/HEAD"));
    let head = fs::read_to_string(&head_path).ok()?;
    match head.trim().strip_prefix("ref:") {
        // symref 目标（如 refs/heads/main）同样经 git_path 解析，worktree 下
        // commondir 重定向由 git 自己处理。
        Some(target) => Some(
            git_path(root, target.trim()).unwrap_or_else(|| root.join(".git").join(target.trim())),
        ),
        None => Some(head_path),
    }
}

// 经 `git rev-parse --git-path <name>` 解析仓库真实 git 目录内的路径。linked
// worktree 中 `.git` 是 gitfile，朴素的 `root.join(".git/...")` 启发式读不到
// index/HEAD/ref 文件，指纹字段会恒为 0 并返回陈旧缓存差异。主工作区的 `.git`
// 是真实目录，从工作区根向上发现必然落在其中，路径拼接与子进程输出逐字节一致，
// 故直接走快路径、不 spawn git——指纹在每次缓存探测都要计算，每次探测多次进程
// 启动会拖垮它所守护的缓存。gitfile 布局（linked worktree、submodule、
// separate-git-dir）走发现路径；git 不可用（非 git 工作区）时回退朴素启发式。
fn git_path(root: &Path, name: &str) -> Option<PathBuf> {
    let dot_git = root.join(".git");
    if dot_git.is_dir() {
        return Some(dot_git.join(name));
    }
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    let output = command
        .current_dir(root)
        .args(["rev-parse", "--git-path", name])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let relative = String::from_utf8_lossy(&output.stdout).trim().to_string();
    (!relative.is_empty()).then(|| root.join(relative))
}

fn diff_cache_get(session_id: &str, root: &Path, relative: &str) -> Option<CachedDiff> {
    let fingerprint = diff_fingerprint(root, relative);
    DIFF_CACHE
        .lock()
        .unwrap_or_else(|error| error.into_inner())
        .get(&(session_id.to_string(), relative.to_string()))
        .filter(|cached| cached.fingerprint == fingerprint)
        .cloned()
}

fn diff_cache_put(session_id: &str, root: &Path, relative: &str, text: String, truncated: bool) {
    let fingerprint = diff_fingerprint(root, relative);
    let mut cache = DIFF_CACHE.lock().unwrap_or_else(|error| error.into_inner());
    // 简单上限：超限整体清空，避免进程内无限膨胀（纯内存，不落盘）。
    if cache.len() >= DIFF_CACHE_MAX_ENTRIES {
        cache.clear();
    }
    cache.insert(
        (session_id.to_string(), relative.to_string()),
        CachedDiff {
            fingerprint,
            text,
            truncated,
        },
    );
}

pub fn workspace_diff(session_id: &str, root: &Path, relative_path: &str) -> Result<WorkspaceDiff> {
    let root = canonical_workspace(root)?;
    let relative = normalize_relative_path(relative_path)?;
    let path = root.join(&relative);
    ensure_path_within_workspace(&root, &path)?;

    if let Some(cached) = diff_cache_get(session_id, &root, &relative) {
        return Ok(WorkspaceDiff {
            relative_path: relative,
            text: cached.text,
            truncated: cached.truncated,
        });
    }

    let mut text = if is_git_workspace(&root) {
        let unstaged = git_output(
            &root,
            &["diff", "--no-ext-diff", "--no-color", "--", &relative],
        )?;
        let staged = git_output(
            &root,
            &[
                "diff",
                "--cached",
                "--no-ext-diff",
                "--no-color",
                "--",
                &relative,
            ],
        )?;
        let mut combined = String::new();
        if !staged.trim().is_empty() {
            combined.push_str("# 已暂存\n");
            combined.push_str(&staged);
        }
        if !unstaged.trim().is_empty() {
            if !combined.is_empty() {
                combined.push('\n');
            }
            combined.push_str("# 未暂存\n");
            combined.push_str(&unstaged);
        }
        if combined.is_empty() && path.is_file() {
            untracked_diff(&path, &relative)?
        } else {
            combined
        }
    } else if path.is_file() {
        let preview = preview_workspace_file(&root, &relative)?;
        preview
            .text
            .unwrap_or_else(|| "该文件不支持文本差异预览。".to_string())
    } else {
        "文件已删除，非 Git 工作区无法还原删除前内容。".to_string()
    };

    let truncated = text.len() > DIFF_LIMIT;
    if truncated {
        // Byte-capped cuts must floor to a char boundary (`String::truncate`
        // panics mid-char — the common case for CJK diffs near the cap), via
        // the crate-wide helper the P0 convention routes all such cuts
        // through. The CLI's diff lane carries the same guard, so the shared
        // `DIFF_LIMIT` cannot panic on one surface and cut cleanly on the
        // other.
        truncate_to_byte_cap(&mut text, DIFF_LIMIT);
        text.push_str("\n\n…差异过大，已截断");
    }
    diff_cache_put(session_id, &root, &relative, text.clone(), truncated);
    Ok(WorkspaceDiff {
        relative_path: relative,
        text,
        truncated,
    })
}

/// The byte-capped cut `workspace_diff` performs: floor to a char boundary
/// via the crate-wide helper, then truncate. A function so the truncation
/// test drives the exact production cut rather than a transcription.
fn truncate_to_byte_cap(text: &mut String, limit: usize) {
    let boundary = crate::platform::strings::truncate_utf8(text, limit).len();
    text.truncate(boundary);
}

/// 校验相对路径落在工作区内，但不要求文件存在（git diff 可展示已删除文件）。
pub fn validate_workspace_relative_path(root: &Path, relative_path: &str) -> Result<()> {
    let relative = normalize_relative_path(relative_path)?;
    let path = root.join(&relative);
    ensure_path_within_workspace(root, &path)
}

pub(super) fn resolve_workspace_references(
    root: &Path,
    relative_paths: &[String],
) -> Result<Vec<WorkspacePromptReference>> {
    let mut unique = BTreeSet::new();
    let mut references = Vec::new();
    for raw in relative_paths {
        let relative = normalize_relative_path(raw)?;
        if !unique.insert(relative.clone()) {
            continue;
        }
        let absolute_path = resolve_existing_path(root, &relative, false)?;
        let metadata = absolute_path
            .metadata()
            .with_context(|| format!("读取工作区文件失败: {}", absolute_path.display()))?;
        if !metadata.is_file() {
            bail!("工作区引用不是文件: {relative}");
        }
        references.push(WorkspacePromptReference {
            relative_path: relative,
            absolute_path,
            size: metadata.len(),
        });
    }
    Ok(references)
}

pub fn resolve_workspace_file(root: &Path, relative_path: &str) -> Result<PathBuf> {
    let relative = normalize_relative_path(relative_path)?;
    resolve_existing_path(root, &relative, false)
}

fn workspace_entry(root: &Path, path: PathBuf) -> Result<Option<WorkspaceEntry>> {
    let metadata = fs::symlink_metadata(&path)
        .with_context(|| format!("读取文件信息失败: {}", path.display()))?;
    if metadata.file_type().is_symlink() {
        return Ok(None);
    }
    let name = path
        .file_name()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_string();
    if name.is_empty() || (metadata.is_dir() && is_ignored_directory(&name)) {
        return Ok(None);
    }
    let kind = if metadata.is_dir() {
        "directory"
    } else if metadata.is_file() {
        "file"
    } else {
        return Ok(None);
    };
    let has_children = metadata.is_dir()
        && fs::read_dir(&path)
            .ok()
            .is_some_and(|mut entries| entries.any(|entry| entry.is_ok()));
    Ok(Some(WorkspaceEntry {
        name,
        relative_path: relative_text(root, &path)?,
        kind: kind.to_string(),
        size: if metadata.is_file() {
            metadata.len()
        } else {
            0
        },
        modified: modified_seconds(&metadata),
        has_children,
    }))
}

fn canonical_workspace(root: &Path) -> Result<PathBuf> {
    let canonical =
        fs::canonicalize(root).with_context(|| format!("工作目录不可用: {}", root.display()))?;
    if !canonical.is_dir() {
        bail!("工作目录不可用: {}", canonical.display());
    }
    Ok(canonical)
}

fn normalize_relative_path(raw: &str) -> Result<String> {
    let path = Path::new(raw);
    if path.is_absolute() {
        bail!("工作区路径必须是相对路径");
    }
    let mut parts = Vec::new();
    for component in path.components() {
        match component {
            Component::Normal(value) => parts.push(value.to_string_lossy().into_owned()),
            Component::CurDir => {}
            Component::ParentDir | Component::RootDir | Component::Prefix(_) => {
                bail!("工作区路径不能越过根目录")
            }
        }
    }
    Ok(parts.join("/"))
}

fn resource_path_from_uri(raw: &str) -> Result<PathBuf> {
    let value = raw.trim();
    if value.is_empty() {
        bail!("工作区资源路径不能为空");
    }
    if value
        .get(..7)
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case("file://"))
    {
        let url = tauri::Url::parse(value).context("工作区资源文件链接无效")?;
        return url
            .to_file_path()
            .map_err(|_| anyhow::anyhow!("工作区资源文件链接无效"));
    }
    if value.contains("://") {
        bail!("不支持的工作区资源链接");
    }
    Ok(PathBuf::from(value))
}

fn resolve_existing_path(root: &Path, relative: &str, directory: bool) -> Result<PathBuf> {
    let root = canonical_workspace(root)?;
    let candidate = if relative.is_empty() {
        root.clone()
    } else {
        root.join(relative)
    };
    let canonical =
        fs::canonicalize(&candidate).with_context(|| format!("工作区路径不存在: {relative}"))?;
    ensure_path_within_workspace(&root, &canonical)?;
    if directory && !canonical.is_dir() {
        bail!("不是目录: {relative}");
    }
    Ok(canonical)
}

fn ensure_path_within_workspace(root: &Path, path: &Path) -> Result<()> {
    if !path.starts_with(root) {
        bail!("工作区路径越过了项目根目录");
    }
    Ok(())
}

fn relative_text(root: &Path, path: &Path) -> Result<String> {
    Ok(path
        .strip_prefix(root)
        .with_context(|| format!("路径不在工作区内: {}", path.display()))?
        .to_string_lossy()
        .replace('\\', "/"))
}

fn modified_seconds(metadata: &fs::Metadata) -> i64 {
    metadata
        .modified()
        .ok()
        .and_then(|value| value.duration_since(UNIX_EPOCH).ok())
        .map(|value| value.as_secs() as i64)
        .unwrap_or_default()
}

fn is_ignored_directory(name: &str) -> bool {
    IGNORED_DIRECTORIES.contains(&name)
}

fn should_walk(entry: &DirEntry) -> bool {
    entry.depth() == 0
        || !entry.file_type().is_dir()
        || !is_ignored_directory(&entry.file_name().to_string_lossy())
}

fn file_kind(path: &Path) -> String {
    // The image-extension set stays in sync with codex_acp::attachments' canonical MIME mapping table.
    if crate::platform::filesystem::image_mime_type(path).is_some() {
        return "image".to_string();
    }
    let extension = path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    if matches!(
        extension.as_str(),
        "md" | "markdown"
            | "txt"
            | "log"
            | "csv"
            | "json"
            | "yaml"
            | "yml"
            | "toml"
            | "xml"
            | "html"
            | "htm"
            | "rs"
            | "py"
            | "js"
            | "jsx"
            | "ts"
            | "tsx"
            | "go"
            | "c"
            | "cpp"
            | "h"
            | "hpp"
            | "sh"
            | "bash"
            | "zsh"
            | "fish"
            | "bat"
            | "cmd"
            | "ps1"
            | "css"
            | "scss"
            | "sass"
            | "less"
            | "vue"
            | "svelte"
            | "sql"
            | "ini"
            | "conf"
            | "cfg"
            | "env"
            | "properties"
            | "diff"
            | "patch"
            | "lock"
            | "proto"
            | "graphql"
            | "gql"
            | "prisma"
            | "java"
            | "kt"
            | "swift"
            | "rb"
            | "php"
            | "cs"
            | "lua"
            | "scala"
            | "gradle"
            | "tf"
            | "tex"
            | "rst"
            | "pl"
            | "pm"
            | "r"
            | "m"
            | "mm"
    ) {
        return "text".to_string();
    }
    // 无扩展名的常见文本文件名（Dockerfile、Makefile、LICENSE、.gitignore 等）。
    let file_name = path
        .file_name()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    if matches!(
        file_name.as_str(),
        "dockerfile"
            | "makefile"
            | "gnumakefile"
            | "jenkinsfile"
            | "vagrantfile"
            | "gemfile"
            | "rakefile"
            | "brewfile"
            | "cmakelists.txt"
            | "license"
            | "licence"
            | "copying"
            | "notice"
            | "authors"
            | "contributors"
            | "changelog"
            | "readme"
            | ".gitignore"
            | ".gitattributes"
            | ".gitmodules"
            | ".editorconfig"
            | ".npmrc"
            | ".yarnrc"
            | ".env"
            | ".envrc"
    ) {
        return "text".to_string();
    }
    // 扩展名白名单之外的内容嗅探：头部 8KB 无 NUL 且为合法 UTF-8 即按文本处理，
    // 覆盖 .hbs/.ipynb/.cmake 等未列举扩展名及无扩展名的文本文件。
    if looks_like_text(path) {
        return "text".to_string();
    }
    "binary".to_string()
}

fn looks_like_text(path: &Path) -> bool {
    let mut file = match fs::File::open(path) {
        Ok(file) => file,
        Err(_) => return false,
    };
    let mut buffer = [0u8; 8192];
    let read = std::io::Read::read(&mut file, &mut buffer).unwrap_or(0);
    let sample = &buffer[..read];
    if sample.contains(&0) {
        return false;
    }
    match std::str::from_utf8(sample) {
        Ok(_) => true,
        // 8KB 截断点恰好落在多字节字符中间时，容忍末尾不完整序列（≤4 字节）。
        Err(error) => error.valid_up_to() > 0 && sample.len() - error.valid_up_to() <= 4,
    }
}

fn image_mime_type(path: &Path) -> &'static str {
    match path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase()
        .as_str()
    {
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "bmp" => "image/bmp",
        _ => "image/png",
    }
}

fn baseline_path(session_id: &str) -> PathBuf {
    crate::platform::paths::sessions_root()
        .join(session_id)
        .join("codex-workspace-baseline.json")
}

fn load_baseline(session_id: &str, root: &Path) -> Result<Option<WorkspaceBaseline>> {
    let path = baseline_path(session_id);
    let bytes = match fs::read(&path) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(error).with_context(|| format!("读取工作区基线失败: {}", path.display()));
        }
    };
    let baseline: WorkspaceBaseline =
        serde_json::from_slice(&bytes).context("解析工作区基线失败")?;
    if baseline.workspace_path != root.to_string_lossy() {
        return Ok(None);
    }
    Ok(Some(baseline))
}

fn snapshot_entries(root: &Path) -> Result<BTreeMap<String, FileFingerprint>> {
    let mut entries = BTreeMap::new();
    for entry in WalkDir::new(root)
        .follow_links(false)
        .into_iter()
        .filter_entry(should_walk)
        .filter_map(|entry| entry.ok())
        .take(WALK_LIMIT)
    {
        if entry.depth() == 0 || !entry.file_type().is_file() {
            continue;
        }
        let relative = relative_text(root, entry.path())?;
        if let Some(fingerprint) = fingerprint(entry.path(), false)? {
            entries.insert(relative, fingerprint);
        }
    }
    Ok(entries)
}

fn fingerprint(path: &Path, include_hash: bool) -> Result<Option<FileFingerprint>> {
    let metadata = match path.metadata() {
        Ok(metadata) if metadata.is_file() => metadata,
        Ok(_) => return Ok(None),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(error).with_context(|| format!("读取文件信息失败: {}", path.display()));
        }
    };
    let sha256 = if include_hash {
        Some(
            crate::platform::hashing::sha256_file(path)
                .with_context(|| format!("读取文件失败: {}", path.display()))?,
        )
    } else {
        None
    };
    Ok(Some(FileFingerprint {
        size: metadata.len(),
        modified: modified_seconds(&metadata),
        sha256,
    }))
}

fn filesystem_changes(
    root: &Path,
    baseline: Option<&WorkspaceBaseline>,
) -> Result<Vec<WorkspaceChange>> {
    let current = snapshot_entries(root)?;
    let Some(baseline) = baseline else {
        return Ok(current
            .keys()
            .map(|path| WorkspaceChange {
                relative_path: path.clone(),
                status: "unknown".to_string(),
                staged: false,
                origin: "unknown".to_string(),
            })
            .collect());
    };
    let mut paths = BTreeSet::new();
    paths.extend(current.keys().cloned());
    paths.extend(baseline.entries.keys().cloned());
    Ok(paths
        .into_iter()
        .filter_map(
            |path| match (baseline.entries.get(&path), current.get(&path)) {
                (None, Some(_)) => Some(("added", path)),
                (Some(_), None) => Some(("deleted", path)),
                (Some(before), Some(after)) if before != after => Some(("modified", path)),
                _ => None,
            },
        )
        .map(|(status, relative_path)| WorkspaceChange {
            relative_path,
            status: status.to_string(),
            staged: false,
            origin: "session".to_string(),
        })
        .collect())
}

fn git_root(root: &Path) -> Option<PathBuf> {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    let output = command
        .current_dir(root)
        .args(["rev-parse", "--show-toplevel"])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    fs::canonicalize(String::from_utf8_lossy(&output.stdout).trim()).ok()
}

fn git_branch(root: &Path) -> Option<String> {
    let branch = git_output(root, &["branch", "--show-current"]).ok()?;
    let branch = branch.trim();
    (!branch.is_empty()).then(|| branch.to_string())
}

fn git_commit_sha(root: &Path) -> Option<String> {
    let commit = git_output(root, &["rev-parse", "HEAD"]).ok()?;
    let commit = commit.trim().to_ascii_lowercase();
    (commit.len() >= 7 && commit.len() <= 64 && commit.chars().all(|ch| ch.is_ascii_hexdigit()))
        .then_some(commit)
}

fn git_repository_url(root: &Path) -> Option<String> {
    let remote = git_output(root, &["remote", "get-url", "origin"]).ok()?;
    sanitize_git_remote_url(remote.trim())
}

fn sanitize_git_remote_url(remote: &str) -> Option<String> {
    let remote = remote.trim();
    if remote.is_empty() {
        return None;
    }
    let candidate = if let Some(rest) = remote.strip_prefix("git@") {
        let (host, path) = rest.split_once(':')?;
        format!("https://{host}/{path}")
    } else if remote.starts_with("ssh://") || remote.starts_with("git://") {
        let parsed = reqwest::Url::parse(remote).ok()?;
        let host = parsed.host_str()?;
        format!("https://{host}{}", parsed.path())
    } else {
        remote.to_string()
    };
    let mut parsed = reqwest::Url::parse(&candidate).ok()?;
    if !matches!(parsed.scheme(), "http" | "https") {
        return None;
    }
    parsed.set_username("").ok()?;
    parsed.set_password(None).ok()?;
    parsed.set_query(None);
    parsed.set_fragment(None);
    let clean_path = parsed
        .path()
        .trim_end_matches('/')
        .trim_end_matches(".git")
        .to_string();
    parsed.set_path(&clean_path);
    Some(parsed.to_string().trim_end_matches('/').to_string())
}

fn git_status_entries(root: &Path) -> Result<Vec<WorkspaceChange>> {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    let output = command
        .current_dir(root)
        .args([
            "status",
            "--porcelain=v1",
            "-z",
            "--untracked-files=all",
            "--",
            ".",
        ])
        .output()
        .context("执行 git status 失败")?;
    if !output.status.success() {
        bail!(
            "git status 失败: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    let records = output.stdout.split(|byte| *byte == 0).collect::<Vec<_>>();
    let mut changes = Vec::new();
    let mut index = 0;
    while index < records.len() {
        let record = records[index];
        index += 1;
        if record.len() < 4 {
            continue;
        }
        let x = record[0] as char;
        let y = record[1] as char;
        let path = String::from_utf8_lossy(&record[3..]).replace('\\', "/");
        if matches!(x, 'R' | 'C') || matches!(y, 'R' | 'C') {
            index += 1;
        }
        changes.push(WorkspaceChange {
            relative_path: path,
            status: git_status_label(x, y).to_string(),
            staged: x != ' ' && x != '?',
            origin: "unknown".to_string(),
        });
    }
    Ok(changes)
}

fn git_status_label(x: char, y: char) -> &'static str {
    if x == '?' && y == '?' {
        "untracked"
    } else if x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D') {
        "conflict"
    } else if x == 'A' || y == 'A' {
        "added"
    } else if x == 'D' || y == 'D' {
        "deleted"
    } else if x == 'R' || y == 'R' {
        "renamed"
    } else if x == 'C' || y == 'C' {
        "copied"
    } else {
        "modified"
    }
}

fn git_output(root: &Path, arguments: &[&str]) -> Result<String> {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    let output = command
        .current_dir(root)
        .args(arguments)
        .output()
        .with_context(|| format!("执行 git {} 失败", arguments.join(" ")))?;
    if !output.status.success() {
        bail!(
            "git {} 失败: {}",
            arguments.join(" "),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    Ok(String::from_utf8_lossy(&output.stdout).into_owned())
}

fn classify_origin(
    root: &Path,
    baseline: Option<&WorkspaceBaseline>,
    relative_path: &str,
) -> Result<String> {
    let Some(baseline) = baseline else {
        return Ok("unknown".to_string());
    };
    if !baseline.git || !baseline.dirty_paths.contains(relative_path) {
        return Ok("session".to_string());
    }
    let before = baseline.entries.get(relative_path);
    let current = fingerprint(&root.join(relative_path), baseline.git)?;
    if current.as_ref() == before {
        Ok("preexisting".to_string())
    } else {
        Ok("preexisting_modified".to_string())
    }
}

fn untracked_diff(path: &Path, relative: &str) -> Result<String> {
    if file_kind(path) != "text" {
        return Ok("未跟踪的二进制文件不支持差异预览。".to_string());
    }
    let preview = fs::read_to_string(path)
        .with_context(|| format!("读取未跟踪文件失败: {}", path.display()))?;
    let mut output = format!(
        "diff --git a/{0} b/{0}\nnew file mode 100644\n--- /dev/null\n+++ b/{0}\n",
        relative
    );
    for line in preview.lines() {
        output.push('+');
        output.push_str(line);
        output.push('\n');
        if output.len() > DIFF_LIMIT {
            break;
        }
    }
    Ok(output)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    struct TestDir(PathBuf);

    impl TestDir {
        fn new(label: &str) -> Self {
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let path = std::env::temp_dir().join(format!(
                "pinvou3-codex-workspace-{label}-{}-{nonce}",
                std::process::id()
            ));
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }

        fn path(&self) -> &Path {
            &self.0
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn rejects_workspace_escape() {
        let root = TestDir::new("escape-root");
        assert!(normalize_relative_path("../secret.txt").is_err());
        assert!(normalize_relative_path("/absolute/secret.txt").is_err());
        assert!(resolve_workspace_file(root.path(), "missing.txt").is_err());
    }

    #[test]
    fn diff_truncation_cuts_on_char_boundaries() {
        // A full cap of CJK.
        let mut text = "界".repeat(DIFF_LIMIT / 3 + 4);
        truncate_to_byte_cap(&mut text, DIFF_LIMIT);
        assert!(text.len() <= DIFF_LIMIT);
        assert!(
            text.chars().all(|c| c == '界'),
            "the cut must land on a char boundary"
        );

        // A 4-byte emoji straddling a small cap.
        let mut emoji = "a🏖b".to_string();
        truncate_to_byte_cap(&mut emoji, 3);
        assert_eq!(emoji, "a");

        let mut small = "ab界界".to_string();
        truncate_to_byte_cap(&mut small, 3);
        assert_eq!(small, "ab");

        let mut exact = "abc".to_string();
        truncate_to_byte_cap(&mut exact, 3);
        assert_eq!(exact, "abc");

        // Under the cap: untouched, whatever the content.
        let mut short = "短文🐱".to_string();
        truncate_to_byte_cap(&mut short, 1024);
        assert_eq!(short, "短文🐱");

        let mut empty = String::new();
        truncate_to_byte_cap(&mut empty, 0);
        assert_eq!(empty, "");
    }

    #[test]
    fn validates_relative_path_without_requiring_file_existence() {
        let root = TestDir::new("validate-relative");
        assert!(validate_workspace_relative_path(root.path(), "src/main.py").is_ok());
        assert!(validate_workspace_relative_path(root.path(), "").is_ok());
        assert!(validate_workspace_relative_path(root.path(), "../secret.txt").is_err());
        assert!(validate_workspace_relative_path(root.path(), "/absolute/secret.txt").is_err());
    }

    #[test]
    fn diff_cache_hits_within_session_and_invalidates_on_file_change() {
        // 无真实 git 仓库时 workspace_diff 走文件系统对比分支，内容随文件变化——正适合验证缓存失效。
        let root = TestDir::new("diff-cache");
        fs::write(root.path().join("main.py"), "print(1)\n").unwrap();

        let first = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert!(first.text.contains("print(1)"));
        // 同会话、文件未变：命中缓存，内容一致。
        let second = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert_eq!(first.text, second.text);

        // 文件变化 → 指纹失效，重新计算出新内容。
        fs::write(root.path().join("main.py"), "print(2)\n").unwrap();
        let third = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert_ne!(first.text, third.text);
        assert!(third.text.contains("print(2)"));
        // 稳定后再次命中缓存。
        let fourth = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert_eq!(third.text, fourth.text);
    }

    // 采样某时刻的 mtime 并可回写:同尺寸同 mtime 重写(比依赖文件系统时间
    // 粒度更确定——没有内容哈希时这些测试在任何平台上都必然失败,而不是只在
    // coarse-mtime 的 Linux CI 上才暴露)。
    fn current_times(path: &Path) -> fs::FileTimes {
        let metadata = fs::metadata(path).unwrap();
        fs::FileTimes::new().set_modified(metadata.modified().unwrap())
    }

    fn apply_times(path: &Path, times: fs::FileTimes) {
        let file = fs::OpenOptions::new().write(true).open(path).unwrap();
        file.set_times(times).unwrap();
    }

    #[test]
    fn diff_fingerprint_covers_file_and_git_index() {
        let root = TestDir::new("diff-fingerprint");
        fs::create_dir_all(root.path().join(".git")).unwrap();
        let index = root.path().join(".git/index");
        fs::write(&index, b"idx1").unwrap();
        fs::write(root.path().join("main.py"), "print(1)\n").unwrap();

        let baseline = diff_fingerprint(root.path(), "main.py");
        // 暂存区变化(.git/index 重写)→ 指纹变化。index 条目定长、同路径同
        // SHA 位数,重写尺寸不变;显式固定回基线 mtime 后,只有内容哈希能失效。
        let index_stamp = current_times(&index);
        fs::write(&index, b"idx2").unwrap();
        apply_times(&index, index_stamp);
        assert_ne!(baseline, diff_fingerprint(root.path(), "main.py"));
        // 内容与 mtime 全部还原 → 指纹回到基线(同内容不产生虚假失效)。
        fs::write(&index, b"idx1").unwrap();
        apply_times(&index, index_stamp);
        assert_eq!(baseline, diff_fingerprint(root.path(), "main.py"));
        // 文件内容变化 → 指纹变化。
        fs::write(root.path().join("main.py"), "print(2)\n").unwrap();
        assert_ne!(baseline, diff_fingerprint(root.path(), "main.py"));
    }

    // 真实仓库的中等 index（4–8 KiB）：80 个文件恰好让 index 越过头部采样区，
    // 又落在"小于等于 8KiB"区间——f075 的条目（偏移 >5KB）与校验和都在文件后半段，
    // 同尺寸同 mtime 的暂存改写只能靠全量哈希失效（此前头尾采样会漏掉该区间）。
    #[test]
    fn diff_fingerprint_invalidates_on_medium_index_staging_change() {
        let Some(root) = init_git_repo("diff-fingerprint-medium-index") else {
            return;
        };
        for index in 1..=80 {
            fs::write(
                root.path().join(format!("f{index:03}")),
                format!("content {index}\n"),
            )
            .unwrap();
        }
        git_output(root.path(), &["add", "."]).unwrap();
        git_output(
            root.path(),
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-m",
                "80 files",
            ],
        )
        .unwrap();
        // read-tree 剥去 add 留下的 TREE 扩展，基线态与篡改态随后都出自同一条
        // update-index 重写路径，保证两次 index 尺寸完全一致。
        git_output(root.path(), &["read-tree", "HEAD"]).unwrap();
        let old_sha = git_output(root.path(), &["rev-parse", ":f075"]).unwrap();
        let tampered_blob = std::env::temp_dir().join(format!(
            "pinvou3-tampered-blob-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::write(&tampered_blob, "tampered\n").unwrap();
        let new_sha = git_output(
            root.path(),
            &["hash-object", "-w", tampered_blob.to_str().unwrap()],
        )
        .unwrap();
        let _ = fs::remove_file(&tampered_blob);
        let old_sha = old_sha.trim();
        let new_sha = new_sha.trim();
        let restage = |sha: &str| {
            git_output(
                root.path(),
                &["update-index", "--cacheinfo", &format!("100644,{sha},f075")],
            )
            .unwrap();
        };

        // 先走一遍篡改→还原，让基线态同样是 update-index 的重写产物。
        restage(new_sha);
        restage(old_sha);
        let index = root.path().join(".git/index");
        let baseline = diff_fingerprint(root.path(), "f001");
        // 前提守卫：index 必须落在头部采样之外、8KiB 之内的目标区间。
        assert!(
            baseline.index_size > 4096 && baseline.index_size <= 8192,
            "index 尺寸 {} 不在 4–8KiB 目标区间",
            baseline.index_size
        );
        let index_stamp = current_times(&index);

        // 等价 update-index 改写暂存 blob：工作区不动，仅 index 后半段内容变化。
        restage(new_sha);
        apply_times(&index, index_stamp);
        // 同尺寸守卫：尺寸若变化，失效就不来自内容哈希，本测试失去意义。
        let after = diff_fingerprint(root.path(), "f001");
        assert_eq!(after.index_size, baseline.index_size);
        assert_eq!(after.index_modified, baseline.index_modified);
        assert_ne!(after, baseline);
        // 还原暂存内容 → 指纹回到基线（同内容不产生虚假失效）。
        restage(old_sha);
        apply_times(&index, index_stamp);
        assert_eq!(diff_fingerprint(root.path(), "f001"), baseline);
    }

    #[test]
    fn diff_fingerprint_invalidates_on_head_change() {
        // git diff --cached 比较 index 与 HEAD；reset --soft / update-ref 只改写 HEAD
        // 指向的 ref 文件、不更新 .git/index 或工作区——指纹必须覆盖此场景，否则缓存
        // 会返回陈旧差异。.git/HEAD 多为 symref（ref: refs/heads/<branch>），目标文件
        // 是 41 字节的 SHA（不同提交同尺寸），且两次改写可能落在同一 mtime 粒度内，
        // 故此处以同尺寸同 mtime 重写验证内容哈希失效。
        let root = TestDir::new("diff-fingerprint-head");
        fs::create_dir_all(root.path().join(".git/refs/heads")).unwrap();
        let head_ref = root.path().join(".git/refs/heads/main");
        fs::write(root.path().join(".git/HEAD"), "ref: refs/heads/main\n").unwrap();
        fs::write(&head_ref, format!("{}\n", "a".repeat(40))).unwrap();
        fs::write(root.path().join("main.py"), "print(1)\n").unwrap();

        let baseline = diff_fingerprint(root.path(), "main.py");
        let ref_stamp = current_times(&head_ref);
        // 等价 reset --soft <other-commit>：分支 ref 同尺寸改写（index 与工作区不变）。
        fs::write(&head_ref, format!("{}\n", "b".repeat(40))).unwrap();
        apply_times(&head_ref, ref_stamp);
        assert_ne!(baseline, diff_fingerprint(root.path(), "main.py"));
        // 内容与 mtime 全部还原 → 指纹回到基线(同内容不产生虚假失效)。
        fs::write(&head_ref, format!("{}\n", "a".repeat(40))).unwrap();
        apply_times(&head_ref, ref_stamp);
        assert_eq!(baseline, diff_fingerprint(root.path(), "main.py"));

        // symref 目标切换（checkout）也算 HEAD 变化：HEAD 文件内容改写即失效。
        // dev ref 与 main 同尺寸、并固定为同一 mtime：失效只能来自内容哈希
        // （解析路径跟随 symref 切到 dev 后，内容哈希不同）。
        let dev_ref = root.path().join(".git/refs/heads/dev");
        fs::write(&dev_ref, format!("{}\n", "c".repeat(40))).unwrap();
        apply_times(&dev_ref, ref_stamp);
        fs::write(root.path().join(".git/HEAD"), "ref: refs/heads/dev\n").unwrap();
        assert_ne!(baseline, diff_fingerprint(root.path(), "main.py"));
    }

    // linked worktree 中 `.git` 是 gitfile，指纹必须经 git 自身解析
    // index/HEAD/ref 路径；朴素 `root.join(".git/...")` 启发式会全部落空、
    // 字段恒为 0，陈旧缓存差异会被当作命中返回。依赖 git 二进制，缺失时
    // 跳过（生产路径有 git_path 的启发式回退兜底）。
    #[test]
    fn diff_fingerprint_tracks_index_and_head_in_linked_worktree() {
        if std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_err()
        {
            eprintln!(
                "skipping diff_fingerprint_tracks_index_and_head_in_linked_worktree: git binary not available"
            );
            return;
        }
        let root = TestDir::new("diff-fingerprint-worktree");
        let repo = root.path().join("repo");
        let linked = root.path().join("linked");
        fs::create_dir_all(&repo).unwrap();
        // 与生产 git 调用点一致剥掉进程级 GIT_* 覆盖：并行测试里持锁者可能把
        // GIT_DIR / GIT_INDEX_FILE 指向无关位置，会把这些子进程带离临时仓库。
        let run_git = |dir: &Path, args: &[&str]| {
            let mut command = std::process::Command::new("git");
            crate::platform::process::strip_all_git_env(&mut command);
            let output = command
                .current_dir(dir)
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .env("GIT_CONFIG_GLOBAL", "/dev/null")
                .env("GIT_AUTHOR_NAME", "test")
                .env("GIT_AUTHOR_EMAIL", "test@example.com")
                .env("GIT_COMMITTER_NAME", "test")
                .env("GIT_COMMITTER_EMAIL", "test@example.com")
                .args(args)
                .output()
                .unwrap_or_else(|error| panic!("failed to spawn git {args:?}: {error}"));
            assert!(
                output.status.success(),
                "git {args:?} failed: {}\nstderr: {}",
                output.status,
                String::from_utf8_lossy(&output.stderr).trim()
            );
        };
        run_git(&repo, &["init", "-b", "main", "."]);
        run_git(&repo, &["config", "user.name", "test"]);
        run_git(&repo, &["config", "user.email", "test@example.com"]);
        fs::write(repo.join("main.py"), "print(1)\n").unwrap();
        run_git(&repo, &["add", "main.py"]);
        run_git(&repo, &["commit", "-m", "init"]);
        run_git(&repo, &["worktree", "add", linked.to_str().unwrap()]);

        // git-path 解析必须到达真实的 per-worktree gitdir…
        let index_path = git_path(&linked, "index").expect("worktree index path");
        assert!(index_path.is_file(), "{} missing", index_path.display());
        assert_eq!(
            fs::canonicalize(&index_path).unwrap(),
            fs::canonicalize(repo.join(".git/worktrees/linked/index")).unwrap()
        );

        let baseline = diff_fingerprint(&linked, "main.py");
        // …index 与 HEAD ref 才会真正进入指纹（朴素 `.git/index` 启发式读到
        // gitfile，字段保持 0）。
        assert_ne!(baseline.index_size, 0);
        assert_ne!(baseline.head_ref_size, 0);

        // worktree 内暂存改动会重写其 index → 指纹变化（暂存变化不得命中陈旧缓存）。
        fs::write(linked.join("main.py"), "print(2)\n").unwrap();
        run_git(&linked, &["add", "main.py"]);
        assert_ne!(baseline, diff_fingerprint(&linked, "main.py"));

        // 先提交暂存改动，再用真·空提交移动一次 HEAD。下面的快照恰好夹住
        // 空提交：它只改写 commondir 里的分支 ref，不碰 worktree 文件与
        // index，因此断言能把差异唯一归因于解析出的 HEAD ref 追踪，而不是
        // 上面暂存步骤留下的文件/index 噪声。
        run_git(&linked, &["commit", "-m", "second"]);
        let after_commit = diff_fingerprint(&linked, "main.py");
        run_git(&linked, &["commit", "--allow-empty", "-m", "third"]);
        let moved = diff_fingerprint(&linked, "main.py");
        // 同一 worktree 文件、同一 index 内容、ref 仍为 41 字节格式……
        assert_eq!(after_commit.file_size, moved.file_size);
        assert_eq!(after_commit.file_modified, moved.file_modified);
        assert_eq!(after_commit.head_tail_hash, moved.head_tail_hash);
        assert_eq!(after_commit.index_size, moved.index_size);
        assert_eq!(after_commit.index_hash, moved.index_hash);
        assert_eq!(after_commit.head_ref_size, moved.head_ref_size);
        // ……只有解析出的 ref 内容变化，指纹必须经 HEAD-ref 字段失效。
        // 若解析失效，head_ref_* 恒 0、两次指纹全同——与暂存前的原始
        // baseline 比较是发现不了这种破坏的。
        assert_ne!(after_commit.head_ref_hash, moved.head_ref_hash);
    }

    #[test]
    fn lists_and_previews_workspace_files_without_build_directories() {
        let root = TestDir::new("listing");
        fs::create_dir_all(root.path().join("src")).unwrap();
        fs::create_dir_all(root.path().join("node_modules/pkg")).unwrap();
        fs::write(root.path().join("src/main.rs"), "fn main() {}").unwrap();
        fs::write(root.path().join("node_modules/pkg/index.js"), "ignored").unwrap();

        let listing = list_workspace(root.path(), None).unwrap();
        assert!(listing.entries.iter().any(|entry| entry.name == "src"));
        assert!(
            !listing
                .entries
                .iter()
                .any(|entry| entry.name == "node_modules")
        );
        let preview = preview_workspace_file(root.path(), "src/main.rs").unwrap();
        assert_eq!(preview.kind, "text");
        assert_eq!(preview.text.as_deref(), Some("fn main() {}"));
    }

    #[test]
    fn resolves_workspace_resources_from_relative_absolute_and_file_uri_paths() {
        let root = TestDir::new("resource-resolve");
        let path = root.path().join("docs/report file.md");
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, "# report\n").unwrap();
        let path = fs::canonicalize(path).unwrap();

        for resource in [
            "docs/report file.md".to_string(),
            path.to_string_lossy().into_owned(),
            tauri::Url::from_file_path(&path).unwrap().to_string(),
        ] {
            assert_eq!(
                resolve_workspace_resource(root.path(), &resource).unwrap(),
                path
            );
        }
        assert!(resolve_workspace_resource(root.path(), "https://example.com/report.md").is_err());
        assert!(resolve_workspace_resource(root.path(), "../report.md").is_err());
        assert!(resolve_workspace_resource(root.path(), "docs").is_err());
    }

    #[test]
    fn preview_kind_covers_special_names_and_sniffs_unknown_text() {
        let root = TestDir::new("file-kind");
        fs::write(root.path().join("Dockerfile"), "FROM scratch\n").unwrap();
        fs::write(root.path().join("LICENSE"), "MIT License\n").unwrap();
        fs::write(root.path().join(".gitignore"), "target/\n").unwrap();
        fs::write(root.path().join("script.rb"), "puts 1\n").unwrap();
        fs::write(root.path().join("notes"), "plain text without extension\n").unwrap();
        fs::write(root.path().join("empty.weird"), "").unwrap();
        fs::write(
            root.path().join("payload.bin"),
            [0x89, 0x50, 0x4E, 0x47, 0x00, 0x0D],
        )
        .unwrap();

        for (path, expected) in [
            ("Dockerfile", "text"),
            ("LICENSE", "text"),
            (".gitignore", "text"),
            ("script.rb", "text"),
            ("notes", "text"),
            ("empty.weird", "text"),
            ("payload.bin", "binary"),
        ] {
            let preview = preview_workspace_file(root.path(), path).unwrap();
            assert_eq!(preview.kind, expected, "unexpected kind for {path}");
        }
        let preview = preview_workspace_file(root.path(), "notes").unwrap();
        assert_eq!(
            preview.text.as_deref(),
            Some("plain text without extension\n")
        );
    }

    #[test]
    fn preview_kind_tolerates_utf8_boundary_at_sniff_cutoff() {
        let root = TestDir::new("utf8-boundary");
        // 8191 个 ASCII + 一个三字节字符：8KB 采样正好切在该字符中间。
        let content = format!("{}中", "a".repeat(8191));
        fs::write(root.path().join("boundary.unknown"), content).unwrap();
        let preview = preview_workspace_file(root.path(), "boundary.unknown").unwrap();
        assert_eq!(preview.kind, "text");
    }

    #[test]
    fn non_git_baseline_detects_added_modified_and_deleted_files() {
        // baseline 文件路径派生自 PINVOU3_HOME，与改 home 的测试并发会互相误删目录。
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let root = TestDir::new("baseline");
        let session_id = format!("workspace-test-{}", std::process::id());
        fs::write(root.path().join("before.txt"), "before").unwrap();
        fs::write(root.path().join("delete.txt"), "delete").unwrap();
        capture_baseline(&session_id, root.path()).unwrap();
        fs::write(root.path().join("before.txt"), "after-and-longer").unwrap();
        fs::remove_file(root.path().join("delete.txt")).unwrap();
        fs::write(root.path().join("added.txt"), "added").unwrap();

        let changes = workspace_changes(&session_id, root.path()).unwrap();
        assert!(
            changes
                .changes
                .iter()
                .any(|change| change.relative_path == "before.txt" && change.status == "modified")
        );
        assert!(
            changes
                .changes
                .iter()
                .any(|change| change.relative_path == "delete.txt" && change.status == "deleted")
        );
        assert!(changes.changes.iter().any(|change| {
            change.relative_path == "added.txt"
                && change.status == "added"
                && change.origin == "session"
        }));
        let _ = fs::remove_file(baseline_path(&session_id));
    }

    /// 初始化一个含 main + feature 两个分支的 git 仓库；git 不可用时返回 None 跳过。
    fn init_git_repo(label: &str) -> Option<TestDir> {
        let root = TestDir::new(label);
        let run = |args: &[&str]| {
            let mut command = crate::platform::process::HiddenCommand::new("git");
            crate::platform::process::strip_git_override_env(&mut command);
            command
                .current_dir(root.path())
                .args(args)
                .output()
                .map(|output| output.status.success())
                .unwrap_or(false)
        };
        if !run(&["init", "-b", "main"]) {
            return None;
        }
        // stash/commit 都会创建提交对象，测试环境未必有全局身份，仓库级配置兜底。
        if !run(&["config", "user.email", "test@example.com"])
            || !run(&["config", "user.name", "test"])
        {
            return None;
        }
        fs::write(root.path().join("file.txt"), "v1").unwrap();
        for args in [
            &["add", "."][..],
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-m",
                "init",
            ][..],
            &["branch", "feature"][..],
        ] {
            if !run(args) {
                return None;
            }
        }
        Some(root)
    }

    #[test]
    fn workspace_branches_lists_local_branches() {
        let Some(root) = init_git_repo("branches") else {
            return;
        };
        let result = workspace_branches(root.path()).unwrap();
        assert!(result.git);
        assert_eq!(result.current.as_deref(), Some("main"));
        assert!(result.branches.contains(&"main".to_string()));
        assert!(result.branches.contains(&"feature".to_string()));
    }

    #[test]
    fn workspace_branches_sorts_by_recent_commit() {
        let Some(root) = init_git_repo("branches-recency") else {
            return;
        };
        // 在 feature 上多一个提交：feature 的 committerdate 更新，应排在 main 前。
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        fs::write(root.path().join("file.txt"), "v2").unwrap();
        git_output(
            root.path(),
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-am",
                "v2",
            ],
        )
        .unwrap();
        git_output(root.path(), &["checkout", "main"]).unwrap();
        let result = workspace_branches(root.path()).unwrap();
        assert_eq!(result.branches.first().map(String::as_str), Some("feature"));
    }

    #[test]
    fn workspace_branches_reports_non_git_workspace() {
        let root = TestDir::new("branches-non-git");
        let result = workspace_branches(root.path()).unwrap();
        assert!(!result.git);
        assert!(result.current.is_none());
        assert!(result.branches.is_empty());
    }

    #[test]
    fn checkout_workspace_branch_switches_current_branch() {
        let Some(root) = init_git_repo("checkout") else {
            return;
        };
        let result =
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Carry, None)
                .unwrap();
        assert_eq!(result.current.as_deref(), Some("feature"));
        // 空分支名与选项注入被拒绝。
        assert!(
            checkout_workspace_branch(root.path(), "  ", BranchSwitchMode::Carry, None).is_err()
        );
        assert!(
            checkout_workspace_branch(root.path(), "--force", BranchSwitchMode::Carry, None)
                .is_err()
        );
        // 未知模式被拒绝。
        assert!(BranchSwitchMode::parse("discard").is_err());
    }

    /// 不存在的分支名若恰是工作区文件路径，裸 `git checkout <name>` 会按
    /// pathspec 还原该文件、丢弃未提交更改；必须先校验分支存在。
    #[test]
    fn checkout_workspace_branch_rejects_non_branch_pathspec() {
        let Some(root) = init_git_repo("checkout-pathspec") else {
            return;
        };
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        assert!(
            checkout_workspace_branch(root.path(), "file.txt", BranchSwitchMode::Carry, None)
                .is_err()
        );
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1-dirty"
        );
        assert_eq!(git_branch(root.path()).as_deref(), Some("main"));
    }

    #[test]
    fn checkout_workspace_branch_stash_mode_restores_changes() {
        let Some(root) = init_git_repo("checkout-stash") else {
            return;
        };
        // 未跟踪文件 + 已跟踪文件修改，stash 模式应全部携带并恢复。
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        fs::write(root.path().join("scratch.txt"), "scratch").unwrap();
        let result =
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None)
                .unwrap();
        assert_eq!(result.current.as_deref(), Some("feature"));
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1-dirty"
        );
        assert_eq!(
            fs::read_to_string(root.path().join("scratch.txt")).unwrap(),
            "scratch"
        );
        assert_eq!(result.dirty_count, 2);
        // stash 已 pop，stash 列表应为空。
        let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
        assert!(stash_list.trim().is_empty());
    }

    /// Stash restore conflict: the switch succeeds, the stash entry is kept,
    /// and the working tree is left with conflict markers; the command fails
    /// without rolling the branch back.
    #[test]
    fn checkout_workspace_branch_stash_pop_conflict_keeps_stash_entry() {
        let Some(root) = init_git_repo("checkout-stash-conflict") else {
            return;
        };
        // feature commits file.txt = v2; restoring the uncommitted main-side
        // edit on top of it conflicts by construction.
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        fs::write(root.path().join("file.txt"), "v2").unwrap();
        git_output(
            root.path(),
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-am",
                "v2",
            ],
        )
        .unwrap();
        git_output(root.path(), &["checkout", "main"]).unwrap();
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        assert!(
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None)
                .is_err()
        );
        let state = workspace_branches(root.path()).unwrap();
        assert_eq!(state.current.as_deref(), Some("feature"));
        let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
        assert!(!stash_list.trim().is_empty());
        let content = fs::read_to_string(root.path().join("file.txt")).unwrap();
        assert!(content.contains("<<<<<<<"));
    }

    /// Stash 模式下 checkout 失败：应先 pop 恢复现场再报错——更改回到工作区、
    /// stash 列表清空、分支保持不变。checkout 失败用「目标分支指向的提交对象
    /// 缺失」构造：git 对 ignored 文件/目录阻挡、只读目录等自然场景会静默
    /// 处理甚至返回 0，对象缺失才是确定性的非零失败，且能通过本地分支名校验。
    #[test]
    fn checkout_workspace_branch_stash_mode_restores_changes_on_checkout_failure() {
        let Some(root) = init_git_repo("checkout-stash-failure") else {
            return;
        };
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        fs::write(root.path().join("file.txt"), "v2").unwrap();
        git_output(
            root.path(),
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-am",
                "v2",
            ],
        )
        .unwrap();
        git_output(root.path(), &["checkout", "main"]).unwrap();
        // 删除 feature 提交的松散对象：ref 仍在（分支列表校验可过），checkout 必败。
        let dangling = git_output(root.path(), &["rev-parse", "feature"]).unwrap();
        let dangling = dangling.trim();
        let object_path = root
            .path()
            .join(".git/objects")
            .join(&dangling[..2])
            .join(&dangling[2..]);
        fs::remove_file(&object_path).unwrap();

        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        assert!(
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None)
                .is_err()
        );
        // 现场已恢复：dirty 更改回到工作区，stash 已 pop，仍在 main。
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1-dirty"
        );
        let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
        assert!(stash_list.trim().is_empty());
        // 不能用 workspace_branches 断言分支：其 --sort=-committerdate 需要读取
        // 全部分支的提交对象，悬空的 feature ref 会让它整体报错。
        assert_eq!(git_branch(root.path()).as_deref(), Some("main"));
    }

    #[test]
    fn workspace_branches_reports_detached_head_without_current() {
        let Some(root) = init_git_repo("branches-detached") else {
            return;
        };
        git_output(root.path(), &["checkout", "--detach"]).unwrap();
        let result = workspace_branches(root.path()).unwrap();
        assert!(result.git);
        assert_eq!(result.current, None);
        assert!(result.branches.contains(&"main".to_string()));
    }

    /// Stash 模式的 push 可能不创建条目（status 非空但无可暂存内容，典型是
    /// 仅子模块工作区内容变更——status 显示 modified 但 push 是 no-op）：此时
    /// pop 会误弹用户已有的 stash 条目并把其内容应用到新分支。应跳过恢复，
    /// 用户 stash 原样保留。
    #[test]
    fn checkout_workspace_branch_stash_mode_skips_pop_when_push_created_no_entry() {
        let Some(root) = init_git_repo("stash-noop-push") else {
            return;
        };
        // 本地子模块仓库（file 协议克隆在新版 git 默认拒绝，需显式允许）。
        let sub = TestDir::new("stash-noop-push-sub");
        let sub_run = |args: &[&str]| {
            let mut command = crate::platform::process::HiddenCommand::new("git");
            crate::platform::process::strip_git_override_env(&mut command);
            command
                .current_dir(sub.path())
                .args(args)
                .output()
                .map(|output| output.status.success())
                .unwrap_or(false)
        };
        assert!(sub_run(&["init", "-b", "main"]));
        assert!(sub_run(&["config", "user.email", "test@example.com"]));
        assert!(sub_run(&["config", "user.name", "test"]));
        fs::write(sub.path().join("lib.txt"), "hi").unwrap();
        assert!(sub_run(&["add", "."]));
        assert!(sub_run(&["commit", "-m", "init"]));
        let sub_url = sub.path().to_string_lossy().into_owned();
        git_output(
            root.path(),
            &[
                "-c",
                "protocol.file.allow=always",
                "submodule",
                "add",
                &sub_url,
                "sub",
            ],
        )
        .unwrap();
        git_output(root.path(), &["commit", "-am", "add submodule"]).unwrap();
        // 用户在切换前就存在的 stash 条目：本流程不得动它。
        fs::write(root.path().join("file.txt"), "user-edit").unwrap();
        git_output(root.path(), &["stash", "push", "-m", "user-stash"]).unwrap();
        // 唯一剩余变更 = 子模块内容修改：push 不创建条目。
        fs::write(root.path().join("sub").join("lib.txt"), "dirty").unwrap();
        assert!(!git_status_entries(root.path()).unwrap().is_empty());

        checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None).unwrap();
        assert_eq!(git_branch(root.path()).as_deref(), Some("feature"));
        // 用户 stash 未被弹出：file.txt 仍是 feature 分支的已提交内容。
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1"
        );
        let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
        assert!(stash_list.contains("user-stash"));
    }

    #[test]
    fn checkout_workspace_branch_commit_mode_commits_before_switch() {
        let Some(root) = init_git_repo("checkout-commit") else {
            return;
        };
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        fs::write(root.path().join("scratch.txt"), "scratch").unwrap();
        // 空提交信息被拒绝。
        assert!(
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Commit, Some("  "))
                .is_err()
        );
        let result = checkout_workspace_branch(
            root.path(),
            "feature",
            BranchSwitchMode::Commit,
            Some("wip: before switch"),
        )
        .unwrap();
        assert_eq!(result.current.as_deref(), Some("feature"));
        // 更改已提交到 main（原分支），目标分支工作区是干净的。
        assert_eq!(result.dirty_count, 0);
        let main_log = git_output(root.path(), &["log", "-1", "--format=%s", "main"]).unwrap();
        assert_eq!(main_log.trim(), "wip: before switch");
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1"
        );
    }

    #[test]
    fn checkout_workspace_branch_carry_conflict_keeps_workspace() {
        let Some(root) = init_git_repo("checkout-conflict") else {
            return;
        };
        // feature 分支上 file.txt 改为 v2 并提交；main 上未提交修改同一文件，
        // carry 模式切换必然被 git 拒绝且工作区保持原样。
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        fs::write(root.path().join("file.txt"), "v2").unwrap();
        git_output(
            root.path(),
            &[
                "-c",
                "user.email=test@example.com",
                "-c",
                "user.name=test",
                "commit",
                "-am",
                "v2",
            ],
        )
        .unwrap();
        git_output(root.path(), &["checkout", "main"]).unwrap();
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        assert!(
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Carry, None)
                .is_err()
        );
        let state = workspace_branches(root.path()).unwrap();
        assert_eq!(state.current.as_deref(), Some("main"));
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1-dirty"
        );
    }

    #[test]
    fn git_baseline_distinguishes_existing_and_session_changes() {
        let root = TestDir::new("git-origin");
        let path = root.path().join("dirty.txt");
        fs::write(&path, "before").unwrap();
        let before = fingerprint(&path, true).unwrap().unwrap();
        let baseline = WorkspaceBaseline {
            workspace_path: root.path().to_string_lossy().into_owned(),
            git: true,
            dirty_paths: BTreeSet::from(["dirty.txt".to_string()]),
            entries: BTreeMap::from([("dirty.txt".to_string(), before)]),
        };

        assert_eq!(
            classify_origin(root.path(), Some(&baseline), "dirty.txt").unwrap(),
            "preexisting"
        );
        fs::write(&path, "after and longer").unwrap();
        assert_eq!(
            classify_origin(root.path(), Some(&baseline), "dirty.txt").unwrap(),
            "preexisting_modified"
        );
        fs::write(root.path().join("new.txt"), "new").unwrap();
        assert_eq!(
            classify_origin(root.path(), Some(&baseline), "new.txt").unwrap(),
            "session"
        );
    }

    #[test]
    fn git_remote_url_is_shareable_and_drops_credentials() {
        assert_eq!(
            sanitize_git_remote_url("git@github.com:pinvou/pinvou-agent.git").as_deref(),
            Some("https://github.com/pinvou/pinvou-agent")
        );
        assert_eq!(
            sanitize_git_remote_url(
                "https://token:secret@example.com/team/repo.git?ref=main#readme"
            )
            .as_deref(),
            Some("https://example.com/team/repo")
        );
        assert_eq!(sanitize_git_remote_url("/private/local/repo"), None);
    }

    /// GIT_* environment isolation: GIT_INDEX_FILE/GIT_OBJECT_DIRECTORY
    /// injected by the host shell (or by tests concurrently holding ENV_LOCK
    /// to write the environment) must not redirect the workspace's branch and
    /// stash operations to unrelated locations. Regression background: under
    /// full parallel runs this pollution really occurred, making the git
    /// subprocess cases fail randomly across runs (serial runs were all
    /// green).
    #[test]
    fn workspace_git_operations_ignore_host_git_environment() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let bogus = TestDir::new("bogus-git-env");
        // Capture the host's values first: restoration must put back exactly
        // this state, not an unconditionally-absent one (the launching shell
        // may legitimately carry these variables).
        let prior_index = std::env::var_os("GIT_INDEX_FILE");
        let prior_objects = std::env::var_os("GIT_OBJECT_DIRECTORY");
        // Panic-safe restore: Drop puts back the captured values on every
        // exit path — normal end, the early return inside catch_unwind, and
        // caught assertion panics alike.
        let env_guard = crate::platform::paths::tests::EnvVarGuard::capture(&[
            "GIT_INDEX_FILE",
            "GIT_OBJECT_DIRECTORY",
        ]);
        // SAFETY: ENV_LOCK serializes process environment writes (the
        // crate-wide convention); the workspace git subprocess itself strips
        // GIT_* override variables and is unaffected by this write.
        unsafe {
            std::env::set_var("GIT_INDEX_FILE", bogus.path().join("index"));
            std::env::set_var("GIT_OBJECT_DIRECTORY", bogus.path().join("objects"));
        }
        let result = std::panic::catch_unwind(|| {
            let Some(root) = init_git_repo("env-isolation") else {
                return;
            };
            fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
            // Commit objects must land in this repository's own object store
            // so branch switching and stash restoration work end to end.
            checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None)
                .unwrap();
            assert_eq!(git_branch(root.path()).as_deref(), Some("feature"));
            assert_eq!(
                fs::read_to_string(root.path().join("file.txt")).unwrap(),
                "v1-dirty"
            );
            let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
            assert!(stash_list.trim().is_empty());
            // The pollution targets stay clean: neither the index nor the
            // objects were redirected.
            assert!(
                !bogus.path().join("index").exists(),
                "GIT_INDEX_FILE must be stripped"
            );
            assert!(
                bogus
                    .path()
                    .read_dir()
                    .map(|entries| entries.count() == 0)
                    .unwrap_or(true),
                "GIT_OBJECT_DIRECTORY must be stripped"
            );
        });
        // Restore before unwrapping the result, so the original environment
        // is back even when the unwrapped panic propagates; then assert the
        // restoration is complete.
        drop(env_guard);
        assert_eq!(
            std::env::var_os("GIT_INDEX_FILE"),
            prior_index,
            "GIT_INDEX_FILE must be restored to its pre-test state"
        );
        assert_eq!(
            std::env::var_os("GIT_OBJECT_DIRECTORY"),
            prior_objects,
            "GIT_OBJECT_DIRECTORY must be restored to its pre-test state"
        );
        result.unwrap();
    }
}
