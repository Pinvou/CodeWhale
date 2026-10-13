//! Structural recognition of the compaction checkpoint in saved history.
//!
//! Leaf module shared by `compaction` and `runtime_handoff`. Hosting the
//! checkpoint-recognition family here keeps `runtime_handoff`'s production
//! code from importing `compaction` (its tests still build carriers through
//! `compaction`'s constructors): `compaction` already imports
//! `runtime_handoff` for restore-time topology relocation, so a reverse edge
//! would close a module dependency cycle.

use crate::models::{ContentBlock, Message, Role};

/// Detection marker for committed compaction-summary text: the stable first
/// sentence of the summary header `compaction` commits (`SUMMARY_HEADER`).
/// `engine/context.rs` restores summaries by the same marker on session load.
pub const COMPACTION_SUMMARY_MARKER: &str = "Another language model started to solve this problem";
/// Marker written by pre-v0.9.6 compaction; sessions saved under the old
/// format must still be recognized so their summary is replaced, not stacked.
pub const LEGACY_COMPACTION_SUMMARY_MARKER: &str = "Conversation Summary (Auto-Generated)";
pub(crate) const COMPACTION_CHECKPOINT_PROVENANCE: &str =
    "<!-- codewhale.compaction-checkpoint.v1 -->";

/// Whether the first text block carries the summary header, current or legacy.
fn has_compaction_summary_header(text: &str) -> bool {
    // `COMPACTION_SUMMARY_MARKER` is the current header's first sentence, and
    // releases before this one also committed carriers with their own body
    // after that sentence, so the prefix — not the whole header — is the
    // stable shape.
    text.starts_with(COMPACTION_SUMMARY_MARKER)
        || text.starts_with(LEGACY_COMPACTION_SUMMARY_MARKER)
}

/// Structural recognition of the one generated checkpoint in saved history.
///
/// The marker substring scans in `compaction` (`extract_compaction_summary`,
/// `strip_summary_text`) stay scoped to system-prompt carriers: on history
/// they match an ordinary user turn that merely *quotes* the header, and
/// every consumer here either deletes or replaces what it matches.
/// Structure instead — a `role="user"` message whose first text block begins
/// with the header and whose remaining block, if any, is exactly the
/// engine-written provenance marker.
///
/// Boundary: the single-block form has no provenance to check, because
/// releases before this one saved the bare summary, and a reload that failed
/// to recognize it would stack a second summary beside it. A user turn that
/// *begins* with the header is therefore still read as a carrier here. Request
/// rewriting does not share that reading — see
/// [`is_generated_compaction_checkpoint`] — so such a turn is never moved or
/// merged on the wire, and history-aware consumers reclassify through
/// [`CheckpointEra`], which reads the loose form only when the history has no
/// stamped carrier. The remaining unconditional loose readers count
/// existence (`inspect_compaction_keep.has_checkpoint`,
/// `last_round_kept_count`'s gate), where both eras answer identically
/// because every stamped carrier also matches this form.
#[must_use]
pub(crate) fn is_compaction_checkpoint_message(message: &Message) -> bool {
    let [
        ContentBlock::Text {
            text,
            cache_control: None,
        },
        rest @ ..,
    ] = message.content.as_slice()
    else {
        return false;
    };
    message.role == Role::User
        && has_compaction_summary_header(text)
        && (rest.is_empty()
            || matches!(
                rest,
                [ContentBlock::Text {
                    text: provenance,
                    cache_control: None,
                }] if provenance == COMPACTION_CHECKPOINT_PROVENANCE
            ))
}

/// The provenance-stamped form, and the only form request rewriting may
/// relocate or merge. Typed input cannot produce this shape — the provenance
/// marker is its own content block and the only writer is
/// [`crate::compaction::compaction_checkpoint_message`] — so an ordinary turn
/// that pastes the whole summary header after a tool result keeps its
/// position. Only a hand-edited session file can forge the two-block form,
/// and doing so flips recognition for that one message, not the protocol.
#[must_use]
pub(crate) fn is_generated_compaction_checkpoint(message: &Message) -> bool {
    let [
        ContentBlock::Text {
            cache_control: None,
            ..
        },
        ContentBlock::Text {
            text: provenance,
            cache_control: None,
        },
    ] = message.content.as_slice()
    else {
        return false;
    };
    provenance == COMPACTION_CHECKPOINT_PROVENANCE && is_compaction_checkpoint_message(message)
}

/// Which checkpoint shape a saved history's recognition family may match.
///
/// The era signal is the history's content, not the session's age: a history
/// that *contains* a stamped carrier is read post-provenance — the engine
/// writes every carrier it produces stamped, so a user turn that merely
/// pastes the summary header is genuine content there. A history without a
/// stamped carrier carries none the engine wrote (never compacted, saved
/// before the stamp existed, or the stamp was truncated away — see below);
/// there the pasted header is indistinguishable from the saved bare carrier,
/// so the loose predicate keeps applying.
///
/// The signal is one-way and destructible: `/edit` and host rewinds cut at
/// user turns, so they can truncate away the only stamped carrier and drop
/// the history back to the legacy reading, where the restore-time loose
/// retain would drop a pasted-header turn on the next reload-with-checkpoint.
/// Foreign imports carry no codewhale stamp at all and stay legacy. Derive
/// the era once per history with [`Self::of_history`] and route every
/// per-message checkpoint classification through [`Self::is_checkpoint`] —
/// classifying messages one at a time without the history is exactly the
/// trap that let a pasted turn be dropped, mis-edited, and unretained
/// (issue pinvou-agent#594).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct CheckpointEra {
    post_provenance: bool,
}

impl CheckpointEra {
    /// The pre-provenance era: loose recognition. For callers that classify a
    /// message with no history in sight, and for tests pinning the legacy
    /// shape; history-aware callers must derive the era instead.
    pub(crate) const LEGACY: Self = Self {
        post_provenance: false,
    };

    /// The post-provenance era: stamped-only recognition. For validators that
    /// hold one history to a standard derived from another (the survival
    /// receipt floor reads a post-provenance original's replacement stamped,
    /// whatever the replacement's own shape); normal consumers derive the era
    /// with [`Self::of_history`].
    pub(crate) const POST_PROVENANCE: Self = Self {
        post_provenance: true,
    };

    #[must_use]
    pub(crate) fn of_history(history: &[Message]) -> Self {
        Self {
            post_provenance: history.iter().any(is_generated_compaction_checkpoint),
        }
    }

    /// Whether this era reads only the stamped form.
    #[must_use]
    pub(crate) fn is_post_provenance(&self) -> bool {
        self.post_provenance
    }

    /// Whether `message` is a compaction checkpoint of this history. In a
    /// post-provenance history only the stamped carrier qualifies; a bare
    /// header-prefixed turn is user content.
    #[must_use]
    pub(crate) fn is_checkpoint(&self, message: &Message) -> bool {
        if self.post_provenance {
            is_generated_compaction_checkpoint(message)
        } else {
            is_compaction_checkpoint_message(message)
        }
    }
}
