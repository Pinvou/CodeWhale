//! Reusable session use-case boundaries.
//!
//! These modules are ordinary Rust and must stay independent of Tauri. Each
//! later implementation task owns one predeclared file, so adding behavior
//! does not require editing this central export surface.

pub(crate) mod admission;
pub(crate) mod dispatch_remote;
pub(crate) mod ports;
pub(crate) mod project_remote;
pub(crate) mod submit_controlled;

#[cfg(test)]
mod tests {
    use super::admission::AdmissionUseCase;
    use super::dispatch_remote::ReserveRemoteUseCase;
    use super::ports::{ApplicationEvent, ApplicationEventPort, UseCaseError, UseCaseFuture};
    use super::project_remote::ProjectRemoteUseCase;
    use super::submit_controlled::SubmitControlledUseCase;

    struct FakeUseCases;

    macro_rules! fake_boundary {
        ($trait_name:ident, $method:ident) => {
            impl $trait_name for FakeUseCases {
                type Request = u64;
                type Response = u64;

                fn $method(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response> {
                    Box::pin(async move { Ok(request + 1) })
                }
            }
        };
    }

    fake_boundary!(AdmissionUseCase, admit);
    fake_boundary!(ReserveRemoteUseCase, reserve);
    fake_boundary!(ProjectRemoteUseCase, project);
    fake_boundary!(SubmitControlledUseCase, submit);

    impl ApplicationEventPort for FakeUseCases {
        fn publish(&self, event: ApplicationEvent) -> Result<(), UseCaseError> {
            assert_eq!(event.topic(), "remote-session:changed");
            assert_eq!(event.payload(), &serde_json::json!({"revision": 2}));
            Ok(())
        }
    }

    #[tokio::test]
    async fn all_predeclared_boundaries_are_callable_without_tauri() {
        let boundary = FakeUseCases;
        assert_eq!(boundary.admit(0).await.unwrap(), 1);
        assert_eq!(boundary.submit(1).await.unwrap(), 2);
        assert_eq!(boundary.reserve(2).await.unwrap(), 3);
        assert_eq!(boundary.project(3).await.unwrap(), 4);

        boundary
            .publish(ApplicationEvent::new(
                "remote-session:changed",
                serde_json::json!({"revision": 2}),
            ))
            .unwrap();
    }

    #[test]
    fn use_case_errors_expose_only_stable_codes() {
        let error = UseCaseError::new("task_session_busy", true);
        assert_eq!(error.code(), "task_session_busy");
        assert!(error.is_retryable());
        assert_eq!(error.to_string(), "task_session_busy");
    }
}
