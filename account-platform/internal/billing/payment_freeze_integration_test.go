package billing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresExistingHoldTerminalPreservesPaymentFreeze(t *testing.T) {
	for _, terminal := range []string{"commit", "release"} {
		t.Run(terminal, func(t *testing.T) {
			r, wallet, transaction := forcedRefundPaidSource(t)
			ctx := context.Background()
			hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-held-freeze", 50, json.RawMessage(`{"confirmed":true}`))
			if err != nil {
				t.Fatal(err)
			}
			used := int64(0)
			if terminal == "commit" {
				used = 50
				_, err = r.Commit(ctx, hold.UsageRequestID, hold.ID, used, map[string]any{"controlled_usage": true})
			} else {
				_, err = r.Release(ctx, hold.UsageRequestID, hold.ID, "controlled original hold release")
			}
			if err != nil {
				t.Fatal("original hold cannot reach its legitimate terminal", err)
			}
			current, err := r.GetWallet(ctx, wallet.ID)
			if err != nil || current.PostedMicrocredits != wallet.PostedMicrocredits-used || current.ReservedMicrocredits != 0 {
				t.Fatal("terminal funds were changed", err)
			}
			var packageStatus, componentStatus, reviewStatus string
			var available, reserved, actualUsed int64
			if err := r.pool.QueryRow(ctx, `SELECT package.status,component.status,component.available_microcredits,component.reserved_microcredits,component.used_microcredits,forced.status FROM entitlement_packages package JOIN entitlement_components component ON component.package_id=package.id JOIN forced_refund_cases forced ON forced.entitlement_package_id=package.id WHERE forced.id=$1`, item.ID).Scan(&packageStatus, &componentStatus, &available, &reserved, &actualUsed, &reviewStatus); err != nil {
				t.Fatal(err)
			}
			if packageStatus != "FROZEN" || componentStatus != "FROZEN" || reviewStatus != "REVIEW_REQUIRED" || reserved != 0 || actualUsed != used || available != wallet.PostedMicrocredits-used {
				t.Fatal("terminal reopened a payment review or broke component conservation")
			}
			if _, err := r.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, time.Now().Add(time.Hour)); err == nil {
				t.Fatal("pending financial review allowed a new hold")
			}
		})
	}
}

func TestPostgresDismissPreservesOtherReviewAndMemberLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"active-version", "removed", "expired-plan"} {
		t.Run(lifecycle, func(t *testing.T) {
			r, wallet, transaction := forcedRefundPaidSource(t)
			ctx, member := context.Background(), uuid.New()
			if lifecycle == "expired-plan" {
				mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
				if err != nil {
					t.Fatal(err)
				}
				coordinator, err := NewPaymentCoordinatorWithProvider(r, &legacyMappingProvider{}, mappings, "test")
				if err != nil {
					t.Fatal(err)
				}
				coordinator.AllowTentativePrices(true)
				order, _, err := coordinator.CreateOrganizationMobileOrder(ctx, uuid.New(), wallet.ID, wallet.OwnerID, "PLAN_TRIAL", "ALIPAY", "", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&transaction); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
				t.Fatal(err)
			}
			request := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}
			if err := r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&request.ComponentID, &request.PackageID); err != nil {
				t.Fatal(err)
			}
			grant, _, err := r.AllocateMemberQuota(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			chargeback, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-two-review-chargeback", json.RawMessage(`{"confirmed":true}`))
			if err != nil {
				t.Fatal(err)
			}
			forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-two-review-forced", 5, json.RawMessage(`{"confirmed":true}`))
			if err != nil {
				t.Fatal(err)
			}
			version := int64(2)
			status := "ACTIVE"
			if lifecycle == "removed" {
				status = "REVOKED"
			}
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, status, version); err != nil {
				t.Fatal(err)
			}
			if lifecycle == "expired-plan" {
				var expires time.Time
				if err := r.pool.QueryRow(ctx, `SELECT valid_until FROM entitlement_packages WHERE id=$1`, request.PackageID).Scan(&expires); err != nil {
					t.Fatal(err)
				}
				r.now = func() time.Time { return expires.Add(time.Second) }
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *chargeback.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled chargeback was disproved", "NONE"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, version, 100, nil, r.now().Add(time.Hour)); err == nil {
				t.Fatal("another pending financial case was reopened")
			}
			var pending, componentStatus string
			if err := r.pool.QueryRow(ctx, `SELECT status FROM forced_refund_cases WHERE id=$1`, forced.ID).Scan(&pending); err != nil || pending != "REVIEW_REQUIRED" {
				t.Fatal("other review disappeared", err)
			}
			if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_components WHERE id=$1`, request.ComponentID).Scan(&componentStatus); err != nil || componentStatus != "FROZEN" {
				t.Fatal("other review's component fence disappeared", err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled forced refund was disproved", "NONE"); err != nil {
				t.Fatal(err)
			}
			if lifecycle == "active-version" {
				var actualVersion int64
				if err := r.pool.QueryRow(ctx, `SELECT membership_version FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&actualVersion); err != nil || actualVersion != version {
					t.Fatal("active member's local version was stranded", err)
				}
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, version, 100, nil, r.now().Add(time.Hour)); err != nil {
					t.Fatal("final disproved review stranded legal quota", err)
				}
			} else {
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, version, 100, nil, r.now().Add(time.Hour)); err == nil {
					t.Fatal("dismiss revived removed or expired quota")
				}
				if lifecycle == "removed" {
					if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 3); err != nil {
						t.Fatal(err)
					}
					if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 3, 100, nil, r.now().Add(time.Hour)); err != nil {
						t.Fatal("later authoritative rejoin cannot recover existing quota", err)
					}
				} else {
					if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 1 {
						t.Fatal("disproved review stranded actual expiry", count, err)
					}
					var expired string
					if err := r.pool.QueryRow(ctx, `SELECT status FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&expired); err != nil || expired != "EXPIRED" {
						t.Fatal("expiry was not the financial winner", err)
					}
				}
			}
		})
	}
}
