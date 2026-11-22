//! Exact-turn user-question ownership; answers are preferences, not tool approvals.
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, OnceLock, Weak};

use deepseek_tui::tools::user_input::{UserInputRequest, UserInputResponse};
use parking_lot::Mutex;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use super::skill_materialization::RemoteTurnOwner;
use crate::features::remote_control::controller::{
    journal::sanitize_text,
    turn_budget::TurnBudget,
    user_input::{
        InputAnswer, InputOption, InputQuestion, InputRequest, MAX_REQUESTS, valid_answer_shape,
        valid_requests,
    },
};

type Turns = HashMap<String, Weak<RemoteInputTurn>>;
static TURNS: OnceLock<Mutex<Turns>> = OnceLock::new();

fn turns() -> &'static Mutex<Turns> {
    TURNS.get_or_init(Mutex::default)
}

struct Question {
    tool_id: String,
    input_id: String,
    request_hash: String,
    raw: Option<UserInputRequest>,
    view: Option<InputRequest>,
    receipt: Option<String>,
}

fn fingerprint(value: &impl serde::Serialize) -> Result<String, String> {
    let bytes = serde_json::to_vec(value).map_err(|_| "user_input_invalid".to_owned())?;
    Ok(Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

#[derive(Default)]
struct State {
    closed: bool,
    engine_turn_id: Option<String>,
    questions: Vec<Question>,
}

pub(crate) struct RemoteInputTurn {
    pub(crate) session_id: String,
    pub(crate) task_id: String,
    pub(crate) generation: u64,
    pub(crate) owner: Arc<RemoteTurnOwner>,
    cancel: CancellationToken,
    budget: TurnBudget,
    state: Mutex<State>,
}

/// Closing the executor's guard revokes authority even when an async caller
/// still holds an Arc. The weak registry never owns a live turn by itself.
pub(crate) struct InputTurnGuard(pub(crate) Arc<RemoteInputTurn>);
impl Drop for InputTurnGuard {
    fn drop(&mut self) {
        let mut state = self.0.state.lock();
        state.closed = true;
        state.questions.clear();
    }
}

impl RemoteInputTurn {
    pub(crate) fn register(
        session_id: &str,
        task_id: &str,
        generation: u64,
        owner: Arc<RemoteTurnOwner>,
        cancel: CancellationToken,
        budget: TurnBudget,
    ) -> Result<InputTurnGuard, String> {
        let mut registry = turns().lock();
        registry.retain(|_, weak| weak.upgrade().is_some_and(|turn| !turn.state.lock().closed));
        if registry.len() >= 128 || registry.contains_key(task_id) || generation == 0 {
            return Err("user_input_unavailable".into());
        }
        let turn = Arc::new(Self {
            session_id: session_id.into(),
            task_id: task_id.into(),
            generation,
            owner,
            cancel,
            budget,
            state: Mutex::default(),
        });
        registry.insert(task_id.into(), Arc::downgrade(&turn));
        Ok(InputTurnGuard(turn))
    }

    pub(crate) fn for_task(session_id: &str, task_id: &str) -> Result<Arc<Self>, String> {
        turns()
            .lock()
            .get(task_id)
            .and_then(Weak::upgrade)
            .filter(|turn| turn.session_id == session_id && turn.task_id == task_id)
            .ok_or_else(|| "user_input_unavailable".into())
    }

    pub(crate) fn for_owner(
        session_id: &str,
        owner: &Arc<RemoteTurnOwner>,
    ) -> Result<Arc<Self>, String> {
        turns()
            .lock()
            .values()
            .filter_map(Weak::upgrade)
            .find(|turn| turn.session_id == session_id && Arc::ptr_eq(&turn.owner, owner))
            .ok_or_else(|| "user_input_stale".into())
    }

    fn live(&self, state: &State) -> Result<(), String> {
        if state.closed {
            return Err("user_input_stale".into());
        }
        self.check_budget()
    }

    pub(crate) fn check_budget(&self) -> Result<(), String> {
        self.budget
            .checkpoint(&self.cancel)
            .map_err(|_| "user_input_stale".into())
    }

    fn bind_engine_turn(&self, generation: u64, turn_id: &str) -> Result<(), String> {
        let mut state = self.state.lock();
        self.live(&state)?;
        if generation != self.generation
            || turn_id.is_empty()
            || state
                .engine_turn_id
                .as_deref()
                .is_some_and(|current| current != turn_id)
        {
            return Err("user_input_stale".into());
        }
        state.engine_turn_id = Some(turn_id.into());
        Ok(())
    }

    fn record(&self, tool_id: &str, questions: Value) -> Result<(), String> {
        if tool_id.is_empty() || tool_id.len() > 128 {
            return Err("user_input_invalid".into());
        }
        let raw = UserInputRequest::from_value(&json!({"questions":questions}))
            .map_err(|_| "user_input_invalid".to_owned())?;
        if !raw.questions.iter().all(|q| {
            q.id.len() <= 128
                && q.header.chars().count() <= 64
                && q.question.chars().count() <= 2048
                && q.options
                    .iter()
                    .all(|o| o.label.chars().count() <= 256 && o.description.chars().count() <= 512)
        }) {
            return Err("user_input_invalid".into());
        }
        let request_hash = fingerprint(&raw)?;
        let mut ids = HashSet::new();
        let view = InputRequest {
            input_id: format!("input-{}", uuid::Uuid::new_v4()),
            questions: raw
                .questions
                .iter()
                .enumerate()
                .map(|(i, q)| InputQuestion {
                    id: i.to_string(),
                    header: sanitize_text(&q.header),
                    question: sanitize_text(&q.question),
                    multi_select: q.multi_select,
                    options: q
                        .options
                        .iter()
                        .enumerate()
                        .map(|(i, option)| InputOption {
                            value: i.to_string(),
                            label: sanitize_text(&option.label),
                            description: sanitize_text(&option.description),
                        })
                        .collect(),
                })
                .collect(),
        };
        if !raw.questions.iter().all(|q| ids.insert(&q.id))
            || !valid_requests(std::slice::from_ref(&view))
        {
            return Err("user_input_invalid".into());
        }
        let mut state = self.state.lock();
        self.live(&state)?;
        if let Some(existing) = state.questions.iter().find(|q| q.tool_id == tool_id) {
            // Repeated delivery of this exact live callback is not a new question.
            return if existing.request_hash == request_hash {
                Ok(())
            } else {
                Err("user_input_invalid".into())
            };
        }
        // Retain consumed IDs until this exact turn closes; never evict a
        // receipt to admit a fresh question with a reused callback ID.
        let mut pending: Vec<_> = state
            .questions
            .iter()
            .filter_map(|q| q.view.clone())
            .collect();
        pending.push(view.clone());
        if state.questions.len() >= 128 || pending.len() > MAX_REQUESTS || !valid_requests(&pending)
        {
            return Err("user_input_unavailable".into());
        }
        state.questions.push(Question {
            tool_id: tool_id.into(),
            input_id: view.input_id.clone(),
            request_hash,
            raw: Some(raw),
            view: Some(view),
            receipt: None,
        });
        Ok(())
    }

    pub(crate) fn pending(&self) -> Result<Vec<InputRequest>, String> {
        let state = self.state.lock();
        self.live(&state)?;
        Ok(state
            .questions
            .iter()
            .filter_map(|q| q.view.clone())
            .collect())
    }

    /// The caller holds the live lease/task gate. `enqueue` is one synchronous
    /// poll of the exact engine's bounded input mailbox, never an awaited RPC.
    pub(crate) fn respond(
        &self,
        input_id: &str,
        cancel: bool,
        answers: &[InputAnswer],
        enqueue: impl FnOnce(&str, Option<UserInputResponse>) -> Result<(), String>,
    ) -> Result<(), String> {
        let mut state = self.state.lock();
        self.live(&state)?;
        if !valid_answer_shape(answers) || (cancel && !answers.is_empty()) {
            return Err("user_input_invalid".into());
        }
        let question = state
            .questions
            .iter_mut()
            .find(|q| q.input_id == input_id)
            .ok_or_else(|| "user_input_stale".to_owned())?;
        let fingerprint = fingerprint(&(cancel, answers))?;
        if let Some(receipt) = &question.receipt {
            return if receipt == &fingerprint {
                Ok(())
            } else {
                Err("user_input_already_answered".into())
            };
        }
        let response = if cancel {
            None
        } else {
            Some(build_response(
                question
                    .raw
                    .as_ref()
                    .ok_or_else(|| "user_input_stale".to_owned())?,
                answers,
            )?)
        };
        enqueue(&question.tool_id, response)?;
        question.receipt = Some(fingerprint);
        question.raw = None;
        question.view = None;
        Ok(())
    }

    /// B's existing local submit/cancel path joins the same one-shot gate. A
    /// cancellation already accepted on B cannot leave an answerable A card.
    pub(crate) fn respond_local(
        &self,
        tool_id: &str,
        response: Option<UserInputResponse>,
        enqueue: impl FnOnce(&str, Option<UserInputResponse>) -> Result<(), String>,
    ) -> Result<(), String> {
        let mut state = self.state.lock();
        self.live(&state)?;
        let question = state
            .questions
            .iter_mut()
            .find(|q| q.tool_id == tool_id)
            .ok_or_else(|| "user_input_stale".to_owned())?;
        let fingerprint = format!("local:{}", fingerprint(&response)?);
        if let Some(receipt) = &question.receipt {
            return if receipt == &fingerprint {
                Ok(())
            } else {
                Err("user_input_already_answered".into())
            };
        }
        enqueue(tool_id, response)?;
        question.receipt = Some(fingerprint);
        question.raw = None;
        question.view = None;
        Ok(())
    }
}

fn build_response(
    raw: &UserInputRequest,
    answers: &[InputAnswer],
) -> Result<UserInputResponse, String> {
    if answers.len() != raw.questions.len() {
        return Err("user_input_invalid".into());
    }
    let mut output = Vec::new();
    for (index, q) in raw.questions.iter().enumerate() {
        let answer = answers
            .iter()
            .find(|answer| answer.question_id == index.to_string())
            .ok_or_else(|| "user_input_invalid".to_owned())?;
        let selected = answer.option_ids.len() + usize::from(answer.custom_text.is_some());
        if !q.multi_select && selected != 1 {
            return Err("user_input_invalid".into());
        }
        for id in &answer.option_ids {
            let option = id
                .parse::<usize>()
                .ok()
                .and_then(|i| q.options.get(i))
                .ok_or_else(|| "user_input_invalid".to_owned())?;
            output.push(json!({"id":q.id,"label":option.label,"value":option.label}));
        }
        // Match the existing product's always-available custom reply policy.
        if let Some(text) = &answer.custom_text {
            output.push(json!({"id":q.id,"label":"Other","value":text}));
        }
    }
    serde_json::from_value(json!({"answers":output})).map_err(|_| "user_input_invalid".into())
}

/// Called only at the native event producer, before its ordinary UI emission.
/// No remote owner means the ordinary local question path is untouched.
pub(crate) fn bind_engine_turn(
    session_id: &str,
    task_id: &str,
    generation: u64,
    turn_id: &str,
) -> Result<(), String> {
    RemoteInputTurn::for_task(session_id, task_id)?.bind_engine_turn(generation, turn_id)
}

pub(crate) fn record_for_turn(
    session_id: &str,
    generation: u64,
    turn_id: &str,
    id: &str,
    questions: Value,
) -> Result<(), String> {
    let candidate = turns()
        .lock()
        .values()
        .filter_map(Weak::upgrade)
        .find(|turn| turn.session_id == session_id && !turn.state.lock().closed);
    if let Some(turn) = candidate {
        if turn.generation != generation
            || turn.state.lock().engine_turn_id.as_deref() != Some(turn_id)
        {
            return Err("user_input_stale".into());
        }
        turn.record(id, questions)?;
    }
    Ok(())
}

pub(crate) fn finish_question(session_id: &str, generation: u64, turn_id: &str, id: &str) {
    let candidates: Vec<_> = turns()
        .lock()
        .values()
        .filter_map(Weak::upgrade)
        .filter(|turn| turn.session_id == session_id && turn.generation == generation)
        .collect();
    for turn in candidates {
        let mut state = turn.state.lock();
        if state.engine_turn_id.as_deref() != Some(turn_id) {
            continue;
        }
        if let Some(q) = state.questions.iter_mut().find(|q| q.tool_id == id) {
            if q.receipt.is_none() {
                q.receipt = Some("closed".into());
            }
            q.raw = None;
            q.view = None;
        }
    }
}

#[cfg(test)]
#[path = "remote_user_input_tests.rs"]
mod tests;
