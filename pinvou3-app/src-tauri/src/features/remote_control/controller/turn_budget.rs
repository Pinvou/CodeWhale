//! B-local total turn deadline, shared by the chat and ACP adapters.
//!
//! The deadline starts before admission/submission and is never refreshed by
//! busy/activity observations. A controls neither this setting nor the clock.

use std::future::Future;
use std::time::Duration;

use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

pub(crate) const TURN_TIMEOUT_CODE: &str = "task_turn_timeout";
pub(crate) const TURN_CANCELLED_CODE: &str = "task_cancelled";
pub(crate) const TURN_TIMEOUT_ENV: &str = "PINVOU_REMOTE_TASK_TURN_TIMEOUT_SECS";
const DEFAULT_SECONDS: u64 = 30 * 60;
const MAX_SECONDS: u64 = 24 * 60 * 60;
// Cleanup may wait for an existing pool gate, but must not block task settlement
// forever. The original identity remains mandatory even after this bounded wait.
pub(crate) const CANCEL_SETTLE_BUDGET: Duration = Duration::from_secs(5);

pub(crate) struct TurnBudget {
    deadline: Instant,
}

impl TurnBudget {
    #[cfg(test)]
    pub(crate) fn for_test(duration: Duration) -> Self {
        Self {
            deadline: Instant::now() + duration,
        }
    }

    pub(crate) fn from_environment() -> Result<Self, String> {
        let seconds = match std::env::var(TURN_TIMEOUT_ENV) {
            Ok(value) => timeout_seconds(Some(&value))?,
            Err(std::env::VarError::NotPresent) => DEFAULT_SECONDS,
            Err(std::env::VarError::NotUnicode(_)) => {
                return Err("task_turn_timeout_config_invalid".into());
            }
        };
        Ok(Self {
            deadline: Instant::now() + Duration::from_secs(seconds),
        })
    }

    pub(crate) async fn run<T>(
        &self,
        cancel: &CancellationToken,
        future: impl Future<Output = Result<T, String>>,
    ) -> Result<T, String> {
        tokio::select! {
            biased;
            _ = cancel.cancelled() => Err(TURN_CANCELLED_CODE.into()),
            _ = tokio::time::sleep_until(self.deadline) => Err(TURN_TIMEOUT_CODE.into()),
            result = future => {
                // Select cannot preempt synchronous work in one poll. Reject
                // overdue/cancelled completion as soon as control returns.
                if cancel.is_cancelled() {
                    Err(TURN_CANCELLED_CODE.into())
                } else if Instant::now() >= self.deadline {
                    Err(TURN_TIMEOUT_CODE.into())
                } else {
                    result
                }
            },
        }
    }
}

fn timeout_seconds(value: Option<&str>) -> Result<u64, String> {
    let Some(value) = value else {
        return Ok(DEFAULT_SECONDS);
    };
    if value.is_empty() || value.len() > 5 || !value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err("task_turn_timeout_config_invalid".into());
    }
    value
        .parse::<u64>()
        .ok()
        .filter(|seconds| (1..=MAX_SECONDS).contains(seconds))
        .ok_or_else(|| "task_turn_timeout_config_invalid".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn ready_submission_after_deadline_must_not_be_accepted() {
        let budget = TurnBudget {
            deadline: Instant::now() + Duration::from_millis(20),
        };
        let cancel = CancellationToken::new();
        let result = budget
            .run(&cancel, async {
                std::thread::sleep(Duration::from_millis(40));
                Ok(())
            })
            .await;
        assert_eq!(result.unwrap_err(), TURN_TIMEOUT_CODE);
    }

    #[tokio::test]
    async fn ready_submission_that_cancels_itself_must_not_be_accepted() {
        let budget = TurnBudget {
            deadline: Instant::now() + Duration::from_secs(30),
        };
        let cancel = CancellationToken::new();
        let result = budget
            .run(&cancel, async {
                cancel.cancel();
                Ok(())
            })
            .await;
        assert_eq!(result.unwrap_err(), TURN_CANCELLED_CODE);
    }

    #[test]
    fn configuration_is_bounded_and_defaults_to_thirty_minutes() {
        assert_eq!(timeout_seconds(None).unwrap(), 1800);
        assert_eq!(timeout_seconds(Some("60")).unwrap(), 60);
        assert_eq!(timeout_seconds(Some("86400")).unwrap(), 86400);
        for value in [
            "",
            "0",
            "86401",
            "-1",
            "1.5",
            " 60",
            "60 ",
            "garbage",
            "99999999999999999",
        ] {
            assert_eq!(
                timeout_seconds(Some(value)).unwrap_err(),
                "task_turn_timeout_config_invalid"
            );
        }
    }

    #[tokio::test(start_paused = true)]
    async fn one_deadline_covers_submission_and_busy_wait_without_refresh() {
        let budget = TurnBudget {
            deadline: Instant::now() + Duration::from_secs(1800),
        };
        let cancel = CancellationToken::new();
        budget
            .run(&cancel, async {
                tokio::time::sleep(Duration::from_secs(1790)).await;
                Ok(())
            })
            .await
            .unwrap();
        let error = budget
            .run(&cancel, async {
                tokio::time::sleep(Duration::from_secs(11)).await;
                Ok(())
            })
            .await
            .unwrap_err();
        assert_eq!(error, TURN_TIMEOUT_CODE);
    }

    #[tokio::test(start_paused = true)]
    async fn pending_submission_is_bounded_and_pre_cancelled_never_starts() {
        let budget = TurnBudget {
            deadline: Instant::now() + Duration::from_secs(30),
        };
        let cancel = CancellationToken::new();
        assert_eq!(
            budget
                .run::<()>(&cancel, std::future::pending())
                .await
                .unwrap_err(),
            TURN_TIMEOUT_CODE
        );
        cancel.cancel();
        assert_eq!(
            budget
                .run(&cancel, async {
                    panic!("cancelled request must not be submitted");
                    #[allow(unreachable_code)]
                    Ok(())
                })
                .await
                .unwrap_err(),
            TURN_CANCELLED_CODE
        );
    }
}
