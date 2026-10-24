package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNegativeAdjustmentMemberBucketConservationPostgres(t *testing.T) {
	for _, mode := range []string{"active", "reserved", "suspended"} {
		t.Run(mode+"-grant-survives-unallocated-exhaustion", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			wallet, _, _, component, member, grant := negativeAdjustmentOrganization(t, r, ctx)
			negativeAdjustmentReserveRelease(t, r, ctx, wallet, member, 1, 100)
			var outstanding Hold
			var usage uuid.UUID
			if mode == "reserved" {
				usage = uuid.New()
				var err error
				outstanding, err = r.ReserveMemberQuota(ctx, usage, wallet.ID, wallet.OwnerID, member, 1, 100, nil, r.now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "suspended" {
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "SUSPENDED", 2); err != nil {
					t.Fatal(err)
				}
			}
			grantBefore := negativeAdjustmentGrantFacts(t, r, ctx, grant.ID)
			posted := negativeAdjustmentPost(t, r, ctx, wallet.ID, 500000)
			if grantBefore != negativeAdjustmentGrantFacts(t, r, ctx, grant.ID) {
				t.Fatal("negative adjustment altered an allocated grant")
			}
			negativeAdjustmentConserved(t, r, ctx, wallet.ID)
			state := negativeAdjustmentState(t, r, ctx, component)
			if state.Available != 0 || state.Active+state.Suspended+state.Reserved != 500000 || state.Revoked != 500000 {
				t.Fatal("wrong four-bucket result", state)
			}
			if mode == "reserved" {
				if _, err := r.Release(ctx, usage, outstanding.ID, "controlled original member release"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "suspended" {
				before := negativeAdjustmentFacts(t, r, ctx)
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 2, 100, nil, r.now().Add(time.Hour)); !errors.Is(err, ErrMemberLifecycleConflict) {
					t.Fatal("suspension bypassed", err)
				}
				if before != negativeAdjustmentFacts(t, r, ctx) {
					t.Fatal("denied suspended hold wrote facts")
				}
				if state.Active != 0 || state.Suspended != 500000 {
					t.Fatal(state)
				}
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 3); err != nil {
					t.Fatal(err)
				}
				negativeAdjustmentReserveRelease(t, r, ctx, wallet, member, 3, 100)
			} else {
				negativeAdjustmentReserveRelease(t, r, ctx, wallet, member, 1, 100)
			}
			state = negativeAdjustmentState(t, r, ctx, component)
			if state.ComponentStatus != "ACTIVE" || state.PackageStatus != "ACTIVE" || state.Active != 500000 || state.Reserved != 0 || state.Suspended != 0 {
				t.Fatal("remaining member funds were terminalized", state)
			}
			before := negativeAdjustmentFacts(t, r, ctx)
			replay, err := r.ExecuteApprovedAdjustment(ctx, posted.ID, *posted.ApprovalRequestID, *posted.ExecutionIdempotencyKey, *posted.ApprovedBy, *posted.DecisionReason)
			if err != nil || replay.ID != posted.ID || replay.Version != posted.Version || replay.JournalEntryID == nil || *replay.JournalEntryID != *posted.JournalEntryID || before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal("execution replay changed financial facts", replay, err)
			}
			if _, err := r.ExecuteApprovedAdjustment(ctx, posted.ID, *posted.ApprovalRequestID, uuid.NewString(), *posted.ApprovedBy, *posted.DecisionReason); !errors.Is(err, ErrConflict) {
				t.Fatal("changed execution key accepted", err)
			}
			if before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal("execution-key conflict wrote facts")
			}
			// Spend the retained quota through the original settlement producer.
			// Its member branch leaves component status ACTIVE; the existing package
			// projection becomes CONSUMED only after all four live buckets are zero.
			version := int64(1)
			if mode == "suspended" {
				version = 3
			}
			usage = uuid.New()
			hold, err := r.ReserveMemberQuota(ctx, usage, wallet.ID, wallet.OwnerID, member, version, 500000, nil, r.now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Commit(ctx, usage, hold.ID, 500000, map[string]any{"controlled": "remaining member consumption"}); err != nil {
				t.Fatal(err)
			}
			state = negativeAdjustmentState(t, r, ctx, component)
			if state.Available+state.Reserved+state.Active+state.Suspended != 0 || state.ComponentStatus != "ACTIVE" || state.PackageStatus != "CONSUMED" {
				t.Fatal("fully exhausted buckets not terminal", state)
			}
			if got, err := r.GetWallet(ctx, wallet.ID); err != nil || got.PostedMicrocredits != 0 || got.ReservedMicrocredits != 0 {
				t.Fatal(got, err)
			}
			negativeAdjustmentConserved(t, r, ctx, wallet.ID)
		})
	}
	for _, amount := range []int64{250000, 1000000} {
		name := "personal-partial"
		if amount == 1000000 {
			name = "personal-exact-exhaustion"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, _, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			var component uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
				t.Fatal(err)
			}
			negativeAdjustmentPost(t, r, ctx, order.WalletID, amount)
			state := negativeAdjustmentState(t, r, ctx, component)
			if state.Available != 1000000-amount || state.Revoked != amount || state.Reserved+state.Active+state.Suspended != 0 {
				t.Fatal(state)
			}
			wantComponent, wantPackage := "ACTIVE", "ACTIVE"
			if amount == 1000000 {
				wantComponent, wantPackage = "REVOKED", "CONSUMED"
			}
			if state.ComponentStatus != wantComponent || state.PackageStatus != wantPackage {
				t.Fatal(state)
			}
			negativeAdjustmentConserved(t, r, ctx, order.WalletID)
		})
	}
	for _, suspended := range []bool{false, true} {
		name := "active"
		if suspended {
			name = "suspended"
		}
		t.Run(name+"-allocated-capacity-is-not-adjustable", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			wallet, _, _, _, member, _ := negativeAdjustmentOrganization(t, r, ctx)
			if suspended {
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "SUSPENDED", 2); err != nil {
					t.Fatal(err)
				}
			}
			requested := uuid.New()
			key := uuid.NewString()
			reason := "controlled adjustment exceeds unallocated capacity"
			adjustment, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, -750000, reason, requested, key)
			if err != nil {
				t.Fatal(err)
			}
			before := negativeAdjustmentFacts(t, r, ctx)
			if _, err := r.ExecuteApprovedAdjustment(ctx, adjustment.ID, uuid.New(), uuid.NewString(), requested, "controlled self approval rejected"); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			if before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal("self approval wrote facts")
			}
			if _, err := r.ExecuteApprovedAdjustment(ctx, adjustment.ID, uuid.New(), uuid.NewString(), uuid.New(), "controlled independent approval"); err == nil {
				t.Fatal("allocated capacity was consumed")
			}
			if before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal("failed partial debit left ledger, component or command effects")
			}
			if replay, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, -750000, reason, requested, key); err != nil || replay.ID != adjustment.ID || before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal(replay, err)
			}
			if _, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, -500000, reason, requested, key); !errors.Is(err, ErrConflict) {
				t.Fatal("different request facts reused key", err)
			}
			if before != negativeAdjustmentFacts(t, r, ctx) {
				t.Fatal("request conflict wrote facts")
			}
			negativeAdjustmentConserved(t, r, ctx, wallet.ID)
		})
	}
	t.Run("financial-review-freeze-still-refuses-adjustment", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		wallet, transaction, _, component, _, _ := negativeAdjustmentOrganization(t, r, ctx)
		if _, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-adjustment-freeze-"+uuid.NewString(), json.RawMessage(`{"controlled":true}`)); err != nil {
			t.Fatal(err)
		}
		state := negativeAdjustmentState(t, r, ctx, component)
		if state.ComponentStatus != "FROZEN" || state.PackageStatus != "FROZEN" {
			t.Fatal("did not use the financial freeze producer", state)
		}
		adjustment, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, -500000, "controlled frozen source debit", uuid.New(), uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		before := negativeAdjustmentFacts(t, r, ctx)
		if _, err := r.ExecuteApprovedAdjustment(ctx, adjustment.ID, uuid.New(), uuid.NewString(), uuid.New(), "controlled independent approval"); err == nil {
			t.Fatal("financial freeze bypassed")
		}
		if before != negativeAdjustmentFacts(t, r, ctx) {
			t.Fatal("frozen-source rejection wrote facts")
		}
		negativeAdjustmentConserved(t, r, ctx, wallet.ID)
	})
}

type negativeAdjustmentBuckets struct {
	Available, Reserved, Active, Suspended, Revoked int64
	ComponentStatus, PackageStatus                  string
}

func negativeAdjustmentState(t *testing.T, r *PostgresRepository, ctx context.Context, component uuid.UUID) negativeAdjustmentBuckets {
	t.Helper()
	var b negativeAdjustmentBuckets
	if err := r.pool.QueryRow(ctx, `SELECT c.available_microcredits,c.reserved_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,c.revoked_microcredits,c.status,p.status FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE c.id=$1`, component).Scan(&b.Available, &b.Reserved, &b.Active, &b.Suspended, &b.Revoked, &b.ComponentStatus, &b.PackageStatus); err != nil {
		t.Fatal(err)
	}
	return b
}
func negativeAdjustmentOrganization(t *testing.T, r *PostgresRepository, ctx context.Context) (Wallet, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, MemberQuotaGrant) {
	t.Helper()
	_, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	var component uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
		t.Fatal(err)
	}
	grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return wallet, transaction, pack, component, member, grant
}
func negativeAdjustmentPost(t *testing.T, r *PostgresRepository, ctx context.Context, wallet uuid.UUID, amount int64) ManualAdjustment {
	t.Helper()
	requester := uuid.New()
	item, err := r.RequestAdjustmentWithIdempotency(ctx, wallet, -amount, "controlled unallocated negative adjustment", requester, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	posted, err := r.ExecuteApprovedAdjustment(ctx, item.ID, uuid.New(), uuid.NewString(), uuid.New(), "controlled independent adjustment approval")
	if err != nil || posted.Status != "POSTED" || posted.ApprovedBy == nil || *posted.ApprovedBy == requester {
		t.Fatal(posted, err)
	}
	return posted
}
func negativeAdjustmentReserveRelease(t *testing.T, r *PostgresRepository, ctx context.Context, wallet Wallet, member uuid.UUID, version, amount int64) {
	t.Helper()
	usage := uuid.New()
	hold, err := r.ReserveMemberQuota(ctx, usage, wallet.ID, wallet.OwnerID, member, version, amount, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatalf("still-funded member cannot reserve after legal unallocated adjustment: %v", err)
	}
	if _, err := r.Release(ctx, usage, hold.ID, "controlled original hold release"); err != nil {
		t.Fatal(err)
	}
}
func negativeAdjustmentGrantFacts(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var raw string
	if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(g)::text FROM member_quota_grants g WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
func negativeAdjustmentFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	facts := map[string]json.RawMessage{}
	for _, table := range []string{"wallets", "credit_lots", "entitlement_packages", "entitlement_components", "member_quota_grants", "member_quota_grant_audits", "member_quota_hold_allocations", "usage_holds", "usage_hold_allocations", "manual_adjustments", "journal_entries", "ledger_postings", "receivables", "payment_orders", "payment_transactions", "payment_evidence", "chargeback_cases", "payment_anomaly_cases", "manual_review_cases", "organization_member_lifecycle_states", "outbox_events"} {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM `+table+` t`).Scan(&raw); err != nil {
			t.Fatal(table, err)
		}
		facts[table] = raw
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func negativeAdjustmentConserved(t *testing.T, r *PostgresRepository, ctx context.Context, wallet uuid.UUID) {
	t.Helper()
	var valid bool
	if err := r.pool.QueryRow(ctx, `SELECT
 w.posted_microcredits=(SELECT COALESCE(sum(l.remaining_microcredits),0) FROM credit_lots l WHERE l.wallet_id=w.id)
 AND w.posted_microcredits=(SELECT COALESCE(sum(p.delta_microcredits),0) FROM ledger_postings p JOIN ledger_accounts a ON a.id=p.ledger_account_id WHERE a.wallet_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=w.id AND c.original_microcredits<>c.available_microcredits+c.reserved_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits+c.used_microcredits+c.expired_microcredits+c.revoked_microcredits+c.reclassified_out_microcredits)
 AND NOT EXISTS(SELECT 1 FROM member_quota_grants g JOIN entitlement_packages p ON p.id=g.package_id WHERE p.wallet_id=w.id AND g.original_microcredits<>g.available_microcredits+g.reserved_microcredits+g.used_microcredits+g.expired_microcredits+g.revoked_microcredits)
 AND NOT EXISTS(SELECT 1 FROM entitlement_packages p WHERE p.wallet_id=w.id AND (p.points_remaining<>(SELECT COALESCE(sum(c.available_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits),0) FROM entitlement_components c WHERE c.package_id=p.id) OR p.points_reserved<>(SELECT COALESCE(sum(c.reserved_microcredits),0) FROM entitlement_components c WHERE c.package_id=p.id)))
 AND NOT EXISTS(SELECT journal_entry_id FROM ledger_postings GROUP BY journal_entry_id HAVING sum(delta_microcredits)<>0)
 FROM wallets w WHERE w.id=$1`, wallet).Scan(&valid); err != nil || !valid {
		t.Fatal("wallet/ledger/lot/component/grant/package conservation failed", valid, err)
	}
}
