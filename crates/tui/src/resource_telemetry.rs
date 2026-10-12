//! Output-token throughput estimation for long-running CodeWhale tasks.
//!
//! This module is a pure, side-effect-free foundation for the footer's live
//! throughput readout. It performs no I/O and no rendering; consumers (the
//! status line and event loop) wire it up directly so the estimation logic
//! can be unit-tested in isolation.

use std::time::Duration;

/// Output-token throughput for a live or completed turn.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct TokenThroughput {
    pub output_tokens: u64,
    pub elapsed_seconds: f64,
}

impl TokenThroughput {
    pub fn new(output_tokens: u64, elapsed: Duration) -> Option<Self> {
        let elapsed_seconds = elapsed.as_secs_f64();
        if output_tokens == 0 || !elapsed_seconds.is_finite() || elapsed_seconds <= 0.0 {
            return None;
        }
        Some(Self {
            output_tokens,
            elapsed_seconds,
        })
    }

    pub fn tokens_per_second(self) -> f64 {
        self.output_tokens as f64 / self.elapsed_seconds
    }
}

/// Estimate output tokens from streamed text before provider usage arrives.
///
/// Provider-reported usage remains canonical at turn completion. During a live
/// stream, this gives the footer a stable approximation without inspecting
/// provider-specific tokenizer internals.
pub fn estimate_output_tokens_from_text(text: &str) -> u64 {
    let chars = text.chars().count() as u64;
    if chars == 0 {
        0
    } else {
        chars.saturating_add(3) / 4
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- throughput -------------------------------------------------------

    #[test]
    fn token_throughput_formats_tokens_per_second() {
        let throughput = TokenThroughput::new(120, Duration::from_secs(6)).expect("throughput");
        assert_eq!(throughput.tokens_per_second(), 20.0);
    }

    #[test]
    fn token_throughput_rejects_empty_or_zero_elapsed_samples() {
        assert!(TokenThroughput::new(0, Duration::from_secs(5)).is_none());
        assert!(TokenThroughput::new(5, Duration::ZERO).is_none());
    }

    #[test]
    fn estimated_streaming_tokens_round_up_from_text_chars() {
        assert_eq!(estimate_output_tokens_from_text(""), 0);
        assert_eq!(estimate_output_tokens_from_text("abc"), 1);
        assert_eq!(estimate_output_tokens_from_text("abcd"), 1);
        assert_eq!(estimate_output_tokens_from_text("abcde"), 2);
    }
}
