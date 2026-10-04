package billing

import (
	"context"
	"time"
)

// RunHoldExpiry sends overdue reservations to review without releasing funds.
// It returns only after cancellation, so callers can close the repository safely.
func (s *Service) RunHoldExpiry(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.repository.ExpireHolds(ctx, 100); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
