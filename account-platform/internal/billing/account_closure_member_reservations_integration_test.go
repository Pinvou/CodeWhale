package billing

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Use the original owned Billing fixture and actual payment/quota/hold producers.
// No personal hold, fabricated reservation counter, or external dispatch is used.
func TestAccountClosureAssessmentIncludesMemberReservations(t *testing.T) {
	cases := []struct {
		name, status, finish        string
		reserveTarget, reserveOther bool
	}{
		{"active-reservation-settles", "ACTIVE", "COMMIT", true, false},
		{"suspended-reservation-releases", "SUSPENDED", "RELEASE", true, false},
		{"active-zero-reservation", "ACTIVE", "", false, false},
		{"suspended-zero-reservation", "SUSPENDED", "", false, false},
		{"other-active-reservation", "ACTIVE", "", false, true},
		{"other-suspended-reservation", "SUSPENDED", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, _, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			organization := *order.BeneficiaryOrganizationID
			member := uuid.New()
			personal, _, err := r.EnsureWallet(ctx, "PERSONAL", member)
			if err != nil {
				t.Fatal(err)
			}
			var component uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
				t.Fatal(err)
			}
			allocate := func(user uuid.UUID) MemberQuotaGrant {
				t.Helper()
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, organization, user, "ACTIVE", 1); err != nil {
					t.Fatal(err)
				}
				grant, created, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: organization, MemberUserID: user, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
				if err != nil || !created || grant.MemberUserID != user || grant.Status != "ACTIVE" || grant.ReservedMicrocredits != 0 {
					t.Fatal("actual new member quota allocation", created, err)
				}
				return grant
			}
			grant := allocate(member)
			reservedUser := member
			reservedGrant := grant
			if tc.reserveOther {
				reservedUser = uuid.New()
				reservedGrant = allocate(reservedUser)
			}
			var hold Hold
			if tc.reserveTarget || tc.reserveOther {
				hold, err = r.ReserveMemberQuota(ctx, uuid.New(), order.WalletID, organization, reservedUser, 1, 100, nil, r.now().Add(time.Hour))
				if err != nil || hold.Status != "ACTIVE" || hold.WalletID != order.WalletID || hold.MaxMicrocredits != 100 {
					t.Fatal("real organization member reservation", err)
				}
			}
			if tc.status == "SUSPENDED" {
				if _, err = r.ApplyOrganizationMemberLifecycle(ctx, organization, reservedUser, "SUSPENDED", 2); err != nil {
					t.Fatal(err)
				}
			}
			var status string
			var reserved int64
			if err = r.pool.QueryRow(ctx, `SELECT status,reserved_microcredits FROM member_quota_grants WHERE id=$1`, reservedGrant.ID).Scan(&status, &reserved); err != nil || status != tc.status || reserved != map[bool]int64{false: 0, true: 100}[tc.reserveTarget || tc.reserveOther] {
				t.Fatal("actual lifecycle and reservation fixture", status, reserved, err)
			}
			var personalIsEmpty bool
			if err = r.pool.QueryRow(ctx, `SELECT posted_microcredits=0 AND reserved_microcredits=0 AND receivable_microcredits=0 AND NOT EXISTS(SELECT 1 FROM usage_holds WHERE wallet_id=$1) FROM wallets WHERE id=$1`, personal.ID).Scan(&personalIsEmpty); err != nil || !personalIsEmpty {
				t.Fatal("member reservation must have no personal hold or balance blocker", personalIsEmpty, err)
			}
			// Every row/value in the related financial, lifecycle, receipt, audit,
			// and outbox tables is frozen; keys differ across these tables, so sort
			// by the full JSON row rather than assuming an id column exists.
			snapshot := func() map[string]string {
				t.Helper()
				facts := make(map[string]string)
				for _, table := range []string{"wallets", "payment_orders", "payment_transactions", "entitlement_packages", "entitlement_components", "credit_lots", "usage_holds", "usage_hold_allocations", "member_quota_grants", "member_quota_hold_allocations", "member_quota_grant_audits", "usage_hold_lease_events", "journal_entries", "ledger_postings", "ledger_accounts", "outbox_events", "account_closure_grant_isolations", "organization_member_lifecycle_states"} {
					var value string
					if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(fact) ORDER BY to_jsonb(fact)::text),'[]'::jsonb)::text FROM `+table+` fact`).Scan(&value); err != nil {
						t.Fatal("read existing fixture table", table, err)
					}
					facts[table] = value
				}
				return facts
			}
			assess := func(blocked bool) {
				t.Helper()
				before := snapshot()
				got, err := r.AccountClosureAssessment(ctx, member)
				if err != nil {
					t.Fatal(err)
				}
				want := AccountClosureAssessment{UserID: member, Ready: !blocked, Blockers: []AccountClosureBlocker{}}
				if blocked {
					want.Blockers = append(want.Blockers, AccountClosureBlocker{Code: "UNSETTLED_HOLD", Domain: "BILLING", ResourceType: "MEMBER_QUOTA_GRANT", ResourceID: member.String(), UserAction: "WAIT_FOR_USAGE_SETTLEMENT"})
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("closure assessment must match the existing member-isolation guard: ready=%v blockers=%v want=%v", got.Ready, got.Blockers, want.Blockers)
				}
				if !reflect.DeepEqual(snapshot(), before) {
					t.Fatal("read-only assessment changed financial, hold, quota, receipt, audit or outbox facts")
				}
			}
			assess(tc.reserveTarget)
			if tc.reserveTarget {
				before := snapshot()
				if _, err = r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2); !errors.Is(err, ErrConflict) || !reflect.DeepEqual(snapshot(), before) {
					t.Fatal("existing reserved isolation rejection or zero-effect boundary changed", err)
				}
				var done Hold
				used := int64(0)
				if tc.finish == "COMMIT" {
					used = 30
					done, err = r.Commit(ctx, hold.UsageRequestID, hold.ID, used, map[string]any{"input_tokens": 3})
					if err == nil && done.Status != "COMMITTED" {
						t.Fatal("actual settlement was not terminal")
					}
				} else {
					done, err = r.Release(ctx, hold.UsageRequestID, hold.ID, "USER_STOPPED")
					if err == nil && done.Status != "RELEASED" {
						t.Fatal("actual release was not terminal")
					}
				}
				if err != nil {
					t.Fatal("original lawful settlement/release", err)
				}
				var terminalFacts bool
				if err = r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits=0 AND g.used_microcredits=$3 AND g.available_microcredits=500000-$3 AND w.reserved_microcredits=0 AND w.posted_microcredits=1000000-$3 FROM member_quota_grants g JOIN wallets w ON w.id=$2 WHERE g.id=$1`, grant.ID, order.WalletID, used).Scan(&terminalFacts); err != nil || !terminalFacts {
					t.Fatal("settlement/release did not preserve original financial counters", terminalFacts, err)
				}
				assess(false)
			}
			// Other-user reservations remain intact even when this user's zero-
			// reserved grant can be isolated, including the suspended case.
			var otherBefore string
			if tc.reserveOther {
				if err = r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',to_jsonb(g),'hold',to_jsonb(h))::text FROM member_quota_grants g JOIN usage_holds h ON h.id=$2 WHERE g.id=$1`, reservedGrant.ID, hold.ID).Scan(&otherBefore); err != nil {
					t.Fatal(err)
				}
			}
			isolation, err := r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2)
			if err != nil || isolation.Replayed || isolation.MemberUserID != member || isolation.IsolatedGrantCount != 1 {
				t.Fatal("ready user cannot complete original isolation", isolation, err)
			}
			if tc.reserveOther {
				var otherAfter string
				if err = r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',to_jsonb(g),'hold',to_jsonb(h))::text FROM member_quota_grants g JOIN usage_holds h ON h.id=$2 WHERE g.id=$1`, reservedGrant.ID, hold.ID).Scan(&otherAfter); err != nil || otherAfter != otherBefore {
					t.Fatal("another member's reservation or hold changed", err)
				}
			}
		})
	}
}
