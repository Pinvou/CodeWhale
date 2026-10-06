// Real PostgreSQL and production coordinator/repository; controlled provider facts, not supplier or ordinary-refund policy acceptance.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"testing"
	"time"
)

func forcedRefundPaidSource(t *testing.T) (*PostgresRepository, Wallet, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewPaymentCoordinatorWithProvider(r, &legacyMappingProvider{}, mappings, "test")
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := c.CreateOrder(ctx, uuid.New(), wallet.ID, "CNY_1", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var transaction uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&transaction); err != nil {
		t.Fatal(err)
	}
	wallet, err = r.GetWallet(ctx, wallet.ID)
	if err != nil || wallet.PostedMicrocredits <= 0 {
		t.Fatal("paid source did not create real wallet funds", err)
	}
	return r, wallet, transaction
}
func TestPostgresForcedRefundSpentBaseCreatesReceivable(t *testing.T) {
	r, wallet, transaction := forcedRefundPaidSource(t)
	ctx := context.Background()
	hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, wallet.PostedMicrocredits, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, hold.UsageRequestID, hold.ID, wallet.PostedMicrocredits, map[string]any{"fixture_usage": true}); err != nil {
		t.Fatal(err)
	}
	item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced-spent", 100, json.RawMessage(`{"confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
	if err != nil {
		t.Fatalf("confirmed forced refund with spent BASE cannot account its receivable: %v", err)
	}
	if applied.ReceivableID == nil {
		t.Fatal("spent BASE loss omitted receivable")
	}
	replay, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
	if err != nil || replay.ID != applied.ID || replay.ReceivableID == nil || *replay.ReceivableID != *applied.ReceivableID {
		t.Fatal("same confirmed forced refund was not idempotent", err)
	}
	var entries int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND'`).Scan(&entries); err != nil || entries != 1 {
		t.Fatal("forced refund created duplicate journal", err)
	}
	var source string
	if err := r.pool.QueryRow(ctx, `SELECT source_type FROM receivables WHERE id=$1`, *applied.ReceivableID).Scan(&source); err != nil || source != "FORCED_REFUND" {
		t.Fatal("wrong receivable source", err)
	}
	current, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || current.PostedMicrocredits != 0 || current.ReservedMicrocredits != 0 || current.ReceivableMicrocredits != wallet.PostedMicrocredits || current.FinancialStatus != "RECEIVABLE_OPEN" {
		t.Fatal("forced refund did not preserve protected financial facts", err)
	}
}
func TestPostgresForcedRefundReplayBindsAmount(t *testing.T) {
	r, _, transaction := forcedRefundPaidSource(t)
	ctx := context.Background()
	first, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced-stable", 100, json.RawMessage(`{"confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	same, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced-stable", 100, json.RawMessage(`{"confirmed":true}`))
	if err != nil || same.ID != first.ID {
		t.Fatal("immutable evidence replay failed", err)
	}
	replay, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced-stable", 50, json.RawMessage(`{"confirmed":true}`))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("same provider refund ID accepted changed payment fact: first=%d replay=%d err=%v", first.AmountFen, replay.AmountFen, err)
	}
}
