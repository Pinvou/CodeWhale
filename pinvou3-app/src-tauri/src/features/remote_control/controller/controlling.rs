//! Controller-device (A) state owner.

use parking_lot::Mutex;

use super::relay_transport::ControllerTransportSender;

#[derive(Default)]
pub(crate) struct ControllingDeviceManager {
    transport: Mutex<Option<ControllerTransportSender>>,
}

impl ControllingDeviceManager {
    pub(crate) fn replace_transport(&self, transport: ControllerTransportSender) {
        if let Some(previous) = self.transport.lock().replace(transport) {
            previous.stop();
        }
    }

    pub(crate) fn stop(&self) {
        if let Some(transport) = self.transport.lock().take() {
            transport.stop();
        }
    }

    pub(crate) fn is_running(&self) -> bool {
        self.transport
            .lock()
            .as_ref()
            .is_some_and(|transport| !transport.is_stopped())
    }
}

impl Drop for ControllingDeviceManager {
    fn drop(&mut self) {
        if let Some(transport) = self.transport.get_mut().take() {
            transport.stop();
        }
    }
}
