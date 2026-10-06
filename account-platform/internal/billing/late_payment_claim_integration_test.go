package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func lateClaimFixture(t *testing.T) (*PostgresRepository, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return now }}
	user, id := uuid.New(), uuid.New()
	wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	// This isolated fixture supplies a pending order; every claim/review write is
	// made by the production repository and never simulates payment confirmation.
	_, err = repo.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at) VALUES($1,$2,$3,$3,$4,'CNY_1','{}','ALIPAY',100,'CNY',1000000,'CREATED','PENDING',$5,$6,$6,$5,$7,$8)`, id, "claim-"+id.String(), user, wallet.ID, now.Add(time.Minute), now, now.Add(72*time.Hour), now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EscalateLatePaymentReview(ctx, id, "PENDING"); err != nil {
		t.Fatal(err)
	}
	return repo, id, user
}

func TestPostgresLatePaymentClaimLostReplyAfterResolutionReplaysCurrentFacts(t *testing.T) {
	repo, order, user := lateClaimFixture(t)
	ctx := context.Background()
	statement := strings.Repeat("\u539f", 10)
	first, err := repo.AddLatePaymentClaim(ctx, order, user, " "+statement+" ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.AddLatePaymentClaim(ctx, order, user, statement)
	if err != nil || second.ID != first.ID {
		t.Fatalf("same claim did not replay: %v", err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='RESOLVED_PAID' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := repo.AddLatePaymentClaim(ctx, order, user, statement)
	if err != nil || replay.ID != first.ID || replay.PaymentOrderID != order || replay.ReviewStatus != "RESOLVED_PAID" {
		t.Fatalf("lost reply failed current fact replay: %s %v", replay.ReviewStatus, err)
	}
	if _, err := repo.AddLatePaymentClaim(ctx, order, user, statement+"new"); !errors.Is(err, ErrConflict) {
		t.Fatalf("new claim reopened terminal case: %v", err)
	}
	if _, err := repo.AddLatePaymentClaim(ctx, order, uuid.New(), statement); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign user read replay: %v", err)
	}
	var evidence, events, journals, transactions int
	err = repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='USER_PAYMENT_CLAIM'),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='USER_PAYMENT_CLAIMED'),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$2)`, first.ID, order).Scan(&evidence, &events, &journals, &transactions)
	if err != nil || evidence != 1 || events != 1 || journals != 0 || transactions != 0 {
		t.Fatalf("duplicate or financial effects: %d %d %d %d %v", evidence, events, journals, transactions, err)
	}
}

func TestPostgresLatePaymentClaimCharacterBounds(t *testing.T) {
	repo, order, user := lateClaimFixture(t)
	ctx := context.Background()
	for _, length := range []int{9, 1001} {
		t.Run(strings.Repeat("x", length%10+1), func(t *testing.T) {
			if _, err := repo.AddLatePaymentClaim(ctx, order, user, strings.Repeat("\u539f", length)); !errors.Is(err, ErrConflict) {
				t.Fatalf("character bound %d accepted: %v", length, err)
			}
		})
	}
	if _, err := repo.AddLatePaymentClaim(ctx, order, user, strings.Repeat("\u539f", 1000)); err != nil {
		t.Fatalf("1000 multibyte characters rejected: %v", err)
	}
	if _, err := repo.AddLatePaymentClaim(ctx, order, user, string([]byte{0xff})+strings.Repeat("x", 10)); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid UTF8 accepted: %v", err)
	}
}
