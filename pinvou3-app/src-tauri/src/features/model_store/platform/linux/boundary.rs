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
pub(crate) fn store_model_name_checked(
    model_name: &str,
    case_insensitive: bool,
) -> Result<Option<String>, String> {
    match db::get_store_model_by_name(model_name, case_insensitive) {
        Ok(Some(record)) => Ok(Some(record.model_name)),
        Ok(None) => Ok(None),
        Err(error) => Err(format!("商店库查询失败：{error}")),
    }
}

fn weights_roots_from(settings: &config::Settings) -> Vec<std::path::PathBuf> {
    let mut roots = vec![config::data_dir()];
    let custom = std::path::PathBuf::from(&settings.models_dir);
    if !roots.contains(&custom) {
        roots.push(custom);
    }
    roots
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
    weights_roots_from(&config::load_settings())
}

/// [`store_weights_roots`] 的可失败版：settings 读取失败时 `Err` 而不是静默回退
/// 默认根。**破坏性的移除路径必须走本函数**——静默回退会让"自定义 models_dir"
/// 的保护恰在配置读取失败的一拍失效，深度守卫照常放行、商店权重被删（五轮评审
/// P3，与商店名单查询的 fail-closed 同口径）；宽松的登记端继续用原函数。
pub(crate) fn store_weights_roots_checked() -> Result<Vec<std::path::PathBuf>, String> {
    Ok(weights_roots_from(&config::try_load_settings()?))
}

/// 本机 Ollama 守护进程基地址：默认 `http://127.0.0.1:11434`，可被商店设置里的
/// `ollama_host`（config.json）覆盖。设备管理（features/device）的外部模型探测、
/// 标签与删除必须与商店的 ollama 生命周期指向**同一**守护进程，否则改配过的机器
/// 上设备页会读写另一个实例（第三轮评审 P3 对齐项）。空值回填默认。
pub(crate) fn ollama_base_url() -> String {
    let host = config::load_settings().ollama_host;
    let host = host.trim();
    if host.is_empty() {
        "http://127.0.0.1:11434".to_string()
    } else {
        host.trim_end_matches('/').to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::super::store_client::DeployTopology;
    use super::config::{self, TempHome};
    use super::{
        db, ollama_base_url, store_model_name, store_model_name_checked, store_weights_roots,
        store_weights_roots_checked,
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
    fn weights_roots_cover_default_plus_custom_models_dir() {
        let _ctx = TempHome::new();
        // 未配置：数据根 + 回填的默认 models_dir（load_settings 把空 models_dir
        // 回填为数据根下的 models/）——两个根都算商店管理的权重。
        let default_roots = store_weights_roots();
        assert_eq!(
            default_roots,
            vec![config::data_dir(), config::data_dir().join("models")]
        );
        // 自定义 models_dir：替换默认回填值、追加为第二个根（与数据根相同则不重复）。
        write_config(r#"{"models_dir": "/data/llm-weights"}"#);
        let roots = store_weights_roots();
        assert_eq!(
            roots,
            vec![
                config::data_dir(),
                std::path::PathBuf::from("/data/llm-weights")
            ]
        );
        write_config(&format!(
            r#"{{"models_dir": "{}"}}"#,
            config::data_dir().display()
        ));
        assert_eq!(
            store_weights_roots().len(),
            1,
            "custom == data root must not duplicate the root"
        );
    }
}
