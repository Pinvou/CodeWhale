package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Real PostgreSQL transactions in the owned UUID fixture database. These tests
// cover Billing's financial boundaries, not a supplier or Token end-to-end run.
func TestPersonalUsageCommitFinancialBoundaries(t *testing.T) {
	t.Run("trusted-overage-books-receivable-once", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Credit(ctx, w.ID, uuid.NewString(), 10000, false); err != nil {
			t.Fatal(err)
		}
		usageID := uuid.New()
		h, err := r.Reserve(ctx, usageID, w.ID, 100, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		usage := map[string]any{"input_tokens": 13, "output_tokens": 2}
		for n := 0; n < 2; n++ {
			got, err := r.Commit(ctx, usageID, h.ID, 150, usage)
			if err != nil || got.Status != "COMMITTED" || got.ActualMicrocredits == nil || *got.ActualMicrocredits != 150 {
				t.Fatal("trusted overage/replay failed", got, err)
			}
		}
		if _, err = r.Commit(ctx, usageID, h.ID, 151, usage); !errors.Is(err, ErrUsageAlreadyTerminal) {
			t.Fatal("changed terminal charge accepted", err)
		}
		var posted, reserved, receivable, used, available, original, outstanding, postings int64
		var financial, hold, source, status string
		var count, journals int
		err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,w.receivable_microcredits,w.financial_status,c.used_microcredits,c.available_microcredits,h.status,v.source_type,v.status,v.original_microcredits,v.outstanding_microcredits,(SELECT count(*) FROM receivables WHERE usage_request_id=$2),(SELECT count(*) FROM journal_entries WHERE entry_type='USAGE'),(SELECT sum(delta_microcredits) FROM ledger_postings) FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id JOIN usage_holds h ON h.wallet_id=w.id JOIN receivables v ON v.usage_request_id=h.usage_request_id WHERE w.id=$1 AND h.usage_request_id=$2`, w.ID, usageID).Scan(&posted, &reserved, &receivable, &financial, &used, &available, &hold, &source, &status, &original, &outstanding, &count, &journals, &postings)
		if err != nil || posted != 9900 || reserved != 0 || receivable != 50 || financial != "RECEIVABLE_OPEN" || used != 100 || available != 9900 || hold != "COMMITTED" || source != "USAGE_OVERAGE" || status != "OPEN" || original != 50 || outstanding != 50 || count != 1 || journals != 1 || postings != 0 {
			t.Fatal("overage/replay lost balanced money facts", posted, reserved, receivable, financial, used, available, hold, source, status, original, outstanding, count, journals, postings, err)
		}
	})
	t.Run("expired-uncertain-release-records-loss-without-debit", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Credit(ctx, w.ID, uuid.NewString(), 10000, false); err != nil {
			t.Fatal(err)
		}
		usageID := uuid.New()
		h, err := r.Reserve(ctx, usageID, w.ID, 100, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Release(ctx, usageID, h.ID, "ENTITLEMENT_EXPIRED_UNCERTAIN"); err != nil {
			t.Fatal(err)
		}
		usage := map[string]any{"input_tokens": 3, "output_tokens": 2}
		for n := 0; n < 2; n++ {
			if _, err = r.Commit(ctx, usageID, h.ID, 30, usage); !errors.Is(err, ErrUsagePlatformLossReview) {
				t.Fatal("late actual did not enter platform loss review", err)
			}
		}
		var posted, reserved, receivable, used, available, claimed, postings int64
		var hold, reason, status string
		var count, journals int
		var bound bool
		err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,w.receivable_microcredits,c.used_microcredits,c.available_microcredits,h.status,l.reason_code,l.status,l.claimed_actual_microcredits,l.usage_request_id=$2 AND l.wallet_id=$1 AND l.usage_snapshot=$3::jsonb,(SELECT count(*) FROM usage_loss_reviews WHERE hold_id=h.id),(SELECT count(*) FROM journal_entries WHERE entry_type='USAGE'),(SELECT sum(delta_microcredits) FROM ledger_postings) FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id JOIN usage_holds h ON h.wallet_id=w.id JOIN usage_loss_reviews l ON l.hold_id=h.id WHERE w.id=$1 AND h.usage_request_id=$2`, w.ID, usageID, `{"input_tokens":3,"output_tokens":2}`).Scan(&posted, &reserved, &receivable, &used, &available, &hold, &reason, &status, &claimed, &bound, &count, &journals, &postings)
		if err != nil || posted != 10000 || reserved != 0 || receivable != 0 || used != 0 || available != 10000 || hold != "RELEASED" || reason != "ENTITLEMENT_EXPIRED_UNCERTAIN" || status != "REVIEW_REQUIRED" || claimed != 30 || !bound || count != 1 || journals != 0 || postings != 0 {
			t.Fatal("late actual debited or lost review identity", posted, reserved, receivable, used, available, hold, reason, status, claimed, bound, count, journals, postings, err)
		}
	})
	t.Run("member-backed-allocation-rejects-personal-settlement-atomically", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Credit(ctx, w.ID, uuid.NewString(), 10000, false); err != nil {
			t.Fatal(err)
		}
		usageID := uuid.New()
		h, err := r.Reserve(ctx, usageID, w.ID, 100, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var pack, component uuid.UUID
		if err = r.pool.QueryRow(ctx, `SELECT package_id,component_id FROM usage_hold_allocations WHERE hold_id=$1`, h.ID).Scan(&pack, &component); err != nil {
			t.Fatal(err)
		}
		grant, actor := uuid.New(), uuid.New()
		if _, err = r.pool.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,reserved_microcredits,status,membership_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',100,0,100,'ACTIVE',1,$3,$1::uuid::text,repeat('a',64),now()+interval '1 hour',now(),now())`, grant, uuid.New(), actor, pack, component); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO member_quota_hold_allocations(hold_id,grant_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,100,now(),now())`, h.ID, grant, component); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			if _, err = r.Commit(ctx, usageID, h.ID, 30, map[string]any{"input_tokens": 3}); !errors.Is(err, ErrConflict) {
				t.Fatal("member facts settled as personal", err)
			}
		}
		var posted, reserved, used, available, lot, memberReserved, memberUsed, settled, released int64
		var hold string
		var journals int
		err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,c.used_microcredits,c.available_microcredits,l.remaining_microcredits,h.status,g.reserved_microcredits,g.used_microcredits,a.settled_microcredits,a.released_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='USAGE') FROM wallets w JOIN usage_holds h ON h.wallet_id=w.id JOIN usage_hold_allocations allocation ON allocation.hold_id=h.id JOIN entitlement_components c ON c.id=allocation.component_id JOIN credit_lots l ON l.id=c.credit_lot_id JOIN member_quota_hold_allocations a ON a.hold_id=h.id JOIN member_quota_grants g ON g.id=a.grant_id WHERE w.id=$1 AND h.id=$2`, w.ID, h.ID).Scan(&posted, &reserved, &used, &available, &lot, &hold, &memberReserved, &memberUsed, &settled, &released, &journals)
		if err != nil || posted != 10000 || reserved != 100 || used != 0 || available != 9900 || lot != 10000 || hold != "ACTIVE" || memberReserved != 100 || memberUsed != 0 || settled != 0 || released != 0 || journals != 0 {
			t.Fatal("rejected settlement left partial money mutation", posted, reserved, used, available, lot, hold, memberReserved, memberUsed, settled, released, journals, err)
		}
	})
}
