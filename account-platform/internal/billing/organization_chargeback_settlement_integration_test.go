package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrganizationChargebackDeletionEvidencePostgres(t *testing.T) {
	for _, scenario := range []string{"dismissed", "open", "claimed", "confirmed", "confirmed-without-ledger", "confirmed-legacy-version-one", "confirmed-legacy-no-ledger", "confirmed-legacy-version-one-modern-anomaly", "confirmed-contradictory-disposition", "confirmed-review-open", "confirmed-review-financial-pending", "confirmed-without-receipt", "confirmed-corrupt-receipt", "confirmed-misbound-anomaly", "confirmed-manual-review-open", "confirmed-receipt-owner-mismatch", "dismissed-without-receipt", "dismissed-corrupt-receipt", "dismissed-with-revocation", "dismissed-with-pending-financial-action", "legacy-resolved", "balance-remains", "hold-remains", "receivable-remains", "dismissed-with-journal", "confirmed-open-receivable"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-deletion-"+uuid.NewString(), json.RawMessage(`{"controlled":true}`))
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			if scenario != "open" {
				if _, err = r.ClaimPaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(scenario, "confirmed") {
				key := uuid.NewString()
				if _, completed, err := r.PreparePaymentAnomalyResolution(ctx, *item.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled reviewed confirmation"); err != nil || completed {
					t.Fatal("prepare original confirmation intent", completed, err)
				}
				if _, err = r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled reviewed confirmation", "APPLIED"); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "open" && scenario != "claimed" {
				if _, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled review was disproved", "NONE"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "hold-remains" {
				member := uuid.New()
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
					t.Fatal(err)
				}
				var component uuid.UUID
				if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
					t.Fatal(err)
				}
				if _, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 10, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}); err != nil {
					t.Fatal(err)
				}
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 1, nil, r.now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "hold-remains" && scenario != "balance-remains" && scenario != "open" && scenario != "claimed" && !strings.HasPrefix(scenario, "confirmed") {
				var until time.Time
				if err = r.pool.QueryRow(ctx, `SELECT valid_until FROM entitlement_packages WHERE id=$1`, pack).Scan(&until); err != nil {
					t.Fatal(err)
				}
				r.now = func() time.Time { return until.Add(time.Second) }
				if n, err := r.ExpirePackages(ctx, 100); err != nil || n != 1 {
					t.Fatal("actual expiry", n, err)
				}
			}
			fault := func(sql string, args ...any) {
				t.Helper()
				if _, err := r.pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "confirmed-contradictory-disposition":
				fault(`UPDATE chargeback_cases SET resolution_code='DISMISS' WHERE id=$1`, item.ID)
			case "confirmed-legacy-version-one", "confirmed-legacy-no-ledger":
				// Synthetic retained pre-029 shape, not a migration or a modern
				// pending review disguised by only resetting its version.
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `
ALTER TABLE payment_anomaly_commands DISABLE TRIGGER payment_anomaly_commands_immutable;
ALTER TABLE payment_anomaly_resolution_intents DISABLE TRIGGER payment_anomaly_resolution_intents_immutable;
ALTER TABLE manual_review_case_events DISABLE TRIGGER manual_review_case_events_immutable;
UPDATE chargeback_cases SET manual_review_case_id=NULL,resolution_code=NULL,resolution_reason=NULL,version=1,resolved_at=NULL;
DELETE FROM payment_anomaly_commands; DELETE FROM payment_anomaly_resolution_intents;
DELETE FROM manual_review_case_events; DELETE FROM payment_anomaly_cases; DELETE FROM manual_review_cases;
ALTER TABLE payment_anomaly_commands ENABLE TRIGGER payment_anomaly_commands_immutable;
ALTER TABLE payment_anomaly_resolution_intents ENABLE TRIGGER payment_anomaly_resolution_intents_immutable;
ALTER TABLE manual_review_case_events ENABLE TRIGGER manual_review_case_events_immutable;`); err != nil {
					t.Fatal(err)
				}
				if scenario == "confirmed-legacy-no-ledger" {
					if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_immutable; UPDATE journal_entries SET business_reference='controlled-corrupt-'||id::text WHERE entry_type='CHARGEBACK'; ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_immutable`); err != nil {
						t.Fatal(err)
					}
				}
			case "confirmed-legacy-version-one-modern-anomaly":
				fault(`UPDATE chargeback_cases SET manual_review_case_id=NULL,resolution_code=NULL,version=1,resolved_at=NULL WHERE id=$1`, item.ID)
			case "confirmed-review-open":
				fault(`UPDATE payment_anomaly_cases SET status='OPEN',resolved_at=NULL WHERE id=$1`, *item.ManualReviewCaseID)
			case "confirmed-review-financial-pending":
				fault(`UPDATE payment_anomaly_cases SET financial_action_status='PENDING' WHERE id=$1`, *item.ManualReviewCaseID)
			case "confirmed-without-receipt":
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE payment_anomaly_commands DISABLE TRIGGER payment_anomaly_commands_immutable; DELETE FROM payment_anomaly_commands WHERE action='CONFIRM_CHARGEBACK'; ALTER TABLE payment_anomaly_commands ENABLE TRIGGER payment_anomaly_commands_immutable`); err != nil {
					t.Fatal(err)
				}
			case "confirmed-corrupt-receipt":
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE payment_anomaly_commands DISABLE TRIGGER payment_anomaly_commands_immutable; UPDATE payment_anomaly_commands SET after_snapshot=jsonb_set(after_snapshot,'{payment_transaction_id}',to_jsonb('00000000-0000-4000-8000-000000000099'::text)) WHERE action='CONFIRM_CHARGEBACK'; ALTER TABLE payment_anomaly_commands ENABLE TRIGGER payment_anomaly_commands_immutable`); err != nil {
					t.Fatal(err)
				}
			case "confirmed-misbound-anomaly":
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE payment_anomaly_cases DISABLE TRIGGER payment_anomaly_cases_protect_evidence; UPDATE payment_anomaly_cases SET payment_transaction_id=NULL WHERE anomaly_type='CHARGEBACK'; ALTER TABLE payment_anomaly_cases ENABLE TRIGGER payment_anomaly_cases_protect_evidence`); err != nil {
					t.Fatal(err)
				}
			case "confirmed-manual-review-open":
				fault(`UPDATE manual_review_cases SET status='OPEN',resolved_at=NULL WHERE id=$1`, *item.ManualReviewCaseID)
			case "confirmed-receipt-owner-mismatch":
				fault(`UPDATE manual_review_cases SET owner_id=$2 WHERE id=$1`, *item.ManualReviewCaseID, uuid.New())
			case "confirmed-without-ledger":
				// Owned-database fault injection; never alter production migration identities.
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_immutable; UPDATE journal_entries SET business_reference='controlled-corrupt-'||id::text WHERE entry_type='CHARGEBACK'; ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_immutable`); err != nil {
					t.Fatal(err)
				}
			case "dismissed-without-receipt":
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE payment_anomaly_commands DISABLE TRIGGER payment_anomaly_commands_immutable; DELETE FROM payment_anomaly_commands WHERE action='DISMISS'; ALTER TABLE payment_anomaly_commands ENABLE TRIGGER payment_anomaly_commands_immutable`); err != nil {
					t.Fatal(err)
				}
			case "dismissed-corrupt-receipt":
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE payment_anomaly_commands DISABLE TRIGGER payment_anomaly_commands_immutable; UPDATE payment_anomaly_commands SET after_snapshot=jsonb_set(after_snapshot,'{payment_transaction_id}',to_jsonb('00000000-0000-4000-8000-000000000099'::text)) WHERE action='DISMISS'; ALTER TABLE payment_anomaly_commands ENABLE TRIGGER payment_anomaly_commands_immutable`); err != nil {
					t.Fatal(err)
				}
			case "dismissed-with-revocation":
				fault(`UPDATE chargeback_cases SET revoked_base_microcredits=1 WHERE id=$1`, item.ID)
			case "dismissed-with-pending-financial-action":
				fault(`UPDATE payment_anomaly_cases SET financial_action_status='PENDING' WHERE id=$1`, *item.ManualReviewCaseID)
			case "legacy-resolved":
				// Simulate a retained pre-029 row without guessing a modern disposition.
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE chargeback_cases DROP CONSTRAINT chargeback_cases_status_check`); err != nil {
					t.Fatal(err)
				}
				fault(`UPDATE chargeback_cases SET status='RESOLVED',resolution_code=NULL WHERE id=$1`, item.ID)
			case "dismissed-with-journal":
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err = insertJournal(ctx, tx, uuid.New(), "CHARGEBACK", "payment-transaction:"+transaction.String()+":chargeback:"+item.ProviderChargebackID, "controlled corrupt evidence", r.now()); err != nil {
					tx.Rollback(ctx)
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			case "receivable-remains", "confirmed-open-receivable":
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = createReceivableWithinTransaction(ctx, tx, wallet.ID, "CHARGEBACK", "controlled-unresolved-"+uuid.NewString(), 1, &order.ID, &transaction, &pack, nil, r.now()); err != nil {
					tx.Rollback(ctx)
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DISABLED", 2, false); err != nil || !a.Ready {
				t.Fatal(a, err)
			}
			before := organizationChargebackRetainedFacts(t, r, ctx)
			beforeDeleteState := organizationChargebackDeletionState(t, r, ctx)
			a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, true)
			if err != nil {
				t.Fatal(err)
			}
			if before != organizationChargebackRetainedFacts(t, r, ctx) {
				t.Fatal("assessment changed financial or review evidence")
			}
			settled := scenario == "dismissed" || scenario == "confirmed" || scenario == "confirmed-legacy-version-one"
			if settled {
				if !a.Ready || len(a.Blockers) != 0 {
					t.Fatalf("fully settled history blocked deletion: %+v", a)
				}
				if a, err = r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, false); err != nil || !a.Ready {
					t.Fatal(a, err)
				}
				if got, err := r.GetWallet(ctx, wallet.ID); err != nil || got.Status != "CLOSED" || got.PostedMicrocredits != 0 || got.ReservedMicrocredits != 0 {
					t.Fatal(got, err)
				}
				if before != organizationChargebackRetainedFacts(t, r, ctx) {
					t.Fatal("logical deletion changed retained financial/review history")
				}
			} else {
				want := "UNRESOLVED_CHARGEBACK"
				if scenario == "balance-remains" {
					want = "WALLET_BALANCE_NOT_ZERO"
				}
				if scenario == "hold-remains" {
					want = "UNSETTLED_USAGE_HOLD"
				}
				if scenario == "receivable-remains" || scenario == "confirmed-open-receivable" {
					want = "WALLET_RECEIVABLE_OPEN"
				}
				found := false
				for _, b := range a.Blockers {
					found = found || b.Code == want
				}
				if a.Ready || !found {
					t.Fatalf("missing %s: %+v", want, a)
				}
				// The write path must preserve the same fence and all retained
				// facts, not only the public preflight assessment.
				if actual, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, false); err != nil || actual.Ready {
					t.Fatal("actual delete escaped fence", actual, err)
				}
				if before != organizationChargebackRetainedFacts(t, r, ctx) {
					t.Fatal("blocked actual deletion changed financial/review facts")
				}
				if beforeDeleteState != organizationChargebackDeletionState(t, r, ctx) {
					t.Fatal("blocked deletion wrote wallet, lifecycle or outbox")
				}
				if got, err := r.GetWallet(ctx, wallet.ID); err != nil || got.Status != "FROZEN" {
					t.Fatal("blocked deletion closed wallet", got, err)
				}
			}
		})
	}
	t.Run("prepared-confirmation-interrupted-before-review-receipt", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
		wallet, err := r.GetWallet(ctx, order.WalletID)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-cutpoint-"+uuid.NewString(), json.RawMessage(`{"controlled":true}`))
		if err != nil {
			t.Fatal(err)
		}
		actor, key := uuid.New(), uuid.NewString()
		reason := "controlled interrupted confirmation"
		retainedClaimAnomaly(t, r, ctx, *c.ManualReviewCaseID, actor)
		if _, done, err := r.PreparePaymentAnomalyResolution(ctx, *c.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", reason); err != nil || done {
			t.Fatal(done, err)
		}
		// Same real two-transaction sequence as adminResolvePaymentAnomaly.
		if _, err := r.ApplyConfirmedChargeback(ctx, c.PaymentEvidenceID, c.ProviderChargebackID); err != nil {
			t.Fatal(err)
		}
		detail, err := r.PaymentAnomalyDetail(ctx, *c.ManualReviewCaseID)
		if err != nil || detail.Case.Status != "OPEN" || detail.Case.FinancialActionStatus != "NONE" {
			t.Fatal("not the genuine production cutpoint", detail, err)
		}
		if a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DISABLED", 2, false); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		before := organizationChargebackRetainedFacts(t, r, ctx)
		beforeDeleteState := organizationChargebackDeletionState(t, r, ctx)
		for _, preflight := range []bool{true, false} {
			a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, preflight)
			found := false
			for _, b := range a.Blockers {
				found = found || b.Code == "UNRESOLVED_CHARGEBACK"
			}
			if err != nil || a.Ready || !found {
				t.Fatal("unfinished real confirmation escaped", preflight, a, err)
			}
			if before != organizationChargebackRetainedFacts(t, r, ctx) {
				t.Fatal("blocked operation changed retained facts")
			}
			if beforeDeleteState != organizationChargebackDeletionState(t, r, ctx) {
				t.Fatal("blocked real cutpoint wrote wallet, lifecycle or outbox")
			}
			var status string
			var version int64
			if err := r.pool.QueryRow(ctx, `SELECT status,account_version FROM organization_lifecycle_states WHERE organization_id=$1`, wallet.OwnerID).Scan(&status, &version); err != nil || status != "DISABLED" || version != 2 {
				t.Fatal("blocked delete changed lifecycle", status, version, err)
			}
			if got, err := r.GetWallet(ctx, wallet.ID); err != nil || got.Status != "FROZEN" {
				t.Fatal(got, err)
			}
		}
		if final, err := r.ResolvePaymentAnomaly(ctx, *c.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", reason, "APPLIED"); err != nil || final.Status != "RESOLVED" {
			t.Fatal(final, err)
		}
		closedFacts := organizationChargebackRetainedFacts(t, r, ctx)
		for _, preflight := range []bool{true, false} {
			if a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, preflight); err != nil || !a.Ready {
				t.Fatal("fully resolved original intent remained blocked", preflight, a, err)
			}
			if closedFacts != organizationChargebackRetainedFacts(t, r, ctx) {
				t.Fatal("resolved delete rewrote retained history")
			}
		}
		if got, err := r.GetWallet(ctx, wallet.ID); err != nil || got.Status != "CLOSED" {
			t.Fatal(got, err)
		}
	})
	t.Run("executed-shortfall-retains-financial-and-review-proof", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
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
		if _, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 1000000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
		usage := uuid.New()
		hold, err := r.ReserveMemberQuota(ctx, usage, wallet.ID, wallet.OwnerID, member, 1, 1000000, nil, r.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(ctx, usage, hold.ID, 1000000, map[string]any{"controlled": "shortfall-consumption"}); err != nil {
			t.Fatal(err)
		}
		c, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-shortfall-"+uuid.NewString(), json.RawMessage(`{"controlled":true}`))
		if err != nil {
			t.Fatal(err)
		}
		actor := uuid.New()
		retainedClaimAnomaly(t, r, ctx, *c.ManualReviewCaseID, actor)
		if _, err := r.ResolvePaymentAnomaly(ctx, *c.ManualReviewCaseID, actor, uuid.NewString(), "ESCALATE_FINANCIAL_REPAIR", "controlled shortfall escalation", "PENDING"); err != nil {
			t.Fatal(err)
		}
		activation := chargebackFixtureActivation(t, ctx, r.pool)("ACTIVE")
		plan, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, ChargebackFinancialRepairCommandInput{ChargebackCaseID: c.ID, PaymentEvidenceID: c.PaymentEvidenceID}, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := r.ApplyChargebackFinancialRepair(ctx, chargebackFrozenInput(plan, activation)); err != nil || result.Status != "SUCCEEDED" {
			t.Fatal(result, err)
		}
		var settled bool
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE((`+organizationChargebackSettledSQL+`),false) FROM chargeback_cases c JOIN wallets w ON w.id=c.wallet_id WHERE c.id=$1`, c.ID).Scan(&settled); err != nil || !settled {
			t.Fatal("exact executed shortfall proof rejected", settled, err)
		}
		if a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DISABLED", 2, false); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		before := organizationChargebackRetainedFacts(t, r, ctx)
		for _, preflight := range []bool{true, false} {
			a, err := r.ApplyOrganizationLifecycle(ctx, wallet.OwnerID, "DELETED", 3, preflight)
			if err != nil || a.Ready {
				t.Fatal("open shortfall debt escaped", a, err)
			}
			debt := false
			for _, b := range a.Blockers {
				if b.Code == "UNRESOLVED_CHARGEBACK" {
					t.Fatal("closed shortfall mislabeled unresolved", a)
				}
				debt = debt || b.Code == "WALLET_RECEIVABLE_OPEN"
			}
			if !debt || before != organizationChargebackRetainedFacts(t, r, ctx) {
				t.Fatal("shortfall deletion weakened debt or mutated evidence", a)
			}
		}
		if _, err := r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='PENDING' WHERE id=$1`, *c.ManualReviewCaseID); err != nil {
			t.Fatal(err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE((`+organizationChargebackSettledSQL+`),false) FROM chargeback_cases c JOIN wallets w ON w.id=c.wallet_id WHERE c.id=$1`, c.ID).Scan(&settled); err != nil || settled {
			t.Fatal("inconsistent executed repair accepted", settled, err)
		}
	})
	t.Run("empty-organization-still-deletes", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		owner := uuid.New()
		for i, status := range []string{"ACTIVE", "DISABLED", "DELETED"} {
			if a, err := r.ApplyOrganizationLifecycle(ctx, owner, status, int64(i+1), false); err != nil || !a.Ready {
				t.Fatal(a, err)
			}
		}
	})
	t.Run("unit-rejects-retained-legacy-and-misbound-terminal-evidence", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		_, _, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
		item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-unit-boundary", json.RawMessage(`{"controlled":true}`))
		if err != nil {
			t.Fatal(err)
		}
		actor := uuid.New()
		if _, err = r.ClaimPaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled unit history review", "NONE"); err != nil {
			t.Fatal(err)
		}
		org, unit := uuid.New(), uuid.New()
		if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 1); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		wallet, _, err := r.EnsureWallet(ctx, "ORG_UNIT", unit)
		if err != nil {
			t.Fatal(err)
		}
		// Unit payment issuance is unsupported today. This is explicitly a
		// corrupt retained cross-wallet row, not a reachable new unit purchase.
		if _, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET wallet_id=$2 WHERE id=$1`, item.ID, wallet.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil {
			t.Fatal(err)
		}
		for _, legacy := range []bool{false, true} {
			if legacy {
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE chargeback_cases DROP CONSTRAINT chargeback_cases_status_check`); err != nil {
					t.Fatal(err)
				}
				if _, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='RESOLVED',resolution_code=NULL WHERE id=$1`, item.ID); err != nil {
					t.Fatal(err)
				}
			}
			a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, b := range a.Blockers {
				found = found || b.Code == "UNRESOLVED_CHARGEBACK"
			}
			if a.Ready || !found {
				t.Fatal("unit accepted unproven retained history", legacy, a)
			}
		}
	})

	t.Run("unit-production-empty-and-financial-boundaries", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		org, unit := uuid.New(), uuid.New()
		if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 1); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		wallet, _, err := r.EnsureWallet(ctx, "ORG_UNIT", unit)
		if err != nil {
			t.Fatal(err)
		}
		adj, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 10, "controlled unit funds", uuid.New(), uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ExecuteApprovedAdjustment(ctx, adj.ID, uuid.New(), uuid.NewString(), uuid.New(), "controlled independent approval"); err != nil {
			t.Fatal(err)
		}
		if _, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil {
			t.Fatal(err)
		}
		a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
		if err != nil || a.Ready {
			t.Fatal("unit funds escaped blockers", a, err)
		}
		empty := uuid.New()
		for _, next := range []struct {
			s string
			v int64
		}{{"ACTIVE", 1}, {"DELETION_PENDING", 1}, {"DELETED", 2}} {
			if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, empty, next.s, next.v); err != nil || !a.Ready {
				t.Fatal(a, err)
			}
		}
	})
}

func organizationChargebackRetainedFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var raw string
	// Wallet lifecycle/version and its outbox are the intended logical-delete
	// effect; financial/review/journal history must remain byte-for-byte retained.
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('wallet_financial',(SELECT jsonb_agg(jsonb_build_object('id',w.id,'posted',w.posted_microcredits,'reserved',w.reserved_microcredits,'receivable',w.receivable_microcredits,'financial_status',w.financial_status) ORDER BY w.id) FROM wallets w),'orders',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM payment_orders o),'transactions',(SELECT jsonb_agg(to_jsonb(tx) ORDER BY id) FROM payment_transactions tx),'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM payment_evidence e),'cases',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_cases c),'commands',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM payment_anomaly_commands c),'anomalies',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM payment_anomaly_cases c),'intents',(SELECT jsonb_agg(to_jsonb(c) ORDER BY payment_anomaly_case_id) FROM payment_anomaly_resolution_intents c),'manual',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM manual_review_cases c),'manual_events',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM manual_review_case_events c),'repair',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_financial_repair_cases c),'journals',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM journal_entries c),'postings',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM ledger_postings c),'receivables',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM receivables c),'packages',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_packages c),'components',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c))::text`).Scan(&raw); err != nil {
		t.Fatal(fmt.Errorf("retained history: %w", err))
	}
	return raw
}

func organizationChargebackDeletionState(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var raw string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(s) ORDER BY organization_id) FROM organization_lifecycle_states s),(SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e))::text`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
