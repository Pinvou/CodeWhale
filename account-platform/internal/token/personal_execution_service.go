package token

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

// WithPersonalModelExecution installs the actual invocation dependencies without
// replacing the already delivered management, finance or cancellation adapters.
func (s *Service) WithPersonalModelExecution(repository Repository, billing ModelExecutionBillingClient, upstream UpstreamClient, maximumInputTokens, defaultOutputTokens, maximumOutputTokens int64) (*Service, error) {
	if repository == nil || billing == nil || upstream == nil || maximumInputTokens <= 0 || defaultOutputTokens <= 0 || maximumOutputTokens < defaultOutputTokens {
		return nil, errors.New("personal model execution configuration is incomplete")
	}
	s.repository = repository
	s.executionBilling = billing
	s.executionUpstream = upstream
	s.maximumInputTokens = maximumInputTokens
	s.defaultMaxOutputTokens = defaultOutputTokens
	s.maximumOutputTokens = maximumOutputTokens
	s.breaker = resilience.NewBreaker()
	s.personalExecutionEnabled = true
	return s, nil
}
func (s *Service) WithClientReadiness(readiness *ClientReadinessService) *Service {
	s.clientReadiness = readiness
	return s
}

func (s *Service) registerActiveUpstream(usageID uuid.UUID, cancel context.CancelFunc) func() {
	s.activeUpstreamCancels.Store(usageID, cancel)
	return func() { s.activeUpstreamCancels.Delete(usageID) }
}
