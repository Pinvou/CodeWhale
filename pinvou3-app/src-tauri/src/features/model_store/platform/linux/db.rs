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

use std::fs;
use std::os::unix::fs::{OpenOptionsExt as _, PermissionsExt as _};
use std::path::{Path, PathBuf};

use rusqlite::{Connection, OptionalExtension, Row, params};

use super::config;
use super::store_client::{DeployProfile, DeployTopology, ResourceRequirements};

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
    -- 原生厂商引擎的独立清理契约（正文 + SHA-256）。不能靠猜目录删除：用户
    -- 删除模型后，唯一安全的清理依据必须跟随已安装引擎本身持久化下来。
    cleanup_script_json TEXT,
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
    -- 最后一次由卡片下载脚本确认“权重已完整”的时间。历史记录没有这一列/值时，
    -- 不能把数据库里的 downloaded 当成磁盘事实，界面应先引导“继续下载/检查”。
    weights_ready_at TEXT,
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
    assets_content_json TEXT,
    -- 模型卡片实测的运行内存档位。缺失不代表 0，而是尚不可可靠预测。
    resource_requirements_json TEXT
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
            // 下载脚本成功返回才写入。存量记录保持 NULL，让它们在下一次使用前
            // 重新经卡片脚本确认，避免“磁盘已删、界面仍说可部署”。
            ("weights_ready_at", "TEXT"),
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
            // 这里原来有一列 `card_source`（"builtin" = 内置测试商店 / "store" =
            // 真实商店）。2026-09-30 内置测试商店整体拿掉之后，卡片只可能来自生产
            // 商店，这一列没有任何读者，连同 `refresh_builtin_card_scripts` 和回填
            // 一起删了。存量设备上的这一列会留在表里不再被写——`ensure_columns` 只
            // 补列不删列，而 INSERT 显式列名、列本身可空，留着不影响任何语句。
            // enable 脚本报上来的容器名（LMM_CONTAINER_NAME 标记行）。
            // 这是判断"某个容器是不是本应用起的"唯一可靠的依据——镜像不是身份，
            // 别人用同一个镜像起的容器跟我们的长得一模一样。
            ("container_name", "TEXT"),
            ("resource_requirements_json", "TEXT"),
        ],
    ),
    ("api_keys", &[("card_id", "TEXT")]),
    (
        "installed_engines",
        &[
            ("docker_image", "TEXT"),
            ("install_script_sha256", "TEXT"),
            ("cleanup_script_json", "TEXT"),
        ],
    ),
];

pub fn db_path() -> PathBuf {
    config::data_dir().join("models.db")
}

/// 每次用到库都新开一个连接。
///
/// `busy_timeout` 不是可选项：设计上允许同时下三个模型，加上界面轮询也在读写，
/// sqlite 默认的 0 意味着**只要撞上一次并发写就立刻 SQLITE_BUSY**。真出事的样子
/// 很难看——几个小时的下载跑到最后一步 `add_store_model` 撞上一个毫秒级的并发写，
/// 整个任务被报成失败，而权重其实已经下完了。5 秒足够任何一次本地写让路。
///
/// `journal_mode=WAL` 是上面那句的配套，不是锦上添花。默认的回滚日志模式下
/// **写阻塞读、读阻塞写**：三个下载任务在写进度，界面每秒轮询在读列表，两边互相
/// 挡。只设 busy_timeout 并没有解决争用，只是把"立刻报 SQLITE_BUSY"换成"最长卡
/// 5 秒"——用户看到的是列表隔一会儿顿一下。WAL 下读不再被写挡住，这才是"本地
/// 单文件库 + 进程内多连接"这个并发形态该配的模式。
///
/// 三个注意点：
/// - WAL 是**写进库文件头的持久属性**，设一次就一直生效；每个连接再设一遍是
///   无害的空操作，写在这里只是保证任何一条打开路径都不会漏掉。
/// - 必须走 `execute_batch`：`PRAGMA journal_mode` 会**返回一行**（生效后的模式名），
///   用 `execute()` 会直接报 "Execute returned results"。
/// - 切不过去（库落在不支持共享内存的网络盘上等）时 sqlite 不报错，只是留在
///   delete 模式——那正好是今天的行为，不会比现在更糟，所以这里不额外校验。
///
/// WAL 会在库文件旁多出 `models.db-wal` / `models.db-shm` 两个文件。已确认全仓
/// 没有任何地方按 `models.db` 这个确切文件名去删除、备份或导出（`db_path()`
/// 只有本文件在用），所以不存在"删掉主库、留下两个残片"的清理漏网。
fn connect() -> rusqlite::Result<Connection> {
    // 开库之前先把库文件的权限收好，库里存着明文网关密钥（见 ensure_db_file_private）。
    ensure_db_file_private();
    let conn = Connection::open(db_path())?;
    // 顺序有讲究：busy_timeout 要先设。切 journal_mode 需要短暂拿排他锁，
    // 万一此刻另一个连接正在写，没有 busy_timeout 兜着就会当场失败。
    conn.busy_timeout(std::time::Duration::from_secs(5))?;
    conn.execute_batch("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;")?;
    // 上一句会把 -wal / -shm 建出来，密钥的最新一次写入就落在 -wal 里。
    tighten_wal_sidecars();
    Ok(conn)
}

/// 库文件的目标权限：只有属主读写。
const DB_FILE_MODE: u32 = 0o600;

/// 保证 `models.db` 不对同机其他用户可读。
///
/// # 为什么库文件要单独管权限
///
/// `api_keys.key_value` 存的是**明文**的网关密钥——局域网里其他应用就是拿它
/// 调本机模型的。而 SQLite 建库走的是默认 umask，真机上落成 0644：同机任何一个
/// 登录用户都能直接 `sqlite3 models.db` 读走 `lmm-` 开头的 key。同模块 config.rs
/// 对 `config.json` 和 HF token 回退文件都是按「同机其他用户不可读」建的
/// （`OpenOptions::mode(0o600)`），装着密钥的库文件没有理由反而更松。
///
/// # 两种情况都要管
///
/// - **库还不存在**：用 `OpenOptions::mode(0o600)` 先把零长度文件建出来
///   （零长度文件就是一个合法的空库，SQLite 照常在上面建表），跟 `save_settings`
///   同款——不留「先按 umask 落成 0644、再 chmod」的那个窗口。这里**不能带
///   `truncate`**：库已经存在时这一句必须只是打开再关上，不碰一个字节。
/// - **库已经存在**：`mode()` 只在创建的那一刻生效，对存量设备（含从 Python 版
///   升级过来、一直是 0644 的那些）不起作用，得补一次 `set_permissions`。
///
/// 失败只记日志不中断：真建不出来的话，紧接着的 `Connection::open` 会给出更
/// 具体的错误；而 chmod 失败（挂载选项不支持等）不该让整个模型商店打不开。
fn ensure_db_file_private() {
    let db = db_path();
    if let Err(e) = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .mode(DB_FILE_MODE)
        .open(&db)
    {
        eprintln!("[model-store] 预建 models.db 失败：{e}");
        return;
    }
    tighten_to_owner_only(&db);
}

/// 把 WAL 的两个边车文件（`-wal` / `-shm`）一并收到 0600。
///
/// 正常情况下这是空操作：SQLite 建 WAL 和 shm 时会**照抄主库文件的权限位**，
/// 主库是 0600 它们就是 0600。要防的是错开的时序——上一个进程被强杀时 `-wal`
/// 不会被删掉，于是设备上可能留着一个在主库还是 0644 的年代建出来的 `-wal`，
/// 而**最近写进去的密钥恰恰在里面**。只在权限确实更松时才写，正常路径上就是
/// 三次 stat。
fn tighten_wal_sidecars() {
    let db = db_path();
    for suffix in ["-wal", "-shm"] {
        let mut name = db.clone().into_os_string();
        name.push(suffix);
        tighten_to_owner_only(Path::new(&name));
    }
}

/// 文件存在且组/其他用户还读得到时，收成 0600。文件不在就什么也不做。
fn tighten_to_owner_only(path: &Path) {
    let Ok(meta) = fs::metadata(path) else { return };
    if meta.permissions().mode() & 0o077 == 0 {
        return;
    }
    if let Err(e) = fs::set_permissions(path, fs::Permissions::from_mode(DB_FILE_MODE)) {
        eprintln!("[model-store] 收紧 {} 的权限失败：{e}", path.display());
    }
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
             (engine_name, engine_version, docker_image, install_script_sha256, cleanup_script_json, installed_at)
         SELECT engine_name, COALESCE(engine_version, ''), docker_image,
                install_script_sha256, cleanup_script_json, installed_at
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

pub fn init_db() -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute_batch(SCHEMA)?;
    // card_script_backup 是「回退上次脚本」功能的快照表，该功能已随界面入口移除
    // （2026-10-19）：更新永远向前取商店当前版本。新库不再建表；存量库里的表和
    // 陈旧快照在下次成功更新时由 replace_card_package 顺带清掉。
    migrate(&conn)?;
    // 顺序要紧：先把缺的列补齐（老表可能还没有 docker_image），再重建主键，
    // 否则搬数据那句 SELECT 会因为列不存在而失败。
    migrate_engine_primary_key(&conn)?;
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
    /// 卡片随引擎下发的、已经校验并固化的独立清理脚本。没有它就不能对原生
    /// 引擎猜路径删除，即使当前磁盘上看得到目录也不行。
    pub cleanup_script: Option<ScriptBlob>,
    pub installed_at: String,
}

impl InstalledEngineRecord {
    fn from_row(row: &Row<'_>) -> rusqlite::Result<Self> {
        Ok(Self {
            engine_name: row.get("engine_name")?,
            engine_version: row.get("engine_version")?,
            docker_image: row.get("docker_image")?,
            install_script_sha256: row.get("install_script_sha256")?,
            cleanup_script: row
                .get::<_, Option<String>>("cleanup_script_json")?
                .and_then(|raw| serde_json::from_str(&raw).ok()),
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
    cleanup_script: Option<&ScriptBlob>,
) -> rusqlite::Result<()> {
    let conn = connect()?;
    let cleanup_script_json = cleanup_script.and_then(|script| serde_json::to_string(script).ok());
    conn.execute(
        "INSERT INTO installed_engines
             (engine_name, engine_version, docker_image, install_script_sha256,
              cleanup_script_json, installed_at)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6)
         ON CONFLICT(engine_name, engine_version) DO UPDATE SET
             docker_image=COALESCE(excluded.docker_image, installed_engines.docker_image),
             install_script_sha256=COALESCE(excluded.install_script_sha256,
                                            installed_engines.install_script_sha256),
             cleanup_script_json=COALESCE(excluded.cleanup_script_json,
                                          installed_engines.cleanup_script_json),
             installed_at=excluded.installed_at",
        params![
            engine_name,
            engine_key(engine_version),
            docker_image,
            install_script_sha256,
            cleanup_script_json,
            now_iso()
        ],
    )?;
    Ok(())
}

/// 给已存在的引擎补一份独立清理契约。
///
/// 这用于升级后的存量设备：它们已经装好了引擎和模型权重，不能为了多一份几十行
/// 的清理脚本要求重新下载几十 GB 权重。调用方必须先从商店按哈希取回并校验脚本；
/// 这里仅把经过验证的正文固化到该引擎记录。
pub fn set_engine_cleanup_script(
    engine_name: &str,
    engine_version: Option<&str>,
    cleanup_script: &ScriptBlob,
) -> rusqlite::Result<bool> {
    let conn = connect()?;
    let raw = serde_json::to_string(cleanup_script)
        .map_err(|error| rusqlite::Error::ToSqlConversionFailure(Box::new(error)))?;
    let changed = conn.execute(
        "UPDATE installed_engines
            SET cleanup_script_json = ?1
          WHERE engine_name = ?2 AND engine_version = ?3
            AND (cleanup_script_json IS NULL OR cleanup_script_json <> ?1)",
        params![raw, engine_name, engine_key(engine_version)],
    )?;
    Ok(changed > 0)
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

/// 将一张已下载卡片曾经使用的引擎身份迁到新的规范身份。
///
/// 引擎的展示名称曾经被错误地写进身份键，随后又出现了同一套原生二进制在
/// “驭光” / “驭光（H3C自研推理引擎）” 两个键下各有一条记录的历史数据。直接
/// 改卡片字段会让老设备把已装的 300MB 引擎看成另一个引擎，既可能重复下载，也
/// 会把真正仍被模型使用的目录误显示成孤儿。
///
/// 这条迁移只搬**同一张卡**当前引用的引擎记录；若仍有其他旧卡引用旧键，就暂
/// 留那条记录，等它们各自完成迁移，绝不抢先删掉共享资源。
pub fn migrate_engine_identity(
    card_id: &str,
    canonical_name: &str,
    canonical_version: Option<&str>,
) -> rusqlite::Result<bool> {
    let mut conn = connect()?;
    let tx = conn.transaction()?;
    let current: Option<(String, Option<String>)> = tx
        .query_row(
            "SELECT engine_name, engine_version FROM store_models WHERE card_id = ?1",
            params![card_id],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .optional()?;
    let Some((old_name, old_version)) = current else {
        return Ok(false);
    };
    let old_key = engine_key(old_version.as_deref());
    let canonical_key = engine_key(canonical_version);
    if old_name == canonical_name && old_key == canonical_key {
        return Ok(false);
    }

    // 早期 Ornith 卡片曾写成「模型记录：驭光（H3C…）+ 空版本；已装引擎：驭光
    // + 二进制版本」。先按模型自身的旧键找；找不到时才认这一个已知的历史展示名
    // `驭光`，并且版本必须恰好等于当前已校验的厂商二进制版本。不能按目录或任意
    // 同版本记录猜，不然可能把别家的资源错误并入原生引擎。
    let exact_engine: Option<(
        String,
        String,
        Option<String>,
        Option<String>,
        Option<String>,
        String,
    )> = tx
        .query_row(
            "SELECT engine_name, engine_version, docker_image, install_script_sha256,
                    cleanup_script_json, installed_at
               FROM installed_engines
              WHERE engine_name = ?1 AND engine_version = ?2",
            params![old_name, old_key],
            |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                ))
            },
        )
        .optional()?;
    let legacy_engine = if exact_engine.is_none() && canonical_key != old_key {
        tx.query_row(
            "SELECT engine_name, engine_version, docker_image, install_script_sha256,
                    cleanup_script_json, installed_at
               FROM installed_engines
              WHERE engine_name = '驭光' AND engine_version = ?1",
            params![canonical_key],
            |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                ))
            },
        )
        .optional()?
    } else {
        None
    };
    let source_engine = exact_engine.or(legacy_engine);
    if let Some((
        _source_name,
        _source_version,
        docker_image,
        install_script_sha256,
        cleanup_script_json,
        installed_at,
    )) = source_engine.as_ref()
    {
        // 已有规范记录时优先信任它；旧键只在规范记录缺字段时补值，避免一次
        // 名称迁移把较新的安装脚本哈希或镜像指针回滚成旧值。
        tx.execute(
            "INSERT INTO installed_engines
                 (engine_name, engine_version, docker_image, install_script_sha256,
                  cleanup_script_json, installed_at)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)
             ON CONFLICT(engine_name, engine_version) DO UPDATE SET
                 docker_image = COALESCE(installed_engines.docker_image, excluded.docker_image),
                 install_script_sha256 = COALESCE(
                     installed_engines.install_script_sha256,
                     excluded.install_script_sha256
                 ),
                 cleanup_script_json = COALESCE(
                     installed_engines.cleanup_script_json,
                     excluded.cleanup_script_json
                 )",
            params![
                canonical_name,
                canonical_key,
                docker_image,
                install_script_sha256,
                cleanup_script_json,
                installed_at,
            ],
        )?;
    }

    tx.execute(
        "UPDATE store_models
            SET engine_name = ?1, engine_version = ?2
          WHERE card_id = ?3",
        params![canonical_name, canonical_key, card_id],
    )?;

    let (source_name, source_version) = source_engine
        .as_ref()
        .map(|(name, version, ..)| (name.as_str(), version.as_str()))
        .unwrap_or((&old_name, old_key.as_str()));
    let still_referenced: Option<i64> = tx
        .query_row(
            "SELECT 1 FROM store_models
              WHERE engine_name = ?1
                AND COALESCE(engine_version, '') = ?2
                AND card_id <> ?3
              LIMIT 1",
            params![source_name, source_version, card_id],
            |row| row.get(0),
        )
        .optional()?;
    if still_referenced.is_none() {
        tx.execute(
            "DELETE FROM installed_engines WHERE engine_name = ?1 AND engine_version = ?2",
            params![source_name, source_version],
        )?;
    }
    tx.commit()?;
    Ok(true)
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
    /// 最近一次由卡片下载脚本确认权重完整的时间。None 代表旧记录或已发现
    /// 权重不完整；它们必须走“继续下载”，不能直接给“部署”按钮。
    pub weights_ready_at: Option<String>,
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
    /// 卡片下发的运行内存需求。空结构表示当前卡片尚未提供可信预算。
    pub resource_requirements: ResourceRequirements,
}

/// 一份固化下来的脚本：正文 + 它的 sha256（来源哈希，用于"检查卡片更新"时
/// 跟商店当前的哈希比对，判断有没有新版本）。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
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
            weights_ready_at: row.get("weights_ready_at")?,
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
            resource_requirements: row
                .get::<_, Option<String>>("resource_requirements_json")?
                .and_then(|raw| serde_json::from_str(&raw).ok())
                .unwrap_or_default(),
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

/// Replace the complete cached package in a single SQLite transaction. No
/// weights, engine files or runtime state change. The legacy rollback
/// snapshot (`card_script_backup`) is dropped, not refreshed: the rollback
/// entry point was removed with its UI, and a stale snapshot would only
/// suggest a capability that no longer exists.
pub fn replace_card_package(
    expected: &StoreModelRecord,
    scripts: &std::collections::BTreeMap<String, ScriptBlob>,
    assets: &std::collections::BTreeMap<String, ScriptBlob>,
) -> Result<bool, String> {
    let mut conn = connect().map_err(|e| e.to_string())?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|e| e.to_string())?;
    let current = tx
        .query_row(
            "SELECT * FROM store_models WHERE card_id = ?1",
            [&expected.card_id],
            StoreModelRecord::from_row,
        )
        .map_err(|e| format!("读取卡片失败，原版本未修改：{e}"))?;
    if current.running
        || serde_json::to_value(&current).map_err(|e| e.to_string())?
            != serde_json::to_value(expected).map_err(|e| e.to_string())?
    {
        return Err("卡片状态在更新期间发生变化，已保留原版本。请刷新后重试。".into());
    }
    if &current.scripts_content == scripts && &current.assets_content == assets {
        return Ok(false);
    }
    let encode = |value: &std::collections::BTreeMap<String, ScriptBlob>| {
        serde_json::to_string(value).map_err(|e| e.to_string())
    };
    // 存量库可能还挂着「回退」时代的快照行；表不在则跳过（新库不建这张表）。
    let has_backup_table = tx
        .query_row(
            "SELECT EXISTS(SELECT 1 FROM sqlite_master \
             WHERE type='table' AND name='card_script_backup')",
            [],
            |row| row.get::<_, i64>(0),
        )
        .map(|found| found != 0)
        .unwrap_or(false);
    if has_backup_table {
        tx.execute(
            "DELETE FROM card_script_backup WHERE card_id=?1",
            [&expected.card_id],
        )
        .map_err(|e| e.to_string())?;
    }
    // Any asset/entry can affect deployment; comparing only deploy.sh misses
    // common.sh fixes. Empty fingerprint explicitly requires a redeploy.
    tx.execute(
        "UPDATE store_models SET scripts_content_json=?1, assets_content_json=?2,
        scripts_fetched_at=?3, deployed_script_sha256='' WHERE card_id=?4",
        params![
            encode(scripts)?,
            encode(assets)?,
            now_iso(),
            expected.card_id
        ],
    )
    .map_err(|e| e.to_string())?;
    tx.commit()
        .map_err(|e| format!("保存更新失败，已保留原版本：{e}"))?;
    Ok(true)
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
              downloaded_at, weights_ready_at, deployed_at, profiles_json, deploy_topology)
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
              weights_ready_at=excluded.weights_ready_at,
              deployed_at=COALESCE(excluded.deployed_at, store_models.deployed_at),
             profiles_json=excluded.profiles_json,
             deploy_topology=excluded.deploy_topology",
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
            &now,
            // 调用方只会在 download_model 成功返回后走到这里；“成功”是卡片
            // 对完整性（文件存在/大小/摘要）的最终声明。把凭据落库，之后才可部署。
            &now,
            deployed_at,
            profiles_json,
            // 存成 "single" / "dual"，跟 DeployTopology 的 serde 表示一致
            serde_json::to_value(p.deploy_topology)
                .ok()
                .and_then(|v| v.as_str().map(str::to_string)),
        ],
    )?;
    conn.query_row(
        "SELECT * FROM store_models WHERE card_id = ?1",
        params![p.card_id],
        StoreModelRecord::from_row,
    )
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
fn clear_deployment_state(card_id: &str) -> rusqlite::Result<()> {
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

pub fn mark_uninstalled(card_id: &str) -> rusqlite::Result<()> {
    clear_deployment_state(card_id)
}

/// 启用时发现部署配置已不在，就撤销数据库里过期的“已部署”声明。
/// 权重和引擎保持不动，用户重新部署即可恢复可用状态。
pub fn mark_deployment_missing(card_id: &str) -> rusqlite::Result<()> {
    clear_deployment_state(card_id)
}

/// 下载成功后把当时卡片下发的运行内存档位固化到本地。
///
/// 启用必须在离线时也能工作，不能每次为了一个内存提示重新请求商店；同时卡片以后
/// 更新档位也不会悄悄改变已下载模型的行为，用户重新下载/更新卡片后才会刷新。
pub fn set_resource_requirements(
    card_id: &str,
    requirements: &ResourceRequirements,
) -> rusqlite::Result<()> {
    let json = serde_json::to_string(requirements).unwrap_or_default();
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models SET resource_requirements_json = ?1 WHERE card_id = ?2",
        params![json, card_id],
    )?;
    Ok(())
}

/// 部署时发现权重缺失或不完整，撤销“已经下载好”的过期声明。
///
/// 这不是删除：卡片信息、脚本副本、模型目录和任何可续传的分片都必须原封不动。
/// 下一步应当是“继续下载”，而不是再次让用户点部署后才看见同一个失败。
pub fn mark_weights_incomplete(card_id: &str) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models
            SET status = 'incomplete',
                weights_ready_at = NULL,
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

/// 用户明确选择“删除模型数据”但卡片脚本没有删干净时，保留一条可继续处理的
/// 记录，而不是把模型从列表里抹掉。
///
/// 这不是“下载不完整”：这里的模型不能部署，也不应该被“继续下载”掩盖。它的
/// 下一步只能是再次清理、打开文件位置，或者由用户明确选择“仅从列表移除”。
/// 运行/部署痕迹同样必须一并撤销，避免网关继续把请求打到已被卸载的服务。
pub fn mark_cleanup_required(card_id: &str) -> rusqlite::Result<()> {
    let conn = connect()?;
    conn.execute(
        "UPDATE store_models
            SET status = 'cleanup_required',
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

/// 删掉一个模型的库记录，连同它专属的 API Key。
///
/// **两条 DELETE 必须在同一个事务里。** 分开执行时，第二条失败会留下一个
/// 「模型没了、key 还在」的中间态——而那些 key 仍然能被 `find_api_key` 命中，
/// 跟下面那句注释说的「模型卸载了 key 也跟着失效」正好相反。这种状态没有任何
/// 界面能看见，也没有任何入口能清掉。
pub fn delete_store_model(card_id: &str) -> rusqlite::Result<()> {
    let mut conn = connect()?;
    let tx = conn.transaction()?;
    tx.execute(
        "DELETE FROM store_models WHERE card_id = ?1",
        params![card_id],
    )?;
    // 模型卸载了，专属它的 key 也跟着失效，不留着占地方
    tx.execute("DELETE FROM api_keys WHERE card_id = ?1", params![card_id])?;
    tx.commit()
}

pub fn total_managed_bytes() -> rusqlite::Result<i64> {
    let conn = connect()?;
    conn.query_row(
        "SELECT COALESCE(SUM(size_bytes), 0) FROM store_models",
        [],
        |r| r.get(0),
    )
}

// Keep the public db API stable while testing its actual key SQL without Tauri.
#[path = "api_keys.rs"]
mod api_keys;
pub use api_keys::{add_api_key, delete_api_key, find_api_key, list_api_keys_for_card};

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

    /// 连接必须是 WAL 模式。回滚日志模式下写阻塞读，而这个库的常态就是
    /// "几个下载在写 + 界面轮询在读"（见 connect 的说明）。
    #[test]
    fn connections_are_opened_in_wal_mode() {
        let _ctx = setup_temp_db();
        let conn = connect().unwrap();
        let mode: String = conn
            .query_row("PRAGMA journal_mode", [], |row| row.get(0))
            .unwrap();
        assert_eq!(mode.to_ascii_lowercase(), "wal", "库没有跑在 WAL 模式上");
    }

    /// 库文件不能对同机其他用户可读：`api_keys.key_value` 是明文的网关密钥。
    /// WAL 的两个边车文件同样算数——最近一次写入就落在 `-wal` 里。
    #[test]
    fn db_files_are_not_readable_by_other_users() {
        let _ctx = setup_temp_db();

        // 模拟存量设备：库在这次改动之前就已经按默认 umask 落成 0644 了。
        fs::set_permissions(db_path(), fs::Permissions::from_mode(0o644)).unwrap();

        // 连接期间 -wal / -shm 才存在（最后一个连接关掉时 SQLite 会删掉它们），
        // 所以这条连接必须活到断言结束。
        let conn = connect().unwrap();
        // 让密钥真的写进 WAL，而不是只断言两个空文件。
        add_api_key("card-perm", Some("权限测试")).unwrap();

        // 三个文件都要查到：只断言主库的话，这条测试正好漏掉带着密钥的那个 -wal。
        for suffix in ["", "-wal", "-shm"] {
            let mut name = db_path().into_os_string();
            name.push(suffix);
            let path = PathBuf::from(name);
            let meta =
                fs::metadata(&path).unwrap_or_else(|e| panic!("{} 应该存在：{e}", path.display()));
            let mode = meta.permissions().mode() & 0o777;
            assert_eq!(
                mode & 0o077,
                0,
                "{} 的权限是 {mode:o}，同机其他用户读得到",
                path.display()
            );
        }
        drop(conn);
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
            None,
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

    /// 真机遗留的 Ornith 状态正是这个组合：模型记录使用了后来改过的展示名，却
    /// 丢了版本；实体引擎仍用早期的“驭光”展示名和真实二进制版本。迁移必须接
    /// 回这份实体记录，不能把它显示成孤儿、更不能要求用户重复下载。
    #[test]
    fn native_engine_identity_migration_reclaims_known_legacy_display_name() {
        let _ctx = setup_temp_db();
        let scripts = std::collections::BTreeMap::new();
        add_store_model(AddStoreModelParams {
            card_id: "ornith15-35b-a3b-nvfp4",
            model_name: "ornith-test",
            card_name: "Ornith test",
            org: None,
            description: None,
            engine_name: "驭光（H3C自研推理引擎）",
            engine_version: None,
            size_bytes: 1,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &scripts,
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::Single,
        })
        .unwrap();
        mark_engine_installed("驭光", Some("0f8a4a7c"), None, Some("legacy-script"), None).unwrap();

        assert!(
            migrate_engine_identity(
                "ornith15-35b-a3b-nvfp4",
                "驭光（H3C自研推理引擎）",
                Some("0f8a4a7c"),
            )
            .unwrap()
        );

        let model = get_store_model("ornith15-35b-a3b-nvfp4").unwrap().unwrap();
        assert_eq!(model.engine_name, "驭光（H3C自研推理引擎）");
        assert_eq!(model.engine_version.as_deref(), Some("0f8a4a7c"));
        assert_eq!(
            get_installed_engine("驭光（H3C自研推理引擎）", Some("0f8a4a7c"))
                .unwrap()
                .unwrap()
                .install_script_sha256
                .as_deref(),
            Some("legacy-script")
        );
        assert!(
            get_installed_engine("驭光", Some("0f8a4a7c"))
                .unwrap()
                .is_none(),
            "旧展示名没有移除，界面仍会把同一引擎显示两次"
        );
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
            None,
        )
        .unwrap();
        mark_engine_installed(
            "vllm",
            Some("dspark-0.1.1"),
            Some("megacube/deepseek-direct:0.1.1"),
            None,
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
        })
        .unwrap();
        assert_eq!(rec.status, "downloaded");
        assert_eq!(
            rec.scripts["deploy"].as_deref(),
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

    fn script_update_record() -> StoreModelRecord {
        add_store_model(AddStoreModelParams {
            card_id: "script-update-test",
            model_name: "model",
            card_name: "Model",
            org: None,
            description: None,
            engine_name: "engine",
            engine_version: None,
            size_bytes: 1234,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "deployed",
            profiles: &[],
            deploy_topology: DeployTopology::Single,
        })
        .unwrap()
    }

    fn test_package(content: &str) -> std::collections::BTreeMap<String, ScriptBlob> {
        std::collections::BTreeMap::from([(
            "deploy".into(),
            ScriptBlob {
                content: content.into(),
                sha256: super::super::script_runner::sha256_hex(content),
            },
        )])
    }

    fn backup_table_exists() -> bool {
        connect()
            .unwrap()
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM sqlite_master \
                 WHERE type='table' AND name='card_script_backup')",
                [],
                |row| row.get::<_, i64>(0),
            )
            .map(|found| found != 0)
            .unwrap_or(false)
    }

    /// 「回退上次脚本」移除后（2026-10-19）：更新只向前，新库不建
    /// card_script_backup，成功更新也不留快照；模型数据（大小/状态/权重时间戳）
    /// 照旧不被脚本更新触碰。
    #[test]
    fn package_update_preserves_model_data_and_leaves_no_rollback_snapshot() {
        let _ctx = setup_temp_db();
        let rec = script_update_record();
        let old = test_package("old");
        let new = test_package("new");
        let assets = std::collections::BTreeMap::new();
        set_scripts_content(&rec.card_id, &old, &assets).unwrap();
        let expected = get_store_model(&rec.card_id).unwrap().unwrap();
        assert!(replace_card_package(&expected, &new, &assets).unwrap());
        let updated = get_store_model(&rec.card_id).unwrap().unwrap();
        assert_eq!(updated.size_bytes, expected.size_bytes);
        assert_eq!(updated.status, expected.status);
        assert_eq!(updated.weights_ready_at, expected.weights_ready_at);
        assert_eq!(updated.deployed_script_sha256.as_deref(), Some(""));
        assert!(!backup_table_exists(), "新库不建回退快照表");
        assert!(!replace_card_package(&updated, &new, &assets).unwrap());
        delete_store_model(&rec.card_id).unwrap();
    }

    /// 存量库还挂着「回退」时代的快照表：下次成功更新要把它清掉，
    /// 别留一个指向已不存在能力的死快照。
    #[test]
    fn package_update_clears_legacy_rollback_snapshot() {
        let _ctx = setup_temp_db();
        let rec = script_update_record();
        let conn = connect().unwrap();
        conn.execute_batch(
            "CREATE TABLE card_script_backup (card_id TEXT PRIMARY KEY, \
             scripts_json TEXT NOT NULL, assets_json TEXT NOT NULL);",
        )
        .unwrap();
        conn.execute(
            "INSERT INTO card_script_backup VALUES (?1, '{}', '{}')",
            [&rec.card_id],
        )
        .unwrap();
        assert!(replace_card_package(&rec, &test_package("new"), &Default::default()).unwrap());
        let left: i64 = connect()
            .unwrap()
            .query_row("SELECT COUNT(*) FROM card_script_backup", [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(left, 0, "陈旧快照随更新清除");
    }

    #[test]
    fn package_update_rejects_concurrent_running_change() {
        let _ctx = setup_temp_db();
        let rec = script_update_record();
        set_running(&rec.card_id, true).unwrap();
        assert!(replace_card_package(&rec, &test_package("new"), &Default::default()).is_err());
        assert!(
            get_store_model(&rec.card_id)
                .unwrap()
                .unwrap()
                .scripts_content
                .is_empty()
        );
        assert!(!backup_table_exists());
    }

    #[test]
    fn failed_package_commit_leaves_original_unchanged() {
        let _ctx = setup_temp_db();
        let rec = script_update_record();
        connect().unwrap().execute_batch("CREATE TRIGGER reject_package BEFORE UPDATE OF scripts_content_json ON store_models
            BEGIN SELECT RAISE(ABORT, 'test write failure'); END;").unwrap();
        assert!(replace_card_package(&rec, &test_package("new"), &Default::default()).is_err());
        assert!(
            get_store_model(&rec.card_id)
                .unwrap()
                .unwrap()
                .scripts_content
                .is_empty()
        );
        assert!(!backup_table_exists());
    }

    #[test]
    fn missing_deployment_receipt_keeps_downloaded_model_and_clears_only_deployment_state() {
        let _ctx = setup_temp_db();
        let scripts = std::collections::BTreeMap::from([(
            "deploy".to_string(),
            "https://example.com/deploy.sh".to_string(),
        )]);
        add_store_model(AddStoreModelParams {
            card_id: "card-with-missing-receipt",
            model_name: "qwen38",
            card_name: "Qwen3.8",
            org: Some("Qwen"),
            description: None,
            engine_name: "sglang",
            engine_version: Some("v0.5.19"),
            size_bytes: 1024,
            quantization: Some("nvfp4"),
            source_repo_id: Some("owner/model"),
            source_filename: None,
            scripts: &scripts,
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::default(),
        })
        .unwrap();
        update_store_model_status("card-with-missing-receipt", "deployed", Some("script-hash"))
            .unwrap();
        set_running("card-with-missing-receipt", true).unwrap();
        set_endpoint_info(
            "card-with-missing-receipt",
            Some("http://127.0.0.1:30000/v1"),
            Some("qwen38"),
        )
        .unwrap();

        mark_deployment_missing("card-with-missing-receipt").unwrap();

        let record = get_store_model("card-with-missing-receipt")
            .unwrap()
            .unwrap();
        assert_eq!(record.status, "downloaded");
        assert!(
            record.weights_ready_at.is_some(),
            "正常下载落库的模型必须有权重已就绪凭据，界面才可显示“部署”"
        );
        assert!(!record.running);
        assert!(record.deployed_at.is_none());
        assert!(record.deployed_script_sha256.is_none());
        assert!(record.container_name.is_none());
        assert!(record.endpoint_url.is_none());
        assert!(record.endpoint_model.is_none());
        // 这条自愈不触碰下载物：权重位置、卡片脚本、下载时间都必须还在。
        assert_eq!(record.source_repo_id.as_deref(), Some("owner/model"));
        assert!(!record.downloaded_at.is_empty());
        assert_eq!(
            record
                .scripts
                .get("deploy")
                .and_then(|script| script.as_deref()),
            Some("https://example.com/deploy.sh")
        );
    }

    #[test]
    fn incomplete_weights_recover_to_resume_without_deleting_the_model_record() {
        let _ctx = setup_temp_db();
        let scripts = std::collections::BTreeMap::from([(
            "download_model".to_string(),
            "https://example.com/download_model.sh".to_string(),
        )]);
        add_store_model(AddStoreModelParams {
            card_id: "card-with-missing-weights",
            model_name: "qwen38",
            card_name: "Qwen3.8",
            org: Some("Qwen"),
            description: None,
            engine_name: "sglang",
            engine_version: Some("v0.5.19"),
            size_bytes: 1024,
            quantization: Some("nvfp4"),
            source_repo_id: Some("owner/model"),
            source_filename: None,
            scripts: &scripts,
            status: "downloaded",
            profiles: &[],
            deploy_topology: DeployTopology::default(),
        })
        .unwrap();
        update_store_model_status("card-with-missing-weights", "deployed", Some("script-hash"))
            .unwrap();
        set_running("card-with-missing-weights", true).unwrap();

        mark_weights_incomplete("card-with-missing-weights").unwrap();

        let record = get_store_model("card-with-missing-weights")
            .unwrap()
            .unwrap();
        assert_eq!(record.status, "incomplete");
        assert!(record.weights_ready_at.is_none());
        assert!(!record.running);
        assert!(record.deployed_at.is_none());
        assert!(record.deployed_script_sha256.is_none());
        assert_eq!(record.source_repo_id.as_deref(), Some("owner/model"));
        assert_eq!(
            record
                .scripts
                .get("download_model")
                .and_then(|script| script.as_deref()),
            Some("https://example.com/download_model.sh"),
            "恢复为继续下载时必须保留下载脚本和所有可续传文件"
        );
    }

    /// Key 详情入口和 LAN 网关共用这些函数；锁住按 id 精确撤销的语义：
    ///
    /// - 只有点名的那一把没了，同一张卡下的兄弟 key 必须毫发无损。少写一次
    ///   `WHERE id = ?1`（或者哪天有人顺手改成按 card_id 删）就是把用户发给
    ///   所有调用方的 key 一次性全废掉，而这是不可逆的——发出去的那些再也收不回来。
    /// - 吊销后 `find_api_key` 必须立刻打不中：网关认的是这条查询，库里没了但
    ///   还能认证，等于吊销只是做做样子。
    ///
    /// 删不存在的 id 不算错误：调用方拿到的是界面上一份可能已经过期的列表，
    /// 两个窗口各点一次删除时第二次必然落空。那时候应该什么都不发生，而不是
    /// 弹一句用户看不懂的失败。
    #[test]
    fn delete_api_key_revokes_only_the_named_key() {
        let _ctx = setup_temp_db();

        let doomed = add_api_key("card-1", Some("给 A 应用的")).unwrap();
        let survivor = add_api_key("card-1", Some("给 B 应用的")).unwrap();
        // 另一张卡：别让"按 card_id 误删"这种写法从旁边溜过去。
        let other_card = add_api_key("card-2", None).unwrap();

        delete_api_key(&doomed.id).unwrap();

        assert!(
            find_api_key(&doomed.key_value).unwrap().is_none(),
            "被吊销的 key 仍然能被网关认出来，等于没吊销"
        );
        assert!(
            find_api_key(&survivor.key_value).unwrap().is_some(),
            "同一张卡下的另一把 key 被连累删掉了"
        );
        assert!(
            find_api_key(&other_card.key_value).unwrap().is_some(),
            "别的卡片的 key 被连累删掉了"
        );
        let left: Vec<String> = list_api_keys_for_card("card-1")
            .unwrap()
            .into_iter()
            .map(|k| k.id)
            .collect();
        assert_eq!(left, vec![survivor.id.clone()]);

        // 幂等：重复吊销同一把，以及吊销一个压根不存在的 id，都不该报错。
        delete_api_key(&doomed.id).unwrap();
        delete_api_key("没有这把钥匙").unwrap();
        assert_eq!(list_api_keys_for_card("card-1").unwrap().len(), 1);
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

    /// `card_source` 是已经废弃的测试商店遗留列：升级过的设备可能还有它，
    /// 新设备没有它。无论哪种表结构，下载完成时写入本地模型记录都必须成功。
    ///
    /// 这条回归覆盖了 2026-10-13 真机上的事故：INSERT 仍列出 card_source，
    /// 但参数列表和 AddStoreModelParams 都已经不再提供它，SQLite 因而报
    /// “Got 23, needed 24”，把已经下载好的模型卡在“无法部署”的假状态。
    #[test]
    fn add_store_model_works_with_or_without_obsolete_card_source_column() {
        let _ctx = setup_temp_db();
        let scripts = std::collections::BTreeMap::new();

        let add = |card_id: &str| {
            add_store_model(AddStoreModelParams {
                card_id,
                model_name: "model",
                card_name: "Model",
                org: None,
                description: None,
                engine_name: "engine",
                engine_version: None,
                size_bytes: 1,
                quantization: None,
                source_repo_id: None,
                source_filename: None,
                scripts: &scripts,
                status: "downloaded",
                profiles: &[],
                deploy_topology: DeployTopology::default(),
            })
        };

        add("fresh-schema").expect("新库不应依赖已删除的 card_source 列");

        let conn = connect().unwrap();
        conn.execute("ALTER TABLE store_models ADD COLUMN card_source TEXT", [])
            .unwrap();
        drop(conn);

        add("upgraded-schema").expect("旧库残留 card_source 列也不应改变写入参数个数");
    }
}
