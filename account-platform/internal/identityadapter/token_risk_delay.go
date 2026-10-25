package identityadapter

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// A hard quota/known-user lock is different from an identifier's cancellable
// pre-verification wait. Neither changes the original fifteen-minute counters.
type tokenAdmissionDecision struct {
	RetryAfter        time.Duration
	VerificationDelay time.Duration
}

const tokenProcessingBudget = 30 * time.Second

func identifierVerificationDelay(failures int64) time.Duration {
	if failures < 5 {
		return 0
	}
	return min(time.Second*time.Duration(1<<min(failures-5, 5)), 30*time.Second)
}

func (a *RedisTokenAdmission) RecheckStableLock(ctx context.Context, d tokenRiskDimensions) (time.Duration, error) {
	if d.action != "password" {
		return 0, nil
	}
	stableDigest, err := a.client.Get(ctx, "pinvou:auth-alias:"+a.digest(d.identifier)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	if stableDigest == "" {
		return 0, nil
	}
	key := "pinvou:auth-failure:user:" + stableDigest
	count, err := a.client.Get(ctx, key).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	if count < 10 {
		return 0, nil
	}
	ttl, err := a.client.TTL(ctx, key).Result()
	return max(ttl, time.Second), err
}

// Only an admitted password/SMS grant with a real progressive wait opts into
// extra response time. Ordinary/code/refresh processing budgets stay unchanged.
// The old thirty-second processing budget plus at most thirty seconds' wait
// gives an absolute, cancellable bound; unsupported transports fail closed.
func tokenVerificationContext(w http.ResponseWriter, r *http.Request, started time.Time, delay time.Duration) (context.Context, context.CancelFunc, error) {
	deadline := started.Add(tokenProcessingBudget + delay)
	if parent, ok := r.Context().Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	return ctx, cancel, nil
}

func waitTokenVerification(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
