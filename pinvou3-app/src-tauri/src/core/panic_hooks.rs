//! Serialize application hook registration, preserving existing diagnostics.
//! Registrants return true only for the panic scope they actually own.
use std::panic::PanicHookInfo;
use std::sync::Mutex;

pub(crate) fn chain(handler: impl Fn(&PanicHookInfo<'_>) -> bool + Send + Sync + 'static) {
    static REGISTRATION: Mutex<()> = Mutex::new(());
    let _registration = REGISTRATION
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let previous = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        if !handler(info) {
            previous(info);
        }
    }));
}
