package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestReceivableSourceTypeContractPostgresIntegration covers the F3 follow-up
// defect: the receivable writer only accepted USAGE_OVERAGE/CHARGEBACK while the
// reviewed forced-refund path creates a FORCED_REFUND BASE shortfall and
// migration 000060 already allows that value. The forced-refund shortfall must
// insert like the other source types (with the same wallet projection), and an
// unknown source type must still fail closed.
func TestReceivableSourceTypeContractPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	now := time.Now().UTC()
	walletID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, walletID, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	create := func(sourceType, reference string, amount int64) (Receivable, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		receivable, err := createReceivableWithinTransaction(ctx, tx, walletID, sourceType, reference, amount, nil, nil, nil, nil, now)
		if err != nil {
			return Receivable{}, err
		}
		return receivable, tx.Commit(ctx)
	}
	// The reviewed forced-refund shortfall now persists and keeps the wallet
	// projection consistent.
	forcedReference := "refund-repair:" + uuid.NewString()
	forced, err := create("FORCED_REFUND", forcedReference, 40)
	if err != nil {
		t.Fatalf("forced refund shortfall receivable: %v", err)
	}
	var sourceType string
	var original, outstanding int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT source_type,original_microcredits,outstanding_microcredits,status FROM receivables WHERE id=$1`, forced.ID).
		Scan(&sourceType, &original, &outstanding, &status); err != nil ||
		sourceType != "FORCED_REFUND" || original != 40 || outstanding != 40 || status != "OPEN" {
		t.Fatalf("receivable=%s/%d/%d/%s err=%v", sourceType, original, outstanding, status, err)
	}
	var receivableCredits, version int64
	var financialStatus, freezeReason string
	if err := pool.QueryRow(ctx, `SELECT receivable_microcredits,version,financial_status,COALESCE(financial_freeze_reason,'') FROM wallets WHERE id=$1`, walletID).
		Scan(&receivableCredits, &version, &financialStatus, &freezeReason); err != nil ||
		receivableCredits != 40 || version != 2 || financialStatus != "RECEIVABLE_OPEN" || freezeReason != "FORCED_REFUND" {
		t.Fatalf("wallet=%d/%d/%s/%s err=%v", receivableCredits, version, financialStatus, freezeReason, err)
	}
	// The same reference is idempotent, and an unknown source type fails closed.
	replayed, err := create("FORCED_REFUND", forcedReference, 40)
	if err != nil || replayed.ID != forced.ID {
		t.Fatalf("forced refund replay=%v err=%v", replayed, err)
	}
	for _, invalid := range []struct {
		sourceType string
		amount     int64
	}{
		{"UNKNOWN_SOURCE", 10},
		{"FORCED_REFUND", 0},
	} {
		if _, err := create(invalid.sourceType, "refund-repair:"+uuid.NewString(), invalid.amount); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid source type %s/%d err=%v", invalid.sourceType, invalid.amount, err)
		}
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM receivables WHERE wallet_id=$1`, walletID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receivable rows=%d err=%v", count, err)
	}
}
