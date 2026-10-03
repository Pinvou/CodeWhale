//! Bounded FIFO execution separate from the lease/liveness control loop.

use std::collections::VecDeque;
use std::future::Future;
use std::sync::Arc;

use parking_lot::Mutex;
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;

/// One executor per authenticated generation. Transport resume clears queued
/// work but lets the current action cache its outcome before any retry. Only
/// generation termination drops/cancels the executor.
pub(super) struct SerialBusinessWorker<T> {
    queue: Arc<Mutex<VecDeque<T>>>,
    wake: Arc<Notify>,
    capacity: usize,
    cancel: CancellationToken,
    task: tokio::task::JoinHandle<()>,
}

#[derive(Debug, Eq, PartialEq)]
pub(super) enum BusinessQueueError {
    Full,
    Closed,
}

impl<T: Send + 'static> SerialBusinessWorker<T> {
    pub(super) fn start<F, Fut>(capacity: usize, mut execute: F) -> Self
    where
        F: FnMut(T) -> Fut + Send + 'static,
        Fut: Future<Output = ()> + Send,
    {
        let queue = Arc::new(Mutex::new(VecDeque::new()));
        let pending = queue.clone();
        let wake = Arc::new(Notify::new());
        let notified = wake.clone();
        let cancel = CancellationToken::new();
        let cancelled = cancel.clone();
        let task = tokio::spawn(async move {
            loop {
                let job = pending.lock().pop_front();
                let Some(job) = job else {
                    tokio::select! {
                        biased;
                        _ = cancelled.cancelled() => return,
                        _ = notified.notified() => {},
                    }
                    continue;
                };
                tokio::select! {
                    biased;
                    _ = cancelled.cancelled() => return,
                    _ = execute(job) => {},
                }
            }
        });
        Self {
            queue,
            wake,
            capacity,
            cancel,
            task,
        }
    }

    pub(super) fn try_submit(&self, job: T) -> Result<(), BusinessQueueError> {
        let mut pending = self.queue.lock();
        if self.cancel.is_cancelled() || self.task.is_finished() {
            return Err(BusinessQueueError::Closed);
        }
        if pending.len() >= self.capacity {
            return Err(BusinessQueueError::Full);
        }
        pending.push_back(job);
        self.wake.notify_one();
        Ok(())
    }

    /// A transport suspension discards only work that has not started. The
    /// in-flight action must finish and cache its outcome before a resumed
    /// retry can run on this same serial executor.
    pub(super) fn clear_pending(&self) {
        self.queue.lock().clear();
    }
}

impl<T> Drop for SerialBusinessWorker<T> {
    fn drop(&mut self) {
        self.cancel.cancel();
        self.task.abort();
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use parking_lot::Mutex;
    use tokio::sync::Notify;

    use super::*;

    #[tokio::test(start_paused = true)]
    async fn blocked_business_does_not_block_control_ticks_and_queue_is_bounded() {
        let started = Arc::new(Notify::new());
        let release = Arc::new(Notify::new());
        let worker = SerialBusinessWorker::start(1, {
            let started = started.clone();
            let release = release.clone();
            move |_: u8| {
                let started = started.clone();
                let release = release.clone();
                async move {
                    started.notify_one();
                    release.notified().await;
                }
            }
        });
        worker.try_submit(1).unwrap();
        started.notified().await;
        worker.try_submit(2).unwrap();
        assert!(matches!(
            worker.try_submit(3),
            Err(BusinessQueueError::Full)
        ));
        // The same scheduling primitive used by B's evidence loop remains
        // live through a legitimate business call longer than its 30s budget.
        let mut poll = tokio::time::interval(std::time::Duration::from_secs(2));
        poll.tick().await;
        for _ in 0..20 {
            poll.tick().await;
        }
        assert!(!worker.task.is_finished());
        drop(worker);
        release.notify_one();
    }

    #[tokio::test]
    async fn execution_is_fifo_and_drop_discards_inflight_and_queued_outputs() {
        let outputs = Arc::new(Mutex::new(Vec::new()));
        let started = Arc::new(Notify::new());
        let release = Arc::new(Notify::new());
        let done = Arc::new(Notify::new());
        let worker = SerialBusinessWorker::start(2, {
            let outputs = outputs.clone();
            let started = started.clone();
            let release = release.clone();
            let done = done.clone();
            move |job: u8| {
                let outputs = outputs.clone();
                let started = started.clone();
                let release = release.clone();
                let done = done.clone();
                async move {
                    if job == 1 {
                        started.notify_one();
                        release.notified().await;
                    }
                    outputs.lock().push(job);
                    done.notify_one();
                }
            }
        });
        worker.try_submit(1).unwrap();
        started.notified().await;
        worker.try_submit(2).unwrap();
        assert!(outputs.lock().is_empty());
        drop(worker);
        release.notify_one();
        tokio::task::yield_now().await;
        assert!(outputs.lock().is_empty());

        let replacement = SerialBusinessWorker::start(2, {
            let outputs = outputs.clone();
            let done = done.clone();
            move |job| {
                let outputs = outputs.clone();
                let done = done.clone();
                async move {
                    outputs.lock().push(job);
                    done.notify_one();
                }
            }
        });
        replacement.try_submit(3).unwrap();
        replacement.try_submit(4).unwrap();
        while outputs.lock().len() < 2 {
            done.notified().await;
        }
        assert_eq!(*outputs.lock(), vec![3, 4]);
    }
}
