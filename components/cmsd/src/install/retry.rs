//! Retry is a distinct, capability-advertised operation, never ordinary install.
use super::*;
use crate::store::{safe_store_id, valid_sha256, StoreContext, StoreRetry};

pub(super) fn parse(body: &[u8]) -> Result<StoreRetry, ApiErr> {
    let invalid = || {
        api_err(
            400,
            "invalid_install_retry",
            "需要原商店任务与完整预览 SHA-256，不能增加其它字段",
        )
    };
    let text = std::str::from_utf8(body).map_err(|_| invalid())?;
    let value = Value::parse(text).map_err(|_| invalid())?;
    let object = value.as_obj().ok_or_else(invalid)?;
    if object.len() != 2
        || object
            .keys()
            .any(|k| !["retry_store_run_id", "expected_preview_sha256"].contains(&k.as_str()))
    {
        return Err(invalid());
    }
    let original_run_id = value
        .get("retry_store_run_id")
        .and_then(Value::as_str)
        .filter(|v| safe_store_id(v))
        .ok_or_else(invalid)?;
    let expected_preview_sha256 = value
        .get("expected_preview_sha256")
        .and_then(Value::as_str)
        .filter(|v| valid_sha256(v))
        .ok_or_else(invalid)?;
    Ok(StoreRetry {
        original_run_id: original_run_id.into(),
        expected_preview_sha256: expected_preview_sha256.into(),
    })
}

pub fn install(app: &Arc<App>, card: &str, body: &[u8]) -> Result<String, ApiErr> {
    let retry = parse(body)?;
    let (logical, local) = crate::store_adoption::selection::installation_route(
        app,
        card,
        has_full_scripts(&app.config.cards_dir.join(card)),
    )?;
    if logical != card {
        return Err(api_err(
            409,
            "install_retry_not_applicable",
            "任务绑定已变化，请通过逻辑商店卡重新安装",
        ));
    }
    let client = StoreClient::new(&app.config.store_socket_path);
    // Network queries never hold Store. The atomic admission below repeats all
    // exclusions after the query and before the first durable task / request.
    let original = client
        .task(&retry.original_run_id)
        .map_err(|e| map_store_err(e, false))?
        .task;
    if original.kind == "install_local" && !local {
        return Err(api_err(
            409,
            "store_package_changed",
            "旧本地配方不可重试，请重新获取商店当前安装包",
        ));
    }
    if original.run_id != retry.original_run_id
        || original.card_id != card
        || !["download", "install_local"].contains(&original.kind.as_str())
        || !["failed", "cancelled"].contains(&original.status.as_str())
    {
        return Err(api_err(
            409,
            "install_retry_not_applicable",
            "原任务不是该卡片已失败或已取消的下载/本地安装任务，请刷新恢复预览",
        ));
    }
    let context = StoreContext {
        kind: original.kind,
        retry: Some(retry),
        cancel_requested: false,
    };
    let task_id = app.task_create_install_context(card, Some(context.clone()))?;
    let run_id = format!("cmsd-{task_id}");
    let task = task_id.clone();
    let card = card.to_string();
    let app = Arc::clone(app);
    // Publish the durable task before slow receiver/network work. A desktop
    // timeout cannot erase admission or force a user to submit another retry.
    std::thread::spawn(move || {
        match client.retry_install(context.retry.as_ref().unwrap(), &run_id) {
            Ok(receipt) if matches_receipt(&receipt, &run_id, &card, &context) => {
                app.task_event(
                    &task,
                    "store",
                    "重试已交给模型商店，保留原任务类型和部分文件；不会自动启用",
                );
            }
            Ok(_) | Err(StoreErr::Offline(_)) | Err(StoreErr::Protocol(_)) => {
                app.task_event(
                    &task,
                    "reconnect",
                    "重试请求回执尚未确认，按持久化的同一关联核实，不创建第二次安装",
                );
            }
            Err(error) => {
                let error = map_store_err(error, false);
                app.task_finish(&task, Err(error.2));
                return;
            }
        }
        mirror_worker(&app, &task, &card, &run_id);
    });
    Ok(task_id)
}

pub(super) fn matches_receipt(
    receipt: &crate::shop::StoreReceipt,
    run_id: &str,
    card: &str,
    context: &StoreContext,
) -> bool {
    receipt.run_id == run_id && receipt.card == card && receipt.kind == context.kind
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn retry_body_cannot_be_downgraded_or_add_paths_flags_or_unknown_fields() {
        let hash = "a".repeat(64);
        assert!(parse(
            format!(r#"{{"retry_store_run_id":"run-1","expected_preview_sha256":"{hash}"}}"#)
                .as_bytes()
        )
        .is_ok());
        for body in [
            r#"{"retry_store_run_id":"x"}"#.into(),
            format!(r#"{{"retry_store_run_id":"../run","expected_preview_sha256":"{hash}"}}"#),
            format!(
                r#"{{"retry_store_run_id":"x","expected_preview_sha256":"{hash}","confirmed":true}}"#
            ),
            format!(
                r#"{{"retry_store_run_id":"x","retry_store_run_id":"y","expected_preview_sha256":"{hash}"}}"#
            ),
            format!(
                r#"{{"retry_store_run_id":"x","expected_preview_sha256":"{}"}}"#,
                "A".repeat(64)
            ),
        ] {
            assert!(parse(body.as_bytes()).is_err(), "{body}");
        }
    }

    #[test]
    fn retry_context_is_atomic_and_corruption_never_drops_its_exclusion() {
        let root = crate::docker::fixtures::tmpdir("retry-context");
        let app = crate::docker::fixtures::make_app(&root, 128, 100);
        let context = StoreContext {
            kind: "install_local".into(),
            retry: Some(StoreRetry {
                original_run_id: "original-run".into(),
                expected_preview_sha256: "a".repeat(64),
            }),
            cancel_requested: false,
        };
        let task = app
            .task_create_install_context("card-a", Some(context.clone()))
            .unwrap();
        let restored = crate::store::StateStore::try_load(&app.config.state_file()).unwrap();
        let record = &restored.state.tasks[&task];
        assert_eq!(record.store_context, Some(context));
        assert_eq!(record.store_run_id, Some(format!("cmsd-{task}")));
        assert!(app.task_create_install("card-b").is_err());
        app.task_request_store_cancel(&task).unwrap();
        let mut value =
            Value::parse(&std::fs::read_to_string(app.config.state_file()).unwrap()).unwrap();
        assert_eq!(
            value
                .get("tasks")
                .unwrap()
                .get(&task)
                .unwrap()
                .get("store_context")
                .unwrap()
                .get("cancel_requested"),
            Some(&Value::Bool(true))
        );
        let Value::Obj(tasks) = (match &mut value {
            Value::Obj(v) => v.get_mut("tasks").unwrap(),
            _ => unreachable!(),
        }) else {
            panic!()
        };
        let Value::Obj(record) = tasks.get_mut(&task).unwrap() else {
            panic!()
        };
        record.insert("store_context".into(), Value::Str("corrupt".into()));
        let corrupt = value.to_string();
        std::fs::write(app.config.state_file(), &corrupt).unwrap();
        assert!(crate::store::StateStore::try_load(&app.config.state_file()).is_err());
        assert_eq!(
            std::fs::read_to_string(app.config.state_file()).unwrap(),
            corrupt
        );
    }
}
