//! Inbound lane data and its diagnostic representation, independent of the
//! service's handshake/recovery loop. The payload remains owned and unchanged.

#[derive(Clone, Eq, PartialEq)]
pub(crate) enum ControllerLaneEvent {
    Frame(String),
    Disconnected,
}

impl std::fmt::Debug for ControllerLaneEvent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Frame(_) => f.debug_tuple("Frame").field(&"[REDACTED]").finish(),
            Self::Disconnected => f.write_str("Disconnected"),
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
        assert_eq!(
            format!("{:?}", ControllerLaneEvent::Disconnected),
            "Disconnected"
        );
    }
}
