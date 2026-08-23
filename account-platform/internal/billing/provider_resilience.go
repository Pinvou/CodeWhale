package billing

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
)

func providerCall[T any](coordinator *PaymentCoordinator, capability, route string, uncertainOnFailure bool, call func() (T, error)) (T, error) {
	var zero T
	key := resilience.Key(coordinator.Capabilities().ProviderID, capability, route)
	var admissionErr error
	if uncertainOnFailure {
		admissionErr = coordinator.breaker.BeforeHighRisk(key)
	} else {
		admissionErr = coordinator.breaker.Before(key)
	}
	if admissionErr != nil {
		return zero, admissionErr
	}
	result, err := call()
	if err == nil {
		coordinator.breaker.Record(key, resilience.Success)
		return result, nil
	}
	outcome := resilience.InfrastructureFailure
	if errors.Is(err, payment.ErrUnsupported) {
		outcome = resilience.BusinessRejection
	} else if uncertainOnFailure {
		outcome = resilience.UncertainResult
	}
	coordinator.breaker.Record(key, outcome)
	return zero, err
}
