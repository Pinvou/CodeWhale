//! Local identity and capability discovery. This is an additive v1 inventory
//! contract; it does not implement the authenticated v2 pairing/plan protocol.
use crate::app::{api_err, ApiErr, App};
use crate::json::Value;
use crate::timeutil::{iso8601, now_epoch};

fn load_record(app: &App) -> Result<Value, ApiErr> {
    let path = app.config.state_dir.join("device-identity.json");
    let machine = std::fs::read_to_string("/etc/machine-id")
        .map_err(|e| api_err(500, "identity_unavailable", &e.to_string()))?;
    let stored = crate::private_storage::read(&path)
        .map_err(|e| api_err(500, "identity_unavailable", &e))?;
    let record = if let Some(content) = stored {
        let content = std::str::from_utf8(&content)
            .map_err(|_| api_err(500, "identity_corrupt", "设备身份UTF-8损坏"))?;
        Value::parse(content).map_err(|e| api_err(500, "identity_corrupt", &e.to_string()))?
    } else {
        let uuid = std::fs::read_to_string("/proc/sys/kernel/random/uuid")
            .map_err(|e| api_err(500, "identity_unavailable", &e.to_string()))?;
        let mut record = Value::obj();
        record.set("device_id", format!("dev-{}", uuid.trim()).into());
        record.set("machine", machine.trim().into());
        crate::private_storage::publish(&path, record.to_string().as_bytes(), false)
            .map_err(|e| api_err(500, "identity_persist_failed", &e))?;
        record
    };
    if record.get("machine").and_then(Value::as_str) != Some(machine.trim()) {
        return Err(api_err(
            409,
            "identity_machine_changed",
            "状态目录来自其他系统，请先核实管理关系再迁移身份",
        ));
    }
    let id = record
        .get("device_id")
        .and_then(Value::as_str)
        .ok_or_else(|| api_err(500, "identity_corrupt", "设备身份字段缺失"))?;
    if id.len() > 128
        || id.is_empty()
        || !id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.:".contains(&b))
    {
        return Err(api_err(500, "identity_corrupt", "设备身份字段不合法"));
    }
    Ok(record)
}

pub(crate) fn record(app: &App) -> Result<Value, ApiErr> {
    crate::private_storage::directory(&app.config.state_dir, false)
        .map_err(|e| api_err(500, "identity_unavailable", &e))?;
    let _lock = crate::private_storage::lock(&app.config.state_dir.join("device-identity.lock"))
        .map_err(|error| match error {
            crate::private_storage::LockError::Busy => {
                api_err(503, "identity_busy", "设备身份正在读取，请稍后重试")
            }
            crate::private_storage::LockError::Unavailable(message) => {
                api_err(503, "identity_unavailable", &message)
            }
        })?;
    load_record(app)
}

pub fn inventory(app: &App) -> Result<Value, ApiErr> {
    let record = record(app)?;
    let id = record
        .get("device_id")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let mut out = Value::obj();
    out.set("device_id", id.into());
    out.set("identity_key_id", Value::Null);
    out.set("identity_scope", "local-state-only".into());
    out.set(
        "boot_id",
        std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
            .unwrap_or_default()
            .trim()
            .into(),
    );
    out.set("observed_at", iso8601(now_epoch()).into());
    out.set("source", "cmsd".into());
    out.set("protocol_versions", Value::Arr(vec!["v1".into()]));
    out.set(
        "capabilities",
        Value::Arr(vec![
            "local.inventory".into(),
            "local.network.read".into(),
            "local.lifecycle".into(),
            "store.delegate".into(),
            "model.install_retry.v1".into(),
            "component.maintenance".into(),
            "model.maintenance".into(),
            "model.maintenance.reservation".into(),
            "model.explicit_adoption".into(),
            "model.store_enrollment.v1".into(),
            "model.recovery.budgeted".into(),
        ]),
    );
    out.set("remote_pairing", false.into());
    out.set("group_coordination", false.into());
    Ok(out)
}
