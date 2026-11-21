//! 管理 API（D1 最小面 + P2 install/cancel）：unix socket 上的 HTTP/1.1。
//!
//! 面：status / models / engines / capacity / tasks(+SSE) / logs /
//! enable / disable / purge / install / tasks cancel。plan_switch/execute 留 P2 后续。

use crate::app::{api_err, ApiErr, App};
use crate::capacity;
use crate::http::{self, Request};
use crate::install;
use crate::json::Value;
use crate::ops;
use crate::redact;
use crate::store::{ModelStatus, TaskStatus};
use crate::timeutil::{iso8601, now_epoch};
use std::io::BufReader;
use std::os::unix::net::UnixStream;
use std::sync::Arc;

pub fn handle_conn(app: &Arc<App>, stream: UnixStream) {
    if stream
        .set_read_timeout(Some(std::time::Duration::from_secs(3)))
        .is_err()
        || stream
            .set_write_timeout(Some(std::time::Duration::from_secs(10)))
            .is_err()
    {
        return;
    }
    let caller = match crate::peer::LocalPeer::from_stream(&stream) {
        Ok(c) => c,
        Err(_) => return,
    };
    let cloned = match stream.try_clone() {
        Ok(s) => s,
        Err(_) => return,
    };
    let mut reader = BufReader::new(http::DeadlineReader::new(
        cloned,
        std::time::Duration::from_secs(10),
    ));
    let mut writer = stream;
    let req = match http::read_request_limited(&mut reader, 64 * 1024) {
        Ok(Some(r)) => r,
        Ok(None) => return,
        Err(error) => {
            let _ = http::write_receive_error(&mut writer, &error);
            return;
        }
    };
    route(app, &req, &mut writer, caller);
}

fn route(app: &Arc<App>, req: &Request, writer: &mut UnixStream, caller: crate::peer::LocalPeer) {
    let segments: Vec<&str> = req.path.split('/').filter(|s| !s.is_empty()).collect();
    // Fast rejection before card scans/backend health I/O; the Store-locked
    // admission check still runs again before a task or side effect is created.
    if matches!(
        (req.method.as_str(), segments.as_slice()),
        (
            "POST",
            [
                "v1",
                "models",
                _,
                "enable"
                    | "disable"
                    | "purge"
                    | "set-default"
                    | "install"
                    | "enable-boot"
                    | "disable-boot"
            ]
        ) | ("POST", ["v1", "models", _, "install", "retry"])
            | ("POST", ["v1", "switch"])
    ) {
        if let Err(error) = crate::component::check_admission(app) {
            write_result(writer, Err(error));
            return;
        }
        let error = app
            .lifecycle_revision_error
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        if let Some(message) = error {
            write_result(
                writer,
                Err(api_err(503, "lifecycle_version_unavailable", &message)),
            );
            return;
        }
    }
    let result: Result<Value, ApiErr> = match (req.method.as_str(), segments.as_slice()) {
        ("GET", ["v1", "status"]) => status(app),
        ("GET", ["v1", "device"]) => crate::device::inventory(app),
        ("GET", ["v1", "network"]) => app.network.view(app),
        ("POST", ["v1", "component-maintenance"]) => crate::component::change(app, &req.body),
        ("GET", ["v1", "models", card, "maintenance"]) => crate::model_maintenance::view(app, card),
        ("POST", ["v1", "models", card, "maintenance"]) => {
            crate::model_maintenance::change(app, card, &req.body, caller)
        }
        ("GET", ["v1", "models"]) => crate::store_adoption::selection::cached(app)
            .map_err(|message| api_err(503, "store_selection_unavailable", &message))
            .map(|_| models(app)),
        ("POST", ["v1", "store-enrollment"]) => {
            crate::store_adoption::register_reader(app, &req.body, caller)
        }
        ("POST", ["v1", "models", card, action @ ("enable-boot" | "disable-boot")]) => {
            crate::recovery::set_boot(app, card, *action == "enable-boot")
        }
        ("GET", ["v1", "engines"]) => Ok(engines(app)),
        ("GET", ["v1", "capacity"]) => capacity_view(app, req.query_param("card")),
        ("GET", ["v1", "tasks"]) => Ok(tasks_view(app)),
        ("GET", ["v1", "tasks", id]) => task_view(app, id),
        ("POST", ["v1", "models", card, "enable"]) => {
            ops::enable(app, card).map(|tid| task_accepted(&tid, "enable", card))
        }
        ("POST", ["v1", "models", card, "disable"]) => maintenance_owner(&req.body)
            .and_then(|owner| match owner {
                Some(owner) => crate::model_maintenance::authorize_owner(app, card, &owner, caller)
                    .and_then(|()| ops::disable_with_maintenance(app, card, Some(&owner))),
                None => ops::disable(app, card),
            })
            .map(|tid| task_accepted(&tid, "disable", card)),
        ("POST", ["v1", "models", card, "purge"]) => maintenance_owner(&req.body)
            .and_then(|owner| match owner {
                Some(owner) => crate::model_maintenance::authorize_owner(app, card, &owner, caller)
                    .and_then(|()| ops::purge_with_maintenance(app, card, Some(&owner))),
                None => ops::purge(app, card),
            })
            .map(|tid| task_accepted(&tid, "purge", card)),
        ("POST", ["v1", "models", card, "set-default"]) => {
            ops::set_default(app, card).map(|tid| task_accepted(&tid, "set_default", card))
        }
        ("POST", ["v1", "models", card, "install"]) => {
            install::install(app, card).map(|tid| task_accepted(&tid, "install", card))
        }
        ("POST", ["v1", "models", card, "install", "retry"]) => {
            install::retry::install(app, card, &req.body)
                .map(|tid| task_accepted(&tid, "install", card))
        }
        ("POST", ["v1", "tasks", id, "cancel"]) => install::cancel(app, id),
        ("POST", ["v1", "tasks", id, "abandon"]) => install::abandon(app, id, &req.body),
        ("POST", ["v1", "switch"]) => switch_view(app, &req.body),
        ("GET", ["v1", "models", card, "logs"]) => logs_view(app, card, req.query_param("tail")),
        ("GET", ["v1", "tasks", id, "events"]) => {
            let Some(_event_permit) = app.event_connections.acquire() else {
                let _ = http::write_response(writer, "503 Service Unavailable", "application/json",
                    b"{\"error\":{\"code\":\"event_connection_limit\",\"message\":\"too many event streams\"}}", &[]);
                return;
            };
            // SSE 自己接管连接生命周期
            sse_events(app, id, writer);
            return;
        }
        _ => Err(api_err(
            404,
            "unknown_endpoint",
            &format!(
                "{} {} 不在 cmsd API 面内（见 README）",
                req.method, req.path
            ),
        )),
    };
    let result = result.map(|mut value| {
        if let Some(task_id) = value
            .get("task_id")
            .and_then(Value::as_str)
            .map(str::to_owned)
        {
            let store = app.store.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(task) = store.state.tasks.get(&task_id) {
                let requested = value.get("card").cloned().unwrap_or(Value::Null);
                value.set("requested_card", requested);
                value.set("card", task.card.clone().into());
                if let Some(selection) = task.selection.as_ref().and_then(Value::as_obj) {
                    for (key, field) in selection {
                        value.set(key, field.clone());
                    }
                }
            }
        }
        value
    });
    write_result(writer, result);
}

fn write_result(stream: &mut UnixStream, result: Result<Value, ApiErr>) {
    let mut writer = http::UnixDeadlineWriter::new(stream, std::time::Duration::from_secs(10));
    match result {
        Ok(v) => {
            let _ = http::write_response(
                &mut writer,
                "200 OK",
                "application/json",
                v.to_string().as_bytes(),
                &[],
            );
        }
        Err((status, code, message)) => {
            let mut v = Value::obj();
            let mut e = Value::obj();
            e.set("code", code.into());
            e.set("message", message.into());
            v.set("error", e);
            let status_text = http::status_line(status);
            let _ = http::write_response(
                &mut writer,
                status_text,
                "application/json",
                v.to_string().as_bytes(),
                &[],
            );
        }
    }
}

fn maintenance_owner(body: &[u8]) -> Result<Option<String>, ApiErr> {
    if body.is_empty() {
        return Ok(None);
    }
    let text =
        std::str::from_utf8(body).map_err(|_| api_err(400, "invalid_json", "请求不是UTF-8"))?;
    let value = Value::parse(text).map_err(|_| api_err(400, "invalid_json", "请求不是合法JSON"))?;
    match value.get("maintenance_owner") {
        None if value.as_obj().is_some() => Ok(None),
        Some(Value::Str(owner))
            if !owner.is_empty()
                && owner.len() <= 128
                && owner
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b)) =>
        {
            Ok(Some(owner.clone()))
        }
        _ => Err(api_err(
            400,
            "invalid_maintenance_owner",
            "maintenance_owner必须是有效事务标识，请求必须是对象",
        )),
    }
}

fn task_accepted(task_id: &str, kind: &str, card: &str) -> Value {
    let mut v = Value::obj();
    v.set("task_id", task_id.into());
    v.set("kind", kind.into());
    v.set("card", card.into());
    v.set("status", "running".into());
    v.set("events_url", format!("/v1/tasks/{task_id}/events").into());
    v
}

fn status(app: &Arc<App>) -> Result<Value, ApiErr> {
    let selection = crate::store_adoption::selection::cached(app)
        .map_err(|message| api_err(503, "store_selection_unavailable", &message))?;
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let mut v = Value::obj();
    v.set("service", "cmsd".into());
    v.set(
        "store_adoption",
        app.store_adoption_status
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone(),
    );
    v.set(
        "lifecycle_revision",
        app.lifecycle_epoch
            .load(std::sync::atomic::Ordering::Acquire)
            .into(),
    );
    let epoch_error = app
        .lifecycle_revision_error
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .clone();
    v.set("lifecycle_version_ready", epoch_error.is_none().into());
    v.set(
        "lifecycle_version_error",
        epoch_error.map(Value::from).unwrap_or(Value::Null),
    );
    v.set(
        "component_maintenance",
        crate::component::check_admission(app).is_err().into(),
    );
    if let Ok(device) = crate::device::inventory(app) {
        v.set("device", device);
    }
    v.set("version", env!("CARGO_PKG_VERSION").into());
    v.set("systemd_mode", app.config.systemd_mode.as_str().into());
    v.set("started_at", app.started_at.into());
    v.set("started_at_iso", iso8601(app.started_at).into());
    v.set("uptime_s", app.uptime_s().into());
    v.set("gateway", app.config.gateway_listen.clone().into());
    v.set("gateway_status", crate::gateway::status(app));
    match guard
        .state
        .active_model
        .as_ref()
        .filter(|card| selection.visible(card))
    {
        Some(m) => v.set("active_model", m.clone().into()),
        None => v.set("active_model", Value::Null),
    }
    v.set(
        "models",
        Value::Arr(
            guard
                .state
                .models
                .values()
                .filter(|model| selection.visible(&model.card))
                .map(model_json)
                .collect(),
        ),
    );
    let degraded: Vec<Value> = guard
        .state
        .models
        .values()
        .filter(|m| m.status == ModelStatus::Degraded && selection.visible(&m.card))
        .map(|m| {
            let mut d = Value::obj();
            d.set("card", m.card.clone().into());
            d.set(
                "reason",
                m.degraded_reason.clone().unwrap_or_default().into(),
            );
            d
        })
        .collect();
    v.set("degraded", Value::Arr(degraded));
    let mut engines_summary = Value::obj();
    engines_summary.set("installed", guard.state.engines.len().into());
    engines_summary.set("external_processes", guard.state.externals.len().into());
    v.set("engines", engines_summary);
    Ok(v)
}

fn model_json(m: &crate::store::ModelState) -> Value {
    let mut v = Value::obj();
    v.set("card", m.card.clone().into());
    v.set("model_id", m.model_id.clone().into());
    v.set("engine_sha8", m.engine_sha8.clone().into());
    v.set("engine_type", m.engine_type.clone().into());
    match &m.container_name {
        Some(c) => v.set("container_name", c.clone().into()),
        None => v.set("container_name", Value::Null),
    }
    match &m.image {
        Some(i) => v.set("image", i.clone().into()),
        None => v.set("image", Value::Null),
    }
    v.set("port", (m.port as u64).into());
    v.set("status", m.status.as_str().into());
    v.set("managed", m.lifecycle_owned.into());
    match &m.degraded_reason {
        Some(r) => v.set("degraded_reason", r.clone().into()),
        None => v.set("degraded_reason", Value::Null),
    }
    v.set("restart_count", m.restart_count.into());
    match m.last_restart_at {
        Some(t) => {
            v.set("last_restart_at", t.into());
            v.set("last_restart_at_iso", iso8601(t).into());
        }
        None => v.set("last_restart_at", Value::Null),
    }
    match m.last_ready_at {
        Some(t) => {
            v.set("last_ready_at", t.into());
            v.set("last_ready_at_iso", iso8601(t).into());
        }
        None => v.set("last_ready_at", Value::Null),
    }
    v.set("weights_bytes", m.weights_bytes.into());
    match m.measured_footprint_gib {
        Some(f) => v.set("measured_footprint_gib", f.into()),
        None => v.set("measured_footprint_gib", Value::Null),
    }
    match m.engine_rss_gib {
        Some(f) => v.set("engine_rss_gib", f.into()),
        None => v.set("engine_rss_gib", Value::Null),
    }
    match m.main_pid {
        Some(p) => v.set("main_pid", (p as u64).into()),
        None => v.set("main_pid", Value::Null),
    }
    v
}

/// Display the same bounded native journal source as the logs endpoint.
/// The current namespace remains an internal cache key, not retained-unit fact.
fn set_native_unit(app: &App, card: &str, value: &mut Value) {
    match crate::store_adoption::runtime::native_unit_name(app, card) {
        Ok(unit) => value.set("unit", unit.map(Value::from).unwrap_or(Value::Null)),
        Err(error) => {
            value.set("unit", Value::Null);
            value.set("unit_error", error.into());
        }
    }
}

fn models(app: &Arc<App>) -> Value {
    let selection = match crate::store_adoption::selection::cached(app) {
        Ok(selection) => selection,
        Err(error) => {
            let mut value = Value::obj();
            value.set("models", Value::Arr(Vec::new()));
            value.set("store_bindings", Value::Null);
            value.set("selection_error", error.into());
            return value;
        }
    };
    let store_bindings = crate::store_adoption::bindings(app);
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
    // 先扫一次卡：登记模型也要带出 weights_complete（调用方据此区分
    // 「已下载」与「未下载」，不能只靠已发现未登记的合并行），下面合并时复用同一份。
    let scanned: std::collections::BTreeMap<String, crate::cards::Card> =
        crate::cards::scan_cards(&app.config.cards_dir)
            .into_iter()
            .map(|c| (c.name.clone(), c))
            .collect();
    let mut arr: Vec<Value> = guard
        .state
        .models
        .values()
        .filter(|model| selection.visible(&model.card))
        .map(|m| {
            let mut v = model_json(m);
            if let Some(metadata) = selection
                .metadata(&m.card)
                .and_then(|data| Value::parse(&data.to_string()).ok())
            {
                for (key, value) in metadata.as_obj().unwrap() {
                    v.set(key, value.clone());
                }
            }
            v.set(
                "can_adopt",
                (!m.lifecycle_owned
                    && selection.routable(&m.card)
                    && scanned
                        .get(&m.card)
                        .is_some_and(|c| c.deploy.is_some() || c.docker_deploy.is_some()))
                .into(),
            );
            v.set(
                "can_reserve_uninstall",
                (selection.maintenance_target(&m.card).is_ok()
                    && (m.lifecycle_owned || scanned.contains_key(&m.card)))
                .into(),
            );
            match scanned.get(&m.card) {
                Some(card) => match card.engine_kind {
                    crate::cards::EngineKind::Container => {
                        // 容器卡：weights_complete 三态（未部署/无清单给 null）
                        match card.docker_weights_complete() {
                            Some(b) => v.set("weights_complete", b.into()),
                            None => v.set("weights_complete", Value::Null),
                        }
                    }
                    crate::cards::EngineKind::Native => {
                        v.set("weights_complete", card.weights_complete().into());
                    }
                },
                None => v.set("weights_complete", Value::Null),
            }
            match live.health.get(&m.port) {
                Some(h)
                    if matches!(m.status, ModelStatus::Running | ModelStatus::Starting)
                        && selection.routable(&m.card)
                        && match m.engine_type.as_str() {
                            "container" => scanned.get(&m.card).is_some_and(|card| {
                                card.docker_deploy
                                    .as_ref()
                                    .is_some_and(|deploy| deploy.port == m.port)
                                    && m.container_name
                                        .as_ref()
                                        .and_then(|name| live.containers.get(name))
                                        .is_some_and(|container| {
                                            container.running
                                                && container.pid > 0
                                                && m.main_pid == Some(container.pid)
                                                && crate::container_runtime::check(
                                                    card,
                                                    Some(m),
                                                    container,
                                                )
                                                .is_ok()
                                        })
                            }),
                            "native" => scanned.get(&m.card).is_some_and(|card| {
                                ops::native_instance_matches(
                                    app,
                                    card,
                                    m.port,
                                    m.main_pid.unwrap_or(0),
                                ) && live.health_instances.get(&m.port).copied()
                                    == m.main_pid.and_then(|pid| {
                                        crate::store_adoption::runtime::instance_key(app, pid)
                                    })
                            }),
                            _ => false,
                        } =>
                {
                    v.set("healthy", h.ok.into());
                    v.set("health_detail", h.detail.clone().into());
                    v.set("health_checked_at", h.checked_at.into());
                    match &h.model_id {
                        Some(id) => v.set("health_model_id", id.clone().into()),
                        None => v.set("health_model_id", Value::Null),
                    }
                    // 引擎自报上下文窗口：接入行 context_window 的事实来源
                    match h.model_len {
                        Some(l) => v.set("max_model_len", l.into()),
                        None => v.set("max_model_len", Value::Null),
                    }
                }
                _ => {
                    v.set("healthy", false.into());
                    v.set(
                        "health_detail",
                        "not_running_or_current_deployment_unverified".into(),
                    );
                    v.set("max_model_len", Value::Null);
                }
            }
            // W6：自启标记按引擎形态取（容器 = restart 策略；原生 = systemd 登记）
            if m.engine_type == "container" {
                let cname = m.container_name.clone().unwrap_or_default();
                match live.containers.get(&cname) {
                    Some(c) => {
                        v.set("container", c.name.clone().into());
                        v.set("container_state", c.state.clone().into());
                        v.set("restart_policy", c.restart_policy.clone().into());
                        v.set("boot_enabled", c.is_autostart().into());
                    }
                    None => {
                        v.set("container", cname.into());
                        v.set("boot_enabled", false.into());
                    }
                }
            } else {
                let unit = app.systemd.unit_name_for(&m.card);
                set_native_unit(app, &m.card, &mut v);
                if let Some(u) = live.units.get(&unit) {
                    v.set("unit_active_state", u.active_state.clone().into());
                    v.set("unit_sub_state", u.sub_state.clone().into());
                    v.set("unit_n_restarts", u.n_restarts.into());
                    v.set("unit_checked_at", u.checked_at.into());
                }
                v.set(
                    "boot_enabled",
                    live.units
                        .get(&unit)
                        .and_then(|u| u.boot_enabled)
                        .map(Value::from)
                        .unwrap_or(Value::Null),
                );
            }
            match crate::recovery::view(app, &m.card) {
                Ok(Some(recovery)) if m.lifecycle_owned => {
                    v.set(
                        "boot_enabled",
                        crate::recovery::boot_enabled(&recovery).into(),
                    );
                    v.set(
                        "can_set_autostart",
                        (selection.routable(&m.card)
                            && recovery.get("phase").and_then(Value::as_str) == Some("ready")
                            && recovery.get("migration_pending").and_then(Value::as_bool)
                                != Some(true)
                            && (recovery.get("source").and_then(Value::as_str)
                                != Some("model-store")
                                || scanned.get(&m.card).is_some_and(|c| {
                                    c.deploy.is_some() || c.docker_deploy.is_some()
                                })))
                        .into(),
                    );
                    v.set("recovery_owner", "cmsd".into());
                    v.set("recovery", recovery);
                }
                Err((_, code, message)) => {
                    v.set("boot_enabled", Value::Null);
                    v.set("recovery_error", format!("{code}: {message}").into());
                }
                _ => {}
            }
            v
        })
        .collect();
    // 已发现但从未 enable 的卡片也列出（统一视图）
    for card in scanned.values() {
        if !selection.visible(&card.name) {
            continue;
        }
        if guard.state.models.contains_key(&card.name) {
            continue;
        }
        let mut v = Value::obj();
        v.set("card", card.name.clone().into());
        if let Some(metadata) = selection
            .metadata(&card.name)
            .and_then(|data| Value::parse(&data.to_string()).ok())
        {
            for (key, value) in metadata.as_obj().unwrap() {
                v.set(key, value.clone());
            }
        }
        v.set("model_id", card.model_id.clone().into());
        v.set("engine_sha8", card.engine_sha8.clone().into());
        v.set("engine_type", card.engine_kind.as_str().into());
        v.set("managed", false.into());
        v.set(
            "can_adopt",
            (selection.routable(&card.name)
                && (card.deploy.is_some() || card.docker_deploy.is_some()))
            .into(),
        );
        v.set(
            "can_reserve_uninstall",
            selection.maintenance_target(&card.name).is_ok().into(),
        );
        v.set("weights_bytes", card.total_bytes.into());
        match card.engine_kind {
            crate::cards::EngineKind::Native => {
                v.set("status", "stopped".into());
                v.set("deployed", card.deploy.is_some().into());
                v.set("weights_complete", card.weights_complete().into());
            }
            crate::cards::EngineKind::Container => {
                v.set("image", card.image.clone().unwrap_or_default().into());
                match &card.container_name {
                    Some(n) => v.set("container_name", n.clone().into()),
                    None => v.set("container_name", Value::Null),
                }
                v.set("deployed", card.docker_deploy.is_some().into());
                // 部署档里声明的服务端口（容器未起时也展示——这是卡片声明的对外端口）
                if let Some(d) = &card.docker_deploy {
                    v.set("port", (d.port as u64).into());
                }
                // compose 一代卡：host 网络直接绑宿主地址（与桥接映射容器区分）
                if let Some(hb) = card
                    .docker_deploy
                    .as_ref()
                    .and_then(|d| d.host_bind.as_ref())
                {
                    v.set("host_bind", hb.clone().into());
                }
                match card.docker_weights_complete() {
                    Some(b) => v.set("weights_complete", b.into()),
                    None => v.set("weights_complete", Value::Null),
                }
                // 未登记的容器也可能在跑（收编前的窗口）：带出事实状态
                let c = card
                    .container_name
                    .as_ref()
                    .and_then(|n| live.containers.get(n));
                match c {
                    Some(c) if c.running => {
                        v.set("status", "running".into());
                        v.set("container_state", c.state.clone().into());
                        v.set("restart_policy", c.restart_policy.clone().into());
                        v.set("boot_enabled", c.is_autostart().into());
                        if let Some(p) = c.host_port {
                            v.set("port", (p as u64).into());
                        }
                    }
                    Some(c) => {
                        v.set("status", "stopped".into());
                        v.set("container_state", c.state.clone().into());
                        v.set("restart_policy", c.restart_policy.clone().into());
                        v.set("boot_enabled", c.is_autostart().into());
                    }
                    None => v.set("status", "stopped".into()),
                }
            }
        }
        arr.push(v);
    }
    let mut v = Value::obj();
    v.set("models", Value::Arr(arr));
    v.set("store_bindings", store_bindings);
    v.set("store_enrollment", 1u64.into());
    v.set("selection_policy", "latest-installed-v1".into());
    v.set("store_reader", crate::store_adoption::reader_view(app));
    match guard
        .state
        .active_model
        .as_ref()
        .filter(|card| selection.visible(card))
    {
        Some(m) => v.set("active_model", m.clone().into()),
        None => v.set("active_model", Value::Null),
    }
    v
}

fn engines(app: &Arc<App>) -> Value {
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
    let mut arr = Vec::new();
    for e in guard.state.engines.values() {
        let mut v = Value::obj();
        v.set("id", e.sha8.clone().into());
        v.set("engine_type", e.engine_type.clone().into());
        v.set("path", e.path.clone().into());
        v.set("installed", true.into());
        v.set("first_seen_at", e.first_seen_at.into());
        v.set("last_seen_at", e.last_seen_at.into());
        // 运行实例：受管（单元 MainPID）与野生（收编登记）
        let mut instances = Vec::new();
        for (unit, us) in &live.units {
            if us.is_active() && us.main_pid > 0 {
                if let Some(proc) = live.procs.iter().find(|p| p.pid == us.main_pid) {
                    if proc.sha8 == e.sha8 {
                        let mut i = Value::obj();
                        let owned = app
                            .systemd
                            .card_from_unit(unit)
                            .and_then(|card| guard.state.models.get(&card))
                            .is_some_and(|model| model.lifecycle_owned);
                        i.set("source", if owned { "managed" } else { "external" }.into());
                        if let Some(card) = app.systemd.card_from_unit(unit) {
                            set_native_unit(app, &card, &mut i);
                        } else {
                            i.set("unit", unit.clone().into());
                        }
                        i.set("pid", (proc.pid as u64).into());
                        match proc.port {
                            Some(p) => i.set("port", (p as u64).into()),
                            None => i.set("port", Value::Null),
                        }
                        match &proc.model_id {
                            Some(m) => i.set("model_id", m.clone().into()),
                            None => i.set("model_id", Value::Null),
                        }
                        if let Some(h) = proc.port.and_then(|p| live.health.get(&p)) {
                            i.set("healthy", h.ok.into());
                        }
                        instances.push(i);
                    }
                }
            }
        }
        for s in guard.state.externals.values() {
            if s.sha8 != e.sha8 {
                continue;
            }
            let alive = live.procs.iter().any(|p| p.pid == s.pid);
            let mut i = Value::obj();
            i.set("source", "external".into());
            i.set("pid", (s.pid as u64).into());
            i.set("alive", alive.into());
            match s.port {
                Some(p) => i.set("port", (p as u64).into()),
                None => i.set("port", Value::Null),
            }
            match &s.model_id {
                Some(m) => i.set("model_id", m.clone().into()),
                None => i.set("model_id", Value::Null),
            }
            if let Some(h) = s.port.and_then(|p| live.health.get(&p)) {
                i.set("healthy", h.ok.into());
            }
            i.set("first_seen_at", s.first_seen_at.into());
            i.set("last_seen_at", s.last_seen_at.into());
            instances.push(i);
        }
        v.set("instances", Value::Arr(instances));
        arr.push(v);
    }
    // W6：容器实例（source: container）。命中容器卡的带出卡名，映射不上的只读
    // 展示（不管理）；显存归属 = nvidia-smi 进程清单 ∩ docker top PID 表。
    let docker_cards: Vec<crate::cards::Card> = crate::cards::scan_cards(&app.config.cards_dir)
        .into_iter()
        .filter(|c| c.engine_kind == crate::cards::EngineKind::Container)
        .collect();
    let mut containers = Vec::new();
    for c in live.containers.values() {
        let mut i = Value::obj();
        i.set("source", "container".into());
        i.set("name", c.name.clone().into());
        i.set("image", c.image.clone().into());
        match docker_cards
            .iter()
            .find(|card| card.container_name.as_deref() == Some(c.name.as_str()))
        {
            Some(card) => {
                i.set("card", card.name.clone().into());
                i.set(
                    "managed",
                    guard
                        .state
                        .models
                        .get(&card.name)
                        .map(|m| m.lifecycle_owned)
                        .unwrap_or(false)
                        .into(),
                );
            }
            None => {
                i.set("card", Value::Null);
                i.set("managed", false.into());
            }
        }
        i.set("state", c.state.clone().into());
        i.set("running", c.running.into());
        i.set("restart_policy", c.restart_policy.clone().into());
        i.set("boot_enabled", c.is_autostart().into());
        match c.host_port {
            Some(p) => i.set("host_port", (p as u64).into()),
            None => i.set("host_port", Value::Null),
        }
        match c.container_port {
            Some(p) => i.set("container_port", (p as u64).into()),
            None => i.set("container_port", Value::Null),
        }
        if c.running && c.pid > 0 {
            i.set("pid", (c.pid as u64).into());
        }
        if !c.running {
            i.set("exit_code", c.exit_code.into());
            if c.oom_killed {
                i.set("oom_killed", true.into());
            }
        }
        if let Some(name) = c.served_model_name() {
            i.set("served_model_name", name.into());
        }
        // 健康探测（host 端口 GET /v1/models，守护 tick 的缓存结果）
        match c.host_port.and_then(|p| live.health.get(&p)) {
            Some(h) => {
                i.set("healthy", h.ok.into());
                match &h.model_id {
                    Some(id) => i.set("health_model_id", id.clone().into()),
                    None => i.set("health_model_id", Value::Null),
                }
            }
            None => i.set("healthy", Value::Null),
        }
        if c.running {
            let mib = live.gpu_mib_for_container(&c.name);
            if mib > 0 {
                i.set("gpu_used_mib", mib.into());
                i.set("gpu_used_gib", round1(mib as f64 / 1024.0).into());
            }
        }
        if let Some(f) = c.mem_fraction_static() {
            i.set("mem_fraction_static", f.into());
        }
        containers.push(i);
    }
    let mut v = Value::obj();
    v.set("engines", Value::Arr(arr));
    v.set("containers", Value::Arr(containers));
    v
}

fn capacity_view(app: &Arc<App>, card: Option<&str>) -> Result<Value, ApiErr> {
    let selected = crate::store_adoption::selection::cached(app)
        .map_err(|error| api_err(503, "store_selection_unavailable", &error))?;
    let resolved = card
        .map(|name| selected.resolve(name).map(|(physical, _)| physical))
        .transpose()
        .map_err(|error| api_err(409, "archived_model_version", &error))?;
    let card = resolved.as_deref();
    let mem = capacity::read_meminfo(&app.config.meminfo_path)
        .map_err(|e| api_err(500, "meminfo_unreadable", &format!("读内存信息失败：{e}")))?;
    let mut mem_json = Value::obj();
    mem_json.set("total_gib", round1(mem.total_gib()).into());
    mem_json.set("used_gib", round1(mem.used_gib()).into());
    mem_json.set("available_gib", round1(mem.available_gib()).into());
    mem_json.set("swap_total_gib", round1(mem.swap_total_gib()).into());
    mem_json.set("swap_free_gib", round1(mem.swap_free_gib()).into());
    if app.config.debug_available_mib.is_some() {
        mem_json.set(
            "debug_available_override_mib",
            app.config.debug_available_mib.unwrap_or(0).into(),
        );
    }
    let mut v = Value::obj();
    v.set("memory", mem_json);
    v.set(
        "safety_margin_gib",
        round1(capacity::safety_margin_gib(mem.total_kb)).into(),
    );
    // 常驻模型实测占用（预算器的标定数据）
    let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
    let resident: Vec<Value> = guard
        .state
        .models
        .values()
        .filter(|m| matches!(m.status, ModelStatus::Running | ModelStatus::Starting))
        .map(|m| {
            let mut r = Value::obj();
            r.set("card", m.card.clone().into());
            r.set(
                "weights_gib",
                round1(m.weights_bytes as f64 / 1073741824.0).into(),
            );
            match m.measured_footprint_gib {
                Some(f) => r.set("measured_footprint_gib", f.into()),
                None => r.set("measured_footprint_gib", Value::Null),
            }
            r
        })
        .collect();
    v.set("resident_models", Value::Arr(resident));
    drop(guard);
    // W6 显存归属：nvidia-smi 进程清单 ∩ docker top PID 表（容器）/ main_pid（原生）。
    // GB10 统一内存：整卡总量查询报 [N/A]，只有进程级占用可归属，视图如实呈现。
    {
        // 锁序与 models() 一致（store → live），防交叉死锁
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        let live = app.live.lock().unwrap_or_else(|e| e.into_inner());
        let docker_cards = crate::cards::scan_cards(&app.config.cards_dir);
        let mut attributions = Vec::new();
        let mut attributed: u64 = 0;
        for c in live.containers.values().filter(|c| c.running) {
            let mib = live.gpu_mib_for_container(&c.name);
            if mib == 0 {
                continue;
            }
            attributed += mib;
            let mut a = Value::obj();
            a.set("container", c.name.clone().into());
            match docker_cards
                .iter()
                .find(|card| card.container_name.as_deref() == Some(c.name.as_str()))
            {
                Some(card) => a.set("card", card.name.clone().into()),
                None => a.set("card", Value::Null),
            }
            a.set("used_mib", mib.into());
            a.set("used_gib", round1(mib as f64 / 1024.0).into());
            if let Some(f) = c.mem_fraction_static() {
                let reserve = f * mem.total_gib();
                a.set("mem_fraction_static", f.into());
                a.set("static_reserve_gib", round1(reserve).into());
                a.set(
                    "note",
                    format!("容器静态预留 --mem-fraction-static {f} × 总量 = {reserve:.1} GiB")
                        .into(),
                );
            }
            attributions.push(a);
        }
        for m in guard.state.models.values() {
            if m.engine_type == "container" {
                continue; // 容器归属在上面（按容器 PID 集，比 main_pid 全）
            }
            let Some(pid) = m.main_pid else { continue };
            let mib = live.gpu_mib_for_pid(pid);
            if mib == 0 {
                continue;
            }
            attributed += mib;
            let mut a = Value::obj();
            a.set("card", m.card.clone().into());
            a.set("pid", (pid as u64).into());
            a.set("used_mib", mib.into());
            a.set("used_gib", round1(mib as f64 / 1024.0).into());
            attributions.push(a);
        }
        let total: u64 = live.gpu_procs.iter().map(|(_, mib)| mib).sum();
        let mut gpu = Value::obj();
        gpu.set("processes_total_mib", total.into());
        gpu.set("unattributed_mib", total.saturating_sub(attributed).into());
        gpu.set("attributions", Value::Arr(attributions));
        gpu.set(
            "note",
            "GB10 统一内存：按 nvidia-smi 进程清单归属（docker top 翻译容器 PID）；整卡总量查询在 GB10 报 [N/A]".into(),
        );
        v.set("gpu", gpu);
        drop(guard);
        drop(live);
    }
    // 指定卡片时给出预算判定（与 enable 同口径：实测优先；容器走静态预留口径链）
    if let Some(card_name) = card {
        let cards = crate::cards::scan_cards(&app.config.cards_dir);
        let c = cards
            .iter()
            .find(|c| c.name == card_name)
            .ok_or_else(|| api_err(404, "unknown_card", &format!("不认识卡片 {card_name}")))?;
        let measured = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard
                .state
                .models
                .get(&c.name)
                .and_then(|m| m.measured_footprint_gib)
        };
        let budget = crate::ops::budget_for(app, c, &mem, measured);
        let mut b = Value::obj();
        b.set("card", c.name.clone().into());
        b.set("basis", budget.basis.clone().into());
        b.set("weights_gib", round1(budget.weights_gib).into());
        b.set("runtime_overhead_gib", round1(budget.overhead_gib).into());
        b.set("required_gib", round1(budget.required_gib).into());
        b.set("safety_margin_gib", round1(budget.margin_gib).into());
        b.set("available_gib", round1(budget.available_gib).into());
        b.set("ok", budget.ok.into());
        b.set("shortfall_gib", round1(budget.shortfall_gib).into());
        b.set("verdict", budget.verdict.clone().into());
        v.set("estimate", b);
    }
    Ok(v)
}

fn tasks_view(app: &Arc<App>) -> Value {
    let mut all: Vec<(u64, Value)> = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        guard
            .state
            .tasks
            .values()
            .map(|t| (t.created_at, task_json(t, false)))
            .collect()
    };
    // Keep local tasks visible while explicitly reporting failed store queries.
    let client = crate::shop::StoreClient::new(&app.config.store_socket_path);
    let mut store_query = Value::obj();
    match client.tasks() {
        Ok(store_tasks) => {
            store_query.set("available", true.into());
            for st in store_tasks {
                all.push((st.created_at, store_task_json(&st)));
            }
        }
        Err(error) => {
            store_query.set("available", false.into());
            let (_, code, message) = install::map_store_err(error, false);
            let mut detail = Value::obj();
            detail.set("code", code.into());
            // r18 #5：store_query.error.message 与直通/镜像边界同口径脱敏。
            detail.set("message", redact::redact_text(&message).into());
            store_query.set("error", detail);
        }
    }
    all.sort_by_key(|(ts, _)| std::cmp::Reverse(*ts));
    let mut v = Value::obj();
    v.set("store_query", store_query);
    v.set(
        "tasks",
        Value::Arr(all.into_iter().take(20).map(|(_, j)| j).collect()),
    );
    v
}

/// 商店任务 → cmsd 任务形状（字段对齐 task_json，缺的给 null；source:"store"）。
fn store_task_json(t: &crate::shop::StoreTask) -> Value {
    let mut v = Value::obj();
    v.set("id", t.run_id.clone().into());
    v.set("kind", t.kind.clone().into());
    v.set("card", t.card_id.clone().into());
    v.set("status", t.status.clone().into());
    if t.created_at == 0 {
        v.set("created_at", Value::Null);
        v.set("created_at_iso", Value::Null);
    } else {
        v.set("created_at", t.created_at.into());
        v.set("created_at_iso", iso8601(t.created_at).into());
    }
    // The store protocol does not publish a completion timestamp.
    v.set("finished_at", Value::Null);
    if t.error.is_empty() {
        v.set("error", Value::Null);
    } else {
        // r15 #3：与镜像落盘边界（install.rs 对 detail.task.error）同口径，
        // 失败任务的错误串出站前必须过脱敏。
        v.set("error", redact::redact_text(&t.error).into());
    }
    v.set("result", Value::Null);
    match t.progress {
        Some((done, total)) => {
            v.set("progress_done_bytes", done.into());
            v.set("progress_total_bytes", total.into());
        }
        None => {
            v.set("progress_done_bytes", Value::Null);
            v.set("progress_total_bytes", Value::Null);
        }
    }
    v.set("store_run_id", t.run_id.clone().into());
    v.set("source", "store".into());
    if t.last_line.is_empty() {
        v.set("last_event", Value::Null);
    } else {
        // r14 R5：商店 run 日志可能含注入凭据行，与镜像落盘边界（install.rs）
        // 和 logs_view 同口径——出站前必须过脱敏。
        let mut le = Value::obj();
        le.set("stage", "store".into());
        le.set("message", redact::redact_text(&t.last_line).into());
        v.set("last_event", le);
    }
    v
}

fn task_view(app: &Arc<App>, id: &str) -> Result<Value, ApiErr> {
    {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(task) = guard.state.tasks.get(id) {
            return Ok(task_json(task, true));
        }
    }
    let detail = crate::shop::StoreClient::new(&app.config.store_socket_path)
        .task(id)
        .map_err(|error| install::map_store_err(error, false))?;
    let mut out = store_task_json(&detail.task);
    // r14 R5：同上——商店直通回退的 detail.lines 与镜像/日志边界同口径脱敏。
    out.set(
        "lines",
        Value::Arr(
            detail
                .lines
                .into_iter()
                .map(|line| redact::redact_text(&line).into())
                .collect(),
        ),
    );
    Ok(out)
}

fn task_json(t: &crate::store::TaskRecord, with_events: bool) -> Value {
    let mut v = Value::obj();
    v.set("id", t.id.clone().into());
    v.set("kind", t.kind.clone().into());
    v.set("card", t.card.clone().into());
    if let Some(selection) = t.selection.as_ref().and_then(Value::as_obj) {
        for (key, value) in selection {
            v.set(key, value.clone());
        }
    }
    v.set("status", t.status.as_str().into());
    v.set("created_at", t.created_at.into());
    v.set("created_at_iso", iso8601(t.created_at).into());
    match t.finished_at {
        Some(ts) => {
            v.set("finished_at", ts.into());
            v.set("finished_at_iso", iso8601(ts).into());
            v.set("duration_s", ts.saturating_sub(t.created_at).into());
        }
        None => v.set("finished_at", Value::Null),
    }
    match &t.error {
        Some(e) => v.set("error", e.clone().into()),
        None => v.set("error", Value::Null),
    }
    match &t.result {
        Some(r) => v.set("result", r.clone().into()),
        None => v.set("result", Value::Null),
    }
    // P2 install 进度字段（无进度的任务给 null，字段恒在）
    match t.progress_done_bytes {
        Some(n) => v.set("progress_done_bytes", n.into()),
        None => v.set("progress_done_bytes", Value::Null),
    }
    match t.progress_total_bytes {
        Some(n) => v.set("progress_total_bytes", n.into()),
        None => v.set("progress_total_bytes", Value::Null),
    }
    // 委托门面：商店侧 run_id 与来源（cmsd 自己的任务恒 "cmsd"）
    match &t.store_run_id {
        Some(r) => v.set("store_run_id", r.clone().into()),
        None => v.set("store_run_id", Value::Null),
    }
    v.set(
        "store_context",
        t.store_context
            .as_ref()
            .map(crate::store::StoreContext::to_json)
            .unwrap_or(Value::Null),
    );
    v.set("source", "cmsd".into());
    v.set("events_count", t.events.len().into());
    // 列表视图带最近一条事件（维护模式窗口的进度行，不用拉全量事件）
    match t.events.last() {
        Some(e) => {
            let mut le = Value::obj();
            le.set("ts", e.ts.into());
            le.set("stage", e.stage.clone().into());
            le.set("message", e.message.clone().into());
            v.set("last_event", le);
        }
        None => v.set("last_event", Value::Null),
    }
    if with_events {
        v.set(
            "events",
            Value::Arr(
                t.events
                    .iter()
                    .map(|e| {
                        let mut ev = Value::obj();
                        ev.set("ts", e.ts.into());
                        ev.set("ts_iso", iso8601(e.ts).into());
                        ev.set("stage", e.stage.clone().into());
                        ev.set("message", e.message.clone().into());
                        ev
                    })
                    .collect(),
            ),
        );
    }
    v
}

/// POST /v1/switch {"target": "<card>"}：串行换胎（停旧→启新→失败回滚）。
fn switch_view(app: &Arc<App>, body: &[u8]) -> Result<Value, ApiErr> {
    let parsed = Value::parse(&String::from_utf8_lossy(body))
        .map_err(|e| api_err(400, "bad_request", &format!("请求体不是合法 JSON：{e}")))?;
    let target = parsed
        .get("target")
        .and_then(|t| t.as_str())
        .ok_or_else(|| api_err(400, "bad_request", "缺少 target 字段（目标卡片名）"))?;
    ops::switch_to(app, target).map(|tid| task_accepted(&tid, "switch", target))
}

fn logs_view(app: &Arc<App>, card: &str, tail: Option<&str>) -> Result<Value, ApiErr> {
    let selection = crate::store_adoption::selection::cached(app)
        .map_err(|e| api_err(503, "store_selection_unavailable", &e))?;
    let (physical, _) = selection
        .resolve(card)
        .map_err(|e| api_err(409, "archived_model_version", &e))?;
    let card = physical.as_str();
    let (known, engine_type, container_name) = {
        let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
        match guard.state.models.get(card) {
            Some(m) => (true, m.engine_type.clone(), m.container_name.clone()),
            None => (false, String::new(), None),
        }
    };
    if !known {
        return Err(api_err(
            404,
            "unknown_model",
            &format!("{card} 不在 cmsd 登记册里；野生引擎的日志请找它的启动方"),
        ));
    }
    let tail_n: u32 = tail.and_then(|t| t.parse().ok()).unwrap_or(100).min(1000);
    // W6：容器卡的日志在 docker（应用 stdout/stderr），不在 journald
    if engine_type == "container" {
        let name = container_name
            .or_else(|| {
                crate::cards::scan_cards(&app.config.cards_dir)
                    .into_iter()
                    .find(|c| c.name == card)
                    .and_then(|c| c.container_name)
            })
            .ok_or_else(|| api_err(500, "no_container_name", "状态里缺容器名"))?;
        let raw = app
            .docker
            .logs_tail(&name, tail_n)
            .map_err(|e| api_err(500, "docker_logs_unavailable", &e))?;
        let redacted = redact::redact_text(&raw);
        let mut v = Value::obj();
        v.set("card", card.into());
        v.set("container", name.into());
        v.set("tail", (tail_n as u64).into());
        v.set("redacted", true.into());
        v.set(
            "lines",
            Value::Arr(redacted.lines().map(|l| l.to_string().into()).collect()),
        );
        return Ok(v);
    }
    let unit = crate::store_adoption::runtime::native_unit_name(app, card)
        .map_err(|message| api_err(409, "native_journal_unverified", &message))?
        .ok_or_else(|| {
            api_err(
                409,
                "native_journal_unavailable",
                "此原生模型由独立进程运行，没有已核实的 systemd 日志单元；请查看原启动方日志",
            )
        })?;
    let raw = app
        .systemd
        .journal_tail(&unit, tail_n)
        .map_err(|e| api_err(500, "journal_unavailable", &e))?;
    let redacted = redact::redact_text(&raw);
    let mut v = Value::obj();
    v.set("card", card.into());
    v.set("unit", unit.into());
    v.set("tail", (tail_n as u64).into());
    v.set("redacted", true.into());
    v.set(
        "lines",
        Value::Arr(redacted.lines().map(|l| l.to_string().into()).collect()),
    );
    Ok(v)
}

/// SSE（T7）：重放已有事件 → 增量推送 → 终结事件后关闭。
/// 客户端断开（写失败）即退出；无静默：进行中每 15s 一个心跳注释。
fn sse_events(app: &Arc<App>, task_id: &str, writer: &mut UnixStream) {
    use std::io::Write;
    let header = "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncache-control: no-cache\r\nconnection: close\r\n\r\n";
    if writer.write_all(header.as_bytes()).is_err() {
        return;
    }
    let mut sent = 0usize;
    let mut last_hb = now_epoch();
    loop {
        let snapshot = {
            let guard = app.store.lock().unwrap_or_else(|e| e.into_inner());
            guard.state.tasks.get(task_id).map(|t| {
                (
                    t.events[sent.min(t.events.len())..].to_vec(),
                    t.status,
                    t.error.clone(),
                    t.result.clone(),
                )
            })
        };
        let Some((events, status, error, result)) = snapshot else {
            let _ = writer.write_all(b"event: error\ndata: {\"code\":\"unknown_task\"}\n\n");
            return;
        };
        for e in &events {
            let mut ev = Value::obj();
            ev.set("ts", e.ts.into());
            ev.set("stage", e.stage.clone().into());
            ev.set("message", e.message.clone().into());
            if writer
                .write_all(format!("event: progress\ndata: {ev}\n\n").as_bytes())
                .is_err()
            {
                return;
            }
        }
        sent += events.len();
        match status {
            TaskStatus::Running => {
                let _ = writer.flush();
                let now = now_epoch();
                if now >= last_hb + 15 {
                    last_hb = now;
                    if writer.write_all(b": heartbeat\n\n").is_err() {
                        return;
                    }
                    let _ = writer.flush();
                }
                std::thread::sleep(std::time::Duration::from_secs(1));
            }
            _ => {
                let mut done = Value::obj();
                done.set("status", status.as_str().into());
                if let Some(e) = error {
                    done.set("error", e.into());
                } else if let Some(r) = result {
                    done.set("result", r.into());
                }
                let _ = writer.write_all(format!("event: done\ndata: {done}\n\n").as_bytes());
                let _ = writer.flush();
                return;
            }
        }
    }
}

fn round1(x: f64) -> f64 {
    (x * 10.0).round() / 10.0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn logical_log_requests_read_current_even_when_historical_name_is_the_alias() {
        use crate::docker::fixtures as fx;
        let root = fx::tmpdir("api-current-logs");
        let app = fx::make_app(&root, 128, 100);
        for (card, container) in [("model", "old-ctr"), ("model--new", "new-ctr")] {
            ops::upsert_model_state(&app, card, |model| {
                model.engine_type = "container".into();
                model.container_name = Some(container.into());
            });
        }
        crate::store_adoption::selection::publish(&app,&serde_json::json!({"selection_policy":"latest-installed-v1","selections":[{"logical_card":"model","card":"model--new","version_id":"latest"}],"archived_models":[{"logical_card":"model","card":"model","version_id":"old"}],"models":[]})).unwrap();
        let logs = logs_view(&app, "model", Some("5")).unwrap();
        assert_eq!(logs.get("card").and_then(Value::as_str), Some("model--new"));
        assert_eq!(
            logs.get("container").and_then(Value::as_str),
            Some("new-ctr")
        );
        assert!(!fx::docker_calls(&root)
            .iter()
            .any(|call| call.contains("old-ctr")));
    }

    #[test]
    fn native_model_unit_is_the_retained_journal_source_and_unknown_is_not_a_new_unit() {
        use crate::docker::fixtures as fx;
        let root = fx::tmpdir("api-retained-native-unit");
        let mut app = fx::make_app(&root, 128, 100);
        Arc::get_mut(&mut app).unwrap().systemd = crate::systemd::Systemd::new(
            app.config.systemd_mode,
            app.config.unit_dir.clone(),
            app.config.env_dir(),
            "test".into(),
        )
        .with_state_namespace(true);
        ops::upsert_model_state(&app, "native", |_| {});
        let directory = app.config.state_dir.join("store-runtime");
        crate::private_storage::directory(&directory, true).unwrap();
        let path = directory.join("native.json");
        let mut receipt = serde_json::json!({"pid":412,"ticks":99,"exe":"/fixture/tachyon-serve", "weights":"/weights/model.tachyon","model_id":"Native","port":18199,"unit":"megacube-model@native.service","boot":false});
        {
            let mut store = app.store.lock().unwrap();
            store.state.engines.insert(
                "01234567".into(),
                crate::store::EngineInfo {
                    sha8: "01234567".into(),
                    engine_type: "native".into(),
                    path: "/fixture".into(),
                    first_seen_at: 1,
                    last_seen_at: 1,
                },
            );
            let mut live = app.live.lock().unwrap();
            live.units.insert(
                app.systemd.unit_name_for("native"),
                crate::systemd::UnitState {
                    active_state: "active".into(),
                    sub_state: "running".into(),
                    main_pid: 412,
                    ..Default::default()
                },
            );
            live.procs.push(crate::engine::ProcInfo {
                pid: 412,
                sha8: "01234567".into(),
                port: Some(18199),
                model_id: Some("Native".into()),
                start_time: 99,
            });
        }
        for unit in [
            "megacube-model@native.service",
            "megacube-model-0123456789abcdef@native.service",
        ] {
            receipt["unit"] = unit.into();
            crate::private_storage::publish(&path, receipt.to_string().as_bytes(), true).unwrap();
            let result = models(&app);
            let row = &result.get("models").unwrap().as_arr().unwrap()[0];
            assert_eq!(row.get("unit").and_then(Value::as_str), Some(unit));
            assert_ne!(unit, app.systemd.unit_name_for("native"));
            let engine_view = engines(&app);
            let engine = &engine_view.get("engines").unwrap().as_arr().unwrap()[0];
            let instance = &engine.get("instances").unwrap().as_arr().unwrap()[0];
            assert_eq!(instance.get("unit").and_then(Value::as_str), Some(unit));
        }
        receipt["unit"] = "ssh.service".into();
        crate::private_storage::publish(&path, receipt.to_string().as_bytes(), true).unwrap();
        let result = models(&app);
        let row = &result.get("models").unwrap().as_arr().unwrap()[0];
        assert_eq!(row.get("unit"), Some(&Value::Null));
        assert_eq!(
            row.get("unit_error").and_then(Value::as_str),
            Some("store_runtime_foreign_unit")
        );
        receipt["unit"] = serde_json::Value::Null;
        crate::private_storage::publish(&path, receipt.to_string().as_bytes(), true).unwrap();
        let result = models(&app);
        assert_eq!(
            result.get("models").unwrap().as_arr().unwrap()[0].get("unit"),
            Some(&Value::Null)
        );
        std::fs::remove_file(&path).unwrap();
        let result = models(&app);
        assert_eq!(
            result.get("models").unwrap().as_arr().unwrap()[0]
                .get("unit")
                .and_then(Value::as_str),
            Some(app.systemd.unit_name_for("native").as_str())
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn task_json_carries_progress_and_cancelled() {
        let t = crate::store::TaskRecord {
            id: "t-1".to_string(),
            kind: "install".to_string(),
            card: "fixture".to_string(),
            status: TaskStatus::Cancelled,
            created_at: 100,
            finished_at: Some(160),
            error: None,
            result: Some("已取消，已下载部分保留，重新安装会从断点继续".to_string()),
            events: vec![],
            progress_done_bytes: Some(1073741824),
            progress_total_bytes: Some(21944459776),
            store_run_id: Some("r-7".to_string()),
            store_context: None,
            selection: None,
        };
        let v = task_json(&t, false);
        assert_eq!(v.get("status").and_then(|x| x.as_str()), Some("cancelled"));
        assert_eq!(v.get("source").and_then(|x| x.as_str()), Some("cmsd"));
        assert_eq!(v.get("store_run_id").and_then(|x| x.as_str()), Some("r-7"));
        assert_eq!(
            v.get("progress_done_bytes").and_then(|x| x.as_u64()),
            Some(1073741824)
        );
        assert_eq!(
            v.get("progress_total_bytes").and_then(|x| x.as_u64()),
            Some(21944459776)
        );
        // 无进度的任务：字段恒在（null），前端不用猜键存不存在
        let t2 = crate::store::TaskRecord {
            progress_done_bytes: None,
            progress_total_bytes: None,
            store_run_id: None,
            store_context: None,
            selection: None,
            ..t
        };
        let v2 = task_json(&t2, false);
        assert!(v2.get("progress_done_bytes").is_some());
        assert_eq!(v2.get("progress_done_bytes"), Some(&Value::Null));
    }

    /// /v1/models：登记模型也要带 weights_complete（卡片目录+权重实物对上才 true），
    /// 调用方据此区分「已下载/未下载」，不用猜卡片在不在 cards_dir。
    #[test]
    fn models_registered_carry_weights_complete() {
        use std::path::PathBuf;
        let root = std::env::temp_dir().join(format!("cmsd-api-models-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        let cards = root.join("cards");
        let models_dir = root.join("models");
        // 两张 fixture 卡：full 权重字节精确就位，partial 缺权重
        for (name, total, write_weights) in [("full", 16u64, true), ("partial", 16u64, false)] {
            let dir = cards.join(name);
            std::fs::create_dir_all(&dir).unwrap();
            std::fs::write(
                dir.join("common.sh"),
                format!(
                    "MODEL_ID=\"{name}\"\nENGINE_BIN_SHA256=\"0123abcd\"\nTOTAL_BYTES={total}\nARTIFACT=\"w.bin\"\n"
                ),
            )
            .unwrap();
            let mdir = models_dir.join(name);
            std::fs::create_dir_all(&mdir).unwrap();
            std::fs::write(
                dir.join(".deploy-env"),
                format!(
                    "PORT=18080\nHOST=\"127.0.0.1\"\nMODEL_DIR=\"{}\"\nENGINE_DIR=\"{}\"\nARTIFACT=\"w.bin\"\nMODEL_ID=\"{name}\"\nMAX_CONCURRENCY=1\n",
                    mdir.display(),
                    root.join("engines").display()
                ),
            )
            .unwrap();
            if write_weights {
                std::fs::write(mdir.join("w.bin"), vec![0u8; total as usize]).unwrap();
            }
        }
        let cfg = crate::config::Config {
            socket_path: root.join("cmsd.sock"),
            state_dir: root.join("state"),
            gateway_listen: "127.0.0.1:0".to_string(),
            systemd_mode: crate::config::SystemdMode::User,
            native_unit_namespace: "legacy".into(),
            cards_dir: cards.clone(),
            engine_root: root.join("engines"),
            unit_dir: root.join("units"),
            pool_start: 18080,
            pool_end: 18099,
            proc_root: PathBuf::from("/proc"),
            meminfo_path: PathBuf::from("/proc/meminfo"),
            debug_available_mib: None,
            ready_timeout_secs: 1500,
            models_dir: models_dir.clone(),
            staging_dir: root.join("staging"),
            hf_endpoint: "https://hf-mirror.example".to_string(),
            docker_cli: "docker".to_string(),
            nvidia_smi_cli: "nvidia-smi".to_string(),
            store_socket_path: root.join("store.sock"),
            store_poll_ms: 50,
            store_inventory_program: None,
            store_inventory_profile: None,
        };
        let mut store = crate::store::StateStore::load(&cfg.state_file());
        for name in ["full", "partial"] {
            store.state.models.insert(
                name.to_string(),
                crate::store::ModelState {
                    lifecycle_owned: true,
                    card: name.to_string(),
                    model_id: name.to_string(),
                    engine_sha8: "0123abcd".to_string(),
                    port: 18080,
                    status: crate::store::ModelStatus::Stopped,
                    degraded_reason: None,
                    restart_count: 0,
                    last_restart_at: None,
                    last_ready_at: None,
                    enabled_at: None,
                    weights_bytes: 16,
                    measured_footprint_gib: None,
                    engine_rss_gib: None,
                    main_pid: None,
                    engine_type: "native".to_string(),
                    container_name: None,
                    image: None,
                },
            );
        }
        let systemd = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".to_string(),
        );
        let app = Arc::new(App::new(cfg, store, systemd));
        let v = models(&app);
        let arr = v.get("models").and_then(|m| m.as_arr()).unwrap();
        let by_card: std::collections::BTreeMap<_, _> = arr
            .iter()
            .filter_map(|m| m.get("card").and_then(|c| c.as_str()))
            .map(|c| {
                let m = arr
                    .iter()
                    .find(|m| m.get("card").and_then(|x| x.as_str()) == Some(c))
                    .unwrap();
                (c.to_string(), m.get("weights_complete").cloned())
            })
            .collect();
        assert!(matches!(by_card.get("full"), Some(Some(Value::Bool(true)))));
        assert!(matches!(
            by_card.get("partial"),
            Some(Some(Value::Bool(false)))
        ));
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn model_inventory_never_runs_backend_commands_under_snapshot_locks() {
        use std::os::unix::fs::PermissionsExt;
        let root = crate::docker::fixtures::tmpdir("api-cached-native-facts");
        let initial = crate::docker::fixtures::make_app(&root, 128, 100);
        let cfg = initial.config.clone();
        let executable = root.join("unexpected-systemctl");
        let marker = root.join("backend-called");
        std::fs::write(
            &executable,
            format!("#!/bin/sh\necho called > '{}'\nsleep 2\n", marker.display()),
        )
        .unwrap();
        std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o700)).unwrap();
        let backend = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".into(),
        )
        .with_test_cli(&executable);
        let app = Arc::new(App::new(
            cfg.clone(),
            crate::store::StateStore::load(&cfg.state_file()),
            backend,
        ));
        let unit = crate::systemd::UnitState {
            active_state: "inactive".into(),
            boot_enabled: Some(true),
            checked_at: 42,
            ..Default::default()
        };
        crate::ops::register_external_managed(&app, "observed-native", &unit);
        app.live
            .lock()
            .unwrap()
            .units
            .insert(crate::store::unit_name_for("observed-native"), unit);
        let response = models(&app);
        let model = &response.get("models").and_then(Value::as_arr).unwrap()[0];
        assert_eq!(
            model.get("boot_enabled").and_then(Value::as_bool),
            Some(true)
        );
        assert_eq!(
            model.get("unit_checked_at").and_then(Value::as_u64),
            Some(42)
        );
        assert!(
            !marker.exists(),
            "snapshot reads must never wait for D-Bus while holding shared state"
        );
    }
}

#[cfg(test)]
mod w6_view_tests {
    use super::*;
    use crate::docker::fixtures as fx;

    /// 造一个「在跑容器 + 容器卡 + 已登记模型」的场景（直接摆 live/store，不走任务）。
    fn scene(tag: &str) -> (std::path::PathBuf, std::sync::Arc<App>) {
        let root = fx::tmpdir(tag);
        let app = fx::make_app(&root, 128, 100);
        fx::write_docker_card(
            &root.join("cards"),
            "fixture-c",
            "fixture-c-ctr",
            30001,
            Some("0.60"),
        );
        crate::ops::register_container_managed(
            &app,
            &crate::cards::scan_cards(&app.config.cards_dir)
                .into_iter()
                .find(|c| c.name == "fixture-c")
                .unwrap(),
            &crate::docker::ContainerInfo {
                name: "fixture-c-ctr".to_string(),
                image: "fixture/img:1.0".to_string(),
                state: "running".to_string(),
                running: true,
                exit_code: 0,
                oom_killed: false,
                restart_policy: "unless-stopped".to_string(),
                host_port: Some(30001),
                container_port: Some(8000),
                pid: 4321,
                mount_sources: Vec::new(),
                args: vec![
                    "entry".to_string(),
                    "--mem-fraction-static".to_string(),
                    "0.60".to_string(),
                    "--served-model-name".to_string(),
                    "Fixture-Model".to_string(),
                ],
                ..Default::default()
            },
        );
        (root, app)
    }

    /// /v1/engines：容器实例以 source: container 列入（关联卡/策略/自启/显存归属），
    /// 未关联容器只读展示。
    #[test]
    fn engines_view_lists_containers() {
        let (root, app) = scene("enginesview");
        {
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.containers.insert(
                "fixture-c-ctr".to_string(),
                crate::docker::ContainerInfo {
                    name: "fixture-c-ctr".to_string(),
                    image: "fixture/img:1.0".to_string(),
                    state: "running".to_string(),
                    running: true,
                    exit_code: 0,
                    oom_killed: false,
                    restart_policy: "unless-stopped".to_string(),
                    host_port: Some(30001),
                    container_port: Some(8000),
                    pid: 4321,
                    mount_sources: Vec::new(),
                    args: vec!["--mem-fraction-static".to_string(), "0.60".to_string()],
                    ..Default::default()
                },
            );
            live.containers.insert(
                "wild-one".to_string(),
                crate::docker::ContainerInfo {
                    name: "wild-one".to_string(),
                    image: "other/img:2.0".to_string(),
                    state: "exited".to_string(),
                    running: false,
                    exit_code: 0,
                    oom_killed: false,
                    restart_policy: "no".to_string(),
                    host_port: None,
                    container_port: None,
                    pid: 0,
                    mount_sources: Vec::new(),
                    args: vec![],
                    ..Default::default()
                },
            );
            // 显存归属：容器 PID 集 ∩ nvidia-smi 清单
            live.container_pids
                .insert("fixture-c-ctr".to_string(), vec![4321, 4322]);
            live.gpu_procs = vec![(4321, 196), (4322, 74814), (9999, 100)];
        }
        let v = engines(&app);
        let containers = v.get("containers").and_then(|c| c.as_arr()).unwrap();
        assert_eq!(containers.len(), 2);
        let assoc = containers
            .iter()
            .find(|c| c.get("name").and_then(|x| x.as_str()) == Some("fixture-c-ctr"))
            .unwrap();
        assert_eq!(
            assoc.get("source").and_then(|x| x.as_str()),
            Some("container")
        );
        assert_eq!(
            assoc.get("card").and_then(|x| x.as_str()),
            Some("fixture-c")
        );
        assert_eq!(
            assoc.get("restart_policy").and_then(|x| x.as_str()),
            Some("unless-stopped")
        );
        assert_eq!(assoc.get("boot_enabled"), Some(&Value::Bool(true)));
        assert_eq!(assoc.get("host_port").and_then(|x| x.as_u64()), Some(30001));
        assert_eq!(
            assoc.get("gpu_used_mib").and_then(|x| x.as_u64()),
            Some(196 + 74814)
        );
        assert_eq!(
            assoc.get("mem_fraction_static").and_then(|x| x.as_f64()),
            Some(0.60)
        );
        let wild = containers
            .iter()
            .find(|c| c.get("name").and_then(|x| x.as_str()) == Some("wild-one"))
            .unwrap();
        assert_eq!(wild.get("card"), Some(&Value::Null));
        assert_eq!(wild.get("boot_enabled"), Some(&Value::Bool(false)));
        std::fs::remove_dir_all(&root).ok();
    }

    /// /v1/models：容器登记模型的 boot_enabled 看 restart 策略；合并行带容器事实状态。
    #[test]
    fn models_view_docker_rows() {
        let (root, app) = scene("modelsview");
        {
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.containers.insert(
                "fixture-c-ctr".to_string(),
                crate::docker::ContainerInfo {
                    name: "fixture-c-ctr".to_string(),
                    image: "fixture/img:1.0".to_string(),
                    state: "running".to_string(),
                    running: true,
                    restart_policy: "unless-stopped".to_string(),
                    host_port: Some(30001),
                    ..Default::default()
                },
            );
        }
        // 再加一张未登记的容器卡（合并行）
        fx::write_docker_card(&root.join("cards"), "loose-card", "loose-ctr", 30002, None);
        let v = models(&app);
        let arr = v.get("models").and_then(|m| m.as_arr()).unwrap();
        let reg = arr
            .iter()
            .find(|m| m.get("card").and_then(|x| x.as_str()) == Some("fixture-c"))
            .unwrap();
        assert_eq!(
            reg.get("engine_type").and_then(|x| x.as_str()),
            Some("container")
        );
        assert_eq!(
            reg.get("container_name").and_then(|x| x.as_str()),
            Some("fixture-c-ctr")
        );
        assert_eq!(reg.get("boot_enabled"), Some(&Value::Bool(true)));
        assert_eq!(
            reg.get("restart_policy").and_then(|x| x.as_str()),
            Some("unless-stopped")
        );
        assert!(
            reg.get("unit").is_none(),
            "容器模型不该有 systemd unit 字段"
        );
        let loose = arr
            .iter()
            .find(|m| m.get("card").and_then(|x| x.as_str()) == Some("loose-card"))
            .unwrap();
        assert_eq!(
            loose.get("engine_type").and_then(|x| x.as_str()),
            Some("container")
        );
        assert_eq!(loose.get("deployed"), Some(&Value::Bool(true)));
        assert_eq!(loose.get("managed"), Some(&Value::Bool(false)));
        assert_eq!(loose.get("can_adopt"), Some(&Value::Bool(true)));
        assert_eq!(loose.get("can_reserve_uninstall"), Some(&Value::Bool(true)));
        assert_eq!(
            loose.get("status").and_then(|x| x.as_str()),
            Some("stopped")
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// /v1/capacity：显存归属段让调用方能说出「sglang 容器静态预留/占用 ~73 GiB」。
    #[test]
    fn capacity_view_gpu_attribution() {
        let (root, app) = scene("capgpu");
        {
            let mut live = app.live.lock().unwrap_or_else(|e| e.into_inner());
            live.containers.insert(
                "fixture-c-ctr".to_string(),
                crate::docker::ContainerInfo {
                    name: "fixture-c-ctr".to_string(),
                    image: "fixture/img:1.0".to_string(),
                    state: "running".to_string(),
                    running: true,
                    restart_policy: "unless-stopped".to_string(),
                    host_port: Some(30001),
                    args: vec!["--mem-fraction-static".to_string(), "0.60".to_string()],
                    ..Default::default()
                },
            );
            live.container_pids
                .insert("fixture-c-ctr".to_string(), vec![4321, 4322]);
            live.gpu_procs = vec![(4321, 196), (4322, 74814), (9999, 100)];
        }
        let v = capacity_view(&app, None).unwrap();
        let gpu = v.get("gpu").unwrap();
        let attrs = gpu.get("attributions").and_then(|a| a.as_arr()).unwrap();
        assert_eq!(attrs.len(), 1);
        let a = &attrs[0];
        assert_eq!(a.get("card").and_then(|x| x.as_str()), Some("fixture-c"));
        assert_eq!(
            a.get("container").and_then(|x| x.as_str()),
            Some("fixture-c-ctr")
        );
        assert_eq!(a.get("used_mib").and_then(|x| x.as_u64()), Some(75010));
        // 128 GiB × 0.60 = 76.8 GiB 静态预留
        assert_eq!(
            a.get("static_reserve_gib").and_then(|x| x.as_f64()),
            Some(76.8)
        );
        assert!(a
            .get("note")
            .and_then(|x| x.as_str())
            .unwrap()
            .contains("--mem-fraction-static 0.6"));
        assert_eq!(
            gpu.get("unattributed_mib").and_then(|x| x.as_u64()),
            Some(100)
        );
        // 容器卡的预算判定走静态预留口径
        let v2 = capacity_view(&app, Some("fixture-c")).unwrap();
        let est = v2.get("estimate").unwrap();
        assert_eq!(est.get("basis").and_then(|x| x.as_str()), Some("static"));
        assert!(est
            .get("verdict")
            .and_then(|x| x.as_str())
            .unwrap()
            .contains("静态预留"));
        std::fs::remove_dir_all(&root).ok();
    }
}

#[cfg(test)]
mod store_merge_tests {
    use super::*;
    use crate::shop::fixtures as sfx;

    fn make_app(root: &std::path::Path) -> std::sync::Arc<App> {
        let cfg = crate::config::Config {
            socket_path: root.join("cmsd.sock"),
            state_dir: root.join("state"),
            gateway_listen: "127.0.0.1:0".to_string(),
            systemd_mode: crate::config::SystemdMode::User,
            native_unit_namespace: "legacy".into(),
            cards_dir: root.join("cards"),
            engine_root: root.join("engines"),
            unit_dir: root.join("units"),
            pool_start: 18080,
            pool_end: 18099,
            proc_root: std::path::PathBuf::from("/proc"),
            meminfo_path: std::path::PathBuf::from("/proc/meminfo"),
            debug_available_mib: None,
            ready_timeout_secs: 1500,
            models_dir: root.join("models"),
            staging_dir: root.join("staging"),
            hf_endpoint: "https://hf-mirror.example".to_string(),
            docker_cli: "docker".to_string(),
            nvidia_smi_cli: "nvidia-smi".to_string(),
            store_socket_path: root.join("store.sock"),
            store_poll_ms: 50,
            store_inventory_program: None,
            store_inventory_profile: None,
        };
        let store = crate::store::StateStore::load(&cfg.state_file());
        let systemd = crate::systemd::Systemd::new(
            cfg.systemd_mode,
            cfg.unit_dir.clone(),
            cfg.env_dir(),
            "test".to_string(),
        );
        std::sync::Arc::new(App::new(cfg, store, systemd))
    }

    #[test]
    fn actual_pinvou_task_remains_first_after_twenty_local_history_records() {
        let root = sfx::tmpdir("merge-real-store-time");
        let app = make_app(&root);
        let fixture = Value::parse(include_str!(
            "../tests/fixtures/pinvou-store-task-rfc3339.json"
        ))
        .unwrap();
        let epoch = fixture
            .get("created_at_epoch")
            .and_then(Value::as_u64)
            .unwrap();
        for index in 0..20 {
            let id = app.task_create("enable", &format!("history-{index}"));
            app.task_finish(&id, Ok("done".into()));
            app.store
                .lock()
                .unwrap()
                .state
                .tasks
                .get_mut(&id)
                .unwrap()
                .created_at = epoch - 100 - index;
        }
        // Feed the real producer's complete JSON through StoreClient's actual
        // HTTP/parser path, not a hand-constructed StoreTask.
        let listener = std::os::unix::net::UnixListener::bind(root.join("store.sock")).unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut reader = BufReader::new(stream.try_clone().unwrap());
            let request = http::read_request(&mut reader).unwrap().unwrap();
            assert_eq!(request.method, "GET");
            assert_eq!(request.path, "/v1/tasks");
            let mut response = Value::obj();
            response.set("tasks", Value::Arr(vec![fixture]));
            http::write_response(
                &mut stream,
                "200 OK",
                "application/json",
                response.to_string().as_bytes(),
                &[],
            )
            .unwrap();
        });
        let view = tasks_view(&app);
        server.join().unwrap();
        let tasks = view.get("tasks").and_then(Value::as_arr).unwrap();
        assert_eq!(tasks.len(), 20);
        let first = &tasks[0];
        assert_eq!(
            first.get("id").and_then(Value::as_str),
            Some("run-surface-new")
        );
        assert_eq!(first.get("source").and_then(Value::as_str), Some("store"));
        assert_eq!(
            first.get("store_run_id").and_then(Value::as_str),
            Some("run-surface-new")
        );
        assert_eq!(first.get("status").and_then(Value::as_str), Some("running"));
        assert_eq!(first.get("created_at").and_then(Value::as_u64), Some(epoch));
        assert_eq!(
            first.get("created_at_iso").and_then(Value::as_str),
            Some("2026-10-21T08:00:00Z")
        );
        assert_eq!(
            first.get("progress_done_bytes").and_then(Value::as_u64),
            Some(1024)
        );
        assert_eq!(first.get("finished_at"), Some(&Value::Null));
        std::fs::remove_dir_all(&root).unwrap();
    }

    /// r15 #3 / P3-4：商店直通回退（task_view 未命中本地任务时走商店
    /// socket）的 detail.lines 与 error 出站前脱敏。
    #[test]
    fn store_task_detail_lines_are_redacted_before_leaving_cmsd() {
        let root = sfx::tmpdir("store-detail-redact");
        let app = make_app(&root);
        let listener = std::os::unix::net::UnixListener::bind(root.join("store.sock")).unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut reader = BufReader::new(stream.try_clone().unwrap());
            let request = http::read_request(&mut reader).unwrap().unwrap();
            assert_eq!(request.path, "/v1/tasks/run-secret");
            let mut response = Value::parse(
                r#"{
                "card_id": "card", "created_at": "2026-10-21T08:00:00+00:00",
                "created_at_epoch": 1792569600, "kind": "download",
                "error": "auth failed for api_key=sk-ANTHROPUS-abc123",
                "last_line": "pull failed: token sk-ANTHROPUS-abc123 rejected",
                "progress": null, "run_id": "run-secret", "status": "failed",
                "lines": ["hf download with token sk-ANTHROPUS-abc123", "retrying"]
            }"#,
            )
            .unwrap();
            response.set(
                "lines",
                Value::Arr(vec![
                    "hf download with token sk-ANTHROPUS-abc123".into(),
                    "retrying".into(),
                ]),
            );
            http::write_response(
                &mut stream,
                "200 OK",
                "application/json",
                response.to_string().as_bytes(),
                &[],
            )
            .unwrap();
        });
        let view = task_view(&app, "run-secret").unwrap();
        server.join().unwrap();
        let lines = view.get("lines").and_then(Value::as_arr).unwrap();
        let joined = lines
            .iter()
            .map(|line| line.as_str().unwrap_or_default())
            .collect::<Vec<_>>()
            .join("\n");
        assert!(!joined.contains("sk-ANTHROPUS-abc123"), "{joined}");
        assert!(joined.contains("***"), "{joined}");
        let error = view.get("error").and_then(Value::as_str).unwrap_or("");
        assert!(!error.contains("sk-ANTHROPUS-abc123"), "{error}");
        let last_event = view
            .get("last_event")
            .and_then(|event| event.get("message"))
            .and_then(Value::as_str)
            .unwrap_or("");
        assert!(!last_event.contains("sk-ANTHROPUS-abc123"), "{last_event}");
        std::fs::remove_dir_all(&root).unwrap();
    }

    #[test]
    fn unknown_store_time_is_not_reported_as_the_epoch_or_completion_time() {
        let task = crate::shop::StoreTask {
            run_id: "original-run".into(),
            card_id: "card".into(),
            kind: "download".into(),
            status: "succeeded".into(),
            created_at: 0,
            progress: Some((4096, 4096)),
            last_line: String::new(),
            error: String::new(),
        };
        let value = store_task_json(&task);
        assert_eq!(value.get("created_at"), Some(&Value::Null));
        assert_eq!(value.get("created_at_iso"), Some(&Value::Null));
        assert_eq!(value.get("finished_at"), Some(&Value::Null));
        assert_eq!(
            value.get("status").and_then(Value::as_str),
            Some("succeeded")
        );
        assert_eq!(
            value.get("store_run_id").and_then(Value::as_str),
            Some("original-run")
        );
    }

    /// r14 R5 / r15 #3：商店直通路径的 last_event 与 error 均与镜像落盘
    /// 边界同口径脱敏。
    #[test]
    fn store_task_last_event_is_redacted_before_leaving_cmsd() {
        let task = crate::shop::StoreTask {
            run_id: "run".into(),
            card_id: "card".into(),
            kind: "download".into(),
            status: "running".into(),
            created_at: 0,
            progress: None,
            last_line: "download failed: token sk-ANTHROPUS-abc123 leaking".into(),
            error: "auth rejected for api_key=sk-ANTHROPUS-abc123".into(),
        };
        let value = store_task_json(&task);
        let message = value
            .get("last_event")
            .and_then(|event| event.get("message"))
            .and_then(Value::as_str)
            .unwrap();
        assert!(!message.contains("sk-ANTHROPUS-abc123"), "{message}");
        assert!(message.contains("***"), "{message}");
        let error = value.get("error").and_then(Value::as_str).unwrap();
        assert!(!error.contains("sk-ANTHROPUS-abc123"), "{error}");
        assert!(error.contains("***"), "{error}");
    }

    /// /v1/tasks 合并透出：cmsd 任务 source="cmsd"，商店任务 source="store"
    /// （进度/last_line/store_run_id 形状对齐）；socket 不在时只给 cmsd 自己的。
    #[test]
    fn tasks_view_merges_store_with_source() {
        let root = sfx::tmpdir("merge");
        let app = make_app(&root);
        // cmsd 自己的任务
        let tid = app.task_create("enable", "cmsd-card");
        app.task_finish(&tid, Ok("ok".to_string()));
        // 商店任务（跑着的下载 + 推进剧本）
        let store = sfx::FakeStore::start(&root, &[], &["store-card"]);
        store.script_for(
            "store-card",
            vec![
                sfx::Step::Progress(1024, 4096),
                sfx::Step::Line("LMM_PROGRESS_BYTES=1024/4096"),
            ],
        );
        // 通过门面建一个委托任务（商店侧就有 running 任务了）
        let dir = root.join("cards").join("store-card");
        std::fs::create_dir_all(&dir).unwrap();
        for f in [
            "install_engine.sh",
            "download_model.sh",
            "deploy.sh",
            "enable.sh",
            "stop.sh",
        ] {
            std::fs::write(dir.join(f), "#!/bin/sh\n").unwrap();
        }
        let _ = crate::install::install(&app, "store-card").unwrap();

        let v = tasks_view(&app);
        let tasks = v.get("tasks").and_then(|x| x.as_arr()).unwrap();
        let cmsd = tasks
            .iter()
            .find(|t| t.get("card").and_then(|x| x.as_str()) == Some("cmsd-card"))
            .expect("缺 cmsd 任务");
        assert_eq!(cmsd.get("source").and_then(|x| x.as_str()), Some("cmsd"));
        let st = tasks
            .iter()
            .find(|t| t.get("source").and_then(|x| x.as_str()) == Some("store"))
            .expect("缺 store 任务");
        assert_eq!(st.get("card").and_then(|x| x.as_str()), Some("store-card"));
        assert_eq!(st.get("status").and_then(|x| x.as_str()), Some("running"));
        assert!(st.get("store_run_id").and_then(|x| x.as_str()).is_some());
        // 排序：created_at 降序（合并后统一排序）
        let created: Vec<u64> = tasks
            .iter()
            .filter_map(|t| t.get("created_at").and_then(|x| x.as_u64()))
            .collect();
        let mut sorted = created.clone();
        sorted.sort_by_key(|x| std::cmp::Reverse(*x));
        assert_eq!(created, sorted);

        // 商店离线：静默跳过，只给 cmsd 自己的
        drop(store);
        std::fs::remove_file(root.join("store.sock")).ok();
        let v2 = tasks_view(&app);
        let tasks2 = v2.get("tasks").and_then(|x| x.as_arr()).unwrap();
        assert!(tasks2
            .iter()
            .all(|t| t.get("source").and_then(|x| x.as_str()) == Some("cmsd")));
        std::fs::remove_dir_all(&root).ok();
    }
}

#[cfg(test)]
mod container_health_tests {
    use super::*;
    use crate::docker::fixtures as fx;
    #[test]
    fn healthy_endpoint_requires_current_container_proof_without_hiding_runtime_facts() {
        let root = fx::tmpdir("container-health-proof");
        let app = fx::make_app(&root, 128, 100);
        let port = fx::serve_models_json("Declared-Model");
        fx::write_docker_card(
            &app.config.cards_dir,
            "selected",
            "selected-ctr",
            port,
            Some("0.01"),
        );
        fx::set_container(
            &root,
            "selected-ctr",
            "fixture/img:1.0",
            true,
            "always",
            0,
            false,
            4321,
            port,
            8000,
            "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
        );
        let task = crate::ops::enable(&app, "selected").unwrap();
        assert_eq!(
            fx::wait_terminal(&app, &task, 10).status,
            TaskStatus::Succeeded
        );
        let row = |value: Value| {
            value
                .get("models")
                .and_then(Value::as_arr)
                .unwrap()
                .iter()
                .find(|row| row.get("card").and_then(Value::as_str) == Some("selected"))
                .unwrap()
                .clone()
        };
        assert_eq!(
            row(models(&app)).get("healthy"),
            Some(&Value::Bool(true)),
            "enable primes a verified snapshot without waiting for supervisor"
        );
        // A cached healthy probe cannot survive disappearance of the actual
        // published endpoint, even when image and process generation still match.
        fx::set_container(
            &root,
            "selected-ctr",
            "fixture/img:1.0",
            true,
            "always",
            0,
            false,
            4321,
            0,
            8000,
            "--served-model-name,Declared-Model,--mem-fraction-static,0.01",
        );
        let unpublished = app.docker.inspect("selected-ctr").unwrap().unwrap();
        app.live
            .lock()
            .unwrap()
            .containers
            .insert("selected-ctr".into(), unpublished);
        std::fs::write(root.join("docker.calls"), "").unwrap();
        let actual = row(models(&app));
        assert_eq!(actual.get("healthy"), Some(&Value::Bool(false)));
        assert_eq!(
            actual.get("container_state").and_then(Value::as_str),
            Some("running")
        );
        assert_eq!(actual.get("managed"), Some(&Value::Bool(true)));
        assert!(fx::docker_calls(&root).is_empty());
        fx::set_container(
            &root,
            "selected-ctr",
            "foreign/image:latest",
            true,
            "always",
            0,
            false,
            8765,
            port,
            8000,
            "--served-model-name,Declared-Model",
        );
        let foreign = app.docker.inspect("selected-ctr").unwrap().unwrap();
        app.live
            .lock()
            .unwrap()
            .containers
            .insert("selected-ctr".into(), foreign);
        std::fs::write(root.join("docker.calls"), "").unwrap();
        let actual = row(models(&app));
        assert_eq!(actual.get("healthy"), Some(&Value::Bool(false)));
        assert_eq!(
            actual.get("container_state").and_then(Value::as_str),
            Some("running")
        );
        assert_eq!(actual.get("managed"), Some(&Value::Bool(true)));
        assert!(
            fx::docker_calls(&root).is_empty(),
            "snapshot view performs no CLI while holding shared state"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod status_selection_tests {
    use super::*;
    use std::io::Write;
    use std::os::unix::net::UnixListener;

    fn exchange(app: &Arc<App>, path: &str) -> (u16, serde_json::Value) {
        exchange_method(app, "GET", path, "")
    }

    fn exchange_method(
        app: &Arc<App>,
        method: &str,
        path: &str,
        body: &str,
    ) -> (u16, serde_json::Value) {
        let (mut client, server) = UnixStream::pair().unwrap();
        let app = Arc::clone(app);
        let worker = std::thread::spawn(move || handle_conn(&app, server));
        client
            .write_all(
                format!(
                    "{method} {path} HTTP/1.1\r\nhost: cmsd\r\ncontent-length: {}\r\n\r\n{body}",
                    body.len()
                )
                .as_bytes(),
            )
            .unwrap();
        let response = http::read_response(&mut BufReader::new(client)).unwrap();
        worker.join().unwrap();
        (
            response.status,
            serde_json::from_slice(&response.body).unwrap(),
        )
    }

    fn classify_diagnostic(app: &Arc<App>, requests: usize) -> (serde_json::Value, i32) {
        let listener = UnixListener::bind(&app.config.socket_path).unwrap();
        let server = Arc::clone(app);
        let worker = std::thread::spawn(move || {
            for _ in 0..requests {
                let (stream, _) = listener.accept().unwrap();
                handle_conn(&server, stream);
            }
        });
        let result = crate::diagnostics::run(&app.config);
        worker.join().unwrap();
        std::fs::remove_file(&app.config.socket_path).unwrap();
        result
    }

    fn registered(app: &Arc<App>) {
        crate::ops::upsert_model_state(app, "registered-model", |model| {
            model.lifecycle_owned = true;
            model.model_id = "Registered".into();
            model.status = ModelStatus::Running;
            model.port = 18081;
            model.main_pid = Some(12345);
        });
        app.store.lock().unwrap().state.active_model = Some("registered-model".into());
    }

    #[test]
    fn unavailable_current_selection_never_becomes_empty_status_or_diagnostic() {
        for mode in ["corrupt", "missing"] {
            let root = crate::docker::fixtures::tmpdir(&format!("status-selection-{mode}"));
            let app = crate::docker::fixtures::make_app(&root, 128, 100);
            registered(&app);
            std::fs::create_dir_all(&app.config.state_dir).unwrap();
            match mode {
                "corrupt" => {
                    std::fs::write(app.config.state_dir.join("store-selection.json"), "{broken")
                        .unwrap()
                }
                _ => std::fs::write(
                    app.config.state_dir.join("store-selection-v1"),
                    "latest-installed-v1\n",
                )
                .unwrap(),
            }
            assert_eq!(app.store.lock().unwrap().state.models.len(), 1);
            let error = status(&app).unwrap_err();
            assert_eq!(
                (error.0, error.1.as_str()),
                (503, "store_selection_unavailable")
            );
            let (code, value) = exchange(&app, "/v1/status");
            assert_eq!(code, 503);
            assert_eq!(value["error"]["code"], "store_selection_unavailable");
            assert!(value.get("models").is_none());
            assert!(value.get("degraded").is_none());
            assert!(value.get("active_model").is_none());
            println!("STATUS_ERROR_{mode}={value}");
            let (code, device) = exchange(&app, "/v1/device");
            assert_eq!(code, 200);
            assert_eq!(device["source"], "cmsd");
            assert!(device["device_id"].is_string());
            // Component rescue is independent of the unavailable model view.
            for action in ["begin", "end"] {
                let body = format!(r#"{{"owner":"status-selection-test","action":"{action}"}}"#);
                let (code, response) =
                    exchange_method(&app, "POST", "/v1/component-maintenance", &body);
                assert_eq!(code, 200);
                assert_eq!(response["maintenance"], action == "begin");
            }
            let (report, code) = classify_diagnostic(&app, 5);
            assert_eq!(code, 4);
            assert_eq!(report["outcome"], "unavailable");
            assert_eq!(report["checks"][0]["state"], "available");
            assert_eq!(report["checks"][1]["state"], "unavailable");
            assert_eq!(report["checks"][1]["code"], "service_unavailable");
            assert!(report["checks"][1]["facts"].is_null());
            assert_eq!(app.store.lock().unwrap().state.models.len(), 1);
            assert_eq!(
                app.store.lock().unwrap().state.active_model.as_deref(),
                Some("registered-model")
            );
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn healthy_empty_and_populated_status_keep_existing_contract() {
        for populated in [false, true] {
            let root = crate::docker::fixtures::tmpdir("status-selection-healthy");
            let app = crate::docker::fixtures::make_app(&root, 128, 100);
            if populated {
                registered(&app);
            }
            let expected = if populated { 1 } else { 0 };
            let direct = status(&app).unwrap();
            assert_eq!(
                direct.get("models").unwrap().as_arr().unwrap().len(),
                expected
            );
            let (code, value) = exchange(&app, "/v1/status");
            assert_eq!(code, 200);
            assert_eq!(value["service"], "cmsd");
            println!("STATUS_HEALTHY_{populated}={value}");
            assert_eq!(value["models"].as_array().unwrap().len(), expected);
            assert_eq!(
                value["active_model"].as_str(),
                populated.then_some("registered-model")
            );
            let (report, _) = classify_diagnostic(&app, 4);
            // The fixture's gateway has not listened; preserve that independent
            // attention state while validating the actual model facts.
            assert_eq!(report["checks"][1]["state"], "degraded");
            let facts = &report["checks"][1]["facts"];
            assert_eq!(facts["registered_models"], expected);
            assert_eq!(facts["managed_models"], expected);
            assert_eq!(facts["running_reported"], expected);
            assert_eq!(facts["active_selection_present"], populated);
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn initial_reader_publication_recovers_status_without_restarting_daemon() {
        let root = crate::docker::fixtures::tmpdir("status-selection-first-reader");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        std::fs::create_dir_all(&app.config.state_dir).unwrap();
        crate::private_storage::publish(
            &app.config.state_dir.join("store-reader.json"),
            br#"{"format":"pinvou-store-reader-v1","program":"/nonexecuted-review-reader","profile":"/nonexecuted-review-profile"}"#,
            false,
        ).unwrap();
        assert_eq!(exchange(&app, "/v1/status").0, 503);
        assert_eq!(exchange(&app, "/v1/device").0, 200);
        crate::store_adoption::selection::publish(&app, &serde_json::json!({
            "selection_policy":"latest-installed-v1", "selections":[], "archived_models":[], "models":[]
        })).unwrap();
        let (code, value) = exchange(&app, "/v1/status");
        assert_eq!(code, 200);
        assert_eq!(value["service"], "cmsd");
        assert_eq!(value["models"], serde_json::json!([]));
        std::fs::remove_dir_all(root).unwrap();
    }
}
