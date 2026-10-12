//! Unit tests for the Tideline receipt stream primitives (the golden work
//! stage suite lives in `work_surface::tideline::tests` — both components
//! share the reserved `work_*` golden names).

use super::tideline_stream::TidelineReceiptState;
use crate::palette::ChromeInk;

#[test]
fn receipt_states_pair_marks_with_words_and_ink_families() {
    assert_eq!(TidelineReceiptState::Working.mark(), "●");
    assert_eq!(TidelineReceiptState::Working.word(), "working");
    assert_eq!(TidelineReceiptState::Working.ink(), ChromeInk::Active);
    assert_eq!(TidelineReceiptState::Done.ink(), ChromeInk::Outcome);
    assert_eq!(TidelineReceiptState::Caution.ink(), ChromeInk::Attention);
    assert_eq!(TidelineReceiptState::Failed.ink(), ChromeInk::Failure);
    // ASCII fallbacks exist for every mark (§2 table).
    for state in [
        TidelineReceiptState::Working,
        TidelineReceiptState::Ready,
        TidelineReceiptState::Done,
        TidelineReceiptState::Caution,
        TidelineReceiptState::Failed,
    ] {
        let mark = state.mark();
        if mark != "!" {
            assert!(
                crate::tui::glyphs::ascii_fallback(mark).is_some(),
                "{mark} needs a declared fallback"
            );
        }
    }
}
