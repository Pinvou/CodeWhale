package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"time"
)

func retainedOrganizationFinanceOrder(t *testing.T, r *PostgresRepository, ctx context.Context, credit int64, paid bool) (*PaymentCoordinator, PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	if _, err := r.ApplyOrganizationLifecycle(ctx, owner, "ACTIVE", 1, false); err != nil {
		t.Fatal(err)
	}
	wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", owner)
	if err != nil {
		t.Fatal(err)
	}
	now := r.now()
	order := PaymentOrder{ID: uuid.New(), MerchantOrderNo: "fixture-" + uuid.NewString(), RequesterID: owner, BeneficiaryOrganizationID: &owner, WalletID: wallet.ID, ProductID: "CNY_1", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CreditMicrocredits: credit, Status: "PENDING", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(time.Hour), ProviderID: "controlled", ProviderVersion: "v1"}
	snapshot, _ := json.Marshal(Product{ProductID: order.ProductID, PlanName: "controlled legacy credit", AmountFen: 100, CreditMicrocredits: credit, BasePoints: 1, ValidityDays: 30, Currency: "CNY"})
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,beneficiary_organization_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','PENDING',$11,$11,$11::timestamptz+interval '72 hours',$11::timestamptz+interval '8 days',$12,$12,$13,$14)`, order.ID, order.MerchantOrderNo, owner, wallet.ID, order.ProductID, snapshot, order.Channel, order.AmountFen, order.Currency, credit, order.ExpiresAt, now, order.ProviderID, order.ProviderVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	provider := &retainedFinancialProvider{result: ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &id, PaidAt: &now, ExternalCustomerKey: owner.String()}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !paid {
		return c, order, uuid.Nil, uuid.Nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	order, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "fixture-paid-"+id, []byte(`{"controlled":true}`), provider.result, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var transaction, pack uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	return c, order, transaction, pack
}

func TestRetainedOrganizationFinancialDismissal(t *testing.T) {
	for _, lifecycle := range []string{"active-version", "removed", "expired-plan", "reserved"} {
		t.Run(lifecycle, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			member := uuid.New()
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
			if lifecycle == "reserved" {
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 100, nil, r.now().Add(time.Hour)); err != nil {
					t.Fatal("legal pre-review reservation failed", err)
				}
			}
			chargeback, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-first-review-forced", 5, json.RawMessage(`{"confirmed":true}`))
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
			firstActor := uuid.New()
			if _, err := r.ClaimPaymentAnomaly(ctx, *chargeback.ManualReviewCaseID, firstActor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *chargeback.ManualReviewCaseID, firstActor, uuid.NewString(), "DISMISS", "controlled chargeback was disproved", "NONE"); err != nil {
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
			actor, dismissalKey := uuid.New(), uuid.NewString()
			if _, err := r.ClaimPaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, dismissalKey, "DISMISS", "controlled forced refund was disproved", "NONE"); err != nil {
				t.Fatal(err)
			}
			var capacity, posted int64
			if err := r.pool.QueryRow(ctx, `SELECT c.available_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits+c.reserved_microcredits,w.posted_microcredits FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id JOIN wallets w ON w.id=p.wallet_id WHERE c.id=$1`, request.ComponentID).Scan(&capacity, &posted); err != nil || capacity != 1000000 || posted != 1000000 {
				t.Fatal("dismissal changed retained capacity or posted money", capacity, posted, err)
			}
			if lifecycle == "reserved" {
				var grantReserved, walletReserved int64
				if err := r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits,w.reserved_microcredits FROM member_quota_grants g JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&grantReserved, &walletReserved); err != nil || grantReserved != 100 || walletReserved != 100 {
					t.Fatal("financial dismissal changed admitted reservation", grantReserved, walletReserved, err)
				}
			}
			facts := retainedOrganizationDismissFacts(t, r, ctx, grant.ID)
			if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, dismissalKey, "DISMISS", "controlled forced refund was disproved", "NONE"); err != nil {
				t.Fatal("same-key dismissal replay failed", err)
			}
			if facts != retainedOrganizationDismissFacts(t, r, ctx, grant.ID) {
				t.Fatal("same-key replay restored capacity twice or issued money/events")
			}
			if lifecycle == "removed" {
				var reason string
				if err := r.pool.QueryRow(ctx, `SELECT suspension_reason FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&reason); err != nil || reason != "MEMBER_REMOVED" {
					t.Fatal("inactive member financial reason was not transferred", reason, err)
				}
			}
			if lifecycle == "active-version" || lifecycle == "reserved" {
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
					var expiredCapacity, posted int64
					if err := r.pool.QueryRow(ctx, `SELECT g.expired_microcredits,w.posted_microcredits FROM member_quota_grants g JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&expiredCapacity, &posted); err != nil || expiredCapacity != 500000 || posted != 0 {
						t.Fatal("ordinary expiry did not conserve member/posted capacity", expiredCapacity, posted, err)
					}
					facts := retainedOrganizationDismissFacts(t, r, ctx, grant.ID)
					if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 {
						t.Fatal("expiry was not single winner", count, err)
					}
					if facts != retainedOrganizationDismissFacts(t, r, ctx, grant.ID) {
						t.Fatal("repeat expiry changed financial facts")
					}
				}
			}
		})
	}
}

func TestRetainedOrganizationChargebackFinancialDismissal(t *testing.T) {
	for _, lifecycle := range []string{"active-version", "removed", "expired-plan", "reserved"} {
		t.Run(lifecycle, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			member := uuid.New()
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
			if lifecycle == "reserved" {
				if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 100, nil, r.now().Add(time.Hour)); err != nil {
					t.Fatal("legal pre-review reservation failed", err)
				}
			}
			chargeback, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-two-review-chargeback", json.RawMessage(`{"confirmed":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if lifecycle == "active-version" {
				retainedOrganizationFinancialDownFence(t, r, ctx, grant.ID)
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
			firstActor := uuid.New()
			if _, err := r.ClaimPaymentAnomaly(ctx, *chargeback.ManualReviewCaseID, firstActor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *chargeback.ManualReviewCaseID, firstActor, uuid.NewString(), "DISMISS", "controlled chargeback was disproved", "NONE"); err != nil {
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
			actor, dismissalKey := uuid.New(), uuid.NewString()
			if _, err := r.ClaimPaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, dismissalKey, "DISMISS", "controlled forced refund was disproved", "NONE"); err != nil {
				t.Fatal(err)
			}
			var capacity, posted int64
			if err := r.pool.QueryRow(ctx, `SELECT c.available_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits+c.reserved_microcredits,w.posted_microcredits FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id JOIN wallets w ON w.id=p.wallet_id WHERE c.id=$1`, request.ComponentID).Scan(&capacity, &posted); err != nil || capacity != 1000000 || posted != 1000000 {
				t.Fatal("dismissal changed retained capacity or posted money", capacity, posted, err)
			}
			if lifecycle == "reserved" {
				var grantReserved, walletReserved int64
				if err := r.pool.QueryRow(ctx, `SELECT g.reserved_microcredits,w.reserved_microcredits FROM member_quota_grants g JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&grantReserved, &walletReserved); err != nil || grantReserved != 100 || walletReserved != 100 {
					t.Fatal("financial dismissal changed admitted reservation", grantReserved, walletReserved, err)
				}
			}
			facts := retainedOrganizationDismissFacts(t, r, ctx, grant.ID)
			if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, dismissalKey, "DISMISS", "controlled forced refund was disproved", "NONE"); err != nil {
				t.Fatal("same-key dismissal replay failed", err)
			}
			if facts != retainedOrganizationDismissFacts(t, r, ctx, grant.ID) {
				t.Fatal("same-key replay restored capacity twice or issued money/events")
			}
			if lifecycle == "removed" {
				var reason string
				if err := r.pool.QueryRow(ctx, `SELECT suspension_reason FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&reason); err != nil || reason != "MEMBER_REMOVED" {
					t.Fatal("inactive member financial reason was not transferred", reason, err)
				}
			}
			if lifecycle == "active-version" || lifecycle == "reserved" {
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
					var expiredCapacity, posted int64
					if err := r.pool.QueryRow(ctx, `SELECT g.expired_microcredits,w.posted_microcredits FROM member_quota_grants g JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&expiredCapacity, &posted); err != nil || expiredCapacity != 500000 || posted != 0 {
						t.Fatal("ordinary expiry did not conserve member/posted capacity", expiredCapacity, posted, err)
					}
					facts := retainedOrganizationDismissFacts(t, r, ctx, grant.ID)
					if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 {
						t.Fatal("expiry was not single winner", count, err)
					}
					if facts != retainedOrganizationDismissFacts(t, r, ctx, grant.ID) {
						t.Fatal("repeat expiry changed financial facts")
					}
				}
			}
		})
	}
}

func retainedOrganizationDismissFacts(t *testing.T, r *PostgresRepository, ctx context.Context, grantID uuid.UUID) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',to_jsonb(g),'component',to_jsonb(c),'wallet',to_jsonb(w),'journal',(SELECT count(*) FROM journal_entries),'outbox',(SELECT count(*) FROM outbox_events),'commands',(SELECT count(*) FROM payment_anomaly_commands))::text FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grantID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestRetainedOrganizationDismissWalletLockGate(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-wallet-serialized-review", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err := r.ClaimPaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	blocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var blockerPID int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM wallets WHERE id=$1 FOR UPDATE`, order.WalletID).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled financial dismissal waits for wallet", "NONE")
		result <- err
	}()
	gateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	observed := false
	for !observed {
		var waits bool
		err := r.pool.QueryRow(gateContext, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waits)
		if err != nil {
			blocker.Rollback(ctx)
			<-result
			t.Fatal("wallet lock gate not observed", err)
		}
		observed = waits
		if !observed {
			select {
			case e := <-result:
				t.Fatal("dismiss completed before wallet lock release", e)
			case <-ticker.C:
			}
		}
	}
	t.Log("observed PostgreSQL blocking-pid wallet gate")
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("serialized dismissal altered money", err)
	}
}

func TestRetainedOrganizationDismissKeepsLegacyRefundFence(t *testing.T) {
	for _, status := range []string{"REQUESTED", "MANUAL_REVIEW"} {
		t.Run(status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			member := uuid.New()
			if _, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
				t.Fatal(err)
			}
			var component uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
				t.Fatal(err)
			}
			grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", ComponentID: component, PackageID: pack, AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			// The mature retained-refund fixture opts in only to construct an existing
			// queued refund through its real producer. Production default remains disabled.
			c.AllowNormalRefundCreation(true)
			refund, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled retained refund reservation")
			if err != nil || !created {
				t.Fatal("retained refund producer", err)
			}
			if status == "MANUAL_REVIEW" {
				refund, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: "controlled conflicting provider fact", AmountFen: refund.AmountFen, Status: "SUCCESS"}, false)
				if err != nil || refund.Status != status {
					t.Fatal("real provider conflict did not preserve review", err)
				}
			}
			review, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled review on legacy refund", nil)
			if err != nil {
				t.Fatal(err)
			}
			rights := func() string {
				var facts string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',to_jsonb(g),'component',to_jsonb(c),'wallet',to_jsonb(w))::text FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&facts); err != nil {
					t.Fatal(err)
				}
				return facts
			}
			actor := uuid.New()
			if _, err := r.ClaimPaymentAnomaly(ctx, *review.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			before := rights()
			if _, err = r.ResolvePaymentAnomaly(ctx, *review.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled disproved chargeback keeps refund fence", "NONE"); err != nil {
				t.Fatal(err)
			}
			if before != rights() {
				t.Fatal("dismissal changed rights behind another pending refund")
			}
			// Recording the external financial fence durably moves REQUESTED to
			// MANUAL_REVIEW; dismissal must retain this uncertainty and its rights fence.
			var actual, componentState string
			if err = r.pool.QueryRow(ctx, `SELECT r.status,c.status FROM refunds r JOIN entitlement_components c ON c.package_id=r.entitlement_package_id AND c.component_type='BASE' WHERE r.id=$1`, refund.ID).Scan(&actual, &componentState); err != nil || actual != "MANUAL_REVIEW" || componentState != "FROZEN" {
				t.Fatal("retained refund financial fence removed", actual, componentState, err)
			}
		})
	}
}

func retainedOrganizationFinancialDownFence(t *testing.T, r *PostgresRepository, ctx context.Context, grantID uuid.UUID) {
	t.Helper()
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User = "billing_migrator"
	configuration.Password = ""
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	full, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = billingschema.Ordered(full); err != nil {
		t.Fatal(err)
	}
	var reason, ledger string
	if err = connection.QueryRow(ctx, `SELECT suspension_reason FROM member_quota_grants WHERE id=$1`, grantID).Scan(&reason); err != nil || reason != "CHARGEBACK_REVIEW" {
		t.Fatal("real chargeback producer did not persist mature reason", reason, err)
	}
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	facts := retainedOrganizationDismissFacts(t, r, ctx, grantID)
	err = migrations.DownOne(ctx, connection, full)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
		t.Fatal("public highest Down failed to preserve mature reason", err)
	}
	var after string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`).Scan(&after); err != nil || ledger != after {
		t.Fatal("rejected rollback changed ledger", err)
	}
	if facts != retainedOrganizationDismissFacts(t, r, ctx, grantID) {
		t.Fatal("rejected rollback changed new-reason quota/financial facts")
	}
}

// Owner-only fixture mutations represent old missing or stale projection evidence.
// Runtime dismissal must retain all entitlement facts instead of guessing ACTIVE.
func TestF06FinancialDismissKeepsUnknownMembership(t *testing.T) {
	for _, evidence := range []string{"missing", "older-version"} {
		t.Run(evidence, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			member := uuid.New()
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "ACTIVE", 2); err != nil {
				t.Fatal(err)
			}
			request := MemberQuotaAllocation{OrganizationID: *order.BeneficiaryOrganizationID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 2, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString(), PackageID: pack}
			if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&request.ComponentID); err != nil {
				t.Fatal(err)
			}
			grant, _, err := r.AllocateMemberQuota(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			review, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-unknown-membership", 5, nil)
			if err != nil {
				t.Fatal(err)
			}
			statement := "DELETE FROM organization_member_lifecycle_states WHERE organization_id='" + request.OrganizationID.String() + "' AND user_id='" + member.String() + "'"
			if evidence == "older-version" {
				statement = "UPDATE organization_member_lifecycle_states SET membership_version=1 WHERE organization_id='" + request.OrganizationID.String() + "' AND user_id='" + member.String() + "'"
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
				t.Fatal(err)
			}
			facts := func() string {
				var value string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',to_jsonb(g),'component',to_jsonb(c),'package',to_jsonb(p),'wallet',to_jsonb(w))::text FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN entitlement_packages p ON p.id=g.package_id JOIN wallets w ON w.id=p.wallet_id WHERE g.id=$1`, grant.ID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			actor := uuid.New()
			if _, err := r.ClaimPaymentAnomaly(ctx, *review.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			before := facts()
			if _, err := r.ResolvePaymentAnomaly(ctx, *review.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled disproved event keeps unknown membership", "NONE"); err != nil {
				t.Fatal(err)
			}
			if after := facts(); after != before {
				t.Fatal("unknown membership changed frozen entitlement or money facts")
			}
		})
	}
}
