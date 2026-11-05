package account

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalAuthorizationRetryDelay(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		for attempt := 1; attempt <= 2; attempt++ {
			t.Run(fmt.Sprintf("%s/attempt%d", code, attempt), func(t *testing.T) {
				calls := 0
				err := approvalAuthorizationRetryDelay(context.Background(), attempt,
					fmt.Errorf("transaction aborted: %w", &pgconn.PgError{Code: code}),
					func(maximum time.Duration) time.Duration {
						calls++
						if maximum != time.Duration(attempt)*25*time.Millisecond {
							t.Fatalf("wrong jitter cap: %v", maximum)
						}
						return 0
					})
				if err != nil || calls != 1 {
					t.Fatalf("eligible retry: %v, jitter calls %d", err, calls)
				}
			})
		}
	}
}

func TestApprovalAuthorizationRetryRejectsOtherFailures(t *testing.T) {
	for _, original := range []error{errors.New("connection lost during commit"), &pgconn.PgError{Code: "23514"}, &pgconn.PgError{Code: "42501"}, context.DeadlineExceeded} {
		got := approvalAuthorizationRetryDelay(context.Background(), 1, original, func(time.Duration) time.Duration {
			t.Fatal("non-retryable failure invoked jitter")
			return 0
		})
		if got != original {
			t.Fatalf("lost original failure: %v", got)
		}
	}
	for _, attempt := range []int{-1, 0, 3, 4} {
		original := &pgconn.PgError{Code: "40001"}
		if got := approvalAuthorizationRetryDelay(context.Background(), attempt, original, func(time.Duration) time.Duration {
			t.Fatal("out-of-budget attempt invoked jitter")
			return 0
		}); got != original {
			t.Fatalf("attempt %d did not preserve error: %v", attempt, got)
		}
	}
}

func TestApprovalAuthorizationRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := approvalAuthorizationRetryDelay(ctx, 1, &pgconn.PgError{Code: "40001"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled context: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	err := approvalAuthorizationRetryDelay(ctx, 1, &pgconn.PgError{Code: "40P01"}, func(maximum time.Duration) time.Duration {
		cancel()
		return maximum * 100
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during delay: %v", err)
	}
}

func TestApprovalAuthorizationRetryJitterBounds(t *testing.T) {
	for _, value := range []time.Duration{-time.Hour, time.Hour} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		started := time.Now()
		err := approvalAuthorizationRetryDelay(ctx, 1, &pgconn.PgError{Code: "40001"}, func(time.Duration) time.Duration { return value })
		cancel()
		if err != nil {
			t.Fatalf("out-of-range jitter was not clamped: %v", err)
		}
		if value > 0 && time.Since(started) < 25*time.Millisecond {
			t.Fatal("positive excessive jitter did not wait for the cap")
		}
	}
	if err := approvalAuthorizationRetryDelay(context.Background(), 1, &pgconn.PgError{Code: "40001"}, nil); err != nil {
		t.Fatalf("default jitter failed: %v", err)
	}
}
