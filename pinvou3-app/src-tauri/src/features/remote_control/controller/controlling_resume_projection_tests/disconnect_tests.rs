//! Real keeper/manager/crypto with controlled event delivery and virtual time.
use super::*;
use crate::features::remote_control::controller::resume::ResumePhase;

#[tokio::test(start_paused = true)]
async fn current_disconnect_ends_each_recovery_phase_once_without_waiting_for_deadline() {
    for (index, phase) in [
        (1, ResumePhase::Prepared),
        (2, ResumePhase::Provisional),
        (3, ResumePhase::ActivationPending),
    ] {
        let f = Fixture::new();
        let mut lane = f.lane();
        let context = f.sender.disconnect_context();
        lane.events
            .insert(index, ControllerLaneEvent::Disconnected(context.clone()));
        lane.events
            .insert(index + 1, ControllerLaneEvent::Disconnected(context));
        // A duplicate and the otherwise valid final remain queued after the
        // first terminal loss; they must not re-enter cleanup or resurrect it.
        let observed = Arc::new(Mutex::new(Vec::new()));
        let phases = observed.clone();
        let manager = f.manager.clone();
        lane.observe = Some(Box::new(move |event| {
            if matches!(event, ControllerLaneEvent::Disconnected(_)) {
                phases.lock().unwrap().push(manager.resume_phase_for_test());
            }
        }));
        let mut states = Vec::new();
        let started = tokio::time::Instant::now();
        serve(&f, &mut lane, &mut |status| states.push(status.kind())).await;
        assert!(
            started.elapsed() < Duration::from_secs(1),
            "second loss waited for the frozen deadline"
        );
        assert_eq!(
            *observed.lock().unwrap(),
            [Some(ResumePhase::Connected), Some(phase)]
        );
        assert_eq!(
            states,
            [
                ControllingStatusKind::Reconnecting,
                ControllingStatusKind::Error
            ]
        );
        assert!(matches!(
            lane.events.front(),
            Some(ControllerLaneEvent::Disconnected(_))
        ));
        assert!(f.manager.live_lease().is_err());
        assert!(f.manager.resume_phase_for_test().is_none());
        assert!(!f.business.is_installed());
        assert!(
            f.manager.connect("3456789").is_ok(),
            "terminal candidate kept the next intent busy"
        );
    }
}

#[tokio::test(start_paused = true)]
async fn foreign_disconnects_in_recovery_preserve_the_current_authenticated_completion() {
    for index in [1, 2, 3] {
        for field in ["owner", "generation", "lease", "preauth"] {
            let f = Fixture::new();
            let mut lane = f.lane();
            let mut foreign = f.sender.disconnect_context();
            match field {
                "owner" => {
                    let (other, _receiver) =
                        super::super::super::relay_transport::transport_channels(
                            ControllerRole::ControllerA,
                            "generation-other",
                            2,
                            2,
                            8192,
                            1024,
                        );
                    foreign.owner = other.owner();
                }
                "generation" => foreign.generation = "generation-retired".into(),
                "lease" => foreign.lease.as_mut().unwrap().lease_epoch = "999".into(),
                "preauth" => foreign.lease = None,
                _ => unreachable!(),
            }
            lane.events
                .insert(index, ControllerLaneEvent::Disconnected(foreign));
            let mut states = Vec::new();
            serve(&f, &mut lane, &mut |status| states.push(status.kind())).await;
            assert_eq!(
                states,
                [
                    ControllingStatusKind::Reconnecting,
                    ControllingStatusKind::Connected
                ],
                "foreign {field} at event {index}"
            );
            assert_eq!(f.manager.status().kind(), ControllingStatusKind::Connected);
            assert!(f.manager.live_lease().is_ok());
            assert!(f.business.is_installed());
            assert!(!f.business.is_suspended());
        }
    }
}
