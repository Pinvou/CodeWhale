//! 导入扫描：告诉卡片脚本「去哪找已经在本机的权重」。
//!
//! # 为什么需要
//!
//! 权重动辄几十上百 GB，而这些设备的出口带宽常常只有几百 KB/s。「先拷到 U 盘、
//! 插到机器上」是常态，所以下载前必须先看看本机是不是已经有了。
//!
//! # 为什么扫描的**判定**不在这里
//!
//! 一开始我想在 Rust 里直接找「这个模型的副本」，然后软链进权重目录。这条路走
//! 不通，原因是个硬事实：**权重目录名是卡片内部的知识**。
//!
//! ```text
//! qwen38fn-nvfp4-single   →  $LMM_MODELS_DIR/Qwen3.8-Flash-Next-NVFP4
//! ornith15-bf16           →  $LMM_MODELS_DIR/Ornith-1.5-35B-A3B
//! minimax-h3-int8-single  →  $LMM_MODELS_DIR/$LMM_MODEL_NAME
//! deepseek-v4-…-dual      →  $CARD_STORE/model
//! ```
//!
//! 卡片元数据里确实有 `source_repo_id`，但拿它的末段当目录名只对上 4/7：
//! `unsloth/Qwen3.8-27B-NVFP4` 那张卡落地成 `Qwen3.8-27B-NVFP4-unsloth`（多一个
//! 后缀，因为同名仓库有两个来源要区分），minimax 的 `source_repo_id` 是两个仓库
//! 拼出来的一句话，deepseek 干脆不放在权重目录下。猜错的代价不是白扫一趟——是
//! 把不相干的目录当成这个模型。
//!
//! 能回答「这个目录是不是这个模型、全不全」的只有卡片脚本自己——它手里有目录名，
//! 也有哈希清单（`weights.sha256` / HF 逐文件 sha256 / 包自己的 SHA256SUMS）。
//!
//! 所以分工是：**这里回答「去哪找」，脚本回答「这份对不对」。**
//!
//! # 补的是哪个缺口
//!
//! 有三张卡（ornith15、z-image-turbo、qwen38-27b-nvfp4-mtp）早就自己实现了这个
//! 扫描，minimax 的 `install_engine.sh::find_delivery()` 找交付包也是同一套写法。
//! 但每张卡都**各自硬编码**了一份根目录列表，彼此还不一样，而且没有一份包含
//! `~/models` 或用户配置的权重目录。
//!
//! 2026-09-20 踩到的就是这一条：用户把 124GB 的 Qwen3.8-Flash-Next 放在 `~/models`
//! （LMM 作为独立应用时的默认目录），而原生化时我把默认权重目录改成了
//! `~/.pinvou3/model-store/models`——于是卡片认的目录里空空如也，照样从网上重下。
//!
//! 这里把根目录列表收敛成一处、并且**可配**，通过 `LMM_IMPORT_DIRS` 下发给所有
//! 脚本，卡片作者不用再各写一遍，用户也终于能把自己的存放位置加进去。

use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

/// 整个扫描的时间预算。
///
/// 卡片脚本里原来用的是 15~20 秒，沿用同一个量级。设备上是 3.7TB 的盘，
/// 没有预算的遍历会让"点了下载却一直没动静"——而用户分不清那是在扫盘还是卡死了。
const SCAN_BUDGET: Duration = Duration::from_secs(15);

/// 从每个根目录往下找几层。
///
/// 2 层够覆盖 `<U盘>/models/<模型名>/` 这种放法。再深下去，代价涨得比收益快：
/// 卡片脚本那边用的是 `find -maxdepth 4`，但它找的是一个具体文件名，而这里要对
/// 每个目录做 `read_dir`，不是一回事。
const SCAN_DEPTH: usize = 2;

/// 可移动介质的挂载点前缀。Linux 桌面把 U 盘挂在这几处。
const REMOVABLE_PREFIXES: [&str; 3] = ["/media/", "/mnt/", "/run/media/"];

/// 一个看起来装着模型权重的目录。
///
/// 「看起来」是字面意思——这里只看有没有权重文件的样子，不判断是哪个模型、全不全。
/// 判定是卡片脚本的事（见模块文档）。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Candidate {
    pub path: PathBuf,
    /// 目录里权重文件的总字节，用来排序和显示，不作为判定依据。
    pub bytes: u64,
    /// 在可移动介质上。这类路径**不能**软链进权重目录——U 盘一拔模型就悬空，
    /// 而那时用户早忘了它是链过来的。要用得整份拷过去。
    pub removable: bool,
}

fn is_removable(path: &Path) -> bool {
    let s = path.to_string_lossy();
    REMOVABLE_PREFIXES.iter().any(|p| s.starts_with(p))
}

fn home() -> PathBuf {
    std::env::var("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/"))
}

/// 当前挂着的可移动介质。
///
/// `/media/<用户>/<卷标>` 和 `/run/media/<用户>/<卷标>` 两层都收进来：有的发行版
/// 直接挂在 `/media/<卷标>`。`/mnt` 下的挂载点也一并算上。
fn removable_mounts() -> Vec<PathBuf> {
    let mut out = Vec::new();
    for base in ["/media", "/run/media"] {
        let Ok(entries) = std::fs::read_dir(base) else {
            continue;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if !path.is_dir() {
                continue;
            }
            // `/media/<卷标>` 这一层本身可能就是盘根
            out.push(path.clone());
            if let Ok(children) = std::fs::read_dir(&path) {
                for child in children.flatten() {
                    if child.path().is_dir() {
                        out.push(child.path());
                    }
                }
            }
        }
    }
    if let Ok(entries) = std::fs::read_dir("/mnt") {
        for entry in entries.flatten() {
            if entry.path().is_dir() {
                out.push(entry.path());
            }
        }
    }
    out
}

/// 下载/安装脚本应该去找的根目录，按优先级从高到低。
///
/// `extra` 是用户在设置里加的目录（`Settings::import_scan_dirs`），排在最前——
/// 用户明确指的地方比我们猜的任何位置都可信。
///
/// 刻意**不含 `$HOME` 本身**：minimax 的 `find_delivery()` 把 `$HOME` 整个扫了
/// 一遍（`-maxdepth 3`），在主目录塞满东西的机器上这一步能耗掉整个时间预算，
/// 挤掉后面真正该看的 U 盘。要扫主目录，用户可以自己把它加进 `extra`。
pub fn roots(extra: &[String], models_dir: &Path, staging_dir: &Path) -> Vec<PathBuf> {
    let home = home();
    let mut out: Vec<PathBuf> = Vec::new();
    let mut push = |p: PathBuf| {
        if p.as_os_str().is_empty() || out.contains(&p) {
            return;
        }
        out.push(p);
    };

    for dir in extra {
        let dir = dir.trim();
        if !dir.is_empty() {
            push(PathBuf::from(dir));
        }
    }
    // 当前配置的权重目录。卡片本来就认这里，但它同时也是扫描根——用户可能
    // 在它下面又套了一层目录。
    push(models_dir.to_path_buf());
    push(staging_dir.to_path_buf());
    // LMM 作为独立应用时的默认权重目录。老用户、以及照着旧文档操作的人会把
    // 模型放这儿，2026-09-20 踩到的正是这一条。
    push(home.join("models"));
    push(home.join("Downloads"));
    push(home.join("下载"));
    push(home.join("Desktop"));
    push(home.join("桌面"));
    for mount in removable_mounts() {
        push(mount);
    }
    out
}

/// 下发给脚本的形式：一行一个路径。
///
/// 用换行而不是 `:` 或空格分隔，因为 U 盘卷标里带空格和中文是常事
/// （`/media/user/我的 移动硬盘`）。脚本侧配 `while IFS= read -r dir` 读。
pub fn encode_dirs(dirs: &[PathBuf]) -> String {
    dirs.iter()
        .map(|p| p.to_string_lossy().to_string())
        .collect::<Vec<_>>()
        .join("\n")
}

/// 这个目录里有没有权重文件的样子。
///
/// 判据刻意宽松，而且只看直接子文件——严格判定是卡片脚本的事。
fn holds_weights(dir: &Path) -> bool {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return false;
    };
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().to_lowercase();
        if name.ends_with(".safetensors") || name.ends_with(".gguf") || name.ends_with(".bin") {
            return true;
        }
    }
    false
}

/// 目录里权重文件的总字节。
///
/// 跳过点开头的文件和目录：HF 的下载暂存区是 `.cache/huggingface/`，半截文件
/// 算进来会让「体积看着够了」这个判断失真。
fn weight_bytes(dir: &Path) -> u64 {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return 0;
    };
    entries
        .flatten()
        .filter(|e| !e.file_name().to_string_lossy().starts_with('.'))
        .filter_map(|e| e.metadata().ok())
        .filter(|m| m.is_file())
        .map(|m| m.len())
        .sum()
}

/// 在给定的根目录下找出所有装着权重的目录。
///
/// 结果是**候选**，不是结论：这里认不出它是哪个模型（见模块文档）。用途有两个——
/// 下发给卡片脚本让它自己对，以及写进运行日志让用户看见「本机已经有这些了」。
///
/// 超过 [`SCAN_BUDGET`] 就停，已经找到的照常返回。宁可少找到几个，不能让用户
/// 点完下载盯着一个不动的进度条。
pub fn scan(roots: &[PathBuf]) -> Vec<Candidate> {
    let deadline = Instant::now() + SCAN_BUDGET;
    let mut out: Vec<Candidate> = Vec::new();

    for root in roots {
        if Instant::now() >= deadline {
            break;
        }
        walk(root, 0, deadline, &mut out);
    }

    // 大的排前面：完整副本总比半截的有用
    out.sort_by(|a, b| b.bytes.cmp(&a.bytes));
    out
}

fn walk(dir: &Path, depth: usize, deadline: Instant, out: &mut Vec<Candidate>) {
    if depth > SCAN_DEPTH || Instant::now() >= deadline || !dir.is_dir() {
        return;
    }
    if holds_weights(dir) {
        if !out.iter().any(|c| c.path == dir) {
            out.push(Candidate {
                path: dir.to_path_buf(),
                bytes: weight_bytes(dir),
                removable: is_removable(dir),
            });
        }
        // 装着权重的目录不再往下钻：底下多半是 `original/`、`.cache/` 这类
        // 附属目录，收进来只会让候选列表里全是同一个模型的零件。
        return;
    }
    if depth == SCAN_DEPTH {
        return;
    }
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        if Instant::now() >= deadline {
            return;
        }
        let name = entry.file_name();
        let name = name.to_string_lossy();
        // 点目录跳过：`.cache`、`.git`、`.Trash-1000` 里不会有用户导进来的权重，
        // 但 `.Trash` 底下可能有几十 GB 的已删除副本，钻进去纯属浪费预算。
        if name.starts_with('.') {
            continue;
        }
        // 不跟软链：`~/models` 里一条指回上层的软链就能让遍历绕不出来。
        match entry.file_type() {
            Ok(t) if t.is_dir() => walk(&entry.path(), depth + 1, deadline, out),
            _ => continue,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mk_model(dir: &Path, bytes: usize) {
        std::fs::create_dir_all(dir).unwrap();
        std::fs::write(dir.join("model-00001.safetensors"), vec![0u8; bytes]).unwrap();
        std::fs::write(dir.join("config.json"), "{}").unwrap();
    }

    #[test]
    fn removable_paths_are_recognised() {
        assert!(is_removable(Path::new("/media/user/usb/Model")));
        assert!(is_removable(Path::new("/run/media/user/usb/Model")));
        assert!(is_removable(Path::new("/mnt/disk/Model")));
        assert!(!is_removable(Path::new("/home/user/models/Model")));
    }

    /// 这条是 2026-09-20 那个坑的回归：用户把权重放在 `~/models`，而配置的权重
    /// 目录是别处。`~/models` 必须在搜索根里，否则那 124GB 谁也看不见。
    #[test]
    fn legacy_models_dir_is_always_searched() {
        // 必须握着 TempHome：这条要把 `roots()` 的结果和 `home()` 的结果对起来，而
        // 同进程里别的测试会改 HOME（TempHome 在 Drop 时还原）。两次读 HOME 之间
        // 正好撞上别人还原，断言就随机红一次——全量并行跑时实测复现过，串行和单独
        // 跑都是绿的。偶发红比一直红更费人，所以这里把 HOME 锁在自己手里。
        let _home = super::super::config::TempHome::new();
        let found = roots(&[], Path::new("/tmp/configured"), Path::new("/tmp/staging"));
        assert!(
            found.contains(&home().join("models")),
            "~/models 不在搜索范围里：{found:?}"
        );
        assert!(found.contains(&PathBuf::from("/tmp/configured")));
    }

    /// 用户自己配的目录要排在我们猜的所有位置前面。
    #[test]
    fn user_configured_dirs_come_first() {
        let extra = vec!["/data/my-models".to_string(), "  ".to_string()];
        let found = roots(
            &extra,
            Path::new("/tmp/configured"),
            Path::new("/tmp/staging"),
        );
        assert_eq!(found[0], PathBuf::from("/data/my-models"));
        assert!(
            !found.iter().any(|p| p.as_os_str().is_empty()),
            "空白项没滤掉"
        );
    }

    /// 路径里有空格时，换行分隔才切得开——U 盘卷标带空格和中文是常事。
    #[test]
    fn dirs_are_encoded_one_per_line() {
        let dirs = vec![
            PathBuf::from("/media/user/我的 移动硬盘"),
            PathBuf::from("/home/user/models"),
        ];
        let encoded = encode_dirs(&dirs);
        assert_eq!(encoded.lines().count(), 2);
        assert_eq!(encoded.lines().next().unwrap(), "/media/user/我的 移动硬盘");
    }

    #[test]
    fn scan_finds_weights_nested_under_a_root() {
        let tmp = tempfile::tempdir().unwrap();
        mk_model(&tmp.path().join("models/Qwen3.8-Flash-Next-NVFP4"), 4096);
        let found = scan(&[tmp.path().to_path_buf()]);
        assert_eq!(found.len(), 1, "{found:?}");
        assert!(found[0].path.ends_with("Qwen3.8-Flash-Next-NVFP4"));
        assert_eq!(found[0].bytes, 4096 + 2, "config.json 的 2 字节也算");
    }

    /// 只有 config.json 的目录不算候选：那种目录报给用户，他会以为模型在那儿。
    #[test]
    fn a_directory_without_weight_files_is_not_a_candidate() {
        let tmp = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(tmp.path().join("empty-model")).unwrap();
        std::fs::write(tmp.path().join("empty-model/config.json"), "{}").unwrap();
        assert!(scan(&[tmp.path().to_path_buf()]).is_empty());
    }

    /// 命中后不再往下钻。否则 `original/consolidated.safetensors` 这种附属目录
    /// 会作为独立候选冒出来，列表里全是同一个模型的零件。
    #[test]
    fn scan_does_not_descend_into_a_matched_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let model = tmp.path().join("Model");
        mk_model(&model, 100);
        mk_model(&model.join("original"), 100);
        let found = scan(&[tmp.path().to_path_buf()]);
        assert_eq!(found.len(), 1, "子目录被当成了独立候选：{found:?}");
        assert_eq!(found[0].path, model);
    }

    /// HF 的下载暂存区在 `.cache/huggingface/` 底下，半截文件不能算进体积，
    /// 否则「看着够大」的判断会被一份下了一半的副本骗过去。
    #[test]
    fn partial_downloads_under_dot_cache_are_ignored() {
        let tmp = tempfile::tempdir().unwrap();
        let model = tmp.path().join("Model");
        mk_model(&model, 100);
        std::fs::create_dir_all(model.join(".cache/huggingface")).unwrap();
        std::fs::write(
            model.join(".cache/huggingface/half.incomplete"),
            vec![0u8; 99999],
        )
        .unwrap();
        let found = scan(&[tmp.path().to_path_buf()]);
        assert_eq!(found[0].bytes, 102, "点目录里的半截文件被算进体积了");
    }

    /// 软链会让遍历绕不出来。`~/models/self -> ~/models` 这种一条就够。
    #[test]
    fn symlink_loops_do_not_hang_the_scan() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("models");
        mk_model(&root.join("Model"), 10);
        std::os::unix::fs::symlink(&root, root.join("self")).unwrap();
        let found = scan(&[tmp.path().to_path_buf()]);
        assert_eq!(found.len(), 1, "{found:?}");
    }

    /// 同一个目录被两个根扫到（`/media/user` 和 `/media/user/盘` 都在根列表里）
    /// 时只能出现一次。
    #[test]
    fn a_directory_reachable_from_two_roots_is_reported_once() {
        let tmp = tempfile::tempdir().unwrap();
        mk_model(&tmp.path().join("a/Model"), 10);
        let found = scan(&[tmp.path().to_path_buf(), tmp.path().join("a")]);
        assert_eq!(found.len(), 1, "{found:?}");
    }
}
