//! Only connection/path plumbing is substituted. Key SQL/token generation below
//! is the exact production module, using a temporary SQLite database per test.
use std::cell::RefCell;
thread_local! { static DIR: RefCell<Option<tempfile::TempDir>> = const { RefCell::new(None) }; }
pub fn reset() {
    DIR.with(|slot| *slot.borrow_mut() = Some(tempfile::tempdir().unwrap()));
    connect().unwrap().execute_batch("CREATE TABLE api_keys (id TEXT PRIMARY KEY,card_id TEXT,label TEXT,key_value TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL)").unwrap();
}
pub fn connect() -> rusqlite::Result<rusqlite::Connection> {
    DIR.with(|slot| {
        rusqlite::Connection::open(
            slot.borrow()
                .as_ref()
                .expect("reset test database")
                .path()
                .join("models.db"),
        )
    })
}
fn now_iso() -> String {
    "2026-10-16T00:00:00Z".into()
}
#[path = "../../pinvou3-app/src-tauri/src/features/model_store/platform/linux/api_keys.rs"]
mod api_keys;
pub use api_keys::*;

pub fn fixtures() {
    reset();
    for (id, card) in [
        ("test-a", Some("a")),
        ("test-a2", Some("a")),
        ("test-b", Some("b")),
        ("legacy", None),
    ] {
        connect()
            .unwrap()
            .execute(
                "INSERT INTO api_keys VALUES (?1,?2,NULL,?1,'test')",
                rusqlite::params![id, card],
            )
            .unwrap();
    }
}

#[derive(Clone)]
pub struct StoreModelRecord {
    pub running: bool,
    pub status: String,
    pub endpoint_url: Option<String>,
    pub endpoint_model: Option<String>,
    pub container_name: Option<String>,
}
thread_local! { static MODEL: RefCell<Option<StoreModelRecord>> = const { RefCell::new(None) }; }
pub fn model_fixture(record: Option<StoreModelRecord>) {
    MODEL.with(|value| *value.borrow_mut() = record);
}
pub fn get_store_model(_: &str) -> rusqlite::Result<Option<StoreModelRecord>> {
    MODEL.with(|value| Ok(value.borrow().clone()))
}

/// r19 P2-5 之后 lan_store 走窄投影查询，harness 经 #[path] 逐字编译生产
/// 代码，桩必须同名同形（评审 P1-1 实锤：漏了它 rust-lint 腿必红）。
/// 字段与 StoreModelRecord 一一对应，直接从同一个 fixture 映射。
#[derive(Clone)]
pub struct StoreModelRouteFields {
    pub running: bool,
    pub status: String,
    pub endpoint_url: Option<String>,
    pub endpoint_model: Option<String>,
    pub container_name: Option<String>,
}
pub fn get_store_model_route_fields(_: &str) -> rusqlite::Result<Option<StoreModelRouteFields>> {
    MODEL.with(|value| {
        Ok(value.borrow().clone().map(|r| StoreModelRouteFields {
            running: r.running,
            status: r.status,
            endpoint_url: r.endpoint_url,
            endpoint_model: r.endpoint_model,
            container_name: r.container_name,
        }))
    })
}
