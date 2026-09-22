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
/// merged on the wire.
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
/// relocate or merge. A user cannot type this shape, so an ordinary turn that
/// pastes the whole summary header after a tool result keeps its position.
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
