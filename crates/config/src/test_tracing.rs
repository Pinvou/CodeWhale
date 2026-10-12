//! Test-only `tracing` capture shared by config-crate unit tests.
//!
//! A minimal in-memory [`tracing::Subscriber`] so tests can assert on their
//! own diagnostics without a subscriber dependency. Clones share the same
//! buffer; install with
//! `tracing::subscriber::with_default(captured.clone(), || …)` around the
//! synchronous code whose events should be captured.

/// One captured `tracing` event: the message plus every structured field the
/// event carried (a repeated field keeps its last value).
#[derive(Debug, Clone, Default)]
pub(crate) struct CapturedEvent {
    pub(crate) message: String,
    fields: std::collections::BTreeMap<String, String>,
}

impl CapturedEvent {
    pub(crate) fn field(&self, name: &str) -> Option<&str> {
        self.fields.get(name).map(String::as_str)
    }
}

/// A capturing subscriber. Clone it to install and to read events back.
#[derive(Clone, Default)]
pub(crate) struct CapturedEvents(std::sync::Arc<std::sync::Mutex<Vec<CapturedEvent>>>);

impl CapturedEvents {
    pub(crate) fn events(&self) -> Vec<CapturedEvent> {
        self.0.lock().expect("event capture lock").clone()
    }
}

impl tracing::Subscriber for CapturedEvents {
    fn enabled(&self, metadata: &tracing::Metadata<'_>) -> bool {
        metadata.level() <= &tracing::Level::WARN
    }

    fn new_span(&self, _attributes: &tracing::span::Attributes<'_>) -> tracing::Id {
        tracing::Id::from_u64(1)
    }

    fn record(&self, _span: &tracing::Id, _values: &tracing::span::Record<'_>) {}

    fn event(&self, event: &tracing::Event<'_>) {
        #[derive(Default)]
        struct Visitor {
            captured: CapturedEvent,
        }
        impl tracing::field::Visit for Visitor {
            fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
                if field.name() == "message" {
                    self.captured.message = format!("{value:?}");
                } else {
                    self.captured
                        .fields
                        .insert(field.name().to_string(), format!("{value:?}"));
                }
            }
        }

        let mut visitor = Visitor::default();
        event.record(&mut visitor);
        self.0
            .lock()
            .expect("event capture lock")
            .push(visitor.captured);
    }

    fn enter(&self, _span: &tracing::Id) {}

    fn exit(&self, _span: &tracing::Id) {}

    fn record_follows_from(&self, _span: &tracing::Id, _follows_from: &tracing::Id) {}
}
