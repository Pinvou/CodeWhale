package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Run over the public Billing migration wrapper and its restricted runtime.
// The catalog, paid organization order, quota grants and pauses all use actual
// production producers. Only the external Provider is controlled by the fixture.
func TestForcedRefundMemberGrantTerminalStatePostgres(t *testing.T) {
	t.Run("inflight", testFinancialReviewPreservesInFlightMemberGrantPostgres)
	for _, originalReason := range []string{"ACTIVE", "MEMBER_REMOVED", "ACCOUNT_CLOSED"} {
		for _, mode := range []string{"full", "partial_remaining", "partial_exhausts_one_grant"} {
			t.Run("prior_"+originalReason+"/"+mode, func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				var runtime string
				var superuser, schemaOwner bool
				if err := r.pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),pg_has_role(current_user,'billing_schema_owner','MEMBER')`).Scan(&runtime, &superuser, &schemaOwner); err != nil || runtime != "billing_service" || superuser || schemaOwner {
					t.Fatal("restricted Billing runtime required", runtime, superuser, schemaOwner, err)
				}
				actor, org := uuid.New(), uuid.New()
				if _, err := r.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); err != nil {
					t.Fatal(err)
				}
				wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", org)
				if err != nil {
					t.Fatal(err)
				}
				input := validRechargeProductForEditTests()
				input.ProductID = "FORCED_GRANT_" + uuid.NewString()
				input.Mapping.ProviderID, input.Mapping.ExternalSKUBOM = "controlled", input.ProductID
				product, err := r.CreateRechargeProduct(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				verification, _, err := r.StartProductAdmissionVerification(ctx, product.CatalogItemID, actor, nil)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled", "controlled-admission", "forced-grant", time.Now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.TransitionRechargeProduct(ctx, product.CatalogItemID, product.Version, "ON_SALE", &proof); err != nil {
					t.Fatal(err)
				}
				provider := &retainedCreateRaceProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
				coordinator, err := newBillingFixturePaymentCoordinator(r, provider, "test")
				if err != nil {
					t.Fatal(err)
				}
				coordinator.AllowTentativePrices(true)
				provider.coordinator = coordinator
				order, _, err := coordinator.CreateOrganizationMobileOrder(ctx, actor, wallet.ID, org, input.ProductID, "ALIPAY", "", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				var transaction, pack, component uuid.UUID
				var base, total int64
				if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id,c.id,c.available_microcredits,w.posted_microcredits FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id JOIN entitlement_components c ON c.package_id=p.id JOIN wallets w ON w.id=p.wallet_id WHERE t.payment_order_id=$1 AND c.component_type='BASE'`, order.ID).Scan(&transaction, &pack, &component, &base, &total); err != nil {
					t.Fatal(err)
				}
				count := int64(1)
				if mode == "partial_exhausts_one_grant" {
					count = 2
				}
				for i := int64(0); i < count; i++ {
					member := uuid.New()
					if _, err = r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 1); err != nil {
						t.Fatal(err)
					}
					_, created, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: org, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: base / count, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()})
					if err != nil || !created {
						t.Fatal("actual grant allocation", err)
					}
					if originalReason != "ACTIVE" {
						if _, err = r.ApplyOrganizationMemberLifecycle(ctx, org, member, "REVOKED", 2); err != nil {
							t.Fatal(err)
						}
						if originalReason == "ACCOUNT_CLOSED" {
							if _, err = r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				cash := []int64{order.AmountFen}
				if mode == "partial_remaining" {
					cash = []int64{order.AmountFen * 95 / 100, order.AmountFen * 5 / 100}
				}
				if mode == "partial_exhausts_one_grant" {
					cash = []int64{order.AmountFen / 2, order.AmountFen / 2}
				}
				var cumulative int64
				for step, amount := range cash {
					var priorGrants string
					if step == 0 {
						if err = r.pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object('id',id,'status',status,'suspension_reason',suspension_reason,'available_microcredits',available_microcredits) ORDER BY id)::text FROM member_quota_grants WHERE package_id=$1`, pack).Scan(&priorGrants); err != nil {
							t.Fatal(err)
						}
					}
					item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, fmt.Sprintf("forced-grant-%d", step), amount, json.RawMessage(`{"controlled_provider":true}`))
					if err != nil {
						t.Fatal("retain confirmed external fact", err)
					}
					var evidence string
					if err = r.pool.QueryRow(ctx, `SELECT resource_snapshot::text FROM payment_anomaly_cases WHERE forced_refund_case_id=$1`, item.ID).Scan(&evidence); err != nil {
						t.Fatal(err)
					}
					if step == 0 {
						// ACTIVE is the producer's pre-review state. The real freeze
						// turns it into SUSPENDED/FORCED_REFUND_REVIEW before Apply.
						frozenReason := originalReason
						if originalReason == "ACTIVE" {
							frozenReason = "FORCED_REFUND_REVIEW"
						}
						var frozenGrants string
						var matching int64
						if err = r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='SUSPENDED' AND suspension_reason=$2),jsonb_agg(jsonb_build_object('id',id,'status',status,'suspension_reason',suspension_reason,'available_microcredits',available_microcredits) ORDER BY id)::text FROM member_quota_grants WHERE package_id=$1`, pack, frozenReason).Scan(&matching, &frozenGrants); err != nil || matching != count {
							t.Fatal("actual pre-Apply grant state differs from the reachable freeze", matching, frozenGrants, err)
						}
						t.Logf("actual grants before review: %s; after RecordAndFreeze/before Apply: %s", priorGrants, frozenGrants)
						var preserved int64
						status, reason := "SUSPENDED", originalReason
						if originalReason == "ACTIVE" {
							status, reason = "ACTIVE", ""
						}
						if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_anomaly_cases a CROSS JOIN LATERAL jsonb_array_elements(a.resource_snapshot->'entitlement_snapshot'->'grants') g WHERE a.forced_refund_case_id=$1 AND g->>'status'=$2 AND g->>'suspension_reason' IS NOT DISTINCT FROM NULLIF($3,'')`, item.ID, status, reason).Scan(&preserved); err != nil || preserved != count {
							t.Fatal("original suspension provenance absent from immutable evidence", preserved, err)
						}
						_, err = r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET resource_snapshot='{}' WHERE forced_refund_case_id=$1`, item.ID)
						var immutable *pgconn.PgError
						if !errors.As(err, &immutable) || immutable.Code != "P0001" || immutable.Message != "payment anomaly evidence is immutable" {
							t.Fatal("original review evidence guard did not execute", err)
						}
					}
					beforeApply := refundRecoveryMoneySnapshot(t, ctx, r)
					result, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
					if err != nil {
						if !reflect.DeepEqual(beforeApply, refundRecoveryMoneySnapshot(t, ctx, r)) {
							t.Fatal("rejected forced refund leaked partial financial effects", err)
						}
						t.Fatalf("confirmed forced refund must atomically reclaim available grant: %v", err)
					}
					if result.ReceivableID != nil || result.RevokedBase != base*amount/order.AmountFen || result.RevokedBonus != (total-base)*amount/order.AmountFen {
						t.Fatal("fully reclaimable source has incorrect receipt or debt", result)
					}
					cumulative += amount
					var available, revoked, posted, postingSum int64
					var terminal, suspended int64
					var valid bool
					if err = r.pool.QueryRow(ctx, `SELECT sum(available_microcredits),sum(revoked_microcredits),count(*) FILTER(WHERE status='REVOKED' AND suspension_reason IS NULL),count(*) FILTER(WHERE status='SUSPENDED' AND suspension_reason=$2),bool_and(available_microcredits+reserved_microcredits+used_microcredits+expired_microcredits+revoked_microcredits=original_microcredits) FROM member_quota_grants WHERE package_id=$1`, pack, map[bool]string{true: "FORCED_REFUND_REVIEW", false: originalReason}[originalReason == "ACTIVE"]).Scan(&available, &revoked, &terminal, &suspended, &valid); err != nil {
						t.Fatal(err)
					}
					expectedRevoked := base * cumulative / order.AmountFen
					if !valid || revoked != expectedRevoked || available != base-expectedRevoked {
						t.Fatal("grant conservation lost", available, revoked, valid)
					}
					if cumulative == order.AmountFen && terminal != count || cumulative < order.AmountFen && mode == "partial_remaining" && (suspended != 1 || terminal != 0) || cumulative < order.AmountFen && mode == "partial_exhausts_one_grant" && (suspended != 1 || terminal != 1) {
						t.Fatal("terminal/remaining pause reason mismatch", terminal, suspended)
					}
					if err = r.pool.QueryRow(ctx, `SELECT bool_and(available_microcredits+active_grant_microcredits+suspended_grant_microcredits+reserved_microcredits+used_microcredits+expired_microcredits+revoked_microcredits+reclassified_out_microcredits=original_microcredits) FROM entitlement_components WHERE package_id=$1`, pack).Scan(&valid); err != nil || !valid {
						t.Fatal("component buckets lost conservation", err)
					}
					if err = r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, wallet.ID).Scan(&posted); err != nil || posted != total-total*cumulative/order.AmountFen {
						t.Fatal("wallet effect mismatch", posted, err)
					}
					if err = r.pool.QueryRow(ctx, `SELECT COALESCE(sum(p.delta_microcredits),0) FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference=$1`, "forced-refund:"+item.ID.String()).Scan(&postingSum); err != nil || postingSum != 0 {
						t.Fatal("unbalanced forced-refund journal", postingSum, err)
					}
					before := refundRecoveryMoneySnapshot(t, ctx, r)
					replay, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
					if err != nil || replay.RevokedBase != result.RevokedBase || replay.RevokedBonus != result.RevokedBonus || replay.Version != result.Version || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
						t.Fatal("forced refund replay repeated a financial effect", err)
					}
					var afterEvidence string
					if err = r.pool.QueryRow(ctx, `SELECT resource_snapshot::text FROM payment_anomaly_cases WHERE forced_refund_case_id=$1`, item.ID).Scan(&afterEvidence); err != nil || afterEvidence != evidence {
						t.Fatal("original evidence lost while clearing terminal reason", err)
					}
				}
			})
		}
	}
}

// The retained paid order uses the supported historical-order fixture; quota,
// reservation, review, settlement and replay run their production writers under
// the public migrations and restricted Billing role. No live channel is used.
func testFinancialReviewPreservesInFlightMemberGrantPostgres(t *testing.T) {
	for _, review := range []string{"forced_refund", "chargeback"} {
		for _, settlement := range []string{"release", "commit_zero", "commit_partial"} {
			t.Run(review+"/"+settlement, func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				_, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
				org, member, actor := *order.BeneficiaryOrganizationID, uuid.New(), uuid.New()
				if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 1); err != nil {
					t.Fatal(err)
				}
				var component uuid.UUID
				if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
					t.Fatal(err)
				}
				grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: org, MemberUserID: member, PackageID: pack, ComponentID: component, ComponentType: "BASE", AmountMicrocredits: 1000000, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
				hold, err := r.ReserveMemberQuota(ctx, uuid.New(), order.WalletID, org, member, 1, 400000, nil, r.now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				var replayReview func() error
				reason := "FORCED_REFUND_REVIEW"
				if review == "forced_refund" {
					item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, uuid.NewString(), 60, json.RawMessage(`{"controlled":true}`))
					if err != nil {
						t.Fatal(err)
					}
					replayReview = func() error { _, err := r.ApplyReviewedForcedRefund(ctx, item.ID); return err }
				} else {
					reason = "CHARGEBACK_REVIEW"
					item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, uuid.NewString(), json.RawMessage(`{"controlled":true}`))
					if err != nil {
						t.Fatal(err)
					}
					replayReview = func() error {
						_, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID)
						return err
					}
				}
				if err := replayReview(); err != nil {
					t.Fatal("apply reviewed financial action", err)
				}
				var status, pause string
				var reserved, available, revoked int64
				if err := r.pool.QueryRow(ctx, `SELECT status,COALESCE(suspension_reason,''),reserved_microcredits,available_microcredits,revoked_microcredits FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&status, &pause, &reserved, &available, &revoked); err != nil || status != "SUSPENDED" || pause != reason || reserved != 400000 || available != 0 || revoked != 600000 {
					t.Fatal("review prematurely retired the grant still owned by an in-flight hold", status, pause, reserved, available, revoked, err)
				}
				used := int64(0)
				if settlement == "commit_partial" {
					used = 100000
				}
				finish := func() error {
					if settlement == "release" {
						_, err := r.Release(ctx, hold.UsageRequestID, hold.ID, "USER_STOPPED")
						return err
					}
					_, err := r.Commit(ctx, hold.UsageRequestID, hold.ID, used, map[string]any{"controlled": true})
					return err
				}
				if err := finish(); err != nil {
					t.Fatal("settle original in-flight hold", err)
				}
				var active, suspended, componentReserved, componentUsed, grantUsed, posted, walletReserved int64
				var conserved bool
				if err := r.pool.QueryRow(ctx, `SELECT g.status,COALESCE(g.suspension_reason,''),g.available_microcredits,g.reserved_microcredits,g.used_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,c.reserved_microcredits,c.used_microcredits,w.posted_microcredits,w.reserved_microcredits,
 g.available_microcredits+g.reserved_microcredits+g.used_microcredits+g.expired_microcredits+g.revoked_microcredits=g.original_microcredits
 AND c.available_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits+c.reserved_microcredits+c.used_microcredits+c.expired_microcredits+c.revoked_microcredits+c.reclassified_out_microcredits=c.original_microcredits
 FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN wallets w ON w.id=$2 WHERE g.id=$1`, grant.ID, order.WalletID).Scan(&status, &pause, &available, &reserved, &grantUsed, &active, &suspended, &componentReserved, &componentUsed, &posted, &walletReserved, &conserved); err != nil || status != "SUSPENDED" || pause != reason || available != 400000-used || reserved != 0 || grantUsed != used || active != 0 || suspended != available || componentReserved != 0 || componentUsed != used || posted != 400000-used || walletReserved != 0 || !conserved {
					t.Fatal("unused reservation must remain paused and conserved, never return to an active orphan bucket", status, pause, available, reserved, grantUsed, active, suspended, componentReserved, componentUsed, posted, walletReserved, conserved, err)
				}
				before := refundRecoveryMoneySnapshot(t, ctx, r)
				if err := finish(); err != nil {
					t.Fatal("terminal hold replay", err)
				}
				if err := replayReview(); err != nil {
					t.Fatal("financial action replay", err)
				}
				if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
					t.Fatal("terminal replay changed financial facts")
				}
			})
		}
	}
}
