package billing

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
)

func providerCall[T any](ctx context.Context, coordinator *PaymentCoordinator, capability, route string, uncertainOnFailure bool, scope ProviderCallScope, call func() (T, error)) (T, error) {
	return providerCallWithAdmissionHook(ctx, coordinator, capability, route, uncertainOnFailure, scope, nil, call)
}

// The optional local hook runs only after circuit/probe admission and before
// recording a provider attempt. Failure returns the permit without attributing
// local persistence failure to the provider. Existing callers use no hook.
func providerCallWithAdmissionHook[T any](ctx context.Context, coordinator *PaymentCoordinator, capability, route string, uncertainOnFailure bool, scope ProviderCallScope, beforeAttempt func() error, call func() (T, error)) (T, error) {
	var zero T
	capabilities := coordinator.Capabilities()
	key := resilience.Key(capabilities.ProviderID+"@"+capabilities.Version, capability, route)
	var admissionErr error
	var cancelProbe func()
	if uncertainOnFailure {
		admissionErr = coordinator.breaker.BeforeHighRisk(key)
	} else {
		cancelProbe, admissionErr = coordinator.breaker.BeforeReadOnly(key)
	}
	if admissionErr != nil {
		return zero, admissionErr
	}
	if beforeAttempt != nil {
		if err := beforeAttempt(); err != nil {
			if cancelProbe != nil {
				cancelProbe()
			}
			return zero, err
		}
	}
	attempt, err := coordinator.repository.beginProviderAttempt(ctx, capabilities.ProviderID, capabilities.Version, capability, route, route, scope)
	if err != nil {
		if cancelProbe != nil {
			cancelProbe()
		}
		return zero, err
	}
	result, err := call()
	if err == nil {
		coordinator.breaker.Record(key, resilience.Success)
		if coordinator.repository.finishProviderAttempt(ctx, attempt, "SUCCESS", nil) == nil {
			_, _ = coordinator.repository.EvaluateProviderStopLosses(ctx, 100)
		}
		return result, nil
	}
	outcome := resilience.InfrastructureFailure
	observationOutcome := "INFRASTRUCTURE_FAILURE"
	if errors.Is(err, payment.ErrUnsupported) {
		outcome = resilience.BusinessRejection
		observationOutcome = "BUSINESS_REJECTION"
	} else if uncertainOnFailure {
		outcome = resilience.UncertainResult
		observationOutcome = "UNCERTAIN_RESULT"
	}
	coordinator.breaker.Record(key, outcome)
	if coordinator.repository.finishProviderAttempt(ctx, attempt, observationOutcome, err) == nil {
		_, _ = coordinator.repository.EvaluateProviderStopLosses(ctx, 100)
	}
	return zero, err
}
