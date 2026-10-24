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
				coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
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
