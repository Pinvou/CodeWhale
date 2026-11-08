package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"time"
)

// A cancelled context makes the real pool's acquisition fail before any
// connection or provider dispatch. This does not run a database fixture.
func TestProviderAttemptLocalFailureNeverDispatchesOrRecordsProviderFailure(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://fixture@127.0.0.1:1/fixture?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &retainedFinancialProvider{}
	breaker := resilience.NewBreaker()
	coordinator := &PaymentCoordinator{repository: &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}, provider: provider, breaker: breaker}
	calls := 0
	for range 5 {
		_, err = providerCall(ctx, coordinator, "ORDER_QUERY", "route", false, ProviderCallScope{}, func() (int, error) { calls++; return 1, nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("local cancellation not exposed: %v", err)
		}
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
	caps := coordinator.Capabilities()
	if err = breaker.BeforeHighRisk(resilience.Key(caps.ProviderID+"@"+caps.Version, "ORDER_QUERY", "route")); err != nil {
		t.Fatalf("local failure was recorded as provider failure: %v", err)
	}
}
