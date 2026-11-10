//! Retire only the sender owned by an unexpectedly ended transport task.
use std::panic::{AssertUnwindSafe, catch_unwind};

use super::*;

pub(super) struct TaskLifetimeGuard {
    sender: ControllerTransportSender,
    sink: Arc<dyn ControllerEventSink>,
}

impl TaskLifetimeGuard {
    pub(super) fn new(
        sender: ControllerTransportSender,
        sink: Arc<dyn ControllerEventSink>,
    ) -> Self {
        Self { sender, sink }
    }
}

impl Drop for TaskLifetimeGuard {
    fn drop(&mut self) {
        // Explicit Stop and unexpected termination compete for one local
        // terminal transition. All identities belong to this sender instance;
        // never look up and retire a replacement through global state.
        let notify = !self.sender.stopping.swap(true, Ordering::AcqRel);
        let context = self.sender.disconnect_context();
        self.sender.stop();
        if notify {
            let lease_epoch = context
                .lease
                .as_ref()
                .map(|lease| lease.lease_epoch.clone());
            let event = ControllerTransportEvent::Disconnected {
                remote_control_generation: context.generation.clone(),
                lease_epoch,
                context,
                error: "controller_transport_task_ended".into(),
            };
            // Retirement completes before the external sink. Its panic cannot
            // double-panic while the worker itself is already unwinding.
            let _ = catch_unwind(AssertUnwindSafe(|| self.sink.emit(event)));
        }
    }
}
