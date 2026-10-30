//! 商店边界查询（`features/device` 的外部模型登记/移除消费）。
//!
//! 从 `platform/mod.rs` 的内联 `#[cfg]` 迁回仓库既有的 linux/unsupported 模块
//! 约定——同一个文件里并存两套平台分支写法是第四轮评审点名的结构债。

use super::config;
use super::db;

/// 商店边界查询（宽松版，登记端用）：按模型名查商店库，命中返回商店里的规范
/// 名。查询出错按未命中处理——登记错最多产生一条之后删不掉的记录，不构成破
/// 坏；**破坏性的移除路径必须走 [`store_model_name_checked`]（fail-closed）**。
pub(crate) fn store_model_name(model_name: &str) -> Option<String> {
    store_model_name_checked(model_name, false).ok().flatten()
}

/// 商店边界查询（可失败版，移除路径 fail-closed 用）：
/// - `Ok(Some)` = 命中商店名单（返回商店里的规范名）；
/// - `Ok(None)` = 确认不在名单；
/// - `Err` = 查询本身失败（库打不开/损坏）。移除端收到 Err 必须拒绝操作：
///   瞬时 `SQLITE_BUSY`（恰在商店部署/删除高峰最常见）若按未命中放行，别名
///   记录就能对着商店管理的 Ollama 模型执行真 DELETE（第四轮评审 P2）。
///
/// `case_insensitive` 供 Ollama 有效删除名臂：daemon 标签按小写归一，sqlite
/// BINARY 精确匹配会漏掉 `Qwen3:30B` → `qwen3:30b` 的别名绕道（第四轮 P3）。
///
/// 两臂直接比对都未命中时，再对**去掉 `:tag` 后缀**的名字按同一 case 口径重查
/// 一次：商店的 `model_name` 由 `suggest_model_name` 生成，冒号一律清洗成 `-`
/// （lifecycle.rs），而 daemon 侧的名字恒带 `:tag`——商店 ollama 卡片以
/// `name:latest` 形态出现在 /api/tags 与手写注册表记录里时，直接比对全部落空
/// （第八轮评审）。误伤方向是"把用户自己的 `x:v2` 当商店模型拒掉"（fail-safe，
/// 报错给出商店出口）；漏放方向是别名记录真删商店部署的 daemon 模型。
pub(crate) fn store_model_name_checked(
    model_name: &str,
    case_insensitive: bool,
) -> Result<Option<String>, String> {
    match db::get_store_model_by_name(model_name, case_insensitive) {
        Ok(Some(record)) => Ok(Some(record.model_name)),
        Ok(None) => {
            let stripped = model_name.split(':').next().unwrap_or(model_name).trim();
            if stripped.is_empty() || stripped == model_name {
                return Ok(None);
            }
            match db::get_store_model_by_name(stripped, case_insensitive) {
                Ok(Some(record)) => Ok(Some(record.model_name)),
                Ok(None) => Ok(None),
                Err(error) => Err(format!("商店库查询失败：{error}")),
            }
        }
        Err(error) => Err(format!("商店库查询失败：{error}")),
    }
}

/// 权重根收集（纯函数，测试直打）：返回**绝对**根集合与被拒的相对条目（回显
/// 限长 64）。入口绝对路径校验上线前写入的存量 config（或手改文件）可能携带
/// 相对路径——相对解析随消费方工作目录漂移，对绝对权重路径的前缀匹配必然
/// 失配，混进根集合只是"看起来有保护"（十四轮评审）。宽松端跳过相对条目
/// （与历史行为等价）；**破坏性的移除端必须据此 Err**（fail-closed），与
/// `set_storage_settings` 的写入侧拒绝同一把 `is_absolute` 尺子。
fn collect_weights_roots(settings: &config::Settings) -> (Vec<std::path::PathBuf>, Vec<String>) {
    let mut roots = vec![config::data_dir()];
    let mut relative: Vec<String> = Vec::new();
    // 当前 models_dir + 历史用过的 models_dir（换目录不搬权重，老根下仍是商店
    // 在册模型，第六轮评审 P2：只看当前值的话改过目录的机器旧根裸奔）+
    // import_scan_dirs（下载脚本会在这些目录里**就地复用**已存在的权重——
    // 复用不搬家，商店模型的权重可能就落在里面）+ staging_dir（下载暂存目录
    // 可在设置里挪走，中断的下载会留下半成品权重；默认值在数据根之下，挪走后
    // 只有这里能兜住，第八轮评审 P3）。空串/重复项无害但白占一行，滤掉。
    for dir in std::iter::once(&settings.models_dir)
        .chain(settings.models_dir_history.iter())
        .chain(settings.import_scan_dirs.iter())
        .chain(std::iter::once(&settings.staging_dir))
    {
        let trimmed = dir.trim();
        if trimmed.is_empty() {
            continue;
        }
        let root = std::path::PathBuf::from(trimmed);
        if !root.is_absolute() {
            relative.push(trimmed.chars().take(64).collect());
            continue;
        }
        if !roots.contains(&root) {
            roots.push(root);
        }
    }
    // 内置导入扫描根（~/models、~/Downloads、~/下载、~/Desktop、~/桌面）与脚本
    // 侧同源（import_scan::builtin_home_roots）：LMM_IMPORT_DIRS 把它们下发给
    // 全部卡片，卡片会就地复用其中的既有权重（复用不搬家），商店在册权重因此
    // 可能落在这些目录下——不收进来，「别名登记 → 移除」就能真删这些权重
    // （十二轮评审 P1）。可移动介质挂载点刻意不收：它们在家目录之外，删除守卫
    // 的「必须位于家目录之下」臂本来就够不着；收进来反而会把「登记 U 盘上的
    // 外部模型」这一合法场景误拒。
    for root in super::import_scan::builtin_home_roots() {
        if !roots.contains(&root) {
            roots.push(root);
        }
    }
    (roots, relative)
}

/// 商店权重的**全部**可能根目录。默认根是 store 数据根（`~/.pinvou3/model-store/`，
/// 默认 `models_dir` 即其下的 `models/`）；用户可在设置里把 `models_dir` 改到别处
///（几十上百 GB 的权重常被挪去别的盘），那个目录同样是商店管理的权重根。
///
/// 设备管理（features/device）的外部模型登记/移除对每个根都做路径边界检查——
/// 设备侧不得自行推断默认值，否则商店调整权重布局时两边会漂移。
///
/// 注意：本函数并非无副作用——`data_dir()` 首次调用会建出商店数据目录，
/// `load_settings()` 读 config.json（坏配置会触发隔离改名），底层 connect 还会
/// 建出空库与 WAL 边车。调用方（features/device）已在 blocking 池中调用并接受
/// 这些副作用；修改副作用语义前先核对这两个调用点。
pub(crate) fn store_weights_roots() -> Vec<std::path::PathBuf> {
    collect_weights_roots(&config::load_settings()).0
}

/// [`store_weights_roots`] 的可失败版：settings 读取失败时 `Err` 而不是静默回退
/// 默认根。**破坏性的移除路径必须走本函数**——静默回退会让"自定义 models_dir"
/// 的保护恰在配置读取失败的一拍失效，深度守卫照常放行、商店权重被删（五轮评审
/// P3，与商店名单查询的 fail-closed 同口径）；宽松的登记端继续用原函数。
pub(crate) fn store_weights_roots_checked() -> Result<Vec<std::path::PathBuf>, String> {
    let (roots, relative) = collect_weights_roots(&config::try_load_settings()?);
    if relative.is_empty() {
        return Ok(roots);
    }
    // 移除端收到的 Err 会被 external_models 的守卫链包上"本次移除已被拒绝"
    // 人话后缀；这里把名单逐个回显，用户照着改设置即可恢复。
    Err(format!(
        "商店权重根设置含相对路径项（{}）：相对路径无法参与权重保护边界，请改为绝对路径",
        relative.join("、")
    ))
}

/// 本机 Ollama 守护进程基地址：默认 `http://127.0.0.1:11434`，可被商店设置里的
/// `ollama_host`（config.json）覆盖。设备管理（features/device）的外部模型探测、
/// 标签与删除必须与商店的 ollama 生命周期指向**同一**守护进程，否则改配过的机器
/// 上设备页会读写另一个实例（第三轮评审 P3 对齐项）。空值回填默认。
pub(crate) fn ollama_base_url() -> String {
    finish_ollama_base_url(&config::load_settings().ollama_host)
}

/// 空值回填默认 + trim + 去尾斜杠（宽松/可失败两版共用的尾段）。
fn finish_ollama_base_url(host: &str) -> String {
    let host = host.trim();
    if host.is_empty() {
        "http://127.0.0.1:11434".to_string()
    } else {
        host.trim_end_matches('/').to_string()
    }
}

/// [`ollama_base_url`] 的可失败版：settings 读取失败（解析坏被隔离、读取失败、
/// 隔离残留）时 `Err` 而不是静默回退默认地址。**破坏性的 daemon 调用（外部模型
/// 删除）必须走本函数**——非默认 `ollama_host` 的机器上，列表刚从原实例读出、
/// 确认移除前 settings 恰变得不可读时，宽松回退会把 DELETE 改投默认 11434：
/// 默认实例上恰有同名非商店模型就会被真删，原实例对象原封不动（三十二轮评审
/// P2）。只读的探测/展示/标签消费方继续用宽松版。
pub(crate) fn ollama_base_url_checked() -> Result<String, String> {
    let host = config::try_load_settings()?.ollama_host;
    Ok(finish_ollama_base_url(&host))
}

#[cfg(test)]
mod tests {
    use super::super::store_client::DeployTopology;
    use super::config::{self, TempHome};
    use super::{
        db, ollama_base_url, ollama_base_url_checked, store_model_name, store_model_name_checked,
        store_weights_roots, store_weights_roots_checked,
    };

    fn insert_store_model(card_id: &str, model_name: &str) {
        let scripts = std::collections::BTreeMap::from([(
            "deploy".to_string(),
            format!("/tmp/{card_id}/deploy.sh"),
        )]);
        db::add_store_model(db::AddStoreModelParams {
            card_id,
            model_name,
            card_name: card_id,
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &scripts,
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert store model");
    }

    fn write_config(json: &str) {
        let dir = config::config_dir();
        std::fs::create_dir_all(&dir).expect("config dir");
        std::fs::write(dir.join("config.json"), json).expect("write config.json");
    }

    #[test]
    fn name_lookup_matches_exactly_and_nocase_variant_folds_case() {
        let _ctx = TempHome::new();
        db::init_db().unwrap();
        insert_store_model("card-a", "qwen3:30b");

        // 精确臂：原名命中、大小写变体不命中（name 臂保持显示名契约）。
        assert_eq!(
            store_model_name_checked("qwen3:30b", false).unwrap(),
            Some("qwen3:30b".to_string())
        );
        assert_eq!(store_model_name_checked("Qwen3:30B", false).unwrap(), None);
        // Ollama 有效删除名臂：大小写不敏感（daemon 标签归一小写）。
        assert_eq!(
            store_model_name_checked("Qwen3:30B", true).unwrap(),
            Some("qwen3:30b".to_string())
        );
        assert_eq!(store_model_name_checked("不存在", true).unwrap(), None);
        // 宽松版与 checked 的精确臂同口径。
        assert_eq!(store_model_name("qwen3:30b"), Some("qwen3:30b".to_string()));
        assert_eq!(store_model_name("Qwen3:30B"), None);
    }

    #[test]
    fn checked_reports_query_failure_instead_of_miss() {
        let _ctx = TempHome::new();
        db::init_db().unwrap();
        // 用垃圾字节覆写库文件：connect 能打开，查询必然失败——这正是移除路径
        // 必须 fail-closed 的场景（按未命中放行 = 对商店模型执行真 DELETE）。
        std::fs::write(
            config::data_dir().join("models.db"),
            b"not a sqlite db at all",
        )
        .expect("corrupt db");
        let result = store_model_name_checked("qwen3:30b", false);
        assert!(
            result.is_err(),
            "corrupt db must surface as Err, not a miss"
        );
        // 宽松版同场景按未命中降级（登记端语义）。
        assert_eq!(store_model_name("qwen3:30b"), None);
    }

    #[test]
    fn ollama_base_url_trims_and_backfills_default() {
        let _ctx = TempHome::new();
        // 未配置/空值 → 出厂默认。
        assert_eq!(ollama_base_url(), "http://127.0.0.1:11434");
        write_config(r#"{"ollama_host": "  "}"#);
        assert_eq!(
            ollama_base_url(),
            "http://127.0.0.1:11434",
            "空白同样回填默认"
        );
        // 配置值 trim + 去尾斜杠；配置为唯一真相源，设备页与商店读写同一实例。
        write_config(r#"{"ollama_host": "http://192.168.1.5:11434/"}"#);
        assert_eq!(ollama_base_url(), "http://192.168.1.5:11434");
        write_config(r#"{"ollama_host": " http://10.0.0.2:11434 "}"#);
        assert_eq!(ollama_base_url(), "http://10.0.0.2:11434");
    }

    #[test]
    fn ollama_base_url_checked_matches_lenient_when_readable() {
        let _ctx = TempHome::new();
        // 未配置 → 出厂默认，与宽松版同值。
        assert_eq!(
            ollama_base_url_checked().ok().as_deref(),
            Some("http://127.0.0.1:11434")
        );
        // 配置了非默认 host：可失败版按同一 trim/去尾斜杠口径返回该 host——
        // 删除必须指向用户配置的实例。
        write_config(r#"{"ollama_host": " http://192.168.1.5:11434/ "}"#);
        assert_eq!(
            ollama_base_url_checked().ok().as_deref(),
            Some("http://192.168.1.5:11434")
        );
    }

    #[test]
    fn ollama_base_url_checked_fails_closed_when_settings_unreadable() {
        let _ctx = TempHome::new();
        // 配置文件存在但解析坏：严格读取走隔离并返回 Err——删除路径据此拒绝，
        // 而不是静默改投默认 11434。非默认 ollama_host 的机器上，列表刚从原
        // 实例读出、确认移除前配置恰坏时，宽松回退会把 DELETE 改投默认实例：
        // 同名非商店模型会被真删、原实例对象原封不动（三十二轮评审 P2）。
        write_config("not json at all");
        let error = ollama_base_url_checked()
            .expect_err("unreadable settings must fail the destructive endpoint read");
        assert!(error.contains("配置"), "unexpected error: {error}");
        // 隔离残留还在时二次严格检查继续拒绝（与 store_weights_roots_checked
        // 的十六轮评审 P1 口径一致）。
        let error =
            ollama_base_url_checked().expect_err("quarantine residue must stay fail-closed");
        assert!(error.contains("隔离"), "unexpected error: {error}");
        // 宽松版同场景维持历史行为：回默认地址（只读探测/展示消费方）。
        assert_eq!(ollama_base_url(), "http://127.0.0.1:11434");
        // 受控恢复：放回合法配置后放行，配置值回到唯一真相源。
        write_config(r#"{"ollama_host": "http://10.0.0.2:11434 "}"#);
        assert_eq!(
            ollama_base_url_checked().ok().as_deref(),
            Some("http://10.0.0.2:11434")
        );
    }

    #[test]
    fn checked_weights_roots_reject_when_settings_unreadable() {
        let _ctx = TempHome::new();
        // 配置文件存在但解析坏：try_load_settings 走隔离改名并返回 Err——移除
        // 路径据此拒绝，而不是静默回退默认根放行删除（五轮评审 P3）。
        write_config("not json at all");
        assert!(store_weights_roots_checked().is_err());
        // 宽松版同场景维持历史行为：默认根集合（隔离改名后按默认设置回填）。
        assert!(!store_weights_roots().is_empty());
    }

    #[test]
    fn quarantined_config_keeps_checked_fail_closed_across_rereads() {
        let _ctx = TempHome::new();
        write_config("not json at all");
        // 第一次严格检查：隔离改名并拒绝。
        assert!(store_weights_roots_checked().is_err());
        // 第二次严格检查：原文件已不在（NotFound），但隔离残留还在——保护范围
        // 未知不能当"全新安装"放行默认根，否则重试一次就把自定义 models_dir 的
        // 保护整个撤掉（十六轮评审 P1）。
        let error =
            store_weights_roots_checked().expect_err("quarantine residue must stay fail-closed");
        assert!(error.contains("隔离"), "unexpected error: {error}");
        // 宽松版同场景维持历史行为：默认根集合。
        assert!(!store_weights_roots().is_empty());
        // 受控恢复：放回合法配置（留存文件不必手删）后放行，自定义根回到保护
        // 集合。
        write_config(r#"{"models_dir": "/data/custom-models"}"#);
        let roots = store_weights_roots_checked().expect("recovered config must pass");
        assert!(roots.contains(&std::path::PathBuf::from("/data/custom-models")));
    }

    #[test]
    fn lenient_read_quarantine_keeps_later_checked_fail_closed() {
        let _ctx = TempHome::new();
        write_config("not json at all");
        // 普通读取先隔离（宽松端照旧承担隔离副作用，如设备列表读 ollama_base_url）。
        assert!(!store_weights_roots().is_empty());
        // 随后的严格检查（移除路径）仍必须拒绝，不能按默认根放行。
        let error =
            store_weights_roots_checked().expect_err("post-quarantine check must fail closed");
        assert!(error.contains("隔离"), "unexpected error: {error}");
        // 受控放弃：显式删掉留存文件后回到全新安装语义（默认根）。
        std::fs::remove_file(config::config_dir().join("config.json.bad"))
            .expect("remove quarantine residue");
        assert!(store_weights_roots_checked().is_ok());
    }

    #[test]
    fn legacy_relative_roots_fail_closed_on_checked_and_skip_on_lenient() {
        let _ctx = TempHome::new();
        // 入口校验上线前的存量 config（或手改文件绕过 set_storage_settings）：
        // 相对条目对绝对权重路径的前缀匹配必然失配——checked 必须拒绝而不是
        // 静默携带一个"永远匹配不上"的假保护根（十四轮评审）。
        write_config(r#"{"import_scan_dirs": ["relative/dir"], "models_dir": "/data/abs"}"#);
        let error = store_weights_roots_checked().expect_err("relative root must fail closed");
        assert!(error.contains("relative/dir"), "unexpected error: {error}");
        assert!(error.contains("绝对路径"), "unexpected error: {error}");
        // 宽松端（登记）跳过相对条目：绝对条目与数据根照常在集合里，集合内
        // 不再混有相对根（与修复前"相对根混入但永远匹配不上"的假保护不同）。
        let roots = store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from("/data/abs")));
        assert!(
            roots.iter().all(|root| root.is_absolute()),
            "lenient roots must not carry relative entries: {roots:?}"
        );
    }

    #[test]
    fn relative_yardstick_covers_every_settings_derived_root_source() {
        let _ctx = TempHome::new();
        // 四个设置来源（当前 models_dir / 历史 models_dir / import_scan_dirs /
        // staging_dir）走同一把 is_absolute 尺子——只查 import_scan_dirs 会给
        // 其余三路留静默缺口。
        for config_json in [
            r#"{"models_dir": "rel-models"}"#.to_string(),
            r#"{"models_dir_history": ["rel-old"]}"#.to_string(),
            r#"{"import_scan_dirs": ["rel-scan"]}"#.to_string(),
            r#"{"staging_dir": "rel-staging"}"#.to_string(),
        ] {
            write_config(&config_json);
            let error = store_weights_roots_checked()
                .expect_err("every relative settings root must fail closed");
            assert!(
                error.contains("rel-"),
                "config {config_json} must name the offending entry: {error}"
            );
        }
    }

    #[test]
    fn weights_roots_cover_default_plus_custom_models_dir() {
        let _ctx = TempHome::new();
        // 内置导入扫描根与脚本侧同一份清单（TempHome 隔离 HOME 后即临时家目录
        // 下的五个静态目录），排在 settings 派生根之后（十二轮评审 P1）。
        let builtin = super::super::import_scan::builtin_home_roots();
        assert_eq!(builtin.len(), 5, "内置静态扫描根清单变化时同步本测试");
        // 未配置：数据根 + 回填的默认 models_dir + 回填的默认 staging_dir
        // （load_settings 把空值分别回填为数据根下的 models/ 与 staging/）——
        // 三个根都算商店管理的权重。
        let mut expected = vec![
            config::data_dir(),
            config::data_dir().join("models"),
            config::data_dir().join("staging"),
        ];
        expected.extend(builtin.clone());
        assert_eq!(store_weights_roots(), expected);
        // 自定义 models_dir：替换默认回填值、追加为第二个根（与数据根相同则不重复）。
        write_config(r#"{"models_dir": "/data/llm-weights"}"#);
        let mut expected = vec![
            config::data_dir(),
            std::path::PathBuf::from("/data/llm-weights"),
            config::data_dir().join("staging"),
        ];
        expected.extend(builtin);
        assert_eq!(store_weights_roots(), expected);
        write_config(&format!(
            r#"{{"models_dir": "{}"}}"#,
            config::data_dir().display()
        ));
        // staging 根独立存在，custom == data root 只是不重复数据根本身。
        // （+5 = 内置导入扫描根，十二轮评审 P1。）
        assert_eq!(
            store_weights_roots().len(),
            2 + 5,
            "custom == data root must not duplicate the data root"
        );
    }

    #[test]
    fn weights_roots_cover_history_and_import_scan_dirs() {
        let _ctx = TempHome::new();
        // 换目录不搬权重：历史 models_dir 与 import_scan_dirs 下的权重同样是
        // 商店在册/复用中的模型，必须落在保护根集合里（第六轮评审 P2）。
        write_config(
            r#"{
                "models_dir": "/data/new-weights",
                "models_dir_history": ["/data/old-weights", "  "],
                "import_scan_dirs": ["/usb/llm", "/data/new-weights"]
            }"#,
        );
        let roots = store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from("/data/old-weights")));
        assert!(roots.contains(&std::path::PathBuf::from("/usb/llm")));
        assert!(roots.contains(&std::path::PathBuf::from("/data/new-weights")));
        // 空白项滤掉、与当前 models_dir 重复的导入目录不重复占位；回填的默认
        // staging 根排在 settings 派生段最后，内置导入扫描根收尾。
        let mut expected = vec![
            config::data_dir(),
            std::path::PathBuf::from("/data/new-weights"),
            std::path::PathBuf::from("/data/old-weights"),
            std::path::PathBuf::from("/usb/llm"),
            config::data_dir().join("staging"),
        ];
        expected.extend(super::super::import_scan::builtin_home_roots());
        assert_eq!(roots, expected);
    }

    #[test]
    fn weights_roots_cover_custom_staging_dir() {
        let _ctx = TempHome::new();
        // 用户把暂存目录挪到别处：中断下载留下的半成品权重同样受保护
        // （第八轮评审 P3——原先只靠默认值落在数据根之下才被兜住）。
        write_config(r#"{"staging_dir": "/data/llm-staging"}"#);
        let roots = store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from("/data/llm-staging")));
        // 自定义 staging 与数据根/models_dir 都不重复占位。
        //（+5 = 内置导入扫描根，十二轮评审 P1。）
        assert_eq!(
            roots.len(),
            3 + 5,
            "data + backfilled models + custom staging"
        );
    }

    #[test]
    fn tag_suffix_store_names_are_caught_by_tagless_fallback() {
        let _ctx = TempHome::new();
        db::init_db().unwrap();
        // 商店 model_name 由 suggest_model_name 生成，冒号清洗成 '-'——永无
        // 冒号；daemon 侧恒带 `:tag`。商店 ollama 卡片以 `name:latest` 形态
        // 出现在 /api/tags 与手写注册表记录里时，直接比对全部落空（第八轮
        // 评审）：去掉 tag 后缀必须命中。
        insert_store_model("card-ollama", "yuguang14b");
        // 两种 case 口径下去 tag 都命中，大小写变体同抓。
        assert_eq!(
            store_model_name_checked("yuguang14b:latest", false).unwrap(),
            Some("yuguang14b".to_string())
        );
        assert_eq!(
            store_model_name_checked("Yuguang14B:latest", true).unwrap(),
            Some("yuguang14b".to_string())
        );
        // 用户自己的带 tag 模型在商店无同名卡片时不受影响。
        assert_eq!(store_model_name_checked("mymodel:v2", false).unwrap(), None);
    }
}
