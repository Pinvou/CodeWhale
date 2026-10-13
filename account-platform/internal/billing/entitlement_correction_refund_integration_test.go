package billing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Uses the inherited opt-in, owned UUID database and real runtime roles. The
// operator only activates the already installed approval command; all financial
// operations below call production coordinators/repositories.
func correctedRefundFixture(t *testing.T, actualBase, expectedBase int64) (*PostgresRepository, context.Context, *PaymentCoordinator, PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	c, pending, _, _ := retainedFinanceOrder(t, r, ctx, 100, false)
	now := r.now()
	order := pending
	order.ID, order.MerchantOrderNo, order.Status, order.FulfillmentStatus = uuid.New(), "corrected-"+uuid.NewString(), "PAID", "CREDITED"
	transaction, pack, journal := uuid.New(), uuid.New(), uuid.New()
	reference := "payment-transaction:" + transaction.String()
	product, err := json.Marshal(Product{ProductID: order.ProductID, PlanName: "controlled split", AmountFen: 100, CreditMicrocredits: 100, BasePoints: expectedBase, BonusPoints: 100 - expectedBase, Currency: "CNY", ValidityDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	walletAccount, err := walletLedgerAccount(ctx, tx, order.WalletID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,$5,$6,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$7,$7,$8,$9,$10,$10,'controlled','v1')`, order.ID, order.MerchantOrderNo, order.RequesterID, order.WalletID, order.ProductID, product, now.Add(time.Hour), now.Add(72*time.Hour), now.Add(8*24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at,fulfillment_status,fulfilled_at,provider_id,provider_version) VALUES($1,$2,'ALIPAY',$3,'PAID',100,'CNY','QUERY',$4,$4,$4,$4,'CREDITED',$4,'controlled','v1')`, transaction, order.ID, "corrected-txn-"+transaction.String(), now); err != nil {
		t.Fatal(err)
	}
	if err = insertJournal(ctx, tx, journal, "RECHARGE", reference, "controlled original funding", now); err != nil {
		t.Fatal(err)
	}
	type originalComponent struct {
		id     uuid.UUID
		kind   string
		amount int64
	}
	components := []originalComponent{}
	if actualBase > 0 {
		components = append(components, originalComponent{uuid.New(), "BASE", actualBase})
	}
	if actualBase < 100 {
		components = append(components, originalComponent{uuid.New(), "BONUS", 100 - actualBase})
	}
	for _, component := range components {
		lotReference := reference
		if component.kind == "BONUS" {
			lotReference += ":bonus"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,$4,$4,$5,$6,$7)`, component.id, order.WalletID, lotReference, component.amount, component.kind == "BASE", now.Add(30*24*time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,$7,'controlled','',100,100,100,$8,$8,$9,'ACTIVE',$8,$8,'PAYMENT',$10)`, pack, order.RequesterID, order.WalletID, order.ID, transaction, components[0].id, order.ProductID, now, now.Add(30*24*time.Hour), reference); err != nil {
		t.Fatal(err)
	}
	for _, component := range components {
		if _, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,$3,$1,$4,$4,$5,$6,'ACTIVE',$5,$5)`, component.id, pack, component.kind, component.amount, now, now.Add(30*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err = insertPosting(ctx, tx, journal, walletAccount, component.amount, &component.id, now); err != nil {
			t.Fatal(err)
		}
	}
	if err = insertPosting(ctx, tx, journal, paymentClearingAccount, -100, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=100,version=version+1 WHERE id=$1`, order.WalletID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	operatorConfig := r.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "billing_migrator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(ctx)
	op, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Rollback(ctx)
	if _, err = op.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err = op.Exec(ctx, `UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2`); err != nil {
		t.Fatal(err)
	}
	if err = op.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := r.planEntitlementComponentCorrection(ctx, pack, order.ID)
	if err != nil {
		t.Fatal("actual correction precheck", err)
	}
	frozen := entitlementCorrectionFrozenFacts{WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PaymentTransactionID: plan.PaymentTransactionID, SourceComponentID: plan.SourceComponentID, SourceLotID: plan.SourceLotID, TargetComponentID: plan.TargetComponentID, TargetLotID: plan.TargetLotID, SourceType: plan.Amounts.SourceType, TargetType: plan.Amounts.TargetType, AmountMicrocredits: plan.Amounts.Amount, SourceEvidenceDigest: plan.SourceEvidenceDigest, FactsDigest: plan.FactsDigest}
	approval := uuid.New()
	result, err := r.ExecuteEntitlementComponentCorrection(ctx, approval, uuid.New(), 2, pack, order.ID, uuid.New(), uuid.New(), "approval:"+approval.String()+":ENTITLEMENT_COMPONENT_ADJUSTMENT", "controlled approved correction", "ORIGINAL_SPLIT_MISMATCH", frozen)
	if err != nil || result.Status != "SUCCEEDED" {
		t.Fatalf("actual correction result=%+v err=%v", result, err)
	}
	correctedRefundAudit(t, ctx, r, c, order.ID, "PAYMENT")
	return r, ctx, c, order, transaction, pack
}

func correctedRefundAudit(t *testing.T, ctx context.Context, r *PostgresRepository, c *PaymentCoordinator, orderID uuid.UUID, required string) {
	t.Helper()
	facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, "ALIPAY", r.now().Add(-time.Hour), r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, fact := range facts {
		if fact.PaymentOrderID != orderID {
			continue
		}
		if !fact.LedgerValid {
			t.Fatalf("corrected %s financial integrity rejected: %s", fact.EntryType, fact.IntegrityCaseType)
		}
		if fact.EntryType == required {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing actual %s reconciliation fact", required)
	}
}

func correctedRefundUnchanged(t *testing.T, ctx context.Context, r *PostgresRepository, before map[string]string) {
	t.Helper()
	if after := entitlementFinancialFactsSnapshot(t, ctx, r); !reflect.DeepEqual(before, after) {
		for table, rows := range before {
			if after[table] != rows {
				t.Errorf("unexpected financial effect in %s", table)
			}
		}
		t.FailNow()
	}
}

func correctedRefundTerminal(t *testing.T, ctx context.Context, r *PostgresRepository, order PaymentOrder, pack uuid.UUID) {
	t.Helper()
	var valid bool
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT posted_microcredits=0 FROM wallets WHERE id=$1) AND (SELECT status='REVOKED' FROM entitlement_packages WHERE id=$2) AND NOT EXISTS(SELECT 1 FROM entitlement_components WHERE package_id=$2 AND (status<>'REVOKED' OR revoked_microcredits+reclassified_out_microcredits<>original_microcredits)) AND NOT EXISTS(SELECT 1 FROM receivables WHERE wallet_id=$1 AND outstanding_microcredits>0) AND NOT EXISTS(SELECT 1 FROM ledger_postings WHERE delta_microcredits=0)`, order.WalletID, pack).Scan(&valid); err != nil || !valid {
		t.Fatalf("terminal effective entitlement accounting invalid: %v", err)
	}
}

func TestF09CorrectedForcedRefundCumulativeAndReplay(t *testing.T) {
	r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 60, 80)
	for i, step := range []struct{ cash, base, bonus int64 }{{1, 2, 1}, {1, 1, 0}, {1, 0, 0}, {97, 77, 19}} {
		item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "corrected-forced-"+uuid.NewString(), step.cash, nil)
		if err != nil {
			t.Fatal(err)
		}
		actor, key := uuid.New(), uuid.NewString()
		retainedClaimAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor)
		if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *item.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled reviewed source"); err != nil {
			t.Fatal(err)
		}
		applied, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
		if err != nil || applied.RevokedBase != step.base || applied.RevokedBonus != step.bonus || applied.ReceivableID != nil {
			t.Fatalf("step%d per-component ceiling receipt=%+v err=%v", i, applied, err)
		}
		if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled reviewed source", "APPLIED"); err != nil {
			t.Fatal(err)
		}
		before := entitlementFinancialFactsSnapshot(t, ctx, r)
		if replay, err := r.ApplyReviewedForcedRefund(ctx, item.ID); err != nil || replay.ID != applied.ID || replay.RevokedBase != applied.RevokedBase || replay.RevokedBonus != applied.RevokedBonus {
			t.Fatalf("financial replay changed: %+v %v", replay, err)
		}
		correctedRefundUnchanged(t, ctx, r, before)
		correctedRefundAudit(t, ctx, r, c, order.ID, "REFUND")
	}
	correctedRefundTerminal(t, ctx, r, order, pack)
}

func TestF09CorrectedChargebackDoesNotCreatePhantomDebt(t *testing.T) {
	r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 100, 80)
	item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "corrected-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID)
	if err != nil || applied.RevokedBaseMicrocredits != 80 || applied.RevokedBonusMicrocredits != 20 || applied.ReceivableID != nil {
		t.Fatalf("chargeback receipt=%+v err=%v", applied, err)
	}
	before := entitlementFinancialFactsSnapshot(t, ctx, r)
	if replay, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err != nil || replay.ID != applied.ID {
		t.Fatal("chargeback replay", err)
	}
	correctedRefundUnchanged(t, ctx, r, before)
	correctedRefundTerminal(t, ctx, r, order, pack)
	correctedRefundAudit(t, ctx, r, c, order.ID, "CHARGEBACK")
}

func TestF09CorrectedOrdinaryFullSourceRefund(t *testing.T) {
	for _, outcome := range []string{"SUCCESS", "FAILED"} {
		t.Run(outcome, func(t *testing.T) {
			r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 0, 100)
			c.AllowNormalRefundCreation(true)
			key := "corrected-refund-" + uuid.NewString()
			refund, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, key, "controlled ordinary refund")
			if err != nil || !created || refund.BaseMicrocredits != 100 || refund.BonusMicrocredits != 0 {
				t.Fatalf("fully reclassified source reservation=%+v err=%v", refund, err)
			}
			var count, zero int
			if err = r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE available_microcredits+active_grant_microcredits+suspended_grant_microcredits=0) FROM refund_component_reservations WHERE refund_id=$1`, refund.ID).Scan(&count, &zero); err != nil || count != 2 || zero != 1 {
				t.Fatalf("zero historical source reservation not retained: %d/%d %v", count, zero, err)
			}
			if _, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: 100, Status: "PENDING"}, false); !errors.Is(err, ErrPaymentProvider) {
				t.Fatalf("unknown provider outcome=%v", err)
			}
			before := entitlementFinancialFactsSnapshot(t, ctx, r)
			if replay, again, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, key, "controlled ordinary refund"); err != nil || again || replay.ID != refund.ID {
				t.Fatal("unknown original key recovery", err)
			}
			correctedRefundUnchanged(t, ctx, r, before)
			result, _, err := c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "corrected-provider-refund", AmountFen: 100, Status: outcome}, false)
			expected := "SUCCEEDED"
			if outcome == "FAILED" {
				expected = "FAILED"
			}
			if err != nil || result.Status != expected {
				t.Fatalf("known result=%+v err=%v", result, err)
			}
			before = entitlementFinancialFactsSnapshot(t, ctx, r)
			if _, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "corrected-provider-refund", AmountFen: 100, Status: outcome}, false); err != nil {
				t.Fatal(err)
			}
			correctedRefundUnchanged(t, ctx, r, before)
			if outcome == "SUCCESS" {
				correctedRefundTerminal(t, ctx, r, order, pack)
				correctedRefundAudit(t, ctx, r, c, order.ID, "REFUND")
			} else {
				var valid bool
				if err = r.pool.QueryRow(ctx, `SELECT (SELECT posted_microcredits=100 FROM wallets WHERE id=$1) AND (SELECT status='ACTIVE' FROM entitlement_packages WHERE id=$2) AND NOT EXISTS(SELECT 1 FROM entitlement_components WHERE package_id=$2 AND status<>'ACTIVE') AND NOT EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='REFUND')`, order.WalletID, pack).Scan(&valid); err != nil || !valid {
					t.Fatalf("failed known outcome did not restore original entitlements: %v", err)
				}
				correctedRefundAudit(t, ctx, r, c, order.ID, "PAYMENT")
			}
		})
	}
}

func TestF09CorrectedOrdinaryRefundKeepsExistingEligibility(t *testing.T) {
	for _, guard := range []string{"zero-effective-base", "active-hold", "consumed", "expired"} {
		t.Run(guard, func(t *testing.T) {
			actual, expected := int64(60), int64(80)
			if guard == "zero-effective-base" {
				actual, expected = 100, 0
			}
			r, ctx, c, order, transaction, pack := correctedRefundFixture(t, actual, expected)
			c.AllowNormalRefundCreation(true)
			if guard == "active-hold" || guard == "consumed" {
				usage := uuid.New()
				hold, err := r.Reserve(ctx, usage, order.WalletID, 1, nil, r.now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if guard == "consumed" {
					if _, err = r.Commit(ctx, usage, hold.ID, 1, map[string]any{"controlled": true}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if guard == "expired" {
				r.now = func() time.Time { return time.Now().UTC().Add(31 * 24 * time.Hour) }
				if count, err := r.ExpirePackages(ctx, 10); err != nil || count != 1 {
					t.Fatalf("actual package expiry %d %v", count, err)
				}
			}
			before := entitlementFinancialFactsSnapshot(t, ctx, r)
			if _, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, "guard-refund-"+uuid.NewString(), "controlled denied refund"); !errors.Is(err, ErrConflict) {
				t.Fatalf("original %s guard relaxed: %v", guard, err)
			}
			correctedRefundUnchanged(t, ctx, r, before)
		})
	}
}
