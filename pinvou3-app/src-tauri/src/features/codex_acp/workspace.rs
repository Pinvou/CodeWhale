// architecture-guard: allow-target-cfg -- the cfg(unix)-gated test fixtures in this file share one non-portable shape: a real symlink. The round-12 baseline-gate convergence pin, the round-48 untracked-diff regression (refuses an escaping link, reads a local one canonically), and the round-51 fingerprint/baseline pins (a dirty symlink→regular-file must be skipped, never hashed through, and the tracked-ness oracle must answer literal paths so a glob-metacharacter filename cannot alias a tracked file) all need std::os::unix::fs::symlink or a `*`-bearing filename that Windows cannot express; the semantics under test are platform-independent, only the fixtures cannot be.
// Round-15 review: the exception now also covers the test module's macOS-gated
// APFS case-folding behavioral pin
// (case_variant_spellings_are_blocked_end_to_end) — that pin is trivially true on
// non-macOS platforms, so it is gated rather than silently green.
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::fs;
use std::io::{Read, Seek, SeekFrom};
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
    /// Number of uncommitted-change entries (git status), for the frontend to prompt for confirmation before switching.
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
    // Round-7 review: same policy as preview/diff — when the requested directory
    // itself crosses a credential component, refuse wholesale instead of returning
    // an empty list; the "success-empty-list vs error" fork itself would leak the
    // blocked directory's existence (listing and search never reveal its existence,
    // see workspace_entry's filtering comment below; child filtering cannot stop
    // the "this directory is listable" signal).
    if relative_path_crosses_blocked_component(&relative) {
        bail!("工作区路径途经凭据文件，拒绝列出: {relative}");
    }
    let directory = resolve_existing_path(&root, &relative, true)?;
    // Resolution leg (same constraint as the preview lane): a clean-named symlink
    // (notes -> .aws) whose resolved landing is a blocked directory is refused too.
    if path_crosses_blocked_component(&directory) {
        bail!("工作区路径途经凭据文件，拒绝列出: {relative}");
    }
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
    if relative_path_crosses_blocked_component(&relative) {
        bail!("工作区路径途经凭据文件，拒绝读取: {relative}");
    }
    let path = resolve_existing_path(root, &relative, false)?;
    // The resolved real landing must still pass the blacklist: within the workspace,
    // the link name `notes.txt -> .env` is itself clean, and blocking on the
    // caller's spelling alone is bypassed by the symlink.
    if path_crosses_blocked_component(&path) {
        bail!("工作区路径途经凭据文件，拒绝读取: {relative}");
    }
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
        // 扩展名→MIME 统一走 attachments 的共享映射表；表外扩展名沿用
        // 原行为回退 image/png。
        let mime = super::attachments::image_mime_type(&path).unwrap_or("image/png");
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
    // Round-16 review (documenting the desktop exemption): the same exemption as
    // resolve_workspace_file — this resolution performs no credential-component
    // check; all call sites (open/reveal/resource) are desktop commands whose
    // content goes to local external apps/readers rather than model context and is
    // unreachable from the phone.
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

/// Whether the session's stored baseline is already current for `root`.
/// Wraps the private `load_baseline`, which self-invalidates when the stored
/// `workspace_path` no longer matches `root` (a workspace moved by a rebind
/// whose recapture failed or never ran): such a baseline reads as absent.
/// `root` is canonicalized first, matching `capture_baseline`'s stored
/// spelling — the rebind drain passes the LEXICAL translation
/// (`to.join(suffix)`), and an inner symlink component (not just the macOS
/// `/var` prefix alias, where both sides already sit canonical) would make
/// the raw-string compare read not-current on every run, so the gate would
/// keep resetting a healthy baseline instead of converging (round-12 P2-1).
/// A root that cannot be canonicalized (vanished mid-run) reads as not
/// current; the best-effort recapture then fails honestly and is logged.
/// The rebind's baseline-recapture drain gates on this (round-11 review M1)
/// so a healthy session with a fresh baseline is never reset — resetting it
/// would wipe the "changes since session start" view (non-git) or flip
/// every currently-dirty file to preexisting (git).
pub fn baseline_is_current(session_id: &str, root: &Path) -> bool {
    let Ok(root) = canonical_workspace(root) else {
        return false;
    };
    matches!(load_baseline(session_id, &root), Ok(Some(_)))
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
            // Round-12 review: blocked entries are never served by the preview/diff
            // lanes, so the baseline fingerprint should not read their content —
            // consistent with the changes lane's established rationale (a blocked
            // entry must not be read first just to be discarded); an unreadable
            // blocked name (like a chmod-000 .env) previously failed the whole
            // fingerprint pass and cascaded into session-creation failure. dirty_paths
            // keeps the full set, so the desktop branch's dirty-count semantics are
            // unchanged.
            if relative_path_crosses_blocked_component(&change.relative_path) {
                continue;
            }
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
    // `atomic_write` stages inside the target's parent, so the directory
    // must exist first (unchanged from the hand-rolled writer).
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("创建工作区基线目录失败: {}", parent.display()))?;
    }
    let payload = serde_json::to_vec_pretty(&baseline).context("序列化工作区基线失败")?;
    // The hand-rolled pid-carrying tmp+rename (the CLI's `code` family
    // captures baselines from a second process against the same session
    // directories, so a fixed `.tmp` name would let two surfaces rename
    // each other's half-written file into place) carried no fsync, and its
    // two cleanup arms existed because a failed write (ENOSPC, EACCES)
    // would strand `…tmp.<pid>` forever. `atomic_write` subsumes both: the
    // unique per-writer staging name, the fsync-before-rename the sibling
    // stores migrated for, and the same litter rule.
    crate::platform::filesystem::atomic_write(&path, &payload)
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
    // The credential filter must precede classify_origin: classification reads the
    // content of dirty files in a git workspace and hashes them (SHA-256); a blocked
    // entry (like .env) must not be read just to produce an echo destined to be
    // discarded. Listing and search never reveal credential files' existence (see
    // list_workspace's filtering comment); the changes lane carries the same
    // constraint — name/status metadata must likewise never echo a blocked
    // component.
    changes.retain(|change| !relative_path_crosses_blocked_component(&change.relative_path));
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
    // Sort by most recent commit time descending (committerdate), matching IDE
    // branch-list conventions; no longer sorted by name.
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

/// Dirty-workspace branch switching strategies: carry = plain checkout (changes
/// ride along with the workspace; any conflict fails the whole operation);
/// stash = `git stash -u` first, then automatically pop after the switch;
/// commit = commit all changes (including untracked files) to the current branch
/// before switching.
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

/// Process-level mutex: serializes a branch switch's full git sequence. Concurrent
/// calls (two windows, rapid clicks) would interleave two switches' stash
/// push/checkout/pop into intermediate states spanning branches and stash entries,
/// leaving neither rollback nor error reporting attributable to a single operation.
static CHECKOUT_LOCK: Mutex<()> = Mutex::new(());

/// What refs/stash currently points at; None when the repository has no stash
/// entries. Used to tell whether a stash push actually created an entry (see
/// checkout_workspace_branch's Stash mode).
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
    // Reject branch names starting with '-' so git cannot misparse them as options.
    if branch.is_empty() || branch.starts_with('-') {
        bail!("无效的分支名: {branch}");
    }
    // Only allow switching to existing local branches. Otherwise, when the given
    // name happens to be a file path inside the workspace, `git checkout <name>`
    // restores that file from the index as a pathspec, silently discarding
    // uncommitted changes.
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
            // Stash (including untracked files) → switch → restore. If checkout fails,
            // pop to restore the scene before reporting; if the restore also fails, the
            // error must state that the changes are safely kept in the stash so the
            // scene does not look lost. When the post-switch pop conflicts, git keeps
            // that stash entry; untracked files may exist only in that entry, so the
            // content must be confirmed before any cleanup — never guide straight to
            // drop.
            // A push may create no entry (status non-empty but nothing stashable —
            // typically only submodule worktree content changed): popping then would
            // wrongly pop the user's existing stash entry and apply its content to the
            // new branch. Compare refs/stash before and after the push; pop only when
            // an entry was actually created.
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
            let commit_args = checkout_commit_args(&root, message);
            let commit_refs: Vec<&str> = commit_args.iter().map(String::as_str).collect();
            if let Err(error) = git_commit_for_checkout(&root, &commit_refs) {
                // The Stash arm restores the scene itself on failure; in commit mode a
                // failed `git commit` (no identity, hook refusal) leaves `git add -A`'s
                // rewrite of the user's index in place — the error must disclose that
                // fact rather than leaving changes silently staged (same disclosure as
                // the CLI lane's round-40, keeping both ends consistent).
                bail!("{error:#}；本次 `git add -A` 已暂存工作区——继续前请先 git status 检查");
            }
            git_output(&root, &["checkout", branch])?;
        }
    }
    workspace_branches(&root)
}

/// The commit-mode lane's `git commit` arguments (a mirror of the CLI's
/// `commit_command_args`, same hardening semantics, keeping the two ends from
/// drifting apart again):
///
/// - `commit.gpgsign=false` is an explicit deviation from the user's git config:
///   this lane runs git with stdin closed and no deadline, so a
///   `commit.gpgsign=true` user's first commit-mode switch would stall on the GPG
///   pinentry prompt and hang the session lane forever. The cost is disclosed in
///   docs: commits made through this lane are unsigned (same semantics as the CLI
///   lane, docs/pinvou-cli.md).
/// - Explicit identity: the identity is read back from the user's config
///   (repo-local first, then global/system) and passed explicitly via `-c`;
///   `GIT_AUTHOR_*`/`GIT_COMMITTER_*` exported into the environment are removed by
///   [`git_commit_for_checkout`] and cannot override the identity resolved here.
///   When no configuration yields an identity, `user.useConfigOnly=true` makes the
///   commit fail honestly instead of fabricating `user@hostname`.
fn checkout_commit_args(root: &Path, message: &str) -> Vec<String> {
    let mut args = vec!["-c".to_owned(), "commit.gpgsign=false".to_owned()];
    match ambient_git_identity(root) {
        Some((name, email)) => {
            args.push("-c".to_owned());
            args.push(format!("user.name={name}"));
            args.push("-c".to_owned());
            args.push(format!("user.email={email}"));
        }
        None => {
            args.push("-c".to_owned());
            args.push("user.useConfigOnly=true".to_owned());
        }
    }
    args.push("commit".to_owned());
    args.push("-m".to_owned());
    args.push(message.to_owned());
    args
}

/// The ambient identity (repo-local first, then global/system) — the identity the
/// GUI session lane would actually commit with. A read-only `git config` probe: no
/// hooks run. Uses the same `strip_git_override_env` path as
/// [`checkout_commit_args`]'s commit, so both sides see the same effective
/// repository and the same set of config files; the user's global config is
/// deliberately visible — what is being read is the identity the user actually
/// commits with.
fn ambient_git_identity(root: &Path) -> Option<(String, String)> {
    let capture = git_output(
        root,
        &["config", "--null", "--get-regexp", r"^user\.(name|email)$"],
    )
    .ok()?;
    let mut name = None;
    let mut email = None;
    for record in capture.split('\0') {
        let Some((key, value)) = record.split_once('\n') else {
            continue;
        };
        match key.trim() {
            "user.name" => name = Some(value.trim().to_owned()),
            "user.email" => email = Some(value.trim().to_owned()),
            _ => {}
        }
    }
    Some((name?, email?))
}

/// The commit-mode lane's own `git commit`: same as [`git_output`], but it
/// additionally removes the identity variables from the environment
/// (`GIT_AUTHOR_*`/`GIT_COMMITTER_*` — platform::process's full
/// `GIT_IDENTITY_KEYS` table) so the identity [`checkout_commit_args`] passes
/// explicitly via `-c` actually takes effect — otherwise an identity exported by
/// the caller's shell would override the one resolved from the user's config.
fn git_commit_for_checkout(root: &Path, arguments: &[&str]) -> Result<String> {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    for key in crate::platform::process::GIT_IDENTITY_KEYS {
        command.env_remove(key);
    }
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
    let relative = git_output(root, &["rev-parse", "--git-path", name])
        .ok()?
        .trim()
        .to_string();
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
    // Simple cap: clear the whole cache when over the limit, avoiding unbounded
    // in-process growth (memory only, never persisted).
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
    // Both the synthetic untracked diff and git diff (including staged) carry file
    // content; credential-component files are refused from the diff lane, same
    // policy as preview_workspace_file.
    if relative_path_crosses_blocked_component(&relative) {
        bail!("工作区路径途经凭据文件，拒绝生成差异: {relative}");
    }
    let path = root.join(&relative);
    ensure_path_within_workspace(&root, &path)?;
    // Round-5 review BLOCKER: git pathspecs recurse by directory prefix, and
    // `:(literal)` turns off globbing but not recursion — `git diff -- ":(literal)src"`
    // would send the full patch of the tracked/staged `src/.env` to the phone while
    // the lexical check only sees the harmless ancestor name "src". Directory targets
    // (including the whole-repo empty relative path) are refused wholesale: the diff
    // lane works on file paths as given by the changes lane, and directory-level
    // diffs were never a contract feature; file-level pathspecs do not recurse.
    // Round-8 review: when the directory vanishes from the worktree (`git rm -r`/
    // `rm -rf`/sparse-checkout/dangling links), is_dir no longer holds, and the git
    // side's expansion check inside the diff arm (pathspec hits must be exactly the
    // requested file) covers the same leak.
    if path.is_dir() {
        bail!("工作区差异仅支持文件路径: {relative}");
    }
    // Resolution leg (same constraint as the preview lane): when the landing through
    // a symlink hits the blacklist, refuse before any content read (including the
    // diff cache's fingerprint hashing). A canonicalize failure (deleted file) keeps
    // the original spelling and is handled by the git arm below via the lexical leg.
    if let Ok(landing) = std::fs::canonicalize(&path) {
        if path_crosses_blocked_component(&landing) {
            bail!("工作区路径途经凭据文件，拒绝生成差异: {relative}");
        }
        // Round-12 review P3: the resolved landing must also be re-checked for
        // containment — canonicalize resolves links but does not guarantee the landing
        // stays inside the workspace. The preview lane (resolve_existing_path) and
        // untracked_diff both have this leg; although the diff git arm only serves
        // repository content, the "canonicalize → blacklist → containment" invariant
        // is aligned explicitly across the three arms, keeping refusal semantics
        // consistent with preview.
        ensure_path_within_workspace(&root, &landing)?;
    }

    if let Some(cached) = diff_cache_get(session_id, &root, &relative) {
        return Ok(WorkspaceDiff {
            relative_path: relative,
            text: cached.text,
            truncated: cached.truncated,
        });
    }

    let mut text = if is_git_workspace(&root) {
        // Round-51 review: `:(literal)` — `--` stops option parsing but not
        // pathspec globbing, so a requested `a*x.txt` used to render the
        // tracked `abx.txt`'s diff instead of its own. Same literal prefix
        // the tracked-ness oracle below uses.
        let literal = format!(":(literal){relative}");
        // Round-8 review BLOCKER: the directory refusal above hinges on the worktree
        // surviving — `git rm -r src`/`rm -rf src`/sparse-checkout/dangling symlinks
        // make the on-disk directory disappear, is_dir no longer holds, a canonicalize
        // failure skips the resolution leg, and the pathspec still expands recursively
        // by directory prefix, sending the full patch of the tracked/staged `src/.env`
        // (the deletion arm renders the old credential bytes as `-` lines) to the
        // phone. Refuse based on git's actual expansion: the index and HEAD entries
        // hit by the pathspec must be exactly the requested file itself. A deleted
        // file's removal diff is unaffected (both lists contain exactly itself); an
        // untracked new file empties both. An ls-tree failure is treated as empty:
        // with an unborn HEAD (empty repo) there is no committed content to expand,
        // and directory expansion in that state is independently refused by the
        // ls-files index leg (the `--cached` arm below needs HEAD and does not fail
        // under an unborn HEAD, so it cannot be relied on as a backstop); with a valid
        // HEAD, an ls-tree failure (corrupt objects) means the diff arm that reads the
        // same trees cannot produce content either.
        // Round-11 review BLOCKER: the expansion check was originally gated on
        // `!path.exists()` — but `rm -rf src && echo x > src` (a same-name file
        // replacing the directory) makes path exist and not be a directory, the gate
        // skipped the whole check, and the leak sat on the index/HEAD side: the
        // pathspec still expanded `src/.env`'s deletion patch by directory prefix. By
        // this point path is necessarily not a directory (the is_dir refusal sits
        // above), so the gate buys no correctness; the expansion check now runs
        // unconditionally (existence only affects the canonicalize leg, handled
        // above).
        let index_entries = git_output(&root, &["ls-files", "-z", "--", literal.as_str()])?;
        let head_entries = git_output(
            &root,
            &[
                "ls-tree",
                "-r",
                "-z",
                "--name-only",
                "HEAD",
                "--",
                literal.as_str(),
            ],
        )
        .unwrap_or_default();
        let expands_beyond_request = index_entries
            .split('\0')
            .chain(head_entries.split('\0'))
            .filter(|entry| !entry.is_empty())
            .any(|entry| entry != relative);
        if expands_beyond_request {
            bail!("工作区差异仅支持文件路径: {relative}");
        }
        // Round-9 复审 BLOCKER:staged rename 的旧内容由 `--cached` 臂按
        // 「新文件」渲染——pathspec 限定目的地时 git 无法与删除侧配对,
        // `new file mode` 直接带出旧 blob 全文,而词法/解析腿只看得见清白
        // 的目的地名。按 index-vs-HEAD 的改名/拷贝对重建 目的地→来源 映射,
        // 请求路径是黑名单来源的改名/拷贝目标时整体拒绝(检测经
        // --find-renames/--find-copies 在命令行强制,仓库 diff.renames
        // 配置拆不散改名对)。
        if let Some(origin) = git_staged_rename_origins(&root)?.get(&relative) {
            if relative_path_crosses_blocked_component(origin) {
                bail!("工作区路径途经凭据文件，拒绝生成差异: {relative}");
            }
        }
        let unstaged = git_output(
            &root,
            &[
                "diff",
                "--no-ext-diff",
                "--no-color",
                "--",
                literal.as_str(),
            ],
        )?;
        let staged = git_output(
            &root,
            &[
                "diff",
                "--cached",
                "--no-ext-diff",
                "--no-color",
                "--",
                literal.as_str(),
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
            // Round-40 review: the synthetic new-file diff is only correct
            // for an UNTRACKED file. A tracked, unmodified file reached this
            // arm too (both diffs empty) and `untracked_diff` reported the
            // whole content as added from /dev/null — asserting a change
            // that does not exist, on the GUI's diff view and the CLI's
            // `code workspace diff` alike. `ls-files --error-unmatch`
            // settles tracked-ness (it exits nonzero for an untracked
            // path); a tracked file answers with no diff at all.
            if ls_files_tracked(&root, &relative) {
                String::new()
            } else {
                untracked_diff(&root, &path, &relative)?
            }
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

/// Validates that a relative path stays inside the workspace, without requiring the
/// file to exist (git diff can show deleted files).
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
        // 词法判定与 preview 同约束:解析前的调用方拼写先过一次黑名单
        // (解析后落点另有一次)。
        if relative_path_crosses_blocked_component(&relative) {
            bail!("工作区引用途经凭据文件，拒绝读取: {relative}");
        }
        if !unique.insert(relative.clone()) {
            continue;
        }
        let absolute_path = resolve_existing_path(root, &relative, false)?;
        // Credential content must never enter the external model context: prompt
        // references carry the same constraint as preview, with the lexical +
        // post-resolution double check (same as preview_workspace_file).
        if path_crosses_blocked_component(&absolute_path) {
            bail!("工作区引用途经凭据文件，拒绝读取: {relative}");
        }
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
    // Round-15 review (documenting the desktop exemption): this resolution, like
    // resolve_workspace_resource, performs no credential-component check — the call
    // sites (open/reveal/resource, plus open_code_reader's non-diff kinds) are all
    // desktop commands whose content goes to local external apps/readers rather than
    // model context, unreachable from the phone; the web side always goes through the
    // two-leg policy lanes of preview/search/diff.
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
    let relative_path = relative_text(root, &path)?;
    // Credential-component entries are silently skipped (the same pattern as
    // file_access::list_host_files): listing and search never reveal their existence;
    // the read and diff lanes refuse them explicitly instead. Relative paths are
    // rooted at the workspace root; when a listed directory is itself blocked, its
    // children are skipped too.
    if relative_path_crosses_blocked_component(&relative_path) {
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
        relative_path,
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

/// Whether a relative path crosses a credential blacklist component (`.env`, `.aws`,
/// `id_rsa`, `credentials.json` etc. matched exactly per component; `.env.`-family
/// variants matched by component prefix — see
/// `path_policy::BLOCKED_COMPONENT_PREFIXES` for the detailed rules). The judgment
/// reuses `path_policy::check_sensitive_components` (internally via the
/// platform-aware `path_component_eq`/`path_component_prefix`; macOS default volumes
/// are case-insensitive); its system-prefix blacklist entries all start with `/` and
/// can never be hit by a relative path, so this is equivalent to the pure component
/// check.
/// For the policy's intent see file_ingest::validate_browsable_path: credential
/// content must never enter the external model context — the codex workspace lanes
/// carry the same constraint as attachment ingestion.
fn relative_path_crosses_blocked_component(relative: &str) -> bool {
    crate::platform::path_policy::check_sensitive_components(Path::new(relative)).is_err()
}

/// Blacklist judgment on the resolved (canonicalize) absolute path: the same
/// constraint as file_ingest::validate_browsable_path's component check on canonical
/// paths. The lexical check of the caller's spelling cannot stop a clean-named
/// symlink (`notes.txt -> .env`); the resolved landing must pass the blacklist again.
fn path_crosses_blocked_component(path: &Path) -> bool {
    crate::platform::path_policy::check_sensitive_components(path).is_err()
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
    // Common text file names without an extension (Dockerfile, Makefile, LICENSE, .gitignore, etc.).
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
    // Content sniffing beyond the extension whitelist: a head 8KB with no NUL and
    // valid UTF-8 is treated as text, covering unlisted extensions like
    // .hbs/.ipynb/.cmake and extension-less text files.
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
        // When the 8KB cutoff lands mid multi-byte character, tolerate the trailing
        // incomplete sequence (≤4 bytes).
        Err(error) => error.valid_up_to() > 0 && sample.len() - error.valid_up_to() <= 4,
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
    // Round-51 review M1: `symlink_metadata`, not `metadata` — the following
    // variant let a dirty symlink→regular-file (an untracked dotfile link or
    // a tracked file→link typechange, both listed by `git status
    // --untracked-files=all`) pass the `is_file` gate and then hit
    // `sha256_file`'s symlink refusal, failing the whole baseline capture
    // (GUI session creation rolled back). Skipping non-regular entries here
    // matches the file-tree arm (WalkDir runs with follow_links(false)) and
    // snapshot_entries' own `is_file` filter, so both baseline arms treat a
    // symlink as "no fingerprint" and classify_origin stays consistent
    // (both sides `None`).
    let metadata = match path.symlink_metadata() {
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
    fs::canonicalize(
        git_output(root, &["rev-parse", "--show-toplevel"])
            .ok()?
            .trim(),
    )
    .ok()
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
        // -c status.renames=true: rename pairs are an input to the blacklist filter;
        // the Round-9 review adversarial probe confirmed that a repo configured with
        // status.renames=false splits a `git mv` into `D`+`??` (the destination escapes
        // filtering and the diff lane still renders the old credential blob).
        // Detection is forced on here and never drifts with repo config.
        .args([
            "-c",
            "status.renames=true",
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
        let path = porcelain_record_path(record);
        if matches!(x, 'R' | 'C') || matches!(y, 'R' | 'C') {
            // Round-12 review P3: a truncated record missing its origin field is
            // handled fail-closed — real git never produces this shape, but every other
            // malformed shape is fail-closed too (the old guard `&& index <
            // records.len()` would let through a destination entry with no origin
            // while consuming no records): no advance, no echo.
            if index >= records.len() {
                continue;
            }
            // Round-9 review BLOCKER: porcelain's R/C record is `destination NUL
            // origin`; the origin is the carrier of the rename pair's old content (the
            // bytes under the blocked name). When the origin hits the blacklist, the
            // destination entry is not echoed at all — it is exactly the vehicle that
            // would carry the old bytes out of the changes lane; renames with a clean
            // origin are listed as usual.
            let origin = porcelain_origin_path(records[index]);
            index += 1;
            if relative_path_crosses_blocked_component(&origin) {
                continue;
            }
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

/// The R/C rename/copy pairs of `git status --porcelain=v1 -z`: destination →
/// origin. The diff lane uses this to refuse rename destinations with blocked
/// origins (with the pathspec limited to the destination, `git diff --cached` does
/// no rename pairing and the old blob would be rendered in full as a "new file" —
/// see the Round-9 comment inside workspace_diff). Rename/copy pairs only exist in
/// index-vs-HEAD anyway, so the map comes directly from
/// `diff --cached --name-status -z`: Round-11 review MAJOR — fetching the same R/C
/// records previously ran a whole-repo `git status --porcelain`, adding a full
/// worktree+untracked traversal to every diff cache miss (in an active repo almost
/// every diff view is a miss; a cold status on a 10k-file repo takes roughly
/// 0.3-2s). `--find-renames`/`--find-copies` are forced on the command line and
/// never drift with the repo's diff.renames config (same rationale as the changes
/// lane's status.renames forcing). Under an unborn HEAD git compares against the
/// empty tree and the command still succeeds (no rename pairs can exist then, so an
/// empty map is correct). The path component separator in diff output is `/` on all
/// platforms (status's Windows backslash quirk does not apply to diff), and `-z`
/// disables quotePath escaping.
fn git_staged_rename_origins(root: &Path) -> Result<HashMap<String, String>> {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    let output = command
        .current_dir(root)
        .args([
            "diff",
            "--cached",
            "--name-status",
            "-z",
            "--find-renames",
            "--find-copies",
            "--",
        ])
        .output()
        .context("执行 git diff 失败")?;
    if !output.status.success() {
        bail!(
            "git diff 失败: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    let fields = output.stdout.split(|byte| *byte == 0).collect::<Vec<_>>();
    let mut origins = HashMap::new();
    let mut index = 0;
    while index < fields.len() {
        let status = fields[index];
        index += 1;
        if status.is_empty() {
            continue;
        }
        // Rename/copy records are `R100\0<origin>\0<dest>\0`: diff's -z name-status
        // puts the origin first and the destination second (verified: `git mv a/.env
        // a/innocent.txt` outputs `R100\0a/.env\0a/innocent.txt`) — exactly the
        // reverse of porcelain's `R  dest\0origin` field order, easy to trip on. Other
        // status codes (A/M/D/T/…) each carry one path field and are skipped as-is.
        if matches!(status[0] as char, 'R' | 'C') && index + 1 < fields.len() {
            let origin = porcelain_origin_path(fields[index]);
            let destination = porcelain_origin_path(fields[index + 1]);
            index += 2;
            if !destination.is_empty() && !origin.is_empty() {
                origins.insert(destination, origin);
            }
        } else {
            index += 1;
        }
    }
    Ok(origins)
}

/// One `git status --porcelain=v1 -z` record's path field: `XY ` prefix
/// stripped, bytes lossily decoded. The `\` → `/` separator rewrite is a
/// WINDOWS-only `status` quirk (git reports Windows paths with backslash
/// separators there). A literal backslash is a legal Unix filename byte —
/// rewriting on unix forged a phantom `back/slash.txt` entry for a real
/// `back\slash.txt` (missing baseline fingerprint, wrong dirty path), so
/// the raw path is kept as-is (round-51 review; same doctrine as the CLI
/// twin's round-37 note).
fn porcelain_record_path(record: &[u8]) -> String {
    porcelain_origin_path(&record[3..])
}

/// A raw porcelain record path (the rename-origin record has no `XY `
/// prefix): lossily decoded, with the same WINDOWS-only separator rewrite
/// as `porcelain_record_path` so both halves of a rename pair stay
/// comparable.
fn porcelain_origin_path(record: &[u8]) -> String {
    let path = String::from_utf8_lossy(record);
    #[cfg(windows)]
    {
        path.replace('\\', "/")
    }
    #[cfg(not(windows))]
    {
        path.into_owned()
    }
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

fn untracked_diff(root: &Path, path: &Path, relative: &str) -> Result<String> {
    // Round-48 review MAJOR: resolve through symlinks and re-check
    // containment BEFORE opening, then read the CANONICAL path — the
    // lexical `root.join(relative)` let an untracked symlink planted in
    // the agent-writable workspace (`notes.md -> ~/.ssh/id_ed25519`) route
    // the target's bytes into the synthetic "new file" diff. Same
    // canonicalize → gate → read-canonical discipline the preview lane has
    // carried since the round-40/46 fixes. The read is also bounded at
    // DIFF_LIMIT+1 (the loop only ever renders DIFF_LIMIT bytes): the
    // previous whole-file `fs::read_to_string` loaded a multi-gigabyte
    // untracked text file into memory just to cut it at render time.
    let canonical_root = std::fs::canonicalize(root)
        .with_context(|| format!("解析工作区根目录失败: {}", root.display()))?;
    let canonical = std::fs::canonicalize(path)
        .with_context(|| format!("读取未跟踪文件失败: {}", path.display()))?;
    if !canonical.starts_with(&canonical_root) {
        bail!("未跟踪路径通过符号链接越出了工作区: {}", relative);
    }
    // Out-of-bounds links were already caught by the containment check above; links
    // landing inside the workspace (`notes.md -> .env`) still need the resolved
    // landing to pass the blacklist, same constraint as preview.
    if path_crosses_blocked_component(&canonical) {
        bail!("工作区路径途经凭据文件，拒绝生成差异: {}", relative);
    }
    if file_kind(&canonical) != "text" {
        return Ok("未跟踪的二进制文件不支持差异预览。".to_string());
    }
    let mut bytes = Vec::new();
    std::fs::File::open(&canonical)
        .and_then(|mut file| {
            (&mut file)
                .take(DIFF_LIMIT as u64 + 1)
                .read_to_end(&mut bytes)
        })
        .with_context(|| format!("读取未跟踪文件失败: {}", canonical.display()))?;
    let preview = String::from_utf8_lossy(&bytes);
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

/// Tracked-ness oracle for the round-40 arm: `ls-files --error-unmatch`
/// exits 0 with the path for a TRACKED file and exits 1 for an untracked
/// one — but any other failure (index.lock contention, corrupt repo, git
/// missing) also lands in `git_output`'s `Err`, which the previous
/// `unwrap_or(false)` flattened into "untracked", degrading a tracked
/// unmodified file back into the fabricated whole-file addition the
/// round-40 fix exists to kill. Tracked-ness UNKNOWN therefore answers
/// "tracked" — silence, never a fabricated diff; a genuinely untracked
/// file diffed during a git outage re-syncs on the next call.
fn ls_files_tracked(root: &Path, relative: &str) -> bool {
    let mut command = crate::platform::process::HiddenCommand::new("git");
    crate::platform::process::strip_git_override_env(&mut command);
    // Round-51 review: `:(literal)` disables pathspec globbing — `--` only
    // stops option parsing, so an untracked `a*x.txt` used to match the
    // tracked `abx.txt`, exit 0, and get misanswered "tracked" (its diff
    // silently disappeared). The literal magic prefix is understood by every
    // git the workspace lane supports.
    let Ok(output) = command
        .current_dir(root)
        .args([
            "ls-files",
            "--error-unmatch",
            "--",
            format!(":(literal){relative}").as_str(),
        ])
        .output()
    else {
        return true;
    };
    if output.status.success() {
        !String::from_utf8_lossy(&output.stdout).trim().is_empty()
    } else {
        // `--error-unmatch` exits 1 exactly for the not-listed (untracked)
        // case; anything else is a git failure, not an answer, and keeps
        // the "tracked" (stay silent) answer.
        output.status.code() != Some(1)
    }
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

    /// Shared git subprocess helper for the workspace tests. Strip
    /// process-wide GIT_* overrides the way production git call sites do:
    /// other tests concurrently holding ENV_LOCK may point GIT_DIR /
    /// GIT_INDEX_FILE at bogus locations, which would redirect these
    /// subprocesses away from the temp repository.
    fn run_git(dir: &Path, args: &[&str]) {
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

    /// A literal backslash is a legal Unix filename byte: the porcelain
    /// record's path must survive verbatim. The unconditional `\` → `/`
    /// rewrite used to forge a phantom `back/slash.txt` entry (and starve
    /// the real file's baseline fingerprint) for it.
    #[cfg(unix)]
    #[test]
    fn porcelain_record_path_keeps_a_unix_backslash_filename_raw() {
        assert_eq!(
            porcelain_record_path(b"?? back\\slash.txt"),
            "back\\slash.txt"
        );
        // Ordinary records lose only the XY prefix, unrewritten.
        assert_eq!(porcelain_record_path(b"M  src/main.py"), "src/main.py");
    }

    /// Round-51 review M1: a dirty symlink→regular-file used to pass the
    /// following-`metadata` gate and then hit `sha256_file`'s symlink
    /// refusal, failing the whole baseline capture (GUI session creation
    /// rolled back). The fingerprint must SKIP the link — never hash through
    /// it, never error — while the regular target still hashes.
    #[cfg(unix)]
    #[test]
    fn fingerprint_skips_symlinks_instead_of_failing() {
        let root = TestDir::new("fingerprint-symlink");
        fs::create_dir_all(root.path().join("secrets")).unwrap();
        fs::write(root.path().join("secrets/prod.env"), "SECRET=1\n").unwrap();
        std::os::unix::fs::symlink(
            root.path().join("secrets/prod.env"),
            root.path().join(".env"),
        )
        .unwrap();

        let link = fingerprint(&root.path().join(".env"), true).unwrap();
        assert!(
            link.is_none(),
            "a symlink must fingerprint as None, got {link:?}"
        );
        let target = fingerprint(&root.path().join("secrets/prod.env"), true).unwrap();
        let target = target.expect("regular file must fingerprint");
        assert!(target.sha256.is_some());
        // A broken symlink is the same skip, not a NotFound error.
        std::os::unix::fs::symlink(
            root.path().join("secrets/missing.env"),
            root.path().join(".broken"),
        )
        .unwrap();
        assert!(
            fingerprint(&root.path().join(".broken"), true)
                .unwrap()
                .is_none()
        );
    }

    /// Round-51 review M1 end-to-end shape: `git status
    /// --untracked-files=all` lists an untracked symlink as a dirty path, and
    /// the baseline/classify pair must treat it consistently (skip, never
    /// hash through, never fail the capture). classify_origin takes the
    /// baseline by reference, so the pair is exercised hermetically with a
    /// hand-built baseline — no store, no env mutation.
    #[cfg(unix)]
    #[test]
    fn git_arm_treats_untracked_symlink_as_a_consistent_skip() {
        let Some(root) = init_git_repo("baseline-untracked-symlink") else {
            return;
        };
        fs::create_dir_all(root.path().join("secrets")).unwrap();
        fs::write(root.path().join("secrets/prod.env"), "SECRET=1\n").unwrap();
        std::os::unix::fs::symlink(
            root.path().join("secrets/prod.env"),
            root.path().join(".env"),
        )
        .unwrap();

        // The git arm's input shape: the link is listed as a dirty path.
        let status = git_status_entries(root.path()).unwrap();
        assert!(
            status.iter().any(|change| change.relative_path == ".env"),
            "git status must list the untracked symlink"
        );
        // The capture-side half of the fix: fingerprint skips the link
        // (asserted in fingerprint_skips_symlinks_instead_of_failing), so the
        // baseline entry map carries no ".env" row.
        let baseline = WorkspaceBaseline {
            workspace_path: root.path().to_string_lossy().into_owned(),
            git: true,
            dirty_paths: BTreeSet::from([".env".to_string()]),
            entries: BTreeMap::new(),
        };
        let origin = classify_origin(root.path(), Some(&baseline), ".env").unwrap();
        assert_eq!(
            origin, "preexisting",
            "a skipped symlink classifies consistently (both sides None), never errors"
        );
    }

    /// Round-51 review: the tracked-ness oracle must match the path
    /// LITERALLY — `--` disables option parsing but not pathspec globbing,
    /// so an untracked `a*x.txt` used to match the tracked `abx.txt`, exit 0,
    /// and get misanswered "tracked" (its diff silently disappeared).
    #[cfg(unix)]
    #[test]
    fn ls_files_tracked_answers_literal_paths_not_globs() {
        let Some(root) = init_git_repo("ls-files-literal") else {
            return;
        };
        fs::write(root.path().join("abx.txt"), "tracked\n").unwrap();
        git_output(root.path(), &["add", "."]).unwrap();
        fs::write(root.path().join("a*x.txt"), "untracked glob-shaped\n").unwrap();

        assert!(ls_files_tracked(root.path(), "abx.txt"));
        assert!(
            !ls_files_tracked(root.path(), "a*x.txt"),
            "an untracked glob-metacharacter filename must not alias a tracked file"
        );
    }

    #[test]
    fn diff_cache_hits_within_session_and_invalidates_on_file_change() {
        // Without a real git repo, workspace_diff takes the filesystem-compare branch whose content tracks file changes — ideal for verifying cache invalidation.
        let root = TestDir::new("diff-cache");
        fs::write(root.path().join("main.py"), "print(1)\n").unwrap();

        let first = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert!(first.text.contains("print(1)"));
        // Same session, file unchanged: cache hit, identical content.
        let second = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert_eq!(first.text, second.text);

        // File change → fingerprint invalidated, new content recomputed.
        fs::write(root.path().join("main.py"), "print(2)\n").unwrap();
        let third = workspace_diff("s-1", root.path(), "main.py").unwrap();
        assert_ne!(first.text, third.text);
        assert!(third.text.contains("print(2)"));
        // After settling, the cache hits again.
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

    // Rewrite a git-tracked fixture the way #590 hardened code_checkpoints'
    // TestDir::write: a same-length in-place rewrite can land inside one
    // timestamp tick with the inode number matching the entry cached in the
    // index (ext4 hands the just-freed inode right back), so the following
    // add/commit trusts the stale blob and the asserted commit silently
    // no-ops with "nothing to commit". Writing a sibling staging file while
    // the target still exists makes the staging inode differ from the cached
    // one by construction, independent of timestamp granularity and inode
    // reuse (safe as long as each path is rewritten at most once between
    // index refreshes, which holds for every test here).
    fn rewrite_git_tracked(path: &Path, content: &str) {
        let mut staging = path.as_os_str().to_os_string();
        staging.push(format!(
            ".pinvou3-new-{}",
            crate::platform::paths::tests::unique_suffix()
        ));
        let staging = PathBuf::from(staging);
        fs::write(&staging, content).unwrap();
        fs::rename(&staging, &path).unwrap();
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

    /// Round-48 review MAJOR: in a GIT workspace the untracked arm must
    /// (a) refuse an untracked symlink that escapes the workspace through
    /// canonical containment (the round-40/46 discipline previously lived
    /// only in the non-git preview arm), (b) still diff a workspace-LOCAL
    /// untracked symlink by reading its canonical (inside-root) target,
    /// and (c) keep answering no-diff for a tracked unmodified file. The
    /// read is bounded at DIFF_LIMIT+1 (the whole-file `fs::read_to_string`
    /// is gone). Requires the `git` binary; skips gracefully otherwise.
    #[test]
    fn untracked_diff_in_git_workspaces_resolves_symlinks_and_bounded_reads() {
        if std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_err()
        {
            eprintln!(
                "skipping untracked_diff_in_git_workspaces_resolves_symlinks_and_bounded_reads: git binary not available"
            );
            return;
        }
        let root = TestDir::new("untracked-git-symlink");
        let repo = root.path();
        run_git(repo, &["init", "-b", "main", "."]);
        run_git(repo, &["config", "user.name", "test"]);
        run_git(repo, &["config", "user.email", "test@example.com"]);
        fs::write(repo.join("tracked.txt"), "hello\n").unwrap();
        run_git(repo, &["add", "tracked.txt"]);
        run_git(repo, &["commit", "-m", "init"]);

        // (c) tracked + unmodified → no diff (exit-code-aware oracle).
        let diff = workspace_diff("sess-untracked-symlink", repo, "tracked.txt").unwrap();
        assert_eq!(diff.text, "", "a tracked unmodified file must show no diff");

        #[cfg(unix)]
        {
            // (a) untracked symlink escaping the workspace must be refused —
            // its target's bytes must never surface as a synthetic diff.
            let outside = std::env::temp_dir().join(format!(
                "pinvou3-untracked-escape-target-{}",
                crate::platform::paths::tests::unique_suffix()
            ));
            fs::write(&outside, "outside secret bytes\n").unwrap();
            std::os::unix::fs::symlink(&outside, repo.join("outside.md")).unwrap();
            let error = workspace_diff("sess-untracked-symlink", repo, "outside.md")
                .err()
                .expect("an escaping untracked symlink must be refused");
            let rendered = format!("{error:#}");
            // 解析落点包含复查先于本臂的专用命名触发:越界拒绝现在以
            // 「越过了项目根目录」更早发生(在任何内容读取之前),untracked
            // 腿的「越出了工作区」保持为纵深命名。两个消息都只可能是越界
            // 拒绝——泄露保证(目标字节不出现在差异里)由上面的 .err() 期
            // 望本身钉住。
            assert!(
                rendered.contains("越出了工作区") || rendered.contains("越过了项目根目录"),
                "the refusal must name the escape, got: {rendered}"
            );
            let _ = fs::remove_file(&outside);

            // (b) a workspace-LOCAL untracked symlink still diffs through
            // its canonical (inside-root) target.
            fs::write(repo.join("inner.txt"), "inner bytes\n").unwrap();
            std::os::unix::fs::symlink("inner.txt", repo.join("inner-link.md")).unwrap();
            let diff = workspace_diff("sess-untracked-symlink", repo, "inner-link.md").unwrap();
            assert!(
                diff.text.contains("+inner bytes"),
                "a local untracked symlink must diff via its canonical target, got: {}",
                diff.text
            );
        }

        // Bounded read: an untracked text file larger than DIFF_LIMIT is
        // capped at the cap (the render loop stops at DIFF_LIMIT anyway,
        // but the READ itself must not load the whole file).
        let big = "x".repeat(DIFF_LIMIT * 4);
        fs::write(repo.join("big.txt"), &big).unwrap();
        let diff = workspace_diff("sess-untracked-symlink", repo, "big.txt").unwrap();
        assert!(
            diff.text.len() <= DIFF_LIMIT + 160,
            "the untracked read must be capped near DIFF_LIMIT, got {}",
            diff.text.len()
        );
        assert!(diff.truncated);
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
        rewrite_git_tracked(&linked.join("main.py"), "print(2)\n");
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

    /// The credential blacklist matches exactly per component; `.env.`-family
    /// variants match by component prefix: `.env`, `.aws/credentials`,
    /// `sub/.ssh/id_rsa`, `.env.local` hit; `.environment` is a different prefix and
    /// passes (blacklist semantics identical to path_policy, no arbitrary substring
    /// matching).
    #[test]
    fn blocked_component_detection_is_exact_per_component() {
        assert!(relative_path_crosses_blocked_component(".env"));
        assert!(relative_path_crosses_blocked_component(".aws/credentials"));
        assert!(relative_path_crosses_blocked_component("sub/.ssh/id_rsa"));
        assert!(relative_path_crosses_blocked_component(".env.production"));
        assert!(relative_path_crosses_blocked_component(".env.local"));
        assert!(!relative_path_crosses_blocked_component(".environment"));
        assert!(!relative_path_crosses_blocked_component("notes.md"));
    }

    /// The workspace lanes previously had no credential policy at all: a phone-minted
    /// workspace grant turned preview into free read access to credential files inside
    /// the granted area. Preview must refuse them explicitly.
    #[test]
    fn preview_workspace_file_rejects_credential_components() {
        let root = TestDir::new("credential-preview");
        fs::create_dir_all(root.path().join(".aws")).unwrap();
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();
        fs::write(root.path().join(".aws/credentials"), "aws_key=1\n").unwrap();

        let error = preview_workspace_file(root.path(), ".env")
            .err()
            .expect("credential file preview must be rejected");
        assert!(error.to_string().contains("凭据"), "got: {error}");
        assert!(preview_workspace_file(root.path(), ".aws/credentials").is_err());
        // Ordinary files are unaffected.
        fs::write(root.path().join("notes.md"), "hello\n").unwrap();
        assert!(preview_workspace_file(root.path(), "notes.md").is_ok());
    }

    /// 列表与搜索静默跳过凭据组件条目（同 file_access::list_host_files 的
    /// 模式）。被点名的黑名单目录从「空列表成功」改为整体
    /// 拒绝——「成功空列表 vs 报错」的分叉本身会泄露黑名单目录的存在;
    /// 拒绝在词法腿触发、不触盘,对存在与否无感(与 preview 车道同形)。
    #[test]
    fn list_and_search_skip_credential_components() {
        let root = TestDir::new("credential-listing");
        fs::write(root.path().join("notes.md"), "hello\n").unwrap();
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();
        fs::create_dir_all(root.path().join(".aws")).unwrap();
        fs::write(root.path().join(".aws/credentials"), "aws_key=1\n").unwrap();
        fs::create_dir_all(root.path().join("sub/.ssh")).unwrap();
        fs::write(root.path().join("sub/.ssh/id_rsa"), "key\n").unwrap();

        let listing = list_workspace(root.path(), None).unwrap();
        assert!(listing.entries.iter().any(|entry| entry.name == "notes.md"));
        assert!(
            !listing
                .entries
                .iter()
                .any(|entry| entry.name == ".env" || entry.name == ".aws"),
            "credential entries must be skipped, got: {:?}",
            listing.entries
        );
        let aws_error = list_workspace(root.path(), Some(".aws"))
            .err()
            .expect("a blocked directory must be refused, not listed as empty");
        assert!(aws_error.to_string().contains("凭据"), "got: {aws_error}");
        let sub_listing = list_workspace(root.path(), Some("sub")).unwrap();
        assert!(!sub_listing.entries.iter().any(|entry| entry.name == ".ssh"));

        let hit = search_workspace(root.path(), "id_rsa").unwrap();
        assert!(
            hit.is_empty(),
            "search must not return credential paths, got: {hit:?}"
        );
        let hit = search_workspace(root.path(), "notes").unwrap();
        assert!(hit.iter().any(|entry| entry.relative_path == "notes.md"));
    }

    /// Both the untracked diff and git diff carry file content; credential-component
    /// files must be refused from the diff lane (reproducible with an untracked `.env`
    /// in a non-git workspace).
    #[test]
    fn workspace_diff_rejects_credential_components() {
        let root = TestDir::new("credential-diff");
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();
        fs::create_dir_all(root.path().join("sub/.ssh")).unwrap();
        fs::write(root.path().join("sub/.ssh/id_rsa"), "key\n").unwrap();

        let error = workspace_diff("s-cred", root.path(), ".env")
            .err()
            .expect("credential file diff must be rejected");
        assert!(error.to_string().contains("凭据"), "got: {error}");
        assert!(workspace_diff("s-cred", root.path(), "sub/.ssh/id_rsa").is_err());
    }

    /// git pathspec 按目录前缀递归,`git diff -- ":(literal)src"` 会把已
    /// 跟踪/已暂存的 `src/.env` 完整补丁发给手机(词法检查只看得到无害
    /// 祖先名 "src")。目录目标在差异车道整体拒绝;本钉在真实 git 仓库上
    /// 同时锁住已跟踪未暂存与已暂存两条泄露臂,并钉住清白兄弟文件的差异
    /// 仍然可用(功能不被一刀切)。直接点名的凭据文件请求(最直接的敌意
    /// 形状)也必须在 git 仓库上被两条检查腿拦住——词法腿(已删除文件
    /// canonicalize 失败时唯一的防线)与解析腿。需要 git 二进制。
    #[test]
    fn workspace_diff_refuses_directory_pathspecs_over_tracked_credentials() {
        if std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_err()
        {
            eprintln!(
                "skipping workspace_diff_refuses_directory_pathspecs_over_tracked_credentials: git binary not available"
            );
            return;
        }
        let root = TestDir::new("credential-dir-diff");
        let repo = root.path();
        run_git(repo, &["init", "-b", "main", "."]);
        run_git(repo, &["config", "user.name", "test"]);
        run_git(repo, &["config", "user.email", "test@example.com"]);
        fs::create_dir_all(repo.join("src")).unwrap();
        fs::write(repo.join("src/.env"), "AWS_SECRETKEY=base\n").unwrap();
        fs::write(repo.join("src/innocent.txt"), "hello\n").unwrap();
        run_git(repo, &["add", "src/.env", "src/innocent.txt"]);
        run_git(repo, &["commit", "-m", "init"]);
        fs::write(repo.join("src/.env"), "AWS_SECRETKEY=dirty\n").unwrap();
        fs::write(repo.join("src/innocent.txt"), "hello v2\n").unwrap();

        // Tracked-unstaged arm: an ancestor directory's diff must not carry the credential patch.
        let error = workspace_diff("s-dir", repo, "src")
            .err()
            .expect("a directory pathspec must be refused in the diff lane");
        assert!(error.to_string().contains("文件路径"), "got: {error}");

        // 直接点名的凭据文件(已跟踪未暂存)必须拒绝。
        let error = workspace_diff("s-dir-file", repo, "src/.env")
            .err()
            .expect("a tracked-dirty credential file diff must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        // Staged arm and the whole-repo spelling: the same shape.
        run_git(repo, &["add", "src/.env", "src/innocent.txt"]);
        assert!(workspace_diff("s-dir", repo, "src").is_err());
        assert!(workspace_diff("s-dir", repo, ".").is_err());
        // A named request for the staged credential file: the same refusal.
        assert!(workspace_diff("s-dir-file", repo, "src/.env").is_err());

        // 已删除的凭据文件——canonicalize 失败,解析腿跳过,词法腿是唯一
        // 防线;删除差异(渲染旧凭据字节为 `-` 行)同样不得离开差异车道。
        fs::remove_file(repo.join("src/.env")).unwrap();
        let error = workspace_diff("s-dir-file", repo, "src/.env")
            .err()
            .expect("a deleted credential file's removal diff must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        // File-level diffs still work: the innocent sibling file's patch returns as usual.
        let diff = workspace_diff("s-dir", repo, "src/innocent.txt").unwrap();
        assert!(diff.text.contains("+hello v2"), "got: {}", diff.text);
        assert!(!diff.text.contains("AWS_SECRETKEY"), "got: {}", diff.text);

        // 整个目录从 worktree 消失(未暂存 rm -rf 与 `git add -A` 后的已
        // 暂存删除)时 is_dir 不再成立——pathspec 仍按前缀递归展开
        // index/HEAD 里存活的子项,必须按 git 侧展开整体拒绝。已删除的
        // 清白文件的删除差异不受影响(ls-files/ls-tree 恰含自身)。
        fs::remove_file(repo.join("src/innocent.txt")).unwrap();
        let diff = workspace_diff("s-dir-del-file", repo, "src/innocent.txt").unwrap();
        assert!(diff.text.contains("-hello v2"), "got: {}", diff.text);
        fs::remove_dir_all(repo.join("src")).unwrap();
        let error = workspace_diff("s-dir-rm", repo, "src")
            .err()
            .expect("a deleted directory pathspec must be refused");
        assert!(error.to_string().contains("文件路径"), "got: {error}");
        run_git(repo, &["add", "-A"]);
        let error = workspace_diff("s-dir-rm-staged", repo, "src")
            .err()
            .expect("a staged whole-directory deletion pathspec must be refused");
        assert!(error.to_string().contains("文件路径"), "got: {error}");
    }

    /// staged rename 的 porcelain 记录是 `目的地 NUL 来源`。来源在黑名单
    /// 内时,变更车道不得回显目的地条目,diff 车道必须拒绝目的地(否则
    /// `--cached` 臂以「新文件」渲染旧凭据 blob 全文);来源清白的改名两条
    /// 车道照常工作。
    #[test]
    fn workspace_lanes_refuse_staged_renames_of_credential_files() {
        let repo = TestDir::new("rename-filter-r9");
        run_git(repo.path(), &["init"]);
        run_git(repo.path(), &["config", "user.name", "test"]);
        run_git(repo.path(), &["config", "user.email", "test@example.com"]);
        fs::write(repo.path().join(".env"), "AWS_SECRETKEY=supersecret\n").unwrap();
        fs::write(repo.path().join("notes.txt"), "hello\n").unwrap();
        run_git(repo.path(), &["add", ".env", "notes.txt"]);
        run_git(repo.path(), &["commit", "-m", "init"]);
        run_git(repo.path(), &["mv", ".env", "innocent.txt"]);

        let changes = workspace_changes("rename-changes", repo.path()).unwrap();
        assert!(
            !changes
                .changes
                .iter()
                .any(|change| change.relative_path == "innocent.txt"),
            "a rename destination with a blocked origin must not be listed, got: {:?}",
            changes
                .changes
                .iter()
                .map(|change| &change.relative_path)
                .collect::<Vec<_>>()
        );

        let error = workspace_diff("rename-diff", repo.path(), "innocent.txt")
            .err()
            .expect("a staged rename destination with a blocked origin must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        // Control: a rename with a clean origin stays listed and its diff is served as usual.
        run_git(repo.path(), &["mv", "notes.txt", "docs.txt"]);
        let changes = workspace_changes("rename-ok", repo.path()).unwrap();
        assert!(
            changes
                .changes
                .iter()
                .any(|change| change.relative_path == "docs.txt"),
            "an innocent rename must stay listed, got: {:?}",
            changes
                .changes
                .iter()
                .map(|change| &change.relative_path)
                .collect::<Vec<_>>()
        );
        let diff = workspace_diff("rename-ok-diff", repo.path(), "docs.txt").unwrap();
        assert!(diff.text.contains("hello"), "got: {}", diff.text);

        // 对抗探针:仓库配置 status.renames=false 不得拆散改名对(否则
        // `git mv` 退化为 `D`+`??`,目的地不被过滤、diff 车道照常渲染旧
        // 凭据 blob)。检测在 spawn 层用 -c 强制,与配置漂移无关。
        run_git(repo.path(), &["config", "status.renames", "false"]);
        run_git(repo.path(), &["mv", "docs.txt", "moved.txt"]);
        let changes = workspace_changes("rename-cfg", repo.path()).unwrap();
        assert!(
            changes
                .changes
                .iter()
                .any(|change| change.relative_path == "moved.txt"),
            "an innocent rename must stay listed even with status.renames=false"
        );
        fs::write(repo.path().join(".env"), "AWS_SECRETKEY=more\n").unwrap();
        run_git(repo.path(), &["add", ".env"]);
        run_git(repo.path(), &["commit", "-m", "second"]);
        run_git(repo.path(), &["mv", ".env", "harmless.txt"]);
        let changes = workspace_changes("rename-cfg2", repo.path()).unwrap();
        assert!(
            !changes
                .changes
                .iter()
                .any(|change| change.relative_path == "harmless.txt"),
            "a blocked rename must stay filtered even with status.renames=false"
        );
        let error = workspace_diff("rename-cfg2-diff", repo.path(), "harmless.txt")
            .err()
            .expect("a blocked rename must be refused even with status.renames=false");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// ls-files(index)腿的判别钉。HEAD 未出生时 ls-tree 降级为空,索引腿
    /// 是唯一防线——删除它,`git diff -- ':(literal)src'` 会把「已暂存、
    /// 未提交、随后整目录从 worktree 消失」的 src/.env 以 `+` 行渲染,而
    /// 此前所有 fixture 的 HEAD 都含同批子项,索引腿被 ls-tree 腿遮蔽。
    #[test]
    fn workspace_diff_index_leg_refuses_staged_directory_without_head() {
        let repo = TestDir::new("index-leg-r9");
        run_git(repo.path(), &["init"]);
        run_git(repo.path(), &["config", "user.name", "test"]);
        run_git(repo.path(), &["config", "user.email", "test@example.com"]);
        fs::create_dir_all(repo.path().join("src")).unwrap();
        fs::write(repo.path().join("src/.env"), "AWS_SECRETKEY=staged\n").unwrap();
        run_git(repo.path(), &["add", "src/.env"]);
        fs::remove_dir_all(repo.path().join("src")).unwrap();
        let error = workspace_diff("index-leg", repo.path(), "src")
            .err()
            .expect("staged directory expansion with unborn HEAD must be refused");
        assert!(error.to_string().contains("文件路径"), "got: {error}");
    }

    /// 同名文件换目录的形状——`rm -rf src && echo x > src` 让 path 存在
    /// 且非目录,展开检查若以 `!path.exists()` 为门会被整个跳过,而泄露在
    /// index/HEAD 侧:pathspec 照样按目录前缀展开出 `src/.env` 的删除补丁
    /// (旧凭据字节渲染成 `-` 行)。展开检查必须无条件执行(到达此处 path
    /// 已必非目录,门没有正确性收益);已暂存臂(--cached 渲染 HEAD 侧删
    /// 除)同一拒绝。
    #[test]
    fn workspace_diff_refuses_directory_replaced_by_same_name_file() {
        let repo = TestDir::new("dir-swap-r11");
        run_git(repo.path(), &["init", "-b", "main", "."]);
        run_git(repo.path(), &["config", "user.name", "test"]);
        run_git(repo.path(), &["config", "user.email", "test@example.com"]);
        fs::create_dir_all(repo.path().join("src")).unwrap();
        fs::write(repo.path().join("src/.env"), "AWS_SECRETKEY=supersecret\n").unwrap();
        fs::write(repo.path().join("src/innocent.txt"), "hello\n").unwrap();
        run_git(repo.path(), &["add", "src/.env", "src/innocent.txt"]);
        run_git(repo.path(), &["commit", "-m", "init"]);

        // Unstaged swap: the directory became a same-name file, and the worktree-side path exists.
        fs::remove_dir_all(repo.path().join("src")).unwrap();
        fs::write(repo.path().join("src"), "payload\n").unwrap();
        let error = workspace_diff("dir-swap", repo.path(), "src")
            .err()
            .expect("a directory replaced by a same-name file must still be refused");
        assert!(error.to_string().contains("文件路径"), "got: {error}");

        // Staged swap: the HEAD-side deletion patch (--cached arm) gets the same refusal.
        run_git(repo.path(), &["add", "src"]);
        let error = workspace_diff("dir-swap-staged", repo.path(), "src")
            .err()
            .expect("a staged directory-replaced-by-file pathspec must still be refused");
        assert!(error.to_string().contains("文件路径"), "got: {error}");

        // Control 1: after reset, index/HEAD still record src as a directory (src/.env
        // survives in the index), so a request for "src" would expand the credential
        // deletion patch (`-AWS_SECRETKEY=…` lines) in the unstaged arm as well — the
        // refusal holds.
        run_git(repo.path(), &["reset", "-q"]);
        let error = workspace_diff("dir-swap-reset", repo.path(), "src")
            .err()
            .expect("a swapped pathspec must stay refused while the index still tracks the old directory");
        assert!(error.to_string().contains("文件路径"), "got: {error}");

        // Control 2: an innocent new file's diff works as usual (both expansion lists contain exactly itself).
        fs::write(repo.path().join("docs.txt"), "visible\n").unwrap();
        let diff = workspace_diff("dir-swap-file", repo.path(), "docs.txt").unwrap();
        assert!(diff.text.contains("+visible"), "got: {}", diff.text);
        assert!(!diff.text.contains("AWS_SECRETKEY"), "got: {}", diff.text);
    }

    /// diff 车道的改名映射取自 `diff --cached --name-status`,检测在
    /// spawn 层用 --find-renames/--find-copies 强制,仓库配置
    /// diff.renames=false 不得拆散改名对(与 changes 车道 status.renames
    /// 强制同理)。
    #[test]
    fn workspace_diff_rename_map_forces_detection_despite_diff_renames_config() {
        let repo = TestDir::new("rename-diff-cfg-r11");
        run_git(repo.path(), &["init"]);
        run_git(repo.path(), &["config", "user.name", "test"]);
        run_git(repo.path(), &["config", "user.email", "test@example.com"]);
        run_git(repo.path(), &["config", "diff.renames", "false"]);
        fs::write(repo.path().join(".env"), "AWS_SECRETKEY=supersecret\n").unwrap();
        run_git(repo.path(), &["add", ".env"]);
        run_git(repo.path(), &["commit", "-m", "init"]);
        run_git(repo.path(), &["mv", ".env", "harmless.txt"]);
        let error = workspace_diff("rename-cfg-r11", repo.path(), "harmless.txt")
            .err()
            .expect("a blocked rename must be refused even with diff.renames=false");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// 列车道的解析腿判别钉——「链接名清白」的符号链接(docs -> .aws)的
    /// 落点在黑名单目录上时拒绝列出。
    #[test]
    #[cfg(unix)]
    fn workspace_list_refuses_blocked_resolved_landing() {
        let root = TestDir::new("list-resolved-r9");
        fs::create_dir_all(root.path().join(".aws")).unwrap();
        std::os::unix::fs::symlink(".aws", root.path().join("docs")).unwrap();
        let error = list_workspace(root.path(), Some("docs"))
            .err()
            .expect("a symlink whose resolved landing is a blocked directory must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// The prompt-reference lane (web_access_codex_acp_prompt's
    /// workspace_references) carries the same constraint as preview: credential
    /// content must never enter the external model context via a ResourceLink.
    #[test]
    fn workspace_references_reject_credential_components() {
        let root = TestDir::new("credential-references");
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();
        fs::write(root.path().join("notes.md"), "hello\n").unwrap();

        let error = resolve_workspace_references(root.path(), &[".env".to_string()])
            .err()
            .expect("credential prompt reference must be rejected");
        assert!(error.to_string().contains("凭据"), "got: {error}");
        // `.env.`-family variants are caught at the prefix judgment (no on-disk resolution needed).
        let error = resolve_workspace_references(root.path(), &[".env.production".to_string()])
            .err()
            .expect("env-variant prompt reference must be rejected");
        assert!(error.to_string().contains("凭据"), "got: {error}");
        let resolved = resolve_workspace_references(root.path(), &["notes.md".to_string()])
            .expect("ordinary references stay allowed");
        assert_eq!(resolved.len(), 1);
    }

    /// A clean-named symlink (`notes.txt -> .env`) must be caught at the judgment on
    /// the resolved landing: a lexical check of the caller's spelling alone is
    /// bypassed. Both read paths — preview and the untracked diff — are pinned.
    #[test]
    #[cfg(unix)]
    fn credential_symlink_alias_is_blocked_after_resolution() {
        use std::os::unix::fs::symlink;
        let root = TestDir::new("credential-symlink-alias");
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();
        symlink(".env", root.path().join("notes.txt")).unwrap();

        let error = preview_workspace_file(root.path(), "notes.txt")
            .err()
            .expect("a symlink resolving onto a credential file must be rejected");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        let error = workspace_diff("s-symlink", root.path(), "notes.txt")
            .err()
            .expect("the untracked-diff read must refuse the resolved credential target");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        // The prompt-reference lane blocks it too.
        assert!(resolve_workspace_references(root.path(), &["notes.txt".to_string()]).is_err());
    }

    /// 差异车道解析腿在「已跟踪文件类型变更」场景下是唯一防线——
    /// notes.txt 原是已跟踪普通文件,后被替换成指向 .env 的符号链接(脏
    /// 的类型变更)。词法腿只看得到清白名;untracked 臂不参与(文件已跟
    /// 踪);preview/untracked 的下游复检也不可达——只有 workspace_diff
    /// 级的 canonicalize 复检能拦住。
    #[test]
    #[cfg(unix)]
    fn workspace_diff_resolved_leg_blocks_tracked_typechanged_symlink() {
        use std::os::unix::fs::symlink;
        if std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_err()
        {
            eprintln!(
                "skipping workspace_diff_resolved_leg_blocks_tracked_typechanged_symlink: git binary not available"
            );
            return;
        }
        let root = TestDir::new("credential-typechange-diff");
        let repo = root.path();
        run_git(repo, &["init", "-b", "main", "."]);
        run_git(repo, &["config", "user.name", "test"]);
        run_git(repo, &["config", "user.email", "test@example.com"]);
        fs::write(repo.join("notes.txt"), "hello\n").unwrap();
        run_git(repo, &["add", "notes.txt"]);
        run_git(repo, &["commit", "-m", "init"]);
        fs::write(repo.join(".env"), "SECRET=1\n").unwrap();
        fs::remove_file(repo.join("notes.txt")).unwrap();
        symlink(".env", repo.join("notes.txt")).unwrap();

        let error = workspace_diff("s-typechange", repo, "notes.txt")
            .err()
            .expect("a tracked file type-changed onto a credential symlink must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// 列表车道对「被请求目录本身」的黑名单判定——目录级请求回空列表
    /// vs 报错的分叉会泄露黑名单目录的存在,词法腿必须整体拒绝;清白目录
    /// 照常可列,根目录列表的子项过滤保持不变。
    #[test]
    fn list_refuses_blocked_directory_requests() {
        let root = TestDir::new("credential-list-dir");
        fs::create_dir_all(root.path().join(".aws")).unwrap();
        fs::write(root.path().join(".aws/credentials"), "k=v\n").unwrap();
        fs::create_dir_all(root.path().join("docs")).unwrap();
        fs::write(root.path().join("docs/readme.md"), "hi\n").unwrap();

        let error = list_workspace(root.path(), Some(".aws"))
            .err()
            .expect("a blocked directory must be refused, not listed as empty");
        assert!(error.to_string().contains("凭据"), "got: {error}");

        let listing = list_workspace(root.path(), Some("docs"))
            .expect("an ordinary directory must still list");
        assert_eq!(listing.entries.len(), 1);
        let listing = list_workspace(root.path(), None).expect("the root must still list");
        assert!(
            listing.entries.iter().all(|entry| entry.name != ".aws"),
            "blocked children stay filtered from the root listing: {:?}",
            listing
                .entries
                .iter()
                .map(|entry| &entry.name)
                .collect::<Vec<_>>()
        );
    }

    /// 词法腿(触盘前的 pre-stat 拒绝)对存在与否无感——不存在的被拦目录
    /// 必须得到凭据拒绝而非「不存在」错误,否则「凭据拒绝 vs 不存在」的
    /// 分叉本身泄露黑名单名是否存在(词法腿被删时解析腿对触盘 fixture
    /// 报同文,旧钉仍绿——live 突变已证;本钉是该腿唯一的判别钉)。
    #[test]
    fn list_refuses_nonexistent_blocked_directory_lexically() {
        let root = TestDir::new("credential-list-nonexistent");
        fs::write(root.path().join("notes.md"), "hello\n").unwrap();

        let error = list_workspace(root.path(), Some(".aws"))
            .err()
            .expect("a nonexistent blocked directory must still be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// 列表车道解析腿——「链接名清白」的符号链接指向凭据目录时同样拒绝
    /// (子项过滤挡不住「这个目录可列」的存在信号)。
    #[test]
    #[cfg(unix)]
    fn list_refuses_symlinked_blocked_directories() {
        use std::os::unix::fs::symlink;
        let root = TestDir::new("credential-list-symlink");
        fs::create_dir_all(root.path().join(".aws")).unwrap();
        symlink(".aws", root.path().join("innocent-link")).unwrap();

        let error = list_workspace(root.path(), Some("innocent-link"))
            .err()
            .expect("a clean-named symlink onto a blocked directory must be refused");
        assert!(error.to_string().contains("凭据"), "got: {error}");
    }

    /// The changes lane carries the same constraint as list/search: a blocked
    /// component's very existence (name/status metadata) must never echo to the phone.
    #[test]
    fn workspace_changes_skip_credential_components() {
        let root = TestDir::new("credential-changes");
        fs::write(root.path().join("notes.md"), "hello\n").unwrap();
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();

        let changes = workspace_changes("s-changes", root.path()).unwrap();
        let names: Vec<&str> = changes
            .changes
            .iter()
            .map(|change| change.relative_path.as_str())
            .collect();
        assert!(names.contains(&"notes.md"), "got: {names:?}");
        assert!(
            !names.iter().any(|name| name.ends_with(".env")),
            "credential names must not leak through the changes lane: {names:?}"
        );
    }

    /// changes 车道 git 半边的过滤排序——凭据 retain 先于
    /// `classify_origin` 的内容读取(SHA-256/来源分类),一个不可读的脏
    /// `.env` 不得让整个 changes 调用失败,更不得出现在列表里。上方非 git
    /// 钉只覆盖过滤本身;本钉在真实 git 仓库上锁住排序(EACCES 腿仅
    /// unix;Windows 无 chmod 语义,过滤断言在所有平台都成立)。
    /// 排序要真实可观察,必须先采集基线——没有基线时
    /// `classify_origin` 在任何内容读取之前就短路成 "unknown",chmod 000
    /// 的 `.env` 根本不会被读到,把 retain 挪到分类之后测试仍然全绿。
    /// 基线在权限收紧前采集(指纹正常),之后 `classify_origin` 对过滤
    /// 幸存的清白文件求指纹;一旦排序退化,不可读的 `.env` 被求指纹 →
    /// EACCES → 整个调用失败,本测试即红。
    #[test]
    fn workspace_changes_git_arm_filters_credentials_before_content_classification() {
        if std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_err()
        {
            eprintln!(
                "skipping workspace_changes_git_arm_filters_credentials_before_content_classification: git binary not available"
            );
            return;
        }
        // The baseline file path derives from PINVOU3_HOME; running concurrently with home-mutating tests would make them delete each other's directories.
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let root = TestDir::new("credential-changes-git");
        let repo = root.path();
        run_git(repo, &["init", "-b", "main", "."]);
        run_git(repo, &["config", "user.name", "test"]);
        run_git(repo, &["config", "user.email", "test@example.com"]);
        fs::write(repo.join(".env"), "SECRET=1\n").unwrap();
        fs::write(repo.join("notes.md"), "hello\n").unwrap();
        run_git(repo, &["add", ".env", "notes.md"]);
        run_git(repo, &["commit", "-m", "init"]);
        fs::write(repo.join(".env"), "SECRET=2\n").unwrap();
        fs::write(repo.join("notes.md"), "hello v2\n").unwrap();
        capture_baseline("s-changes-git", repo)
            .expect("the baseline must be captured while every dirty file is readable");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(repo.join(".env"), fs::Permissions::from_mode(0o000)).unwrap();
        }
        let changes = workspace_changes("s-changes-git", repo)
            .expect("an unreadable dirty credential file must be filtered, not fail the lane");
        let names: Vec<&str> = changes
            .changes
            .iter()
            .map(|change| change.relative_path.as_str())
            .collect();
        assert!(names.contains(&"notes.md"), "got: {names:?}");
        assert!(
            !names.iter().any(|name| name.ends_with(".env")),
            "credential names must not leak through the git changes arm: {names:?}"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(repo.join(".env"), fs::Permissions::from_mode(0o644)).unwrap();
        }
        let _ = fs::remove_file(baseline_path("s-changes-git"));
    }

    /// 大小写变体经真实入口的端到端钉(macOS 独有意义:APFS 默认卷大小写
    /// 不敏感,`.ENV` 会解析到磁盘上的 `.env`;词法检查经 path_component_eq
    /// 的折叠在同平台上必须拦住它)。Linux 大小写敏感,同一拼写是另一个
    /// 不存在的文件,断言平凡成立,故仅在 macOS 上运行。
    #[test]
    #[cfg(target_os = "macos")]
    fn case_variant_spellings_are_blocked_end_to_end() {
        let root = TestDir::new("credential-case-variant");
        fs::write(root.path().join(".env"), "SECRET=1\n").unwrap();

        for spelling in [".ENV", ".Env"] {
            let error = preview_workspace_file(root.path(), spelling)
                .err()
                .expect("case-variant credential spellings must be blocked");
            assert!(
                error.to_string().contains("凭据"),
                "{spelling} must hit the credential gate, got: {error}"
            );
        }
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
        // 8191 ASCII bytes plus one three-byte character: the 8KB sample cuts exactly mid-character.
        let content = format!("{}中", "a".repeat(8191));
        fs::write(root.path().join("boundary.unknown"), content).unwrap();
        let preview = preview_workspace_file(root.path(), "boundary.unknown").unwrap();
        assert_eq!(preview.kind, "text");
    }

    #[test]
    fn non_git_baseline_detects_added_modified_and_deleted_files() {
        // The baseline file path derives from PINVOU3_HOME; running concurrently with home-mutating tests would make them delete each other's directories.
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

    #[test]
    fn baseline_is_current_distinguishes_stale_from_current_roots() {
        // round-12 P2-1 companion: the rebind recapture drain gates on this
        // predicate, so the absent / corrupt / stale / current classification
        // is pinned directly — an absent or corrupt baseline reads
        // not-current (recapture rewrites it), a fresh one at the SAME root
        // reads current, and a different root reads stale.
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let nonce = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let session_id = format!("baseline-gate-{}-{nonce}", std::process::id());
        let a = TestDir::new("baseline-gate-a");
        let b = TestDir::new("baseline-gate-b");
        fs::write(a.path().join("file.txt"), "a").unwrap();
        fs::write(b.path().join("file.txt"), "b").unwrap();
        assert!(
            !baseline_is_current(&session_id, a.path()),
            "no baseline yet: not current, recapture runs"
        );
        capture_baseline(&session_id, a.path()).unwrap();
        assert!(
            baseline_is_current(&session_id, a.path()),
            "fresh baseline at the same root: current, drain skips"
        );
        assert!(
            !baseline_is_current(&session_id, b.path()),
            "a moved workspace (different root) reads stale"
        );
        fs::write(baseline_path(&session_id), b"not json").unwrap();
        assert!(
            !baseline_is_current(&session_id, a.path()),
            "a corrupt baseline reads not-current (the drain's recapture then rewrites it)"
        );
        let _ = fs::remove_file(baseline_path(&session_id));
        let _ = fs::remove_dir_all(a.path());
        let _ = fs::remove_dir_all(b.path());
    }

    #[test]
    #[cfg(unix)]
    fn baseline_is_current_converges_across_a_path_alias_symlink() {
        // round-12 P2-1: capture stores the CANONICALIZED root while the
        // rebind drain passes the LEXICAL to.join(suffix) translation — a
        // symlink anywhere in that spelling (this pin uses a whole-path
        // alias, the same shape; an inner component alias resolves the same
        // way) must not make the gate read stale on every run (it would
        // keep resetting a healthy baseline instead of converging after
        // one idempotent recapture).
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let nonce = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let session_id = format!("baseline-gate-link-{}-{nonce}", std::process::id());
        let real = TestDir::new("baseline-gate-real");
        let alias = std::env::temp_dir().join(format!(
            "pinvou3-baseline-gate-alias-{}-{nonce}",
            std::process::id()
        ));
        fs::write(real.path().join("file.txt"), "x").unwrap();
        std::os::unix::fs::symlink(real.path(), &alias).expect("symlink");
        // Bind through the alias: the stored baseline holds the canonical
        // (real) spelling, as every capture does.
        capture_baseline(&session_id, real.path()).unwrap();
        assert!(
            baseline_is_current(&session_id, &alias),
            "the lexical alias spelling must resolve to the canonical stored root"
        );
        let _ = fs::remove_file(baseline_path(&session_id));
        let _ = fs::remove_dir_all(real.path());
        let _ = fs::remove_file(&alias);
    }

    /// Initializes a git repo with main + feature branches; returns None to skip when git is unavailable.
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
        // Both stash and commit create commit objects; the test environment may lack a global identity, so repo-level config is the fallback.
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
        // One extra commit on feature: feature's committerdate updates, so it should sort before main.
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        rewrite_git_tracked(&root.path().join("file.txt"), "v2");
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
        // Empty branch names and option injection are refused.
        assert!(
            checkout_workspace_branch(root.path(), "  ", BranchSwitchMode::Carry, None).is_err()
        );
        assert!(
            checkout_workspace_branch(root.path(), "--force", BranchSwitchMode::Carry, None)
                .is_err()
        );
        // Unknown modes are refused.
        assert!(BranchSwitchMode::parse("discard").is_err());
    }

    /// When a nonexistent branch name happens to be a workspace file path, a bare
    /// `git checkout <name>` restores that file as a pathspec and discards uncommitted
    /// changes; branch existence must be validated first.
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
        // An untracked file plus a tracked-file modification: stash mode should carry and restore both.
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
        // The stash was popped; the stash list should be empty.
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
        rewrite_git_tracked(&root.path().join("file.txt"), "v2");
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

    /// Stash mode with a failing checkout: pop to restore the scene before reporting —
    /// changes return to the worktree, the stash list empties, and the branch stays
    /// unchanged. The checkout failure is constructed as "the commit object the target
    /// branch points at is missing": for natural scenarios like ignored-file/blocking
    /// directories or read-only directories git can silently cope or even exit 0;
    /// a missing object is the deterministic nonzero failure, and it passes the
    /// local-branch-name validation.
    #[test]
    fn checkout_workspace_branch_stash_mode_restores_changes_on_checkout_failure() {
        let Some(root) = init_git_repo("checkout-stash-failure") else {
            return;
        };
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        rewrite_git_tracked(&root.path().join("file.txt"), "v2");
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
        // Delete feature's loose commit object: the ref remains (passing the branch-list validation), so checkout must fail.
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
        // Scene restored: the dirty changes are back in the worktree, the stash was popped, and the branch is still main.
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1-dirty"
        );
        let stash_list = git_output(root.path(), &["stash", "list"]).unwrap();
        assert!(stash_list.trim().is_empty());
        // workspace_branches cannot assert the branch: its --sort=-committerdate needs
        // every branch's commit objects, and the dangling feature ref would fail the
        // whole call.
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

    /// In Stash mode the push may create no entry (status non-empty but nothing
    /// stashable — typically only submodule worktree content changed, which status
    /// shows as modified while push is a no-op): popping then would wrongly pop the
    /// user's existing stash entry and apply its content to the new branch. The
    /// restore must be skipped and the user's stash left untouched.
    #[test]
    fn checkout_workspace_branch_stash_mode_skips_pop_when_push_created_no_entry() {
        let Some(root) = init_git_repo("stash-noop-push") else {
            return;
        };
        // A local submodule repo (newer git refuses file-protocol clones by default; explicitly allowed).
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
        // The user's pre-existing stash entry: this flow must not touch it.
        fs::write(root.path().join("file.txt"), "user-edit").unwrap();
        git_output(root.path(), &["stash", "push", "-m", "user-stash"]).unwrap();
        // The only remaining change = the submodule content modification: push creates no entry.
        fs::write(root.path().join("sub").join("lib.txt"), "dirty").unwrap();
        assert!(!git_status_entries(root.path()).unwrap().is_empty());

        checkout_workspace_branch(root.path(), "feature", BranchSwitchMode::Stash, None).unwrap();
        assert_eq!(git_branch(root.path()).as_deref(), Some("feature"));
        // The user's stash was not popped: file.txt still holds feature's committed content.
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
        // An empty commit message is refused.
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
        // The changes were committed to main (the original branch), leaving the target branch's worktree clean.
        assert_eq!(result.dirty_count, 0);
        let main_log = git_output(root.path(), &["log", "-1", "--format=%s", "main"]).unwrap();
        assert_eq!(main_log.trim(), "wip: before switch");
        assert_eq!(
            fs::read_to_string(root.path().join("file.txt")).unwrap(),
            "v1"
        );
    }

    #[test]
    fn checkout_workspace_branch_commit_mode_suppresses_gpg_signing() {
        // The same hardening as the CLI lane (round-40 review, both ends consistent):
        // a commit.gpgsign=true user's first commit-mode switch must not stall on GPG
        // signing (this lane's stdin is closed with no deadline, and the pinentry
        // prompt would hang the session lane forever). The signing key is pinned to a
        // key id that certainly does not exist, so "without suppression the commit must
        // fail" holds on any machine.
        let Some(root) = init_git_repo("checkout-commit-gpgsign") else {
            return;
        };
        git_output(root.path(), &["config", "commit.gpgsign", "true"]).unwrap();
        git_output(
            root.path(),
            &["config", "user.signingkey", "0xDEADBEEF_NO_SUCH_KEY"],
        )
        .unwrap();
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        let result = checkout_workspace_branch(
            root.path(),
            "feature",
            BranchSwitchMode::Commit,
            Some("wip: gpgsign suppressed"),
        )
        .expect("commit.gpgsign=true must not fail the commit-mode switch");
        assert_eq!(result.current.as_deref(), Some("feature"));
        let main_log = git_output(root.path(), &["log", "-1", "--format=%s", "main"]).unwrap();
        assert_eq!(main_log.trim(), "wip: gpgsign suppressed");
    }

    #[test]
    fn checkout_workspace_branch_commit_mode_discloses_mutated_index_on_failure() {
        // A rejecting pre-commit hook makes the commit fail after `git add -A` (same
        // class as no-identity or server-side hook refusals): the error must disclose
        // that the index was rewritten; changes must not sit silently staged.
        let Some(root) = init_git_repo("checkout-commit-disclosure") else {
            return;
        };
        let hook = root.path().join(".git").join("hooks").join("pre-commit");
        fs::write(&hook, b"#!/bin/sh\nexit 1\n").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&hook, fs::Permissions::from_mode(0o755)).unwrap();
        }
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        let error = checkout_workspace_branch(
            root.path(),
            "feature",
            BranchSwitchMode::Commit,
            Some("wip: hook rejects"),
        )
        .expect_err("a rejecting hook must fail the commit-mode switch");
        let error = error.to_string();
        assert!(
            error.contains("已暂存工作区"),
            "commit failure must disclose the mutated index: {error}"
        );
        // The rewritten index is a fact, not wording: the changes really are staged.
        let staged = git_output(root.path(), &["status", "--porcelain"]).unwrap();
        assert!(
            staged.contains("M  file.txt"),
            "changes must be staged: {staged}"
        );
    }

    #[test]
    fn checkout_workspace_branch_commit_mode_prefers_config_identity_over_ambient_env() {
        // Explicit identity: the commit lane removes the GIT_AUTHOR_*/GIT_COMMITTER_*
        // env vars so the `-c user.name=` identity (resolved from the user's config)
        // actually takes effect — an identity exported by the caller's shell must not
        // override it.
        let Some(root) = init_git_repo("checkout-commit-identity") else {
            return;
        };
        fs::write(root.path().join("file.txt"), "v1-dirty").unwrap();
        // Round-8 review minor: snapshot-and-restore through the shared
        // panic-safe guard — a panic inside checkout_workspace_branch used to
        // leak the seeded GIT_* values to later tests (a straight-line restore
        // never runs on unwind).
        let (_env_lock, _env) = crate::platform::test_support::locked_env(&[
            "GIT_AUTHOR_NAME",
            "GIT_AUTHOR_EMAIL",
            "GIT_COMMITTER_NAME",
            "GIT_COMMITTER_EMAIL",
        ]);
        // SAFETY: locked_env holds platform::paths::tests::ENV_LOCK; lib tests
        // run with --test-threads=1, env writes are serialized in-process.
        unsafe { std::env::set_var("GIT_AUTHOR_NAME", "env-author") };
        unsafe { std::env::set_var("GIT_COMMITTER_NAME", "env-committer") };
        let result = checkout_workspace_branch(
            root.path(),
            "feature",
            BranchSwitchMode::Commit,
            Some("wip: identity"),
        )
        .expect("commit-mode switch must succeed");
        assert_eq!(result.current.as_deref(), Some("feature"));
        let author = git_output(root.path(), &["log", "-1", "--format=%an", "main"]).unwrap();
        assert_eq!(
            author.trim(),
            "test",
            "ambient GIT_AUTHOR_NAME must not outrank the -c identity from the user's config"
        );
        let committer = git_output(root.path(), &["log", "-1", "--format=%cn", "main"]).unwrap();
        assert_eq!(committer.trim(), "test");
    }

    #[test]
    fn checkout_workspace_branch_carry_conflict_keeps_workspace() {
        let Some(root) = init_git_repo("checkout-conflict") else {
            return;
        };
        // On feature, file.txt is changed to v2 and committed; on main the same file has
        // uncommitted modifications, so a carry-mode switch must be refused by git with
        // the workspace left untouched.
        git_output(root.path(), &["checkout", "feature"]).unwrap();
        rewrite_git_tracked(&root.path().join("file.txt"), "v2");
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
