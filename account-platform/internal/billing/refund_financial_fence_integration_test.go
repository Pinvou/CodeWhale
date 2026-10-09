package billing

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestRefundChargebackTransfersOutstandingRequestToReview(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled refund")
	if err != nil {
		t.Fatal(err)
	}
	chargeback, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "unexpected"}}
	c.provider = provider
	for range 2 {
		if err := c.ProcessRefund(ctx, refund.ID); err != nil {
			t.Fatal("durable review must stop ordinary command redelivery", err)
		}
	}
	var status, dispatch, packageStatus string
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT refund.status,refund.dispatch_state,package.status,wallet.posted_microcredits FROM refunds refund JOIN entitlement_packages package ON package.id=refund.entitlement_package_id JOIN wallets wallet ON wallet.id=package.wallet_id WHERE refund.id=$1`, refund.ID).Scan(&status, &dispatch, &packageStatus, &posted); err != nil {
		t.Fatal(err)
	}
	if status != "MANUAL_REVIEW" || dispatch != "RESULT_UNKNOWN" || packageStatus != "FROZEN" || posted != 1000000 || provider.calls != 0 || chargeback.ManualReviewCaseID == nil {
		t.Fatalf("chargeback did not retain review and money: %s/%s/%s posted=%d calls=%d", status, dispatch, packageStatus, posted, provider.calls)
	}
	var reviewCases, reviewEvents int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_anomaly_cases WHERE refund_id=$1 AND anomaly_type='REFUND_FAILURE'),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.refund.review-required.v1')`, refund.ID).Scan(&reviewCases, &reviewEvents); err != nil || reviewCases != 1 || reviewEvents != 1 {
		t.Fatalf("refund lacks actionable durable review: cases=%d events=%d err=%v", reviewCases, reviewEvents, err)
	}
}

func TestRefundLegacySourceCannotCreateNewRefund(t *testing.T) {
	for _, resolution := range []string{"", "DISMISS"} {
		t.Run("legacy-"+resolution, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			source, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-legacy-admission", 100, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce an earlier incorrect thaw without inventing a resolution
			// receipt. Source evidence must still fence the current runtime API.
			if _, err := r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code=NULLIF($2,''),resolved_at=now() WHERE id=$1`, source.ID, resolution); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET status='ACTIVE' WHERE id=$1`, pack); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET status='ACTIVE' WHERE package_id=$1`, pack); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET attention_summary='NORMAL' WHERE id=$1`, order.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled source guard"); !errors.Is(err, ErrConflict) {
				t.Fatalf("legacy source admitted new refund: %v", err)
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM refunds WHERE entitlement_package_id=$1`, pack).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected creation wrote refund: %d %v", count, err)
			}
		})
	}
}

func TestDismissFinancialFreezeRequiresOriginEvidence(t *testing.T) {
	for _, kind := range []string{"chargeback", "forced"} {
		for _, state := range []string{"ACTIVE", "FROZEN", "legacy-missing-snapshot", "legacy-other-resolved"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				_, _, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
				if state == "FROZEN" {
					if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN' WHERE package_id=$1`, pack); err != nil {
						t.Fatal(err)
					}
				}
				var caseID uuid.UUID
				if kind == "chargeback" {
					source, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-origin-chargeback", nil)
					if err != nil {
						t.Fatal(err)
					}
					caseID = *source.ManualReviewCaseID
				} else {
					source, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-origin-forced", 100, nil)
					if err != nil {
						t.Fatal(err)
					}
					caseID = *source.ManualReviewCaseID
				}
				if state == "legacy-other-resolved" {
					old, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-unproven-legacy", 100, nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolved_at=now() WHERE id=$1`, old.ID); err != nil {
						t.Fatal(err)
					}
				}
				if state == "legacy-missing-snapshot" {
					statement := fmt.Sprintf(`ALTER TABLE payment_anomaly_cases DISABLE TRIGGER payment_anomaly_cases_protect_evidence; UPDATE payment_anomaly_cases SET resource_snapshot=resource_snapshot-'entitlement_snapshot' WHERE id='%s'; ALTER TABLE payment_anomaly_cases ENABLE TRIGGER payment_anomaly_cases_protect_evidence`, caseID)
					if kind == "forced" {
						statement = fmt.Sprintf(`UPDATE forced_refund_cases SET entitlement_snapshot='{}'::jsonb WHERE manual_review_case_id='%s'`, caseID)
					}
					if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := retainedResolveClaimedAnomaly(t, r, ctx, caseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled disproved financial evidence", "NONE"); err != nil {
					t.Fatal(err)
				}
				var packageStatus string
				var frozenComponents int
				if err := r.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM entitlement_components WHERE package_id=$1 AND status='FROZEN') FROM entitlement_packages WHERE id=$1`, pack).Scan(&packageStatus, &frozenComponents); err != nil {
					t.Fatal(err)
				}
				want := "FROZEN"
				if state == "ACTIVE" {
					want = "ACTIVE"
				}
				if packageStatus != want || (state != "ACTIVE" && frozenComponents == 0) {
					t.Fatalf("origin lost: package=%s components=%d want=%s", packageStatus, frozenComponents, want)
				}
			})
		}
	}
}

func TestRefundProcessorFinancialFenceStopsRedeliveryWithoutProvider(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled refund")
	if err != nil {
		t.Fatal(err)
	}
	// An independently recorded receivable is a durable wallet-wide gate.
	if _, err = r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced", 100, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate an old outstanding request predating the writer transition.
	if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='REQUESTED' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "unexpected"}}
	c.provider = provider
	for range 2 {
		if err := c.ProcessRefund(ctx, refund.ID); err != nil {
			t.Fatal("fenced ordinary command should transfer to durable review", err)
		}
	}
	var status string
	var leaseClear bool
	var anomalies int
	if err := r.pool.QueryRow(ctx, `SELECT status,dispatch_lease_until IS NULL,(SELECT count(*) FROM payment_anomaly_cases WHERE refund_id=$1 AND anomaly_type='REFUND_FAILURE') FROM refunds WHERE id=$1`, refund.ID).Scan(&status, &leaseClear, &anomalies); err != nil {
		t.Fatal(err)
	}
	if status != "MANUAL_REVIEW" || !leaseClear || provider.calls != 0 || anomalies != 1 {
		t.Fatalf("fence did not terminate retry safely: status=%s lease=%v calls=%d cases=%d", status, leaseClear, provider.calls, anomalies)
	}
}

func TestRefundFailedPreservesLegacyResolvedForcedFreeze(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled refund")
	if err != nil {
		t.Fatal(err)
	}
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-legacy", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolved_at=now() WHERE id=$1`, forced.ID); err != nil {
		t.Fatal(err)
	}
	// A late provider FAILED receipt may end its own request, but is not
	// evidence that an older, separately resolved financial freeze is safe.
	if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='REQUESTED' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	result, _, err := c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: "FAILED"}, false)
	if err != nil || result.Status != "FAILED" {
		t.Fatalf("own terminal receipt rejected: %s %v", result.Status, err)
	}
	var packageStatus, forcedStatus string
	var activeComponents int
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT package.status,forced.status,wallet.posted_microcredits,(SELECT count(*) FROM entitlement_components WHERE package_id=package.id AND status='ACTIVE') FROM entitlement_packages package JOIN wallets wallet ON wallet.id=package.wallet_id JOIN forced_refund_cases forced ON forced.entitlement_package_id=package.id WHERE forced.id=$1`, forced.ID).Scan(&packageStatus, &forcedStatus, &posted, &activeComponents); err != nil {
		t.Fatal(err)
	}
	if packageStatus != "FROZEN" || forcedStatus != "RESOLVED" || activeComponents != 0 || posted != 1000000 {
		t.Fatalf("legacy evidence was implicitly released: package=%s forced=%s active=%d posted=%d", packageStatus, forcedStatus, activeComponents, posted)
	}
}

func TestDismissFreezeSnapshotDoesNotAuthorizeLaterAdministrativeEpisode(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	first, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-previous-episode", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *first.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled previous financial episode", "NONE"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN' WHERE id=$1;`, pack); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN' WHERE package_id=$1`, pack); err != nil {
		t.Fatal(err)
	}
	second, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-later-episode", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *second.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled later disproved chargeback", "NONE"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&status); err != nil || status != "FROZEN" {
		t.Fatalf("stale source resurrected administrative freeze: %s %v", status, err)
	}
}

func TestDismissRejectsFreezeSnapshotBoundToOtherWallet(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	source, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-snapshot-binding", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`UPDATE forced_refund_cases SET entitlement_snapshot=jsonb_set(entitlement_snapshot,'{package,wallet_id}',to_jsonb('%s'::text)) WHERE id='%s'`, uuid.New(), source.ID)
	if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *source.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled mismatched snapshot receipt", "NONE"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&status); err != nil || status != "FROZEN" {
		t.Fatalf("foreign snapshot restored rights: %s %v", status, err)
	}
}

func TestFailedRefundRestoresOnlyItsOwnGrantSuspension(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	var component uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
		t.Fatal(err)
	}
	active, suspended := uuid.New(), uuid.New()
	for _, item := range []struct {
		id             uuid.UUID
		status, reason string
	}{{active, "ACTIVE", ""}, {suspended, "SUSPENDED", "ACCOUNT_CLOSED"}} {
		if _, err := r.pool.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,suspension_reason,membership_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',100000,100000,$6,NULLIF($7,''),1,$3,$1::uuid::text,repeat('a',64),now()+interval '1 day',now(),now())`, item.id, uuid.New(), uuid.New(), pack, component, item.status, item.reason); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=800000,active_grant_microcredits=100000,suspended_grant_microcredits=100000 WHERE id=$1`, component); err != nil {
		t.Fatal(err)
	}
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled member grant refund")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE member_quota_grants SET suspension_reason='MEMBER_REMOVED' WHERE id=$1`, suspended); err != nil {
		t.Fatal(err)
	}
	c.provider = &reviewRefundProvider{refund: ProviderRefund{Status: "FAILED"}}
	if err := c.ProcessRefund(ctx, refund.ID); err != nil {
		t.Fatal(err)
	}
	var restored, held, heldReason string
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT status FROM member_quota_grants WHERE id=$1),(SELECT status FROM member_quota_grants WHERE id=$2),(SELECT suspension_reason FROM member_quota_grants WHERE id=$2)`, active, suspended).Scan(&restored, &held, &heldReason); err != nil || restored != "ACTIVE" || held != "SUSPENDED" || heldReason != "MEMBER_REMOVED" {
		t.Fatalf("foreign administrative suspension overwritten: %s %s %s %v", restored, held, heldReason, err)
	}
}
