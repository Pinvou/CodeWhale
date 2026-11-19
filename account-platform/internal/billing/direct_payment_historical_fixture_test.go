package billing

import (
	"context"
	"testing"
	"time"
)

// Only the exact 900031 historical rollback fixture uses the preserved original
// supplier/strong-payment observation flow. The current public SyncOrder starts
// the 900047 generation protocol and deliberately requires that migration first.
// This is not a production missing-column fallback or a claim that new SyncOrder
// works on an old schema. Normal current fixtures keep the public entry point.
func directRepairHistoricalFixtureBefore900047(t *testing.T, r *PostgresRepository, ctx context.Context) (*PaymentCoordinator, PaymentOrder, *directRepairProvider) {
	t.Helper()
	var exact bool
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT max(version)=900031 FROM schema_migrations) AND NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.payment_orders'::regclass AND attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT attisdropped)`).Scan(&exact); err != nil || !exact {
		t.Fatal("historical direct producer requires its real pre-generation prefix", err)
	}
	current := r.now
	r.now = func() time.Time { return current().Add(-2 * time.Hour) }
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	r.now = current
	provider := &directRepairProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider)}
	paid := current()
	provider.result.PaidAt = &paid
	c.provider = provider
	// These are the original pre-I/O public-wrapper guards, frozen from main67c
	// payment_sync.go. The extracted production observation body retains the
	// original providerCall, identity checks and financial transaction writers.
	stored, err := getPaymentOrderByID(ctx, r.pool, order.ID, false)
	if err != nil || !c.Capabilities().OrderQuerySupported || c.Capabilities().ProviderID != stored.ProviderID || c.Capabilities().Version != stored.ProviderVersion {
		t.Fatal("historical provider/order admission differs", err)
	}
	var observation ProviderOrder
	review, err := c.syncOrderObservation(ctx, stored, false, &observation)
	if err == nil {
		// The frozen public SyncOrder wrapper reads the final database projection
		// after the original financial flow; that read also predates 900047.
		review, err = c.currentPaymentOrderProjection(ctx, review)
	}
	if err != nil || review.Status != "MANUAL_REVIEW" || review.FulfillmentStatus != "MANUAL_REVIEW" || observation.Status != "SUCCESS" || observation.ChannelTransactionID == nil || observation.PaidAt == nil || provider.gets.Load() != 1 {
		t.Fatal("original late strong-payment observation did not require review", err)
	}
	return c, review, provider
}
