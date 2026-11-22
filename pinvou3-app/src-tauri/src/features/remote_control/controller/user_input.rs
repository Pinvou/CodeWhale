//! Bounded controller user questions. These are preferences, never permission grants.
use serde::{Deserialize, Serialize};
use std::collections::HashSet;

pub(crate) const MAX_REQUESTS: usize = 8;
pub(crate) const MAX_QUESTIONS: usize = 3;
pub(crate) const MAX_CUSTOM_CHARS: usize = 4096;
pub(crate) const MAX_VIEW_BYTES: usize = 192 * 1024;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct InputOption {
    pub value: String,
    pub label: String,
    pub description: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct InputQuestion {
    pub id: String,
    pub header: String,
    pub question: String,
    pub options: Vec<InputOption>,
    pub multi_select: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct InputRequest {
    pub input_id: String,
    pub questions: Vec<InputQuestion>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct InputAnswer {
    pub question_id: String,
    pub option_ids: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub custom_text: Option<String>,
}

fn bounded(text: &str, max: usize) -> bool {
    !text.is_empty() && text.chars().count() <= max
}

pub(crate) fn valid_requests(requests: &[InputRequest]) -> bool {
    if requests.len() > MAX_REQUESTS
        || serde_json::to_vec(requests).map_or(true, |bytes| bytes.len() > MAX_VIEW_BYTES)
    {
        return false;
    }
    let mut ids = HashSet::new();
    requests.iter().all(|request| {
        super::protocol::is_valid_identifier(&request.input_id)
            && ids.insert(&request.input_id)
            && !request.questions.is_empty()
            && request.questions.len() <= MAX_QUESTIONS
            && request.questions.iter().enumerate().all(|(i, q)| {
                q.id == i.to_string()
                    && bounded(&q.header, 64)
                    && bounded(&q.question, 2048)
                    && (2..=4).contains(&q.options.len())
                    && q.options.iter().enumerate().all(|(i, option)| {
                        option.value == i.to_string()
                            && bounded(&option.label, 256)
                            && option.description.chars().count() <= 512
                    })
            })
    })
}

pub(crate) fn valid_answer_shape(answers: &[InputAnswer]) -> bool {
    let mut seen = HashSet::new();
    answers.len() <= MAX_QUESTIONS
        && answers.iter().all(|answer| {
            ["0", "1", "2"].contains(&answer.question_id.as_str())
                && seen.insert(&answer.question_id)
                && answer.option_ids.len() <= 4
                && answer
                    .option_ids
                    .iter()
                    .all(|id| ["0", "1", "2", "3"].contains(&id.as_str()))
                && answer.option_ids.iter().collect::<HashSet<_>>().len() == answer.option_ids.len()
                && answer.custom_text.as_ref().is_none_or(|text| {
                    !text.trim().is_empty() && text.chars().count() <= MAX_CUSTOM_CHARS
                })
        })
}
