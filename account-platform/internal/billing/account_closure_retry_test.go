package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Callback faults verify which errors may restart a financial transaction.
// Actual transaction rollback, receipts and monetary effects are checked by
// TestF08ClosedGrantConcurrentReceiptReplayPostgres, not simulated here.
func TestAccountClosureRetryOnlyDefiniteSerializationAbort(t *testing.T) {
	serialization := &pgconn.PgError{Code: "40001"}
	for _, test := range []struct {
		name            string
		err             error
		failures, calls int
	}{
		{"success", nil, 0, 1},
		{"wrapped-serialization-recovers", fmt.Errorf("definite abort: %w", serialization), 2, 3},
		{"serialization-exhausted", serialization, 8, 4},
		{"deadlock-not-retried", &pgconn.PgError{Code: "40P01"}, 8, 1},
		{"constraint-not-retried", &pgconn.PgError{Code: "23505"}, 8, 1},
		{"unknown-commit-transport-not-retried", io.ErrUnexpectedEOF, 8, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := retryAccountClosureSerialization(context.Background(), func() error {
				calls++
				if calls <= test.failures {
					return test.err
				}
				return nil
			})
			if calls != test.calls {
				t.Fatalf("transaction attempts=%d want=%d", calls, test.calls)
			}
			if test.failures < test.calls {
				if err != nil {
					t.Fatal("recovered callback", err)
				}
			} else if err != test.err {
				t.Fatalf("original terminal error changed: %v", err)
			}
		})
	}
	t.Run("cancel-during-retry-wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		err := retryAccountClosureSerialization(ctx, func() error {
			calls++
			cancel()
			return serialization
		})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancelled retry continued: calls=%d err=%v", calls, err)
		}
	})
}
