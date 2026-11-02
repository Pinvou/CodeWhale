//! Inbound lane data and its diagnostic representation, independent of the
//! service's handshake/recovery loop. The payload remains owned and unchanged.

#[derive(Clone, Eq, PartialEq)]
pub(crate) enum ControllerLaneEvent {
    Frame(String),
    Disconnected(super::relay_transport::ControllerDisconnectContext),
    Failed(ControllerLaneFailure),
}

/// Local ingress failures never carry remote text or frame contents.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ControllerLaneFailure {
    ItemCapacity,
    ByteBudget,
    Closed,
}

impl ControllerLaneFailure {
    pub(crate) fn code(self) -> &'static str {
        match self {
            Self::ItemCapacity => "inbound_item_capacity",
            Self::ByteBudget => "inbound_byte_budget",
            Self::Closed => "inbound_closed",
        }
    }
}

impl std::fmt::Debug for ControllerLaneEvent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Frame(_) => f.debug_tuple("Frame").field(&"[REDACTED]").finish(),
            Self::Disconnected(_) => f.write_str("Disconnected"),
            Self::Failed(reason) => f.debug_tuple("Failed").field(reason).finish(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn inbound_debug_redacts_lane_frame_without_changing_payload() {
        let payload = json!({
            "type": "controller_resumed",
            "new_resume_token_b64": "fake-lane-resume-secret",
            "unknown_payload": "fake-lane-future-secret",
        })
        .to_string();
        let event = ControllerLaneEvent::Frame(payload.clone());
        for diagnostic in [format!("{event:?}"), format!("{event:#?}")] {
            assert!(!diagnostic.contains("fake-lane-resume-secret"));
            assert!(!diagnostic.contains("fake-lane-future-secret"));
            assert!(diagnostic.contains("Frame"));
            assert!(diagnostic.contains("[REDACTED]"));
        }
        assert_eq!(event, ControllerLaneEvent::Frame(payload));
        let (sender, _receiver) = super::super::relay_transport::transport_channels(
            super::super::relay_transport::ControllerRole::ControllerA,
            "generation-diagnostic",
            2,
            2,
            4096,
            1024,
        );
        assert_eq!(
            format!(
                "{:?}",
                ControllerLaneEvent::Disconnected(sender.disconnect_context())
            ),
            "Disconnected"
        );
    }
}
