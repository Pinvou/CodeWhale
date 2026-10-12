package token

import (
	"context"

	"github.com/google/uuid"
)

func (s *Service) abortActiveUpstream(usageID uuid.UUID) bool {
	value, ok := s.activeUpstreamCancels.Load(usageID)
	if !ok {
		return false
	}
	cancel, ok := value.(context.CancelFunc)
	if ok {
		cancel()
	}
	return ok
}
