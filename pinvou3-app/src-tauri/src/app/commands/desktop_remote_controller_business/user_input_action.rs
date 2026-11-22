//! Native user questions use the existing authenticated action/task boundaries.
use super::*;
use crate::features::assistant::{engine_pool::EnginePool, remote_user_input::RemoteInputTurn};
use crate::features::remote_control::controller::user_input::InputAnswer;
use deepseek_tui::tools::user_input::UserInputResponse;

pub(super) type InputSender =
    Box<dyn FnOnce(&str, Option<UserInputResponse>) -> Result<(), String> + Send>;

pub(super) struct UserInputActionHandler<'a> {
    pub(super) controlled: &'a ControlledDeviceManager,
    pub(super) tasks: &'a RemoteTaskService,
}

pub(super) async fn handle(
    app: &AppHandle,
    lease: &LeaseContext,
    session_ref: &str,
    operation: &str,
    body: &Value,
) -> Result<Value, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    let tasks = app
        .state::<Arc<super::super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
        .service();
    let pool = app.state::<EnginePool>();
    UserInputActionHandler {
        controlled: &controlled,
        tasks: &tasks,
    }
    .handle(lease, session_ref, operation, body, |turn| async move {
        let engine = pool
            .remote_input_handle(&turn.session_id, &turn.owner)
            .await
            .ok_or_else(|| "user_input_stale".to_owned())?;
        Ok(Box::new(move |id: &str, response| {
            engine.try_remote_user_input(turn.generation, id, response, || turn.check_budget())
        }) as InputSender)
    })
    .await
}

impl UserInputActionHandler<'_> {
    pub(super) async fn handle<F, Fut>(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        operation: &str,
        body: &Value,
        capture: F,
    ) -> Result<Value, String>
    where
        F: FnOnce(Arc<RemoteInputTurn>) -> Fut + Send,
        Fut: std::future::Future<Output = Result<InputSender, String>> + Send,
    {
        if !matches!(operation, "user_input_list" | "user_input_respond") {
            return Err("remote_action_invalid".into());
        }
        let controlled = self.controlled;
        let tasks = self.tasks;
        let task_id = body
            .get("task_id")
            .and_then(Value::as_str)
            .ok_or_else(|| "remote_action_invalid".to_owned())?;
        let (session_id, record) = controlled
            .with_current_business_lease(lease, || {
                let session_id = tasks
                    .session_for(session_ref, &lease.remote_control_generation)
                    .ok_or_else(|| "task_session_binding_mismatch".to_owned())?;
                let record = tasks
                    .ledger()
                    .record(task_id)
                    .map_err(|e| e.code.to_owned())?;
                if record.session_id != session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                Ok((session_id, record))
            })
            .map_err(|e| e.code.to_owned())??;
        let turn = RemoteInputTurn::for_task(&session_id, task_id)?;
        let enqueue = if operation == "user_input_respond" {
            Some(capture(turn.clone()).await?)
        } else {
            None
        };
        // Revalidate after the pool await. Keep the lease, cancellation decision,
        // task incarnation and synchronous mailbox enqueue in one gated operation.
        controlled
            .with_current_business_lease(lease, || {
                tasks
                    .ledger()
                    .with_live_permission_task(task_id, record.sequence, &session_id, lease, || {
                        if operation == "user_input_list" {
                            return turn.pending().map(|requests| json!({"requests":requests}));
                        }
                        let input_id = body
                            .get("input_id")
                            .and_then(Value::as_str)
                            .ok_or_else(|| "user_input_invalid".to_owned())?;
                        let cancel = body
                            .get("cancel")
                            .and_then(Value::as_bool)
                            .ok_or_else(|| "user_input_invalid".to_owned())?;
                        let answers: Vec<InputAnswer> =
                            serde_json::from_value(body["answers"].clone())
                                .map_err(|_| "user_input_invalid".to_owned())?;
                        let enqueue = enqueue.ok_or_else(|| "user_input_stale".to_owned())?;
                        turn.respond(input_id, cancel, &answers, |id, response| {
                            enqueue(id, response)
                        })?;
                        Ok(json!({"state":"accepted"}))
                    })
                    .map_err(|e| e.code.to_owned())?
            })
            .map_err(|e| e.code.to_owned())?
    }
}

#[cfg(test)]
#[path = "user_input_action_tests.rs"]
mod tests;
