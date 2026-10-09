//! Serialize application hook registration, preserving existing diagnostics.
//! Registrants return true only for the panic scope they actually own.
use std::panic::PanicHookInfo;
use std::sync::{Arc, Mutex, Once, OnceLock, RwLock};

type Handler = dyn Fn(&PanicHookInfo<'_>) -> bool + Send + Sync;
type HandlerSnapshot = Arc<Vec<Arc<Handler>>>;

static REGISTRATION: Mutex<()> = Mutex::new(());
static INSTALL: Once = Once::new();
static HANDLERS: OnceLock<RwLock<HandlerSnapshot>> = OnceLock::new();

fn handlers() -> &'static RwLock<HandlerSnapshot> {
    HANDLERS.get_or_init(|| RwLock::new(Arc::new(Vec::new())))
}

pub(crate) fn chain(handler: impl Fn(&PanicHookInfo<'_>) -> bool + Send + Sync + 'static) {
    let _registration = REGISTRATION
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    // No owned scope may become active until its registration returns. Only
    // the first install exchanges the process hook; later registrations must
    // not temporarily restore the default hook while another scope is active.
    INSTALL.call_once(|| {
        let previous = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            // Do not keep a registry lock while invoking arbitrary handlers.
            // The immutable snapshot needs no allocation in the panic hook.
            let snapshot = handlers()
                .read()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .clone();
            for handler in snapshot.iter().rev() {
                if handler(info) {
                    return;
                }
            }
            previous(info);
        }));
    });
    #[cfg(test)]
    registration_checkpoint();
    let mut current = handlers()
        .write()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let mut next = (**current).clone();
    next.push(Arc::new(handler));
    *current = Arc::new(next);
}

#[cfg(test)]
static CHECKPOINT: Mutex<Option<(std::sync::mpsc::Sender<()>, std::sync::mpsc::Receiver<()>)>> =
    Mutex::new(None);

#[cfg(test)]
fn registration_checkpoint() {
    let checkpoint = CHECKPOINT.lock().unwrap().take();
    if let Some((entered, release)) = checkpoint {
        entered.send(()).unwrap();
        release
            .recv_timeout(std::time::Duration::from_secs(5))
            .unwrap();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn late_registration_preserves_active_redaction_without_a_default_hook_gap() {
        const CHILD: &str = "PINVOU_PANIC_HOOK_GAP_CHILD";
        const SENTINEL: &str = "test-owned-panic-payload-sentinel";
        if std::env::var_os(CHILD).is_some() {
            chain(|_| {
                eprintln!("owned_panic_redacted");
                true
            });
            let (entered_tx, entered_rx) = std::sync::mpsc::channel();
            let (release_tx, release_rx) = std::sync::mpsc::channel();
            *CHECKPOINT.lock().unwrap() = Some((entered_tx, release_rx));
            let registrant = std::thread::spawn(|| chain(|_| false));
            entered_rx
                .recv_timeout(std::time::Duration::from_secs(5))
                .unwrap();
            assert!(std::panic::catch_unwind(|| panic!("{SENTINEL}")).is_err());
            release_tx.send(()).unwrap();
            registrant.join().unwrap();
            return;
        }
        let module = module_path!().split_once("::").unwrap().1;
        let filter = format!(
            "{module}::late_registration_preserves_active_redaction_without_a_default_hook_gap"
        );
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", &filter, "--nocapture"])
            .env(CHILD, "1")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "isolated hook registration regression failed"
        );
        assert!(
            String::from_utf8(output.stdout)
                .unwrap()
                .contains("1 passed"),
            "isolated test matched zero cases"
        );
        let stderr = String::from_utf8(output.stderr).unwrap();
        assert!(
            !stderr.contains(SENTINEL),
            "owned panic reached default hook during registration"
        );
        assert_eq!(stderr.matches("owned_panic_redacted").count(), 1);
    }
}
