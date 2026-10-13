package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Uses the existing opt-in owned Billing fixture and public migration hooks.
// No supplier/Token dispatch or external deployment is exercised here.
func TestMemberHoldIdentityMutationBoundaries(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	reserve := func(t *testing.T, maximum int64) (Hold, MemberQuotaGrant, uuid.UUID, uuid.UUID) {
		t.Helper()
		_, order, _, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
		org, member := *order.BeneficiaryOrganizationID, uuid.New()
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 1); err != nil {
			t.Fatal(err)
		}
		var component uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
			t.Fatal(err)
		}
		grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: org, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		h, err := r.ReserveMemberQuota(ctx, uuid.New(), order.WalletID, org, member, 1, maximum, nil, r.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return h, grant, org, member
	}
	t.Run("commit-after-membership-version-advances", func(t *testing.T) {
		h, g, org, member := reserve(t, 100)
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "SUSPENDED", 2); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 3); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			got, err := r.Commit(ctx, h.UsageRequestID, h.ID, 30, map[string]any{"input_tokens": 3})
			if err != nil || got.Status != "COMMITTED" {
				t.Fatal(got, err)
			}
		}
		var reserved, used, available, version, posted, walletReserved int64
		if err := r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits,g.used_microcredits,g.available_microcredits,g.membership_version,w.posted_microcredits,w.reserved_microcredits FROM member_quota_grants g JOIN wallets w ON w.id=$2 WHERE g.id=$1`, g.ID, h.WalletID).Scan(&reserved, &used, &available, &version, &posted, &walletReserved); err != nil || reserved != 0 || used != 30 || available != 499970 || version != 3 || posted != 999970 || walletReserved != 0 {
			t.Fatal(reserved, used, available, version, posted, walletReserved, err)
		}
	})
	t.Run("release-while-member-suspended", func(t *testing.T) {
		h, g, org, member := reserve(t, 100)
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "SUSPENDED", 2); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			got, err := r.Release(ctx, h.UsageRequestID, h.ID, "USER_STOPPED")
			if err != nil || got.Status != "RELEASED" {
				t.Fatal(got, err)
			}
		}
		var reserved, used, available, suspended, walletReserved int64
		if err := r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits,g.used_microcredits,g.available_microcredits,c.suspended_grant_microcredits,w.reserved_microcredits FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN wallets w ON w.id=$2 WHERE g.id=$1`, g.ID, h.WalletID).Scan(&reserved, &used, &available, &suspended, &walletReserved); err != nil || reserved != 0 || used != 0 || available != 500000 || suspended != 500000 || walletReserved != 0 {
			t.Fatal(reserved, used, available, suspended, walletReserved, err)
		}
	})
	t.Run("extend-complete-member-budget", func(t *testing.T) {
		h, g, _, _ := reserve(t, 100)
		got, err := r.ExtendHold(ctx, h.UsageRequestID, h.ID, h.ExpiresAt.Add(time.Minute), 20)
		if err != nil || got.MaxMicrocredits != 120 {
			t.Fatal(got, err)
		}
		var quota, allocation, generic, wallet int64
		if err := r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits,m.reserved_microcredits,a.reserved_microcredits,w.reserved_microcredits FROM member_quota_grants g JOIN member_quota_hold_allocations m ON m.grant_id=g.id JOIN usage_hold_allocations a ON a.hold_id=m.hold_id AND a.component_id=m.component_id JOIN wallets w ON w.id=$3 WHERE g.id=$1 AND m.hold_id=$2`, g.ID, h.ID, h.WalletID).Scan(&quota, &allocation, &generic, &wallet); err != nil || quota != 120 || allocation != 120 || generic != 120 || wallet != 120 {
			t.Fatal(quota, allocation, generic, wallet, err)
		}
	})
	t.Run("zero-budget-member-terminal-paths", func(t *testing.T) {
		for _, commit := range []bool{true, false} {
			h, _, _, _ := reserve(t, 0)
			var got Hold
			var err error
			if commit {
				got, err = r.Commit(ctx, h.UsageRequestID, h.ID, 0, nil)
			} else {
				got, err = r.Release(ctx, h.UsageRequestID, h.ID, "USER_STOPPED")
			}
			if err != nil || got.Status == "ACTIVE" {
				t.Fatal(got, err)
			}
		}
	})
	t.Run("synthetic-personal-member-rejects-all-mutations", func(t *testing.T) {
		w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Credit(ctx, w.ID, uuid.NewString(), 10000, false); err != nil {
			t.Fatal(err)
		}
		h, err := r.Reserve(ctx, uuid.New(), w.ID, 100, nil, r.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var pack, component uuid.UUID
		if err = r.pool.QueryRow(ctx, `SELECT package_id,component_id FROM usage_hold_allocations WHERE hold_id=$1`, h.ID).Scan(&pack, &component); err != nil {
			t.Fatal(err)
		}
		grant := uuid.New()
		if _, err = r.pool.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,reserved_microcredits,status,membership_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',100,0,100,'ACTIVE',1,$6,$7,$8,$9,$10,$10)`, grant, uuid.New(), uuid.New(), pack, component, uuid.New(), uuid.NewString(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r.now().Add(time.Hour), r.now()); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO member_quota_hold_allocations(hold_id,grant_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,100,$4,$4)`, h.ID, grant, component, r.now()); err != nil {
			t.Fatal(err)
		}
		// Reserve legitimately creates one CREATED lease event. Freeze the
		// persisted rows (including its timestamp), rather than assuming none.
		var persistedExpiry time.Time
		if err = r.pool.QueryRow(ctx, `SELECT expires_at FROM usage_holds WHERE id=$1`, h.ID).Scan(&persistedExpiry); err != nil {
			t.Fatal(err)
		}
		snapshot := func() string {
			t.Helper()
			var facts string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('wallet',to_jsonb(w),'hold',to_jsonb(h),'grant',to_jsonb(g),'member_allocation',to_jsonb(m),'usage_allocation',to_jsonb(a),'component',to_jsonb(c),'package',to_jsonb(p),'lot',to_jsonb(l),'journals',(SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY j.id),'[]'::jsonb) FROM journal_entries j WHERE j.business_reference=$3),'postings',(SELECT COALESCE(jsonb_agg(to_jsonb(lp) ORDER BY lp.id),'[]'::jsonb) FROM ledger_postings lp JOIN journal_entries j ON j.id=lp.journal_entry_id WHERE j.business_reference=$3),'leases',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM usage_hold_lease_events e WHERE e.hold_id=h.id))::text FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN member_quota_hold_allocations m ON m.hold_id=h.id JOIN member_quota_grants g ON g.id=m.grant_id JOIN usage_hold_allocations a ON a.hold_id=h.id AND a.component_id=m.component_id JOIN entitlement_components c ON c.id=a.component_id JOIN entitlement_packages p ON p.id=a.package_id JOIN credit_lots l ON l.id=c.credit_lot_id WHERE h.id=$1 AND g.id=$2`, h.ID, grant, "usage:"+h.UsageRequestID.String()).Scan(&facts); err != nil {
				t.Fatal(err)
			}
			return facts
		}
		before := snapshot()
		for _, mutate := range []func() error{
			func() error { _, e := r.Commit(ctx, h.UsageRequestID, h.ID, 30, nil); return e },
			func() error { _, e := r.Release(ctx, h.UsageRequestID, h.ID, "USER_STOPPED"); return e },
			func() error {
				_, e := r.ExtendHold(ctx, h.UsageRequestID, h.ID, h.ExpiresAt.Add(time.Minute), 0)
				return e
			},
			func() error {
				_, e := r.ExtendHold(ctx, h.UsageRequestID, h.ID, h.ExpiresAt.Add(time.Minute), 20)
				return e
			},
		} {
			if err := mutate(); !errors.Is(err, ErrConflict) {
				t.Fatal("synthetic member facts accepted", err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected mutation changed the persisted financial/hold/allocation/journal/lease snapshot")
			}
		}
		var unchanged bool
		if err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits=10000 AND w.reserved_microcredits=100 AND h.status='ACTIVE' AND h.max_microcredits=100 AND h.expires_at=$3 AND g.reserved_microcredits=100 AND g.used_microcredits=0 AND m.settled_microcredits=0 AND m.released_microcredits=0 AND a.reserved_microcredits=100 AND a.settled_microcredits=0 AND a.released_microcredits=0 AND NOT EXISTS(SELECT 1 FROM journal_entries WHERE business_reference=$4) AND (SELECT count(*) FROM usage_hold_lease_events WHERE hold_id=h.id AND event_type='CREATED')=1 AND NOT EXISTS(SELECT 1 FROM usage_hold_lease_events WHERE hold_id=h.id AND event_type='EXTENDED') FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN member_quota_hold_allocations m ON m.hold_id=h.id JOIN member_quota_grants g ON g.id=m.grant_id JOIN usage_hold_allocations a ON a.hold_id=h.id AND a.component_id=m.component_id WHERE h.id=$1 AND g.id=$2`, h.ID, grant, persistedExpiry, "usage:"+h.UsageRequestID.String()).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("rejected mutation changed money/hold/allocation", unchanged, err)
		}
	})
}
