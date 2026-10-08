//! Redact only controller-owned panic payloads, without holding thread-local
//! state across await or exchanging the process hook for each request.
use std::cell::Cell;
use std::future::Future;
use std::pin::Pin;
use std::sync::Once;
use std::task::{Context, Poll};

thread_local! {
    static REDACT_DEPTH: Cell<u32> = const { Cell::new(0) };
}

struct Scope(u32);

impl Scope {
    fn enter() -> Self {
        static INSTALL: Once = Once::new();
        INSTALL.call_once(|| {
            crate::core::panic_hooks::chain(move |_| {
                if REDACT_DEPTH.with(|depth| depth.get() > 0) {
                    eprintln!("[remote-control] task panic: payload withheld");
                    true
                } else {
                    false
                }
            });
        });
        Self(REDACT_DEPTH.with(|depth| {
            let old = depth.get();
            depth.set(old.saturating_add(1));
            old
        }))
    }
}

impl Drop for Scope {
    fn drop(&mut self) {
        REDACT_DEPTH.with(|depth| depth.set(self.0));
    }
}

pub(super) fn run<T>(action: impl FnOnce() -> T) -> T {
    let _scope = Scope::enter();
    action()
}

pub(super) struct GuardedFuture<F> {
    inner: Option<Pin<Box<F>>>,
}

pub(super) fn future<F: Future>(future: F) -> GuardedFuture<F> {
    GuardedFuture {
        inner: Some(Box::pin(future)),
    }
}

impl<F: Future> Future for GuardedFuture<F> {
    type Output = F::Output;

    fn poll(self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        // A completed wrapper is fused: never poll/drop its old inner again.
        let Some(inner) = this.inner.as_mut() else {
            return Poll::Pending;
        };
        let result = run(|| inner.as_mut().poll(context));
        if result.is_ready() {
            // Destructor panics are part of a completed dispatch and must reach
            // its outer catch_unwind, with the same payload redaction.
            run(|| drop(this.inner.take()));
        }
        result
    }
}

impl<F: Future> futures_util::future::FusedFuture for GuardedFuture<F> {
    fn is_terminated(&self) -> bool {
        self.inner.is_none()
    }
}

impl<F> Drop for GuardedFuture<F> {
    fn drop(&mut self) {
        // A dropped Pending dispatch has no result channel. Redact and contain
        // destructor panic, including while unwinding from an earlier panic.
        // This is not a promise that aborting a dispatch committed its terminal.
        if std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            run(|| drop(self.inner.take()))
        }))
        .is_err()
        {
            eprintln!("[remote-control] task future cleanup panicked");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures_util::FutureExt;
    use std::future::poll_fn;
    use std::panic::{AssertUnwindSafe, catch_unwind};

    fn test_filter(name: &str) -> String {
        // libtest lists module paths without the crate-name prefix returned
        // by module_path!, both in the product and in the standalone harness.
        let (_, module) = module_path!().split_once("::").expect("test module path");
        format!("{module}::{name}")
    }

    #[test]
    fn panic_payload_is_redacted_in_real_stderr_and_other_panics_are_forwarded() {
        const CHILD: &str = "PINVOU_CONTROLLER_PANIC_SCOPE_CHILD";
        const SECRET: &str = "remote-task-private-panic-sentinel";
        if std::env::var_os(CHILD).is_some() {
            std::panic::set_hook(Box::new(|_| eprintln!("unrelated_panic_hook_forwarded")));
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            runtime.block_on(async {
                let result = AssertUnwindSafe(future(async {
                    tokio::task::yield_now().await;
                    panic!("{SECRET}");
                }))
                .catch_unwind()
                .await;
                assert!(result.is_err());
                assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
                struct PanicDrop;
                impl Drop for PanicDrop {
                    fn drop(&mut self) {
                        panic!("{SECRET}");
                    }
                }
                struct ReadyDropFuture(PanicDrop);
                impl Future for ReadyDropFuture {
                    type Output = ();
                    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                        let _ = &self.0;
                        Poll::Ready(())
                    }
                }
                let completed = AssertUnwindSafe(future(ReadyDropFuture(PanicDrop)))
                    .catch_unwind()
                    .await;
                assert!(completed.is_err());
                let captured = PanicDrop;
                let mut pending = Box::pin(future(async move {
                    let _ = &captured;
                    std::future::pending::<()>().await;
                }));
                let mut context = Context::from_waker(futures_util::task::noop_waker_ref());
                assert!(pending.as_mut().poll(&mut context).is_pending());
                drop(pending);
                assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
                struct PanicPollDropFuture(PanicDrop);
                impl Future for PanicPollDropFuture {
                    type Output = ();
                    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                        let _ = &self.0;
                        panic!("{SECRET}");
                    }
                }
                let double = AssertUnwindSafe(future(PanicPollDropFuture(PanicDrop)))
                    .catch_unwind()
                    .await;
                assert!(double.is_err());
                assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
            });
            assert!(catch_unwind(|| run(|| panic!("{SECRET}"))).is_err());
            assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
            assert!(catch_unwind(|| panic!("unrelated")).is_err());
            return;
        }
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                test_filter(
                    "panic_payload_is_redacted_in_real_stderr_and_other_panics_are_forwarded",
                )
                .as_str(),
                "--nocapture",
            ])
            .env(CHILD, "1")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "isolated panic scope regression failed"
        );
        assert!(
            String::from_utf8(output.stdout)
                .unwrap()
                .contains("1 passed"),
            "isolated panic scope test matched zero cases"
        );
        let stderr = String::from_utf8(output.stderr).unwrap();
        assert!(
            !stderr.contains(SECRET),
            "private panic payload reached stderr"
        );
        assert_eq!(stderr.matches("task panic: payload withheld").count(), 6);
        assert_eq!(stderr.matches("unrelated_panic_hook_forwarded").count(), 1);
    }

    #[test]
    fn concurrent_application_hook_registration_keeps_every_handler() {
        const CHILD: &str = "PINVOU_CONTROLLER_HOOK_CHAIN_CHILD";
        if std::env::var_os(CHILD).is_some() {
            std::panic::set_hook(Box::new(|_| {}));
            let barrier = std::sync::Arc::new(std::sync::Barrier::new(8));
            let counts = std::sync::Arc::new(
                (0..8)
                    .map(|_| std::sync::atomic::AtomicUsize::new(0))
                    .collect::<Vec<_>>(),
            );
            let workers = (0..8)
                .map(|index| {
                    let barrier = barrier.clone();
                    let counts = counts.clone();
                    std::thread::spawn(move || {
                        barrier.wait();
                        crate::core::panic_hooks::chain(move |_| {
                            counts[index].fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                            false
                        });
                    })
                })
                .collect::<Vec<_>>();
            for worker in workers {
                worker.join().unwrap();
            }
            assert!(catch_unwind(|| panic!("chain fixture")).is_err());
            assert!(
                counts
                    .iter()
                    .all(|count| count.load(std::sync::atomic::Ordering::SeqCst) == 1)
            );
            return;
        }
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                test_filter("concurrent_application_hook_registration_keeps_every_handler")
                    .as_str(),
                "--nocapture",
            ])
            .env(CHILD, "1")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "concurrent application hook registration lost a handler"
        );
        assert!(
            String::from_utf8(output.stdout)
                .unwrap()
                .contains("1 passed"),
            "isolated hook chain test matched zero cases"
        );
    }

    #[test]
    fn pending_poll_restores_thread_scope() {
        use std::task::{Context, Poll};
        let mut guarded = Box::pin(future(poll_fn(|_| {
            assert_eq!(REDACT_DEPTH.with(Cell::get), 1);
            Poll::<()>::Pending
        })));
        let mut context = Context::from_waker(futures_util::task::noop_waker_ref());
        assert!(guarded.as_mut().poll(&mut context).is_pending());
        assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
        drop(guarded);
        assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
    }

    #[test]
    fn completed_poll_is_fused_without_repolling_or_leaking_the_redaction_scope() {
        use futures_util::future::FusedFuture;
        let polls = Cell::new(0);
        let mut guarded = Box::pin(future(poll_fn(|_| {
            polls.set(polls.get() + 1);
            Poll::Ready(42)
        })));
        let mut context = Context::from_waker(futures_util::task::noop_waker_ref());
        assert!(!guarded.is_terminated());
        assert_eq!(guarded.as_mut().poll(&mut context), Poll::Ready(42));
        assert!(guarded.is_terminated());
        assert!(guarded.as_mut().poll(&mut context).is_pending());
        assert_eq!(polls.get(), 1);
        assert_eq!(REDACT_DEPTH.with(Cell::get), 0);
    }
}
