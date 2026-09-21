//! 本地状态存储（SQLite）。
//!
//! 记三类东西：
//! 1. installed_engines —— 本机通过"模型商店"装过哪些推理引擎（避免每次下载模型
//!    都重新跑一遍 install_engine 脚本）。
//! 2. store_models —— 从商店下载/部署过的模型：绑定的引擎、来源信息，该模型卡片
//!    背后六类脚本各自的下载地址，以及启用成功后拿到的对外调用地址
//!    （endpoint_url / endpoint_model，给 API 网关转发用）。
//! 3. api_keys —— API 网关的访问密钥，供局域网内其他应用调用模型时鉴权用。
//!
//! Schema 跟 Python 版完全一致，共用同一个 `models.db` 文件（见 `config.rs`
//! 的路径说明），保证从 Python 版升级过来的用户数据原样可读。

use std::path::PathBuf;

use rand::Rng as _;
use rusqlite::{Connection, OptionalExtension, Row, params};

use super::config;
use super::store_client::{DeployProfile, DeployTopology};

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS installed_engines (
    engine_name TEXT NOT NULL,
    -- 版本必须是主键的一部分。只拿 engine_name 当主键时，两张卡片都叫 vllm
    -- 但版本不同（v0.27.1-aarch64 / dspark-0.1.1），后装的会把先装的整行覆盖：
    -- 界面上变成一个引擎「被两张卡使用」，而那一行的 docker_image 只剩后者的，
    -- 于是点删除会 rmi 错的镜像，先装那张卡的镜像反而永远删不掉。真机上出现过。
    --
    -- 用 '' 而不是 NULL 表示「没有版本号」：SQLite 主键里的 NULL 互不相等，
    -- 留 NULL 会让同一个引擎能插进无数行，等于没有约束。
    engine_version TEXT NOT NULL DEFAULT '',
    docker_image TEXT,
    -- 上次成功执行的那份 install_engine 脚本的 sha256。install_engine 不只装
    -- 引擎，还负责把部署包（compose/scripts）落到 vendor 目录；只按「引擎在不在」
    -- 判断跳过的话，商店修好的脚本永远送不到已装过引擎的设备上。存下哈希，
    -- 下次发现脚本变了就重跑一遍把部署包刷新掉。
    install_script_sha256 TEXT,
    installed_at TEXT NOT NULL,
    PRIMARY KEY (engine_name, engine_version)
);

CREATE TABLE IF NOT EXISTS store_models (
    card_id TEXT PRIMARY KEY,
    model_name TEXT NOT NULL,
    card_name TEXT NOT NULL,
    org TEXT,
    description TEXT,
    engine_name TEXT NOT NULL,
    engine_version TEXT,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    quantization TEXT,
    source_repo_id TEXT,
    source_filename TEXT,
    status TEXT NOT NULL DEFAULT 'downloaded',
    running INTEGER NOT NULL DEFAULT 0,
    script_install_engine TEXT,
    script_deploy TEXT,
    script_download_model TEXT,
    script_enable TEXT,
    script_stop TEXT,
    script_uninstall TEXT,
    downloaded_at TEXT NOT NULL,
    deployed_at TEXT,
    endpoint_url TEXT,
    endpoint_model TEXT,
    profiles_json TEXT,
    active_profile_id TEXT,
    -- 六类脚本的正文 + sha256 的本地固化副本（JSON）。下载模型时抓一次存在
    -- 这里，之后部署/启用/停用/卸载全部用本地副本执行，**一次网都不联**——
    -- 商店重启、上架新卡、改脚本都不会打断用户正在进行的工作流程。
    -- 脚本更新由用户主动点「检查卡片更新」触发，不再每次操作偷偷去拉最新的。
    scripts_content_json TEXT,
    scripts_fetched_at TEXT,
    deployed_script_sha256 TEXT,
    deploy_topology TEXT,
    assets_content_json TEXT
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    card_id TEXT,
    label TEXT,
    key_value TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
";

/// store_models/api_keys/installed_engines 表在加这些列之前就已经存在于早期
/// 安装的设备上了，`CREATE TABLE IF NOT EXISTS` 不会给已存在的表补列，这里做
/// 一个很轻量的迁移：缺哪列就补哪列。
const MIGRATION_COLUMNS: &[(&str, &[(&str, &str)])] = &[
    (
        "store_models",
        &[
            ("endpoint_url", "TEXT"),
            ("endpoint_model", "TEXT"),
            ("description", "TEXT"),
            ("profiles_json", "TEXT"),
            ("active_profile_id", "TEXT"),
            // 脚本正文 + 哈希的本地固化副本（见 scripts_content_json 的说明）
            ("scripts_content_json", "TEXT"),
            ("scripts_fetched_at", "TEXT"),
            // 上次成功部署时用的 deploy 脚本哈希（见 deployed_script_sha256）
            ("deployed_script_sha256", "TEXT"),
            // 这张卡需要几台设备（"single" / "dual"），见 store_client::DeployTopology
            ("deploy_topology", "TEXT"),
            // 附属文件的本地固化副本（相对路径 -> 正文+哈希），见 assets_content
            ("assets_content_json", "TEXT"),
            // 卡片来自哪里："builtin" = 测试商店（脚本打包在应用二进制里）、
            // "store" = 真实商店。见 refresh_builtin_card_scripts 的说明。
            ("card_source", "TEXT"),
            // enable 脚本报上来的容器名（LMM_CONTAINER_NAME 标记行）。
            // 这是判断"某个容器是不是本应用起的"唯一可靠的依据——镜像不是身份，
            // 别人用同一个镜像起的容器跟我们的长得一模一样。
            ("container_name", "TEXT"),
        ],
    ),
    ("api_keys", &[("card_id", "TEXT")]),
    (
        "installed_engines",
        &[("docker_image", "TEXT"), ("install_script_sha256", "TEXT")],
    ),
];

pub fn db_path() -> PathBuf {
    config::data_dir().join("models.db")
}

fn connect() -> rusqlite::Result<Connection> {
    let conn = Connection::open(db_path())?;
    conn.execute_batch("PRAGMA foreign_keys = ON;")?;
    Ok(conn)
}

fn migrate(conn: &Connection) -> rusqlite::Result<()> {
    for (table, columns) in MIGRATION_COLUMNS {
        let mut stmt = conn.prepare(&format!("PRAGMA table_info({table})"))?;
        let existing: std::collections::HashSet<String> = stmt
            .query_map([], |row| row.get::<_, String>(1))?
            .filter_map(|r| r.ok())
            .collect();
        for (col, col_type) in *columns {
            if existing.contains(*col) {
                continue;
            }
            // `ALTER TABLE ... ADD COLUMN` 没有 IF NOT EXISTS，而「先 PRAGMA 查、
            // 再 ADD」这两步之间有窗口：另一个连接（第二个应用实例，或并发跑的
            // 测试）可能刚好在这中间把同一列加上了，我们这句就会报
            // "duplicate column name"，把整个启动带崩。
            //
            // 但那个报错说明的恰恰是「目标状态已经达成」，属于成功而不是失败。
            match conn.execute_batch(&format!("ALTER TABLE {table} ADD COLUMN {col} {col_type}")) {
                Ok(()) => {}
                Err(e) if is_duplicate_column(&e) => {}
                Err(e) => return Err(e),
            }
        }
    }
    Ok(())
}

fn is_duplicate_column(err: &rusqlite::Error) -> bool {
    err.to_string().contains("duplicate column name")
}

/// 把老设备上"主键只有 engine_name"的 installed_engines 换成 (name, version)
/// 复合主键。SQLite 改不了主键，只能重建表再把数据搬过去。
///
/// 搬过去的数据里，被覆盖掉的那一行是找不回来的（它在被覆盖时就没了）：受影响
/// 的引擎会在下次跑 install_engine 时重新登记。这里不去猜，宁可少一行也不写
/// 一行错的——docker_image 错了会导致删除时 rmi 掉别的模型在用的镜像。
fn migrate_engine_primary_key(conn: &Connection) -> rusqlite::Result<()> {
    let sql: Option<String> = conn
        .query_row(
            "SELECT sql FROM sqlite_master WHERE type='table' AND name='installed_engines'",
            [],
            |r| r.get(0),
        )
        .optional()?;
    let Some(sql) = sql else { return Ok(()) };
    if sql.contains("PRIMARY KEY (engine_name, engine_version)") {
        return Ok(()); // 已经是新结构
    }
    // 整段放进一个 IMMEDIATE 事务：重命名、建新表、搬数据、删旧表要么全成、
    // 要么全不成。中途失败留下一张 installed_engines_legacy 而没有正表，比不
    // 迁移糟得多——应用会彻底起不来。
    let migration = format!(
        "BEGIN IMMEDIATE;
         ALTER TABLE installed_engines RENAME TO installed_engines_legacy;
         {SCHEMA}
         INSERT OR REPLACE INTO installed_engines
             (engine_name, engine_version, docker_image, install_script_sha256, installed_at)
         SELECT engine_name, COALESCE(engine_version, ''), docker_image,
                install_script_sha256, installed_at
           FROM installed_engines_legacy;
         DROP TABLE installed_engines_legacy;
         COMMIT;"
    );
    if let Err(e) = conn.execute_batch(&migration) {
        let _ = conn.execute_batch("ROLLBACK;");
        // 另一个连接可能已经把这件事做完了。做完了就当成功，没做完才是真失败。
        let done: Option<String> = conn
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='table' AND name='installed_engines'",
                [],
                |r| r.get(0),
            )
            .optional()?;
        if !done
            .unwrap_or_default()
            .contains("PRIMARY KEY (engine_name, engine_version)")
        {
            return Err(e);
        }
    }
    Ok(())
}

/// 给 card_source 还是 NULL 的存量记录补上来源。
///
/// 判据是脚本地址的形态，不是卡片 id——**真实商店里存在跟内置测试卡同名的
/// 卡片**（测试卡本来就是照着真实卡做的，ornith15-bf16 两边都有）。只按 id 猜
/// 会把用户从真实商店下的卡片标成 builtin，然后用内置副本把它的脚本覆盖掉。
///
/// 而地址形态是确定的：测试商店存的是本地文件路径（test_store_scripts 目录下），
/// 真实商店存的是 http(s) URL。
fn backfill_card_source(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute(
        "UPDATE store_models SET card_source =
             CASE WHEN COALESCE(script_deploy, '') LIKE '%/test_store_scripts/%'
                  THEN 'builtin' ELSE 'store' END
         WHERE card_source IS NULL",
        [],
    )?;
    Ok(())
}

pub fn init_db() -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute_batch(SCHEMA)?;
    migrate(&conn)?;
    // 顺序要紧：先把缺的列补齐（老表可能还没有 docker_image），再重建主键，
    // 否则搬数据那句 SELECT 会因为列不存在而失败。
    migrate_engine_primary_key(&conn)?;
    backfill_card_source(&conn)?;
    Ok(())
}

fn now_iso() -> String {
    chrono::Utc::now().to_rfc3339()
}

// ---------------- 引擎安装记录 ----------------

#[derive(Debug, Clone, serde::Serialize)]
pub struct InstalledEngineRecord {
    pub engine_name: String,
    pub engine_version: Option<String>,
    pub docker_image: Option<String>,
    pub install_script_sha256: Option<String>,
    pub installed_at: String,
}

impl InstalledEngineRecord {
    fn from_row(row: &Row<'_>) -> rusqlite::Result<Self> {
        Ok(Self {
            engine_name: row.get("engine_name")?,
            engine_version: row.get("engine_version")?,
            docker_image: row.get("docker_image")?,
            install_script_sha256: row.get("install_script_sha256")?,
            installed_at: row.get("installed_at")?,
        })
    }
}

/// 引擎的身份是「名字 + 版本」。只按名字查会把 vllm v0.27.1 和 vllm
/// dspark-0.1.1 当成同一个东西——它们是两个完全不同的镜像。
fn engine_key(engine_version: Option<&str>) -> String {
    engine_version.unwrap_or("").to_string()
}

pub fn is_engine_installed(
    engine_name: &str,
    engine_version: Option<&str>,
) -> rusqlite::Result<bool> {
    let conn = connect()?;
    let exists: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM installed_engines WHERE engine_name = ?1 AND engine_version = ?2",
            params![engine_name, engine_key(engine_version)],
            |r| r.get(0),
        )
        .optional()?;
    Ok(exists.is_some())
}

pub fn mark_engine_installed(
    engine_name: &str,
    engine_version: Option<&str>,
    docker_image: Option<&str>,
    install_script_sha256: Option<&str>,
) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "INSERT INTO installed_engines
             (engine_name, engine_version, docker_image, install_script_sha256, installed_at)
         VALUES (?1, ?2, ?3, ?4, ?5)
         ON CONFLICT(engine_name, engine_version) DO UPDATE SET
             docker_image=COALESCE(excluded.docker_image, installed_engines.docker_image),
             install_script_sha256=COALESCE(excluded.install_script_sha256,
                                            installed_engines.install_script_sha256),
             installed_at=excluded.installed_at",
        params![
            engine_name,
            engine_key(engine_version),
            docker_image,
            install_script_sha256,
            now_iso()
        ],
    )?;
    Ok(())
}

pub fn list_installed_engines() -> rusqlite::Result<Vec<InstalledEngineRecord>> {
    let conn = connect()?;
    let mut stmt =
        conn.prepare("SELECT * FROM installed_engines ORDER BY engine_name, engine_version")?;
    let rows = stmt
        .query_map([], InstalledEngineRecord::from_row)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows)
}

pub fn get_installed_engine(
    engine_name: &str,
    engine_version: Option<&str>,
) -> rusqlite::Result<Option<InstalledEngineRecord>> {
    let conn = connect()?;
    conn.query_row(
        "SELECT * FROM installed_engines WHERE engine_name = ?1 AND engine_version = ?2",
        params![engine_name, engine_key(engine_version)],
        InstalledEngineRecord::from_row,
    )
    .optional()
}

pub fn delete_installed_engine(
    engine_name: &str,
    engine_version: Option<&str>,
) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "DELETE FROM installed_engines WHERE engine_name = ?1 AND engine_version = ?2",
        params![engine_name, engine_key(engine_version)],
    )?;
    Ok(())
}

/// 引擎可能被多张已下载/部署的卡片共用，删除前用这个查有哪些模型受影响，
/// 给用户看清楚再确认。
pub fn list_model_names_using_engine(
    engine_name: &str,
    engine_version: Option<&str>,
) -> rusqlite::Result<Vec<String>> {
    let conn = connect()?;
    // 版本也要比：同名不同版本是两个不同的引擎，不该把用另一个版本的卡片
    // 也算成"受影响"，否则删除确认框里会列出一堆其实不受影响的模型。
    let mut stmt = conn.prepare(
        "SELECT card_name FROM store_models
          WHERE engine_name = ?1 AND COALESCE(engine_version, '') = ?2
          ORDER BY card_name",
    )?;
    let rows = stmt
        .query_map(params![engine_name, engine_key(engine_version)], |r| {
            r.get::<_, String>(0)
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows)
}

/// 记下这次启用真正起来的容器名（enable 脚本用 LMM_CONTAINER_NAME= 报上来）。
pub fn set_container_name(card_id: &str, container_name: Option<&str>) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET container_name = ?1 WHERE card_id = ?2",
        params![container_name, card_id],
    )?;
    Ok(())
}

// ---------------- 商店模型记录 ----------------

#[derive(Debug, Clone, serde::Serialize)]
pub struct StoreModelRecord {
    pub card_id: String,
    pub model_name: String,
    pub card_name: String,
    pub org: Option<String>,
    pub description: Option<String>,
    pub engine_name: String,
    pub engine_version: Option<String>,
    pub size_bytes: i64,
    pub quantization: Option<String>,
    pub source_repo_id: Option<String>,
    pub source_filename: Option<String>,
    pub status: String,
    pub running: bool,
    pub scripts: std::collections::BTreeMap<String, Option<String>>,
    pub downloaded_at: String,
    pub deployed_at: Option<String>,
    /// enable 脚本报上来的容器名。判断"某个容器是不是本应用起的"只认它——
    /// 镜像不是身份，别人用同一个镜像起的容器跟我们的长得一模一样。
    pub container_name: Option<String>,
    pub endpoint_url: Option<String>,
    pub endpoint_model: Option<String>,
    pub profiles: Vec<DeployProfile>,
    pub active_profile_id: Option<String>,
    /// 六类脚本的本地固化副本（script_type -> 正文+哈希）。有它就完全不用
    /// 联商店；老记录（升级前下载的）这里是空的，操作时会退回按 URL 拉取。
    pub scripts_content: std::collections::BTreeMap<String, ScriptBlob>,
    /// 附属文件的本地固化副本（相对路径 -> 正文+哈希）。多文件卡片才有。
    pub assets_content: std::collections::BTreeMap<String, ScriptBlob>,
    pub scripts_fetched_at: Option<String>,
    /// 这张卡需要几台设备。老记录（这个字段是后加的）读出来是 Single，跟改动前
    /// 行为一致——历史上架的卡片本来就全是单机。
    pub deploy_topology: DeployTopology,
    /// 上次成功部署时用的 deploy 脚本哈希。
    ///
    /// 用来回答「这个模型现在到底需不需要重新部署」：跟当前本地脚本副本的哈希
    /// 一比就知道。以前界面对所有已部署模型都常驻一个「重新部署」按钮，用户
    /// 根本不知道自己为什么要点它。
    pub deployed_script_sha256: Option<String>,
}

/// 一份固化下来的脚本：正文 + 它的 sha256（来源哈希，用于"检查卡片更新"时
/// 跟商店当前的哈希比对，判断有没有新版本）。
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct ScriptBlob {
    pub content: String,
    pub sha256: String,
}

impl StoreModelRecord {
    fn from_row(row: &Row<'_>) -> rusqlite::Result<Self> {
        let profiles_raw: Option<String> = row.get("profiles_json")?;
        let profiles = profiles_raw
            .and_then(|s| serde_json::from_str::<Vec<DeployProfile>>(&s).ok())
            .unwrap_or_default();
        let mut scripts = std::collections::BTreeMap::new();
        scripts.insert(
            "install_engine".to_string(),
            row.get::<_, Option<String>>("script_install_engine")?,
        );
        scripts.insert("deploy".to_string(), row.get("script_deploy")?);
        scripts.insert(
            "download_model".to_string(),
            row.get("script_download_model")?,
        );
        scripts.insert("enable".to_string(), row.get("script_enable")?);
        scripts.insert("stop".to_string(), row.get("script_stop")?);
        scripts.insert("uninstall".to_string(), row.get("script_uninstall")?);

        Ok(Self {
            card_id: row.get("card_id")?,
            model_name: row.get("model_name")?,
            card_name: row.get("card_name")?,
            org: row.get("org")?,
            description: row.get("description")?,
            engine_name: row.get("engine_name")?,
            engine_version: row.get("engine_version")?,
            size_bytes: row.get("size_bytes")?,
            quantization: row.get("quantization")?,
            source_repo_id: row.get("source_repo_id")?,
            source_filename: row.get("source_filename")?,
            status: row.get("status")?,
            running: row.get::<_, i64>("running")? != 0,
            scripts,
            downloaded_at: row.get("downloaded_at")?,
            deployed_at: row.get("deployed_at")?,
            container_name: row.get("container_name")?,
            endpoint_url: row.get("endpoint_url")?,
            endpoint_model: row.get("endpoint_model")?,
            profiles,
            active_profile_id: row.get("active_profile_id")?,
            scripts_content: row
                .get::<_, Option<String>>("scripts_content_json")?
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_default(),
            assets_content: row
                .get::<_, Option<String>>("assets_content_json")?
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_default(),
            scripts_fetched_at: row.get("scripts_fetched_at")?,
            deploy_topology: row
                .get::<_, Option<String>>("deploy_topology")?
                .and_then(|s| serde_json::from_value(serde_json::Value::String(s)).ok())
                .unwrap_or_default(),
            deployed_script_sha256: row.get("deployed_script_sha256")?,
        })
    }
}

/// 把六类脚本的正文固化到本地。下载模型成功后调用一次，之后所有操作都用这份
/// 副本执行，不再依赖商店可达。
/// 固化脚本与附属文件的本地副本。
///
/// 两者必须**一起**写：入口脚本和它依赖的附属文件是一套东西，只更新一半会得到
/// 一个既不是旧版也不是新版的混合包，跑出来的结果没人能解释。
pub fn set_scripts_content(
    card_id: &str,
    blobs: &std::collections::BTreeMap<String, ScriptBlob>,
    assets: &std::collections::BTreeMap<String, ScriptBlob>,
) -> rusqlite::Result<()> {
    let json = serde_json::to_string(blobs).unwrap_or_default();
    let assets_json = serde_json::to_string(assets).unwrap_or_default();
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET scripts_content_json = ?1, assets_content_json = ?2,          scripts_fetched_at = ?3 WHERE card_id = ?4",
        params![json, assets_json, now_iso(), card_id],
    )?;
    Ok(())
}

pub struct AddStoreModelParams<'a> {
    pub card_id: &'a str,
    pub model_name: &'a str,
    pub card_name: &'a str,
    pub org: Option<&'a str>,
    pub description: Option<&'a str>,
    pub engine_name: &'a str,
    pub engine_version: Option<&'a str>,
    pub size_bytes: i64,
    pub quantization: Option<&'a str>,
    pub source_repo_id: Option<&'a str>,
    pub source_filename: Option<&'a str>,
    pub scripts: &'a std::collections::BTreeMap<String, String>,
    pub status: &'a str,
    pub profiles: &'a [DeployProfile],
    pub deploy_topology: DeployTopology,
    /// "builtin"（测试商店，脚本随应用发布）或 "store"（真实商店）
    pub card_source: &'a str,
}

pub fn add_store_model(p: AddStoreModelParams<'_>) -> rusqlite::Result<StoreModelRecord> {
    let now = now_iso();
    let deployed_at = if p.status == "deployed" {
        Some(now.clone())
    } else {
        None
    };
    let profiles_json = if p.profiles.is_empty() {
        None
    } else {
        Some(serde_json::to_string(p.profiles).unwrap_or_default())
    };
    let conn = connect()?;
    conn.execute(
        "INSERT INTO store_models
            (card_id, model_name, card_name, org, description, engine_name, engine_version,
             size_bytes, quantization, source_repo_id, source_filename, status,
             script_install_engine, script_deploy, script_download_model,
             script_enable, script_stop, script_uninstall,
             downloaded_at, deployed_at, profiles_json, deploy_topology, card_source)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21, ?22, ?23)
         ON CONFLICT(card_id) DO UPDATE SET
             model_name=excluded.model_name, card_name=excluded.card_name,
             org=excluded.org, description=excluded.description, engine_name=excluded.engine_name,
             engine_version=excluded.engine_version, size_bytes=excluded.size_bytes,
             quantization=excluded.quantization, source_repo_id=excluded.source_repo_id,
             source_filename=excluded.source_filename, status=excluded.status,
             script_install_engine=excluded.script_install_engine,
             script_deploy=excluded.script_deploy,
             script_download_model=excluded.script_download_model,
             script_enable=excluded.script_enable, script_stop=excluded.script_stop,
             script_uninstall=excluded.script_uninstall,
             downloaded_at=excluded.downloaded_at,
             deployed_at=COALESCE(excluded.deployed_at, store_models.deployed_at),
             profiles_json=excluded.profiles_json,
             deploy_topology=excluded.deploy_topology,
             card_source=excluded.card_source",
        params![
            p.card_id,
            p.model_name,
            p.card_name,
            p.org,
            p.description,
            p.engine_name,
            p.engine_version,
            p.size_bytes,
            p.quantization,
            p.source_repo_id,
            p.source_filename,
            p.status,
            p.scripts.get("install_engine"),
            p.scripts.get("deploy"),
            p.scripts.get("download_model"),
            p.scripts.get("enable"),
            p.scripts.get("stop"),
            p.scripts.get("uninstall"),
            now,
            deployed_at,
            profiles_json,
            // 存成 "single" / "dual"，跟 DeployTopology 的 serde 表示一致
            serde_json::to_value(p.deploy_topology)
                .ok()
                .and_then(|v| v.as_str().map(str::to_string)),
            p.card_source,
        ],
    )?;
    conn.query_row(
        "SELECT * FROM store_models WHERE card_id = ?1",
        params![p.card_id],
        StoreModelRecord::from_row,
    )
}

/// 把测试商店（内置）卡片的固化脚本副本刷新成应用里的当前版本。
///
/// **为什么必须有这个。** 卡片脚本在下载时被固化进数据库，之后所有操作都用那份
/// 本地副本——这个设计对真实商店是对的（商店改脚本不该打断用户正在进行的流程，
/// 更新由用户点「更新脚本」显式触发）。但对测试商店是错的：它的脚本就打包在应用
/// 二进制里，压根没有"服务器"这个概念，应用升级就等于脚本升级。
///
/// 不刷新的后果在真机上撞到过：推了新版应用、脚本改动一个都没生效，卡片还在跑
/// 下载那天固化的旧脚本，而报错完全看不出是这个原因——查了半天才发现落地的
/// common.sh 是 6425 字节而应用里那份是 19405 字节。
///
/// 只动 `card_source = 'builtin'` 的记录。测试卡的 id 跟真实商店卡片可能重名
/// （测试卡本来就是照着真实卡做的），只按 id 匹配会把用户从真实商店下的卡片
/// 用内置副本覆盖掉。
pub fn refresh_builtin_card_scripts(
    card_id: &str,
    blobs: &std::collections::BTreeMap<String, ScriptBlob>,
    assets: &std::collections::BTreeMap<String, ScriptBlob>,
) -> rusqlite::Result<bool> {
    let json = serde_json::to_string(blobs).unwrap_or_default();
    let assets_json = serde_json::to_string(assets).unwrap_or_default();
    let conn = connect()?;
    let changed = conn.execute(
        "UPDATE store_models
            SET scripts_content_json = ?1, assets_content_json = ?2, scripts_fetched_at = ?3
          WHERE card_id = ?4 AND card_source = 'builtin'
            AND (scripts_content_json IS NOT ?1 OR assets_content_json IS NOT ?2)",
        params![json, assets_json, now_iso(), card_id],
    )?;
    Ok(changed > 0)
}

/// `deploy_script_sha256` 只在部署成功时传：记下这次部署用的是哪个版本的
/// deploy 脚本，之后脚本一更新就能算出「这个模型需要重新部署」。
pub fn update_store_model_status(
    card_id: &str,
    status: &str,
    deploy_script_sha256: Option<&str>,
) -> rusqlite::Result<()> {
    let conn = connect()?;
    if status == "deployed" {
        conn.execute(
            "UPDATE store_models SET status = ?1, deployed_at = ?2, deployed_script_sha256 = ?3              WHERE card_id = ?4",
            params![status, now_iso(), deploy_script_sha256, card_id],
        )?;
    } else {
        conn.execute(
            "UPDATE store_models SET status = ?1 WHERE card_id = ?2",
            params![status, card_id],
        )?;
    }
    Ok(())
}

pub fn set_running(card_id: &str, running: bool) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET running = ?1 WHERE card_id = ?2",
        params![running as i64, card_id],
    )?;
    Ok(())
}

/// 把记录退回「已下载」：卸载成功、但权重按用户的选择留在磁盘上时用这个。
///
/// 为什么不能复用 `update_store_model_status(card_id, "downloaded", None)`
/// -----------------------------------------------------------------
/// 那个函数在非 deployed 分支上**只改 status 一列**，其余部署痕迹原样留着。
/// 卸载完还挂着 `running = 1`、旧的 `container_name` 和 `endpoint_url`，界面
/// 会把一个已经不存在的服务显示成运行中，网关也会照着一个早就没了的地址转发。
/// 所以这里把这次部署产生的每一列都清干净：
///
///   status / running           这次卸载真正要表达的状态
///   deployed_at                部署时间戳，服务都没了留着就是假的
///   deployed_script_sha256     留着会让「需要重新部署」的判断错位
///   container_name             容器已删，这个名字再匹配到就是别人的容器了
///   endpoint_url / _model      服务地址已失效
///
/// **刻意保留**的是 `profiles_json` / `active_profile_id`（上次选的参数方案）
/// 和 `scripts_content_json` / `assets_content_json`（脚本本地副本）：重新部署
/// 时要照着它们来，清掉等于让用户把选择重做一遍，而这跟卸载没有关系。
/// `downloaded_at` 同理不动——权重还在盘上，它什么时候下的没有变。
pub fn mark_uninstalled(card_id: &str) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models
            SET status = 'downloaded',
                running = 0,
                deployed_at = NULL,
                deployed_script_sha256 = NULL,
                container_name = NULL,
                endpoint_url = NULL,
                endpoint_model = NULL
          WHERE card_id = ?1",
        params![card_id],
    )?;
    Ok(())
}

/// 记下最近一次成功部署用的是哪套参数方案：下次重新部署/修复时默认沿用
/// 这一套，而不是每次都退回卡片给的第一个方案。
pub fn set_active_profile(card_id: &str, profile_id: Option<&str>) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET active_profile_id = ?1 WHERE card_id = ?2",
        params![profile_id, card_id],
    )?;
    Ok(())
}

/// 启用成功后记下这个模型对外的调用地址，供 API 网关转发用。停用/卸载时不用
/// 清空——下次启用会覆盖，展示层只在 running=true 时才把这个当有效信息用。
pub fn set_endpoint_info(
    card_id: &str,
    endpoint_url: Option<&str>,
    endpoint_model: Option<&str>,
) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET endpoint_url = ?1, endpoint_model = ?2 WHERE card_id = ?3",
        params![endpoint_url, endpoint_model, card_id],
    )?;
    Ok(())
}

pub fn list_store_models() -> rusqlite::Result<Vec<StoreModelRecord>> {
    let conn = connect()?;
    let mut stmt = conn.prepare("SELECT * FROM store_models ORDER BY downloaded_at DESC")?;
    let rows = stmt
        .query_map([], StoreModelRecord::from_row)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows)
}

pub fn get_store_model(card_id: &str) -> rusqlite::Result<Option<StoreModelRecord>> {
    let conn = connect()?;
    conn.query_row(
        "SELECT * FROM store_models WHERE card_id = ?1",
        params![card_id],
        StoreModelRecord::from_row,
    )
    .optional()
}

pub fn get_store_model_by_name(model_name: &str) -> rusqlite::Result<Option<StoreModelRecord>> {
    let conn = connect()?;
    conn.query_row(
        "SELECT * FROM store_models WHERE model_name = ?1",
        params![model_name],
        StoreModelRecord::from_row,
    )
    .optional()
}

pub fn delete_store_model(card_id: &str) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "DELETE FROM store_models WHERE card_id = ?1",
        params![card_id],
    )?;
    // 模型卸载了，专属它的 key 也跟着失效，不留着占地方
    conn.execute("DELETE FROM api_keys WHERE card_id = ?1", params![card_id])?;
    Ok(())
}

pub fn total_managed_bytes() -> rusqlite::Result<i64> {
    let conn = connect()?;
    conn.query_row(
        "SELECT COALESCE(SUM(size_bytes), 0) FROM store_models",
        [],
        |r| r.get(0),
    )
}

// ---------------- API 网关密钥 ----------------
// 每把 key 绑定一个具体模型（card_id），只能用来调用那一个模型——不同应用要接入
// 不同模型时分开发 key、分开吊销，互不影响。card_id 为空代表老版本遗留下的
// "全局 key"（改造前生成的），继续当成"可以调用任意模型"处理，不强行作废。

#[derive(Debug, Clone, serde::Serialize)]
pub struct ApiKeyRecord {
    pub id: String,
    pub card_id: Option<String>,
    pub label: Option<String>,
    pub key_value: String,
    pub created_at: String,
}

impl ApiKeyRecord {
    fn from_row(row: &Row<'_>) -> rusqlite::Result<Self> {
        Ok(Self {
            id: row.get("id")?,
            card_id: row.get("card_id")?,
            label: row.get("label")?,
            key_value: row.get("key_value")?,
            created_at: row.get("created_at")?,
        })
    }
}

fn random_hex(num_bytes: usize) -> String {
    let mut bytes = vec![0u8; num_bytes];
    rand::rng().fill_bytes(&mut bytes);
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn random_urlsafe_token(num_bytes: usize) -> String {
    use base64::Engine;
    let mut bytes = vec![0u8; num_bytes];
    rand::rng().fill_bytes(&mut bytes);
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

pub fn add_api_key(card_id: &str, label: Option<&str>) -> rusqlite::Result<ApiKeyRecord> {
    let key_id = random_hex(8);
    let key_value = format!("lmm-{}", random_urlsafe_token(32));
    let now = now_iso();
    let conn = connect()?;
    conn.execute(
        "INSERT INTO api_keys (id, card_id, label, key_value, created_at) VALUES (?1, ?2, ?3, ?4, ?5)",
        params![key_id, card_id, label, key_value, now],
    )?;
    Ok(ApiKeyRecord {
        id: key_id,
        card_id: Some(card_id.to_string()),
        label: label.map(|s| s.to_string()),
        key_value,
        created_at: now,
    })
}

/// 这个模型专属的 key，加上改造前生成的、没绑定具体模型的老 key。
pub fn list_api_keys_for_card(card_id: &str) -> rusqlite::Result<Vec<ApiKeyRecord>> {
    let conn = connect()?;
    let mut stmt = conn.prepare(
        "SELECT * FROM api_keys WHERE card_id = ?1 OR card_id IS NULL ORDER BY created_at DESC",
    )?;
    let rows = stmt
        .query_map(params![card_id], ApiKeyRecord::from_row)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows)
}

pub fn delete_api_key(key_id: &str) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute("DELETE FROM api_keys WHERE id = ?1", params![key_id])?;
    Ok(())
}

pub fn find_api_key(key_value: &str) -> rusqlite::Result<Option<ApiKeyRecord>> {
    if key_value.is_empty() {
        return Ok(None);
    }
    let conn = connect()?;
    conn.query_row(
        "SELECT * FROM api_keys WHERE key_value = ?1",
        params![key_value],
        ApiKeyRecord::from_row,
    )
    .optional()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// db 测试用环境变量（HOME）切换各自的临时数据库目录，但环境
    /// 变量是进程全局的，而 Rust 测试默认同进程多线程并行——两个 db 测试并发
    /// 时会互相把对方的目录切走（曾真实撞出过 "no such table" 的 flaky 失败：
    /// migration 测试刚 DROP 掉自己库里的表，lifecycle 测试的连接却被切到了
    /// 同一个库上）。用 config::ENV_TEST_LOCK 这把**跨模块共享**的锁串行化：
    /// 别的模块（orphans）也会改同一组环境变量，各锁各的等于没锁。
    /// 拿锁 + 建独立临时目录。返回的守卫必须活到测试结束——它一析构就会
    /// 还原 HOME 并放锁。
    fn setup_temp_db() -> config::TempHome {
        // TempHome 自带那把跨模块的锁，并在析构时把 HOME 还原回去。
        let home = config::TempHome::new();
        init_db().unwrap();
        home
    }

    #[test]
    fn engine_lifecycle() {
        let _ctx = setup_temp_db();
        assert!(!is_engine_installed("ollama", Some("0.5.0")).unwrap());
        mark_engine_installed(
            "ollama",
            Some("0.5.0"),
            Some("ollama/ollama:latest"),
            Some("abc123"),
        )
        .unwrap();
        assert!(is_engine_installed("ollama", Some("0.5.0")).unwrap());
        let engines = list_installed_engines().unwrap();
        assert_eq!(engines.len(), 1);
        assert_eq!(
            engines[0].docker_image.as_deref(),
            Some("ollama/ollama:latest")
        );
        // install_engine 的版本哈希要能存下来——它决定了脚本更新后要不要
        // 重跑一遍安装以刷新部署包
        assert_eq!(engines[0].install_script_sha256.as_deref(), Some("abc123"));
        delete_installed_engine("ollama", Some("0.5.0")).unwrap();
        assert!(!is_engine_installed("ollama", Some("0.5.0")).unwrap());
    }

    /// 同名不同版本必须是两条独立记录。
    ///
    /// 真机上撞过：ornith15 用 vllm v0.27.1-aarch64，双机卡用 vllm dspark-0.1.1，
    /// 后装的把前者整行覆盖了——界面上一个引擎显示"被两张卡使用"，而 docker_image
    /// 只剩后者的，点删除会 rmi 掉另一张卡还在用的镜像。
    #[test]
    fn same_engine_name_different_versions_are_separate_rows() {
        let _ctx = setup_temp_db();
        mark_engine_installed(
            "vllm",
            Some("v0.27.1-aarch64"),
            Some("vllm/vllm-openai:v0.27.1-aarch64"),
            None,
        )
        .unwrap();
        mark_engine_installed(
            "vllm",
            Some("dspark-0.1.1"),
            Some("megacube/deepseek-direct:0.1.1"),
            None,
        )
        .unwrap();

        let engines = list_installed_engines().unwrap();
        assert_eq!(engines.len(), 2, "同名不同版本被合成了一条：{engines:?}");

        // 各自的镜像不能串
        assert_eq!(
            get_installed_engine("vllm", Some("v0.27.1-aarch64"))
                .unwrap()
                .unwrap()
                .docker_image
                .as_deref(),
            Some("vllm/vllm-openai:v0.27.1-aarch64")
        );
        assert_eq!(
            get_installed_engine("vllm", Some("dspark-0.1.1"))
                .unwrap()
                .unwrap()
                .docker_image
                .as_deref(),
            Some("megacube/deepseek-direct:0.1.1")
        );

        // 删掉一个不能带走另一个
        delete_installed_engine("vllm", Some("dspark-0.1.1")).unwrap();
        assert!(
            is_engine_installed("vllm", Some("v0.27.1-aarch64")).unwrap(),
            "删一个版本把另一个版本也删了"
        );
        assert!(!is_engine_installed("vllm", Some("dspark-0.1.1")).unwrap());
    }

    /// 老库（主键只有 engine_name）能升级到复合主键，数据不丢。
    #[test]
    fn legacy_engine_table_migrates_to_composite_key() {
        let _ctx = setup_temp_db();
        let conn = connect().unwrap();
        // 造一张老结构的表
        conn.execute_batch(
            "DROP TABLE installed_engines;
             CREATE TABLE installed_engines (
                 engine_name TEXT PRIMARY KEY,
                 engine_version TEXT,
                 installed_at TEXT NOT NULL,
                 docker_image TEXT,
                 install_script_sha256 TEXT
             );
             INSERT INTO installed_engines VALUES
                 ('sglang', 'v0.5.16', '2026-01-01T00:00:00Z', 'lmsysorg/sglang:v0.5.16', 'h1'),
                 ('diffusers', NULL, '2026-01-02T00:00:00Z', 'z-image:latest', NULL);",
        )
        .unwrap();
        drop(conn);

        init_db().unwrap();

        let engines = list_installed_engines().unwrap();
        assert_eq!(engines.len(), 2, "迁移把数据弄丢了：{engines:?}");
        assert!(is_engine_installed("sglang", Some("v0.5.16")).unwrap());
        // 老记录里 engine_version 是 NULL，迁移后应当变成空串而不是丢掉
        assert!(is_engine_installed("diffusers", None).unwrap());
        // 迁移完必须是新结构，不能每次启动都重来一遍
        assert!(is_engine_installed("sglang", Some("v0.5.16")).unwrap());
    }

    /// 内置（测试商店）卡片的固化脚本要跟着应用升级刷新；真实商店的不能碰。
    ///
    /// 这是真机上撞过的坑：推了新版应用、脚本改动一个都没生效，卡片还在跑下载
    /// 那天固化的旧脚本，报错完全看不出是这个原因——查了半天才发现落地的
    /// common.sh 是 6425 字节而应用里那份是 19405 字节。
    #[test]
    fn only_builtin_cards_get_their_scripts_refreshed() {
        let _ctx = setup_temp_db();
        let blob = |c: &str| ScriptBlob {
            content: c.to_string(),
            sha256: c.to_string(),
        };
        let old_pkg = std::collections::BTreeMap::from([("deploy".to_string(), blob("旧脚本"))]);
        let new_pkg = std::collections::BTreeMap::from([("deploy".to_string(), blob("新脚本"))]);
        let empty = std::collections::BTreeMap::new();

        for (card_id, source) in [("builtin-card", "builtin"), ("store-card", "store")] {
            let scripts = std::collections::BTreeMap::from([(
                "deploy".to_string(),
                format!("/tmp/{card_id}/deploy.sh"),
            )]);
            add_store_model(AddStoreModelParams {
                card_id,
                model_name: "m",
                card_name: "M",
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
                card_source: source,
            })
            .unwrap();
            set_scripts_content(card_id, &old_pkg, &empty).unwrap();
        }

        assert!(
            refresh_builtin_card_scripts("builtin-card", &new_pkg, &empty).unwrap(),
            "内置卡片没被刷新"
        );
        assert!(
            !refresh_builtin_card_scripts("store-card", &new_pkg, &empty).unwrap(),
            "真实商店的卡片不该被内置副本覆盖"
        );

        let builtin = get_store_model("builtin-card").unwrap().unwrap();
        assert!(
            builtin
                .scripts_content
                .get("deploy")
                .unwrap()
                .content
                .contains("新脚本")
        );
        let store = get_store_model("store-card").unwrap().unwrap();
        assert!(
            store
                .scripts_content
                .get("deploy")
                .unwrap()
                .content
                .contains("旧脚本")
        );

        // 内容没变时不该白写一遍（会把 scripts_fetched_at 刷掉、界面上凭空出现"更新过"）
        assert!(
            !refresh_builtin_card_scripts("builtin-card", &new_pkg, &empty).unwrap(),
            "内容没变还报告刷新过"
        );
    }

    /// 存量记录（card_source 为 NULL）按脚本地址形态回填，而不是按卡片 id 猜。
    ///
    /// 真实商店里存在跟内置测试卡同名的卡片（ornith15-bf16 两边都有），按 id 猜
    /// 会把用户从真实商店下的卡片标成 builtin，然后用内置副本覆盖掉它的脚本。
    #[test]
    fn legacy_records_get_their_source_backfilled_by_script_location() {
        let _ctx = setup_temp_db();
        let conn = connect().unwrap();
        conn.execute_batch(
            "INSERT INTO store_models
                 (card_id, model_name, card_name, engine_name, status, downloaded_at, script_deploy)
             VALUES
                 ('same-id-from-test', 'm', 'M', 'e', 'downloaded', '2026-01-01',
                  '/home/sky/.local/share/local-model-manager/test_store_scripts/x/deploy.sh'),
                 ('same-id-from-store', 'm', 'M', 'e', 'downloaded', '2026-01-01',
                  'http://47.120.8.237/model-store/api/v1/cards/x/deploy.sh');",
        )
        .unwrap();
        drop(conn);

        init_db().unwrap();

        let conn = connect().unwrap();
        let src = |id: &str| -> String {
            conn.query_row(
                "SELECT card_source FROM store_models WHERE card_id = ?1",
                params![id],
                |r| r.get(0),
            )
            .unwrap()
        };
        assert_eq!(src("same-id-from-test"), "builtin");
        assert_eq!(src("same-id-from-store"), "store");
    }

    #[test]
    fn store_model_lifecycle() {
        let _ctx = setup_temp_db();
        let scripts = std::collections::BTreeMap::from([(
            "deploy".to_string(),
            "https://example.com/deploy.sh".to_string(),
        )]);
        let rec = add_store_model(AddStoreModelParams {
            card_id: "card-1",
            model_name: "qwen38",
            card_name: "Qwen3.8",
            org: Some("Qwen"),
            description: None,
            engine_name: "sglang",
            engine_version: None,
            size_bytes: 1024,
            quantization: Some("nvfp4"),
            source_repo_id: None,
            source_filename: None,
            scripts: &scripts,
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::default(),
            card_source: "store",
        })
        .unwrap();
        assert_eq!(rec.status, "downloaded");
        assert_eq!(
            rec.scripts.get("deploy").unwrap().as_deref(),
            Some("https://example.com/deploy.sh")
        );

        update_store_model_status("card-1", "deployed", Some("deadbeef")).unwrap();
        let fetched = get_store_model("card-1").unwrap().unwrap();
        assert_eq!(fetched.status, "deployed");
        assert!(fetched.deployed_at.is_some());

        set_running("card-1", true).unwrap();
        assert!(get_store_model("card-1").unwrap().unwrap().running);

        let key = add_api_key("card-1", Some("test key")).unwrap();
        assert!(key.key_value.starts_with("lmm-"));
        assert_eq!(list_api_keys_for_card("card-1").unwrap().len(), 1);
        assert!(find_api_key(&key.key_value).unwrap().is_some());

        delete_store_model("card-1").unwrap();
        assert!(get_store_model("card-1").unwrap().is_none());
        assert!(list_api_keys_for_card("card-1").unwrap().is_empty());
    }

    #[test]
    fn migration_adds_missing_columns() {
        let _ctx = setup_temp_db();
        let conn = connect().unwrap();
        // 模拟老版本表（没有 profiles_json 这些后加的列）
        conn.execute_batch("DROP TABLE store_models").unwrap();
        conn.execute_batch(
            "CREATE TABLE store_models (
                card_id TEXT PRIMARY KEY, model_name TEXT NOT NULL, card_name TEXT NOT NULL,
                org TEXT, engine_name TEXT NOT NULL, engine_version TEXT,
                size_bytes INTEGER NOT NULL DEFAULT 0, quantization TEXT,
                source_repo_id TEXT, source_filename TEXT, status TEXT NOT NULL DEFAULT 'downloaded',
                running INTEGER NOT NULL DEFAULT 0, script_install_engine TEXT, script_deploy TEXT,
                script_download_model TEXT, script_enable TEXT, script_stop TEXT, script_uninstall TEXT,
                downloaded_at TEXT NOT NULL, deployed_at TEXT
            )",
        )
        .unwrap();
        migrate(&conn).unwrap();
        let cols: std::collections::HashSet<String> = conn
            .prepare("PRAGMA table_info(store_models)")
            .unwrap()
            .query_map([], |r| r.get::<_, String>(1))
            .unwrap()
            .filter_map(|r| r.ok())
            .collect();
        assert!(cols.contains("profiles_json"));
        assert!(cols.contains("active_profile_id"));
        assert!(cols.contains("endpoint_url"));
    }
}
