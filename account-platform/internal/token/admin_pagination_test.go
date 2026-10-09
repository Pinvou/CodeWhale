package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAdminPaginationLimitsAndStableErrors(t *testing.T) {
	for _, item := range []struct {
		limit, defaultLimit, maximum, want int
		err                                bool
	}{{0, 50, 200, 50, false}, {1, 50, 200, 1, false}, {200, 50, 200, 200, false}, {201, 50, 200, 0, true}, {51, 20, 50, 0, true}} {
		got, err := NormalizeAdminPaginationLimit(item.limit, item.defaultLimit, item.maximum)
		if got != item.want || item.err != (err != nil) {
			t.Fatalf("NormalizeAdminPaginationLimit(%d,%d,%d)=(%d,%v)", item.limit, item.defaultLimit, item.maximum, got, err)
		}
	}
	for err, want := range map[error]AdminPaginationHTTPError{
		ErrAdminPaginationLimitInvalid:        {Status: 400, Code: "INVALID_LIMIT"},
		ErrAdminPaginationCursorInvalid:       {Status: 400, Code: "INVALID_CURSOR"},
		ErrAdminPaginationCursorExpired:       {Status: 410, Code: "CURSOR_EXPIRED"},
		ErrAdminPaginationFilterRequired:      {Status: 422, Code: "PAGINATION_FILTER_REQUIRED"},
		ErrAdminPaginationSnapshotLimited:     {Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 1},
		ErrAdminPaginationQueryTimeout:        {Status: 504, Code: "QUERY_TIMEOUT"},
		ErrAdminPaginationSnapshotUnavailable: {Status: 503, Code: "PAGINATION_SNAPSHOT_UNAVAILABLE", RetryAfter: 5},
	} {
		if got := ClassifyAdminPaginationError(err); got != want {
			t.Fatalf("ClassifyAdminPaginationError(%v)=%+v want=%+v", err, got, want)
		}
	}
	limited := adminPaginationSnapshotRateLimitError{retryAfter: 37}
	if got, want := ClassifyAdminPaginationError(limited), (AdminPaginationHTTPError{Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 37}); got != want {
		t.Fatalf("ClassifyAdminPaginationError(rate limited)=%+v want=%+v", got, want)
	}
	if !errors.Is(limited, ErrAdminPaginationSnapshotLimited) {
		t.Fatal("rate limit error must preserve the stable sentinel")
	}
}

func TestAdminPaginationCursorBindsCompleteSnapshotIdentity(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := adminCursorEnvelope{
		Version: 1, SnapshotID: uuid.New(), Position: 50, SubjectID: uuid.New(), AuthorizationVersion: 7,
		Resource: "models", ScopeDigest: adminPaginationDigest("platform"), FilterDigest: adminPaginationDigest(map[string]string{"status": "PUBLISHED"}),
		Sort: "display_name_normalized ASC,id ASC", Limit: 50,
		HighWaterUnixNano: time.Now().UTC().UnixNano(), ExpiresUnixNano: time.Now().UTC().Add(AdminSnapshotTTL).UnixNano(),
	}
	raw, err := signedAdminPaginationCursor(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseAdminPaginationCursor(raw, key)
	if err != nil || parsed != payload {
		t.Fatalf("cursor round trip=%+v, %v", parsed, err)
	}
	for index := 0; index < len(raw); index += max(1, len(raw)/7) {
		forged := []byte(raw)
		if forged[index] == 'A' {
			forged[index] = 'B'
		} else {
			forged[index] = 'A'
		}
		if _, err := parseAdminPaginationCursor(string(forged), key); !errors.Is(err, ErrAdminPaginationCursorInvalid) {
			t.Fatalf("tampered cursor byte %d returned %v", index, err)
		}
	}
}

func TestAdminPaginationFeatureGateFailsClosedWithoutState(t *testing.T) {
	repository := &PostgresRepository{}
	request := AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 1, Resource: "models", Sort: "id ASC", Limit: 50}
	if _, err := repository.AdminPaginationPage(t.Context(), request, func(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ time.Time, _ int) error { return nil }); !errors.Is(err, ErrAdminPaginationDisabled) {
		t.Fatalf("disabled gate returned %v", err)
	}
}

func TestAdminPaginationSerializationRetryRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	if err := waitAdminSerializationRetry(ctx, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry returned %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("canceled retry waited %s", elapsed)
	}
}

func TestAdminSerializationRetryDelayIsBoundedAndJittered(t *testing.T) {
	for attempt := 0; attempt < adminSerializationAttempts-1; attempt++ {
		base := 10 * time.Millisecond * time.Duration(1<<min(attempt, 4))
		minimum := adminSerializationRetryDelay(attempt, 0)
		maximum := adminSerializationRetryDelay(attempt, uint64(base-1))
		if minimum < base/2 || minimum >= base+base/2 {
			t.Fatalf("attempt %d minimum delay=%s outside [%s,%s)", attempt, minimum, base/2, base+base/2)
		}
		if maximum < base/2 || maximum >= base+base/2 {
			t.Fatalf("attempt %d maximum delay=%s outside [%s,%s)", attempt, maximum, base/2, base+base/2)
		}
		if minimum == maximum {
			t.Fatalf("attempt %d delay did not vary with jitter: %s", attempt, minimum)
		}
	}
}

func TestAdminSerializationRetryRecoversTransientDatabaseConflict(t *testing.T) {
	for _, sqlState := range []string{"40001", "40P01"} {
		t.Run(sqlState, func(t *testing.T) {
			attempts := 0
			err := retryAdminSerializationWithWait(t.Context(), func() error {
				attempts++
				if attempts < 3 {
					return &pgconn.PgError{Code: sqlState, Message: "synthetic transient conflict"}
				}
				return nil
			}, func(context.Context, int) error { return nil })
			if err != nil || attempts != 3 {
				t.Fatalf("retry result attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestAdminSerializationRetryDoesNotRepeatPermanentFailure(t *testing.T) {
	for _, want := range []error{
		errors.New("permanent failure"),
		&pgconn.PgError{Code: "23505", Message: "synthetic permanent constraint violation"},
	} {
		attempts := 0
		err := retryAdminSerialization(t.Context(), func() error {
			attempts++
			return want
		})
		if !errors.Is(err, want) || attempts != 1 {
			t.Fatalf("retry result attempts=%d err=%v", attempts, err)
		}
	}
}

func TestAdminSerializationRetryDoesNotStartWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	attempts := 0
	err := retryAdminSerialization(ctx, func() error {
		attempts++
		return nil
	})
	if !errors.Is(err, context.Canceled) || attempts != 0 {
		t.Fatalf("retry result attempts=%d err=%v", attempts, err)
	}
}

func TestAdminSerializationRetryStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	err := retryAdminSerialization(ctx, func() error {
		attempts++
		cancel()
		return &pgconn.PgError{Code: "40001", Message: "synthetic serialization conflict"}
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("retry result attempts=%d err=%v", attempts, err)
	}
}

func TestAdminSerializationFailureGivesContextPriority(t *testing.T) {
	transient := &pgconn.PgError{Code: "40001", Message: "synthetic serialization conflict"}
	err := errors.Join(context.Canceled, transient)
	if adminSerializationFailure(err) {
		t.Fatalf("context cancellation was classified as retryable: %v", err)
	}
}

func TestAdminSerializationRetryGivesFinalAttemptContextPriority(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		err := retryAdminSerializationWithWait(ctx, func() error {
			attempts++
			if attempts == adminSerializationAttempts {
				cancel()
			}
			return &pgconn.PgError{Code: "40001", Message: "synthetic serialization conflict"}
		}, func(context.Context, int) error { return nil })
		if !errors.Is(err, context.Canceled) || attempts != adminSerializationAttempts {
			t.Fatalf("final cancellation attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		attempts := 0
		err := retryAdminSerializationWithWait(ctx, func() error {
			attempts++
			if attempts == adminSerializationAttempts {
				<-ctx.Done()
			}
			return &pgconn.PgError{Code: "40P01", Message: "synthetic deadlock conflict"}
		}, func(context.Context, int) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) || attempts != adminSerializationAttempts {
			t.Fatalf("final deadline attempts=%d err=%v", attempts, err)
		}
	})
}

func TestAdminSerializationRetryIsBounded(t *testing.T) {
	attempts := 0
	waits := 0
	err := retryAdminSerializationWithWait(t.Context(), func() error {
		attempts++
		return &pgconn.PgError{Code: "40001", Message: "synthetic serialization conflict"}
	}, func(context.Context, int) error {
		waits++
		return nil
	})
	if !adminSerializationFailure(err) || attempts != adminSerializationAttempts || waits != adminSerializationAttempts-1 {
		t.Fatalf("retry result attempts=%d waits=%d err=%v", attempts, waits, err)
	}
}
