package billing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

type f16MemberRepairFixture struct {
	Repository                                                         *PostgresRepository
	Context                                                            context.Context
	Clock                                                              *time.Time
	OrganizationID, MemberID, WalletID                                 uuid.UUID
	HoldA, HoldB                                                       Hold
	UsageRequestA, UsageRequestB, LossReviewID                         uuid.UUID
	ExpiredPackage, SurvivingPackage, SurvivingComponent, SurvivingLot uuid.UUID
	SurvivingGrants                                                    []MemberQuotaGrant
	Claimed                                                            int64
	Activation                                                         int64
}

type f16MemberRepairOptions struct {
	BeforeExpiry       func(*testing.T, *f16MemberRepairFixture)
	Grants             []int64
	HoldMaximum        int64
	NoSurvivingPackage bool
	NewHold            bool
	Suspend            bool
}

// Only funding/pricing and the clock are controlled. Hold allocations, whole-
// hold release on package expiry, and late loss review all come from the actual
// production methods with the original public migrations and isolated actors.
func f16ActualReleasedMemberFixture(t *testing.T, options f16MemberRepairOptions) f16MemberRepairFixture {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	origin := clock
	r.now = func() time.Time { return clock }
	f := f16MemberRepairFixture{Repository: r, Context: ctx, Clock: &clock, OrganizationID: uuid.New(), MemberID: uuid.New(), UsageRequestA: uuid.New()}
	if _, err := r.ApplyOrganizationLifecycle(ctx, f.OrganizationID, "ACTIVE", 1, false); err != nil {
		t.Fatal("actual organization lifecycle", err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, f.OrganizationID, f.MemberID, "ACTIVE", 1); err != nil {
		t.Fatal("actual member lifecycle", err)
	}
	wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", f.OrganizationID)
	if err != nil {
		t.Fatal("actual organization wallet", err)
	}
	f.WalletID = wallet.ID
	credit := func(reference string) (uuid.UUID, uuid.UUID, uuid.UUID) {
		if _, err := r.Credit(ctx, wallet.ID, reference, 100, true); err != nil {
			t.Fatal("actual controlled funding", err)
		}
		var packageID, componentID, lotID uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT package.id,component.id,component.credit_lot_id FROM entitlement_packages package JOIN entitlement_components component ON component.package_id=package.id WHERE package.wallet_id=$1 AND package.source_reference=$2`, wallet.ID, reference).Scan(&packageID, &componentID, &lotID); err != nil {
			t.Fatal(err)
		}
		return packageID, componentID, lotID
	}
	allocate := func(packageID, componentID uuid.UUID, amount int64) MemberQuotaGrant {
		grant, created, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: f.OrganizationID, MemberUserID: f.MemberID, PackageID: packageID, ComponentID: componentID, ComponentType: "BASE", AmountMicrocredits: amount, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
		if err != nil || !created {
			t.Fatal("actual member quota allocation", err)
		}
		return grant
	}
	expiredPackage, expiredComponent, _ := credit("f16-expiring:" + f.OrganizationID.String())
	f.ExpiredPackage = expiredPackage
	allocate(expiredPackage, expiredComponent, 100)
	clock = origin.AddDate(0, 0, 1)
	if !options.NoSurvivingPackage {
		f.SurvivingPackage, f.SurvivingComponent, f.SurvivingLot = credit("f16-surviving:" + f.OrganizationID.String())
		amounts := options.Grants
		if len(amounts) == 0 {
			amounts = []int64{100}
		}
		for _, amount := range amounts {
			f.SurvivingGrants = append(f.SurvivingGrants, allocate(f.SurvivingPackage, f.SurvivingComponent, amount))
		}
	}
	priceID, formulaID := uuid.New(), uuid.MustParse("00000000-0000-7000-8200-000000000001")
	model := "f16-member-" + f.OrganizationID.String()
	price := PriceVersion{ID: priceID, Model: model, ProtocolCode: "CHAT", FormulaVersionID: formulaID, CreditUnit: "MICROCREDIT", InputPerMillion: 1000000}
	pricing, err := json.Marshal(price)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,published_at) VALUES($1,$2,1000000,0,0,$3,$3,'MODEL',$2,'CHAT',$4,'MICROCREDIT','ACTIVE',$3)`, priceID, model, clock, formulaID); err != nil {
		t.Fatal("controlled frozen pricing catalog", err)
	}
	holdPricing := &HoldPricingContext{ModelID: model, ProtocolCode: "CHAT", FormulaVersionID: formulaID, PricingSnapshot: pricing, RequestSnapshot: json.RawMessage(`{}`)}
	// Reserve shortly before one package expires, with an ordinary finite hold
	// still ACTIVE across that expiry. The second package expires a day later.
	clock = origin.AddDate(0, 0, 30).Add(-time.Minute)
	maximum := options.HoldMaximum
	if maximum == 0 {
		maximum = 150
	}
	if options.NoSurvivingPackage {
		maximum = 100
	}
	f.Claimed = maximum
	f.HoldA, err = r.ReserveMemberQuotaPriced(ctx, f.UsageRequestA, wallet.ID, f.OrganizationID, f.MemberID, 1, maximum, &priceID, clock.Add(time.Hour), holdPricing)
	if err != nil {
		t.Fatal("actual priced member reserve", err)
	}
	if options.BeforeExpiry != nil {
		options.BeforeExpiry(t, &f)
	}
	clock = origin.AddDate(0, 0, 30).Add(time.Minute)
	if count, err := r.ExpirePackages(ctx, 1); err != nil || count != 1 {
		t.Fatal("actual package expiry and whole-hold release", err)
	}
	var status, reason string
	var aggregateReserved, aggregateSettled, aggregateReleased, memberReserved, memberSettled, memberReleased int64
	if err := r.pool.QueryRow(ctx, `SELECT status,usage_snapshot->>'release_reason' FROM usage_holds WHERE id=$1`, f.HoldA.ID).Scan(&status, &reason); err != nil || status != "RELEASED" || reason != "ENTITLEMENT_EXPIRED_UNCERTAIN" {
		t.Fatal("real expiry did not release the original hold", err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT sum(reserved_microcredits) FROM usage_hold_allocations WHERE hold_id=$1),(SELECT sum(settled_microcredits) FROM usage_hold_allocations WHERE hold_id=$1),(SELECT sum(released_microcredits) FROM usage_hold_allocations WHERE hold_id=$1),(SELECT sum(reserved_microcredits) FROM member_quota_hold_allocations WHERE hold_id=$1),(SELECT sum(settled_microcredits) FROM member_quota_hold_allocations WHERE hold_id=$1),(SELECT sum(released_microcredits) FROM member_quota_hold_allocations WHERE hold_id=$1)`, f.HoldA.ID).Scan(&aggregateReserved, &aggregateSettled, &aggregateReleased, &memberReserved, &memberSettled, &memberReleased); err != nil || aggregateReserved != maximum || aggregateSettled != 0 || aggregateReleased != maximum || memberReserved != maximum || memberSettled != 0 || memberReleased != maximum {
		t.Fatal("actual expiry member/aggregate release differs", err)
	}
	if options.NewHold {
		f.UsageRequestB = uuid.New()
		f.HoldB, err = r.ReserveMemberQuotaPriced(ctx, f.UsageRequestB, wallet.ID, f.OrganizationID, f.MemberID, 1, 20, &priceID, clock.Add(time.Hour), holdPricing)
		if err != nil {
			t.Fatal("actual new hold after original release", err)
		}
	}
	if options.Suspend {
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, f.OrganizationID, f.MemberID, "SUSPENDED", 2); err != nil {
			t.Fatal("actual member suspension after release", err)
		}
	}
	if _, err := r.Commit(ctx, f.UsageRequestA, f.HoldA.ID, maximum, map[string]any{"input_tokens": maximum, "output_tokens": int64(0), "cached_input_tokens": int64(0)}); !errors.Is(err, ErrUsagePlatformLossReview) {
		t.Fatal("actual late usage loss review", err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT id FROM usage_loss_reviews WHERE hold_id=$1 AND status='REVIEW_REQUIRED'`, f.HoldA.ID).Scan(&f.LossReviewID); err != nil {
		t.Fatal(err)
	}
	f.Activation = usageRepairFixtureActivation(t, ctx, r.pool)("ACTIVE")
	return f
}

func f16MemberRepairInput(t *testing.T, f f16MemberRepairFixture) (UsageSettlementRepairPlan, UsageSettlementRepairExecutionInput) {
	t.Helper()
	r, ctx := f.Repository, f.Context
	fixture := usageRepairFixture{HoldID: f.HoldA.ID, UsageRequestID: f.UsageRequestA, LossReviewID: f.LossReviewID}
	input := usageRepairInput(t, ctx, r, fixture, f.Activation)
	plan, err := usageSettlementRepairPlanFrom(ctx, r.pool, input.Command, 1, 1, f.Activation)
	if err != nil {
		t.Fatal("actual member owner plan", err)
	}
	proof, err := r.UsageSettlementRepairProofInput(ctx, input.Command)
	if err != nil {
		t.Fatal("actual bounded member owner proof input", err)
	}
	var critical struct {
		FactsDigest string `json:"facts_digest"`
	}
	var financial struct {
		AllocationsDigest string `json:"allocations_digest"`
		Expected          int64  `json:"expected_microcredits"`
		Settled           int64  `json:"settled_microcredits"`
		Overage           int64  `json:"overage_microcredits"`
	}
	if json.Unmarshal(proof.CriticalResourceVersions, &critical) != nil || json.Unmarshal(proof.FinancialSnapshot, &financial) != nil || critical.FactsDigest != plan.FactsDigest || financial.AllocationsDigest != plan.AllocationsDigest || financial.Expected != plan.ExpectedMicrocredits || financial.Settled != plan.SettledMicrocredits || financial.Overage != plan.OverageMicrocredits {
		t.Fatal("real proof input differs from the member owner plan")
	}
	return plan, input
}

func f16MemberFinancialSnapshot(t *testing.T, f f16MemberRepairFixture) string {
	t.Helper()
	var raw string
	if err := f.Repository.pool.QueryRow(f.Context, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(q) ORDER BY id) FROM member_quota_grants q),(SELECT jsonb_agg(to_jsonb(a) ORDER BY hold_id,grant_id) FROM member_quota_hold_allocations a),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a),(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return usageRepairEffectSnapshot(t, f.Context, f.Repository) + "\n" + raw
}

func TestF16ReleasedMemberUsageRepairActualPublicChainPostgres(t *testing.T) {
	for _, test := range []struct {
		name                       string
		options                    f16MemberRepairOptions
		settled, overage, personal int64
	}{
		{"fully-granted-surviving-bucket", f16MemberRepairOptions{}, 50, 100, 0},
		{"personal-remainder-is-not-member-funding", f16MemberRepairOptions{Grants: []int64{60}}, 50, 100, 40},
		{"new-hold-reservation-must-survive", f16MemberRepairOptions{NewHold: true}, 50, 100, 0},
		{"suspended-current-membership-remains-suspended", f16MemberRepairOptions{Suspend: true}, 50, 100, 0},
		{"expired-capacity-zero-is-real-overage", f16MemberRepairOptions{NoSurvivingPackage: true}, 0, 100, 0},
		{"multiple-original-grants-one-component", f16MemberRepairOptions{Grants: []int64{40, 40}, HoldMaximum: 180}, 80, 100, 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := f16ActualReleasedMemberFixture(t, test.options)
			r, ctx := f.Repository, f.Context
			plan, input := f16MemberRepairInput(t, f)
			if plan.ExpectedMicrocredits != f.Claimed || plan.SettledMicrocredits != test.settled || plan.OverageMicrocredits != test.overage || plan.SettledMicrocredits+plan.OverageMicrocredits != plan.ExpectedMicrocredits {
				t.Fatalf("member plan expected=%d settled=%d overage=%d", plan.ExpectedMicrocredits, plan.SettledMicrocredits, plan.OverageMicrocredits)
			}
			var holdBBefore string
			if test.options.NewHold {
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT to_jsonb(h) FROM usage_holds h WHERE id=$1),(SELECT jsonb_agg(to_jsonb(a) ORDER BY component_id) FROM usage_hold_allocations a WHERE hold_id=$1),(SELECT jsonb_agg(to_jsonb(a) ORDER BY grant_id) FROM member_quota_hold_allocations a WHERE hold_id=$1))::text`, f.HoldB.ID).Scan(&holdBBefore); err != nil {
					t.Fatal(err)
				}
			}
			var releasesBefore int64
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits WHERE action='HOLD_RELEASED'`).Scan(&releasesBefore); err != nil {
				t.Fatal(err)
			}
			result, err := r.ApplyUsageSettlementRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || result.SettledMicrocredits != test.settled || result.OverageMicrocredits != test.overage {
				t.Fatal("real member repair", err)
			}
			var posted, reserved, receivable, aggregateSettled, aggregateReleased, memberSettled, memberReleased int64
			expectedPosted := int64(100) - test.settled
			if test.options.NoSurvivingPackage {
				expectedPosted = 0
			}
			expectedReserved := int64(0)
			if test.options.NewHold {
				expectedReserved = 20
			}
			if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,reserved_microcredits,receivable_microcredits FROM wallets WHERE id=$1`, f.WalletID).Scan(&posted, &reserved, &receivable); err != nil || posted != expectedPosted || reserved != expectedReserved || receivable != test.overage {
				t.Fatalf("member wallet posted=%d reserved=%d receivable=%d err=%v", posted, reserved, receivable, err)
			}
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT sum(settled_microcredits) FROM usage_hold_allocations WHERE hold_id=$1),(SELECT sum(released_microcredits) FROM usage_hold_allocations WHERE hold_id=$1),(SELECT sum(settled_microcredits) FROM member_quota_hold_allocations WHERE hold_id=$1),(SELECT sum(released_microcredits) FROM member_quota_hold_allocations WHERE hold_id=$1)`, f.HoldA.ID).Scan(&aggregateSettled, &aggregateReleased, &memberSettled, &memberReleased); err != nil || aggregateSettled != test.settled || aggregateReleased != f.Claimed-test.settled || memberSettled != test.settled || memberReleased != f.Claimed-test.settled {
				t.Fatal("original released hold allocation conservation", err)
			}
			var sum, count, walletLeg int64
			expectedPostingCount, expectedWalletLeg := int64(2), int64(0)
			if test.settled > 0 {
				expectedPostingCount, expectedWalletLeg = 3, 1
			}
			if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(delta_microcredits),0),count(*),count(*) FILTER(WHERE credit_lot_id IS NOT NULL) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).Scan(&sum, &count, &walletLeg); err != nil || sum != 0 || count != expectedPostingCount || walletLeg != expectedWalletLeg {
				t.Fatal("real repair journal conservation", err)
			}
			var releasesAfter, settledAudit int64
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE action='HOLD_RELEASED'),COALESCE(sum(amount_microcredits) FILTER(WHERE action='HOLD_SETTLED'),0) FROM member_quota_grant_audits`).Scan(&releasesAfter, &settledAudit); err != nil || releasesAfter != releasesBefore || settledAudit != test.settled {
				t.Fatal("repair duplicated release or missed positive settlement audit", err)
			}
			if test.options.NoSurvivingPackage {
				var expiredGrantPreserved bool
				if err := r.pool.QueryRow(ctx, `SELECT bool_and(status='EXPIRED' AND available_microcredits=0 AND reserved_microcredits=0 AND used_microcredits=0 AND expired_microcredits=100 AND revoked_microcredits=0) FROM member_quota_grants WHERE package_id=$1`, f.ExpiredPackage).Scan(&expiredGrantPreserved); err != nil || !expiredGrantPreserved {
					t.Fatal("zero-capacity repair reopened or consumed expired member quota", err)
				}
			}
			if !test.options.NoSurvivingPackage {
				var available, active, suspended, componentReserved, used, lotRemaining int64
				if err := r.pool.QueryRow(ctx, `SELECT c.available_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,c.reserved_microcredits,c.used_microcredits,l.remaining_microcredits FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id WHERE c.id=$1`, f.SurvivingComponent).Scan(&available, &active, &suspended, &componentReserved, &used, &lotRemaining); err != nil || available != test.personal || componentReserved != expectedReserved || used != test.settled || lotRemaining != 100-test.settled || available+active+suspended+componentReserved+used != 100 {
					t.Fatal("member component/lot buckets conserved", err)
				}
				var grantAvailable, grantReserved, grantUsed int64
				var preserved bool
				expectedStatus, expectedMembership := "ACTIVE", int64(1)
				if test.options.Suspend {
					expectedStatus, expectedMembership = "SUSPENDED", 2
				}
				if err := r.pool.QueryRow(ctx, `SELECT sum(available_microcredits),sum(reserved_microcredits),sum(used_microcredits),bool_and(status=$2 AND membership_version=$3 AND (CASE WHEN $2='SUSPENDED' THEN suspension_reason='MEMBER_REMOVED' ELSE suspension_reason IS NULL END)) FROM member_quota_grants WHERE component_id=$1`, f.SurvivingComponent, expectedStatus, expectedMembership).Scan(&grantAvailable, &grantReserved, &grantUsed, &preserved); err != nil || !preserved || grantReserved != expectedReserved || grantUsed != test.settled || grantAvailable+grantReserved+grantUsed != 100-test.personal {
					t.Fatal("grant identity/status and other-hold reserved balance preserved", err)
				}
				if test.options.Suspend && (active != 0 || suspended != grantAvailable) {
					t.Fatal("suspended credit was reactivated")
				}
			}
			if test.options.NewHold {
				var holdBAfter string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT to_jsonb(h) FROM usage_holds h WHERE id=$1),(SELECT jsonb_agg(to_jsonb(a) ORDER BY component_id) FROM usage_hold_allocations a WHERE hold_id=$1),(SELECT jsonb_agg(to_jsonb(a) ORDER BY grant_id) FROM member_quota_hold_allocations a WHERE hold_id=$1))::text`, f.HoldB.ID).Scan(&holdBAfter); err != nil || holdBAfter != holdBBefore {
					t.Fatal("repair stole or rewrote new hold B", err)
				}
				if committed, err := r.Commit(ctx, f.UsageRequestB, f.HoldB.ID, 20, map[string]any{"input_tokens": int64(20), "output_tokens": int64(0), "cached_input_tokens": int64(0)}); err != nil || committed.Status != "COMMITTED" {
					t.Fatal("real new hold B could not still settle", err)
				}
			}
			beforeReplay := f16MemberFinancialSnapshot(t, f)
			usageRepairFixtureActivation(t, ctx, r.pool)("INACTIVE")
			same, err := r.ApplyUsageSettlementRepair(ctx, input)
			if err != nil || !reflect.DeepEqual(same, result) || f16MemberFinancialSnapshot(t, f) != beforeReplay {
				t.Fatal("member repair receipt replay changed live financial/audit facts", err)
			}
		})
	}
}

func TestF16ReleasedMemberFrozenFactsRejectDriftPostgres(t *testing.T) {
	f := f16ActualReleasedMemberFixture(t, f16MemberRepairOptions{})
	_, input := f16MemberRepairInput(t, f)
	r, ctx := f.Repository, f.Context
	// This is a real lifecycle advance: it preserves the hold's historical v1,
	// wallet balances and component totals, but changes current grant-only facts.
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, f.OrganizationID, f.MemberID, "ACTIVE", 2); err != nil {
		t.Fatal(err)
	}
	before := f16MemberFinancialSnapshot(t, f)
	if _, err := r.ApplyUsageSettlementRepair(ctx, input); !errors.Is(err, ErrApprovalSnapshotMismatch) || f16MemberFinancialSnapshot(t, f) != before {
		t.Fatal("changed member facts passed frozen proof or wrote partial effects", err)
	}
}

func TestF16ReleasedMemberMalformedFactsRejectWithoutEffectsPostgres(t *testing.T) {
	for _, kind := range []string{"wrong-member-identity", "partial-original-release"} {
		t.Run(kind, func(t *testing.T) {
			f := f16ActualReleasedMemberFixture(t, f16MemberRepairOptions{})
			_, input := f16MemberRepairInput(t, f)
			r, ctx := f.Repository, f.Context
			switch kind {
			case "wrong-member-identity":
				if _, err := r.pool.Exec(ctx, `UPDATE member_quota_grants SET member_user_id=$2 WHERE id=$1`, f.SurvivingGrants[0].ID, uuid.New()); err != nil {
					t.Fatal("legal identity counterexample", err)
				}
			case "partial-original-release":
				if _, err := r.pool.Exec(ctx, `UPDATE member_quota_hold_allocations SET released_microcredits=reserved_microcredits-1 WHERE hold_id=$1 AND grant_id=$2`, f.HoldA.ID, f.SurvivingGrants[0].ID); err != nil {
					t.Fatal("legal incomplete-release counterexample", err)
				}
			}
			before := f16MemberFinancialSnapshot(t, f)
			if _, err := r.ApplyUsageSettlementRepair(ctx, input); !errors.Is(err, ErrApprovalSnapshotMismatch) || f16MemberFinancialSnapshot(t, f) != before {
				t.Fatal("malformed original member facts passed or wrote financial effects", err)
			}
		})
	}
}
