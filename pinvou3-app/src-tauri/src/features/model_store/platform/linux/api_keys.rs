//! Existing SQLite API-key lifecycle, shared by commands and LAN authentication.
use super::{connect, now_iso};
use rand::Rng as _;
use rusqlite::{OptionalExtension, Row, params};

// ---------------- API 网关密钥 ----------------
// 每把 key 绑定一个具体模型（card_id），只能用来调用那一个模型——不同应用要接入
// 不同模型时分开发 key、分开吊销，互不影响。card_id 为空代表老版本遗留下的
// "全局 key"（改造前生成的），保留可读和撤销；新 LAN 网关不扩大它的授权范围。

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

/// 按 id 单独撤销，包括旧全局 key；下一次服务端认证立即失效。
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
