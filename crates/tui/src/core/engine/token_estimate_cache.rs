//! Process-local memoization for [`crate::compaction::estimate_input_tokens_conservative`].
//!
//! The token estimator walks the full [`crate::models::Message`] history and the
//! active system prompt, which is by far the most expensive per-turn CPU cost
//! in the engine hot path. The same input data is queried from at least five
//! sites per turn: capacity pre/post tool checkpoints, error escalation,
//! the seam manager, and the trimmed-message budget check, plus four more
//! from the TUI footer, `/status`, `/debug`, and the context inspector.
//!
//! Without memoization, a 200-message history with 5 KB of tool results costs
//! ~2 ms per call; that is 20 ms of pure waste on a single turn. The estimator
//! itself is a pure function of `(messages, system_prompt)`, so a
//! content-versioned cache is safe: the caller bumps `messages_revision`
//! on every mutation, and we also include a fast fingerprint of the system
//! prompt as part of the key.
//!
//! The cache is process-local only — cross-session persistence is intentionally
//! out of scope (see PR #2520 for the cross-session prompt-base disk cache).

use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};

use crate::compaction::estimate_input_tokens_conservative;
use crate::models::{Message, SystemPrompt};

/// Process-local memoization for `estimate_input_tokens_conservative`.
///
/// The cache is keyed on the `(messages_revision, system_fingerprint)`
/// pair, both of which the engine bumps on every content change. On a hit
/// the previously stored token estimate is returned without re-walking the
/// message list. On a miss, the estimator runs and the result is stored.
#[derive(Debug, Default, Clone)]
pub struct TokenEstimateCache {
    /// Monotonic counter bumped by the engine on every message mutation.
    messages_revision: u64,
    /// Stable 64-bit hash of the current system prompt text. Computed once
    /// per `lookup_or_compute` call when the cache misses.
    system_fingerprint: u64,
    /// Cached token count, valid iff both keys match the current inputs.
    cached_tokens: Option<usize>,
}

impl TokenEstimateCache {
    /// Construct a fresh, empty cache. `messages_revision` defaults to 0;
    /// the engine passes the current revision on every
    /// [`lookup_or_compute`](Self::lookup_or_compute) call so the next
    /// lookup correctly invalidates.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Returns the cached token estimate, recomputing on miss.
    ///
    /// `messages_revision` is the engine's monotonic counter; bump it on
    /// every add/remove/clear. `system_prompt` may be `None`. `messages` is
    /// borrowed for the duration of the call so a miss can re-tokenize.
    pub fn lookup_or_compute(
        &mut self,
        messages_revision: u64,
        system_prompt: Option<&SystemPrompt>,
        messages: &[Message],
    ) -> usize {
        let system_fingerprint = fingerprint_system_prompt(system_prompt);

        if self.messages_revision == messages_revision
            && self.system_fingerprint == system_fingerprint
            && let Some(tokens) = self.cached_tokens
        {
            return tokens;
        }

        let tokens = estimate_input_tokens_conservative(messages, system_prompt);
        self.messages_revision = messages_revision;
        self.system_fingerprint = system_fingerprint;
        self.cached_tokens = Some(tokens);
        tokens
    }
}

/// Stable 64-bit hash of the system prompt text. Walks the same shape the
/// estimator consumes: a `Text` variant or a list of `Blocks`. Returns 0
/// for `None` so the empty case is distinguishable but cheap to compare.
fn fingerprint_system_prompt(system: Option<&SystemPrompt>) -> u64 {
    let Some(system) = system else {
        return 0;
    };
    let mut hasher = DefaultHasher::new();
    match system {
        SystemPrompt::Text(text) => {
            "text".hash(&mut hasher);
            text.hash(&mut hasher);
        }
        SystemPrompt::Blocks(blocks) => {
            "blocks".hash(&mut hasher);
            blocks.len().hash(&mut hasher);
            for block in blocks {
                block.block_type.hash(&mut hasher);
                block.text.hash(&mut hasher);
            }
        }
    }
    hasher.finish()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::Role;
    use crate::models::{ContentBlock, SystemBlock};

    fn user_text(s: &str) -> Message {
        Message {
            role: Role::User,
            content: vec![ContentBlock::Text {
                text: s.to_string(),
                cache_control: None,
            }],
        }
    }

    fn sys_text(s: &str) -> SystemPrompt {
        SystemPrompt::Text(s.to_string())
    }

    #[test]
    fn first_call_computes_a_positive_estimate() {
        let mut cache = TokenEstimateCache::new();
        let messages = vec![user_text("hello world")];
        let tokens = cache.lookup_or_compute(1, None, &messages);
        assert!(tokens > 0);
    }

    #[test]
    fn repeated_call_with_same_revision_returns_the_cached_estimate() {
        let mut cache = TokenEstimateCache::new();
        let messages = vec![user_text("hello world")];
        let first = cache.lookup_or_compute(1, None, &messages);
        let second = cache.lookup_or_compute(1, None, &messages);
        assert!(first > 0);
        assert_eq!(first, second);
    }

    #[test]
    fn revision_bump_recomputes_the_same_estimate() {
        let mut cache = TokenEstimateCache::new();
        let messages = vec![user_text("hi")];
        let a = cache.lookup_or_compute(1, None, &messages);
        let b = cache.lookup_or_compute(2, None, &messages);
        assert!(a > 0);
        assert_eq!(a, b);
    }

    #[test]
    fn system_prompt_change_recomputes_the_same_estimate() {
        let mut cache = TokenEstimateCache::new();
        let messages = vec![user_text("hi")];
        let a = cache.lookup_or_compute(1, Some(&sys_text("alpha")), &messages);
        let b = cache.lookup_or_compute(1, Some(&sys_text("beta")), &messages);
        assert!(a > 0);
        assert_eq!(a, b);
    }

    #[test]
    fn blocks_system_prompt_lookups_stay_consistent() {
        let blocks_a = SystemPrompt::Blocks(vec![SystemBlock {
            block_type: "text".to_string(),
            text: "alpha".to_string(),
            cache_control: None,
        }]);
        let blocks_b = SystemPrompt::Blocks(vec![SystemBlock {
            block_type: "text".to_string(),
            text: "beta".to_string(),
            cache_control: None,
        }]);
        let mut cache = TokenEstimateCache::new();
        let messages = vec![user_text("hi")];
        let a = cache.lookup_or_compute(1, Some(&blocks_a), &messages);
        let b = cache.lookup_or_compute(1, Some(&blocks_b), &messages);
        assert!(a > 0);
        assert_eq!(a, b);
    }
}
