package billing

// Historical paid-data producer pinned to main21624c43f4e69b356604f22d27305bf4ca3aa445.
// The two original payment methods below preserve the pre-principal projection;
// only their names/call target and explicit fixture eligibility guards differ.
// This test-only writer models actual038 history before the index upgrade. It
// cannot run on a current schema or a wallet with any receivable rows: the
// current receivable recovery path also requires principal schema44. Current
// production payment paths and all fresh-schema tests retain the PR writer.
// Source function SHA256 (before name/guard adaptations):
// mergeSuccessfulPaymentWithinTransaction: 2ebe0ba992ec7599a4afd56c9642822d9cbd312b671a71d7a7917afd0852cb1d
// projectPaymentOrderWithinTransaction: 7f197efb6dd99242082886a2cee3730b4ec90ca36990cb6a1d237b8718408104
// retainedFinanceOrder: 0e918b71e1f8fcf9fc0f610f55140ca1717faa1e7462fcad513dfd30d96fc3c2

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

var errProviderHealth038Fixture = errors.New("exact pre-principal038 debt-free historical fixture required")

type providerHealth038Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func requireProviderHealth038Schema(ctx context.Context, q providerHealth038Querier) error {
	var legacy bool
	if err := q.QueryRow(ctx, `SELECT (SELECT COALESCE(max(version),0) FROM public.schema_migrations)=900038 AND to_regclass('public.chargeback_cash_principals') IS NULL AND current_user='billing_service'`).Scan(&legacy); err != nil {
		return err
	}
	if !legacy {
		return errProviderHealth038Fixture
	}
	return nil
}

func providerHealth038FixtureOrder(t *testing.T, r *PostgresRepository, ctx context.Context, credit int64, paid bool) (*PaymentCoordinator, PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	if err := requireProviderHealth038Schema(ctx, r.pool); err != nil {
		t.Fatal(err)
	}

	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	now := r.now()
	order := PaymentOrder{ID: uuid.New(), MerchantOrderNo: "fixture-" + uuid.NewString(), RequesterID: owner, WalletID: wallet.ID, ProductID: "CNY_1", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CreditMicrocredits: credit, Status: "PENDING", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(time.Hour), ProviderID: "controlled", ProviderVersion: "v1"}
	snapshot, _ := json.Marshal(Product{ProductID: order.ProductID, PlanName: "controlled legacy credit", AmountFen: 100, CreditMicrocredits: credit, BasePoints: 1, ValidityDays: 30, Currency: "CNY"})
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','PENDING',$11,$11,$11::timestamptz+interval '72 hours',$11::timestamptz+interval '8 days',$12,$12,$13,$14)`, order.ID, order.MerchantOrderNo, owner, wallet.ID, order.ProductID, snapshot, order.Channel, order.AmountFen, order.Currency, credit, order.ExpiresAt, now, order.ProviderID, order.ProviderVersion)
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
	order, _, err = c.mergeProviderHealth038FixturePayment(ctx, tx, order, "QUERY", "fixture-paid-"+id, []byte(`{"controlled":true}`), provider.result, false, now)
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

func (p *PaymentCoordinator) mergeProviderHealth038FixturePayment(ctx context.Context, tx pgx.Tx, order PaymentOrder, evidenceType, evidenceKey string, payload []byte, provider ProviderOrder, repair bool, now time.Time) (PaymentOrder, uuid.UUID, error) {
	if err := requireProviderHealth038Schema(ctx, tx); err != nil {
		return order, uuid.Nil, err
	}
	var debtFree bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM receivables WHERE wallet_id=$1)`, order.WalletID).Scan(&debtFree); err != nil {
		return order, uuid.Nil, err
	}
	if !debtFree {
		return order, uuid.Nil, errProviderHealth038Fixture
	}

	if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil {
		return order, uuid.Nil, ErrConflict
	}
	valid := providerMatchesOrder(provider, order)
	if !valid {
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, nil, evidenceType, evidenceKey, "REVIEW_REQUIRED", provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary='REVIEW_REQUIRED',status=CASE WHEN status IN ('CREATED','PENDING','EXPIRED') THEN 'MANUAL_REVIEW' ELSE status END,fulfillment_status=CASE WHEN fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE fulfillment_status END,payment_url=NULL,qr_data=NULL,summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return order, uuid.Nil, err
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "order channel amount or currency mismatch"}, now); err != nil {
			return order, uuid.Nil, err
		}
		return order, evidenceID, ErrConflict
	}

	var transaction PaymentTransaction
	err := tx.QueryRow(ctx, `SELECT id,payment_order_id,provider_id,provider_version,provider_transaction_id,channel,status,fulfillment_status,amount_fen,currency,COALESCE(provider_status,''),provider_paid_at,verified_at,fulfilled_at
FROM payment_transactions WHERE provider_transaction_id=$1 FOR NO KEY UPDATE`, *provider.ChannelTransactionID).Scan(
		&transaction.ID, &transaction.PaymentOrderID, &transaction.ProviderID, &transaction.ProviderVersion,
		&transaction.ProviderTransactionID, &transaction.Channel, &transaction.Status, &transaction.FulfillmentStatus,
		&transaction.AmountFen, &transaction.Currency, &transaction.ProviderStatus, &transaction.PaidAt,
		&transaction.VerifiedAt, &transaction.FulfilledAt)
	if err == nil {
		mergeResult := "DUPLICATE"
		if transaction.PaymentOrderID != order.ID || transaction.ProviderID != order.ProviderID || transaction.ProviderVersion != order.ProviderVersion || transaction.AmountFen != order.AmountFen || transaction.Currency != order.Currency {
			mergeResult = "REVIEW_REQUIRED"
		}
		var evidenceTransactionID *uuid.UUID
		if transaction.PaymentOrderID == order.ID {
			evidenceTransactionID = &transaction.ID
		}
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, evidenceTransactionID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if mergeResult == "REVIEW_REQUIRED" {
			if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P0", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "provider transaction conflicts with an existing owner or immutable snapshot"}, now); err != nil {
				return order, uuid.Nil, err
			}
			return order, evidenceID, ErrConflict
		}
		if repair && transaction.FulfillmentStatus == "REVIEW_REQUIRED" {
			if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
				return order, evidenceID, err
			}
			packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, true)
			if err != nil {
				return order, evidenceID, err
			}
			resolvedStatus := "PAID"
			if transaction.PaidAt != nil && (!order.ExpiresAt.After(*transaction.PaidAt) || (order.CloseStatus != "" && order.CloseStatus != "OPEN")) {
				resolvedStatus = "LATE_PAID"
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET status=$2,fulfillment_status='CREDITED',fulfilled_at=$3,updated_at=$3,evidence_version=evidence_version+1 WHERE id=$1 AND fulfillment_status='REVIEW_REQUIRED'`, transaction.ID, resolvedStatus, now); err != nil {
				return order, evidenceID, err
			}
			if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
				"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
				"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
				"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
				"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
			}); err != nil {
				return order, evidenceID, err
			}
		}
		order, err = projectProviderHealth038FixtureOrder(ctx, tx, order.ID, now)
		return order, evidenceID, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return order, uuid.Nil, err
	}

	if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
		return order, uuid.Nil, err
	}
	var priorTransactions int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&priorTransactions); err != nil {
		return order, uuid.Nil, err
	}
	transaction.ID = uuid.Must(uuid.NewV7())
	transactionStatus, fulfillmentStatus, requiresReview := classifyPaymentTransactionAfterClose(order.ExpiresAt, *provider.PaidAt, order.CloseStatus, p.Policy(), priorTransactions, repair)
	_, err = tx.Exec(ctx, `INSERT INTO payment_transactions(
id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,
amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$14,$14)`,
		transaction.ID, order.ID, order.ProviderID, order.ProviderVersion, order.Channel, *provider.ChannelTransactionID,
		transactionStatus, fulfillmentStatus, order.AmountFen, order.Currency, evidenceType, provider.ProviderStatus,
		provider.PaidAt, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	mergeResult := "MERGED"
	if requiresReview {
		mergeResult = "REVIEW_REQUIRED"
	}
	evidenceID, err := p.insertPaymentEvidence(ctx, tx, order, &transaction.ID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	if requiresReview {
		anomalyType := "LATE_PAYMENT_CONFLICT"
		if priorTransactions > 0 {
			anomalyType = "DUPLICATE_PAYMENT"
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, anomalyType, "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentTransactionID: &transaction.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "provider_paid_at": provider.PaidAt, "prior_transaction_count": priorTransactions}, now); err != nil {
			return order, uuid.Nil, err
		}
	}
	if !requiresReview {
		packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, false)
		if err != nil {
			return order, evidenceID, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET fulfillment_status='CREDITED',fulfilled_at=$2,updated_at=$2 WHERE id=$1`, transaction.ID, now); err != nil {
			return order, evidenceID, err
		}
		if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
			"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
			"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
			"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
			"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
		}); err != nil {
			return order, evidenceID, err
		}
	}
	if err := reopenArchivedLatePaymentCase(ctx, tx, order.ID, now); err != nil {
		return order, evidenceID, err
	}
	order, err = projectProviderHealth038FixtureOrder(ctx, tx, order.ID, now)
	return order, evidenceID, err
}

func projectProviderHealth038FixtureOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) (PaymentOrder, error) {
	if err := requireProviderHealth038Schema(ctx, tx); err != nil {
		return PaymentOrder{}, err
	}
	return projectPrePrincipalFixtureOrder(ctx, tx, orderID, now)
}

// Frozen projection from before 900044. Only callers that have first checked
// their exact historical schema may use it.
func projectPrePrincipalFixtureOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) (PaymentOrder, error) {
	_, err := tx.Exec(ctx, `WITH facts AS (
SELECT count(*) FILTER (WHERE status IN ('PAID','LATE_PAID')) paid_count,
       COALESCE(sum(amount_fen) FILTER (WHERE status IN ('PAID','LATE_PAID')),0) paid_amount_fen,
       count(*) FILTER (WHERE status='LATE_PAID') late_paid_count,
       count(*) FILTER (WHERE status='REVIEW_REQUIRED' OR fulfillment_status='REVIEW_REQUIRED') review_count,
       count(*) FILTER (WHERE fulfillment_status='CREDITED') credited_count,
       min(provider_transaction_id) FILTER (WHERE status IN ('PAID','LATE_PAID')) primary_transaction,
       min(provider_paid_at) FILTER (WHERE status IN ('PAID','LATE_PAID')) first_paid_at
FROM payment_transactions WHERE payment_order_id=$1), refunds AS (
SELECT COALESCE(sum(amount_fen) FILTER (WHERE status='SUCCEEDED'),0) succeeded_amount_fen,
       count(*) FILTER (WHERE status='REQUESTED') pending_count,
       count(*) FILTER (WHERE status='MANUAL_REVIEW') review_count
FROM refunds WHERE payment_order_id=$1), forced_refunds AS (
SELECT count(*) FILTER (WHERE status='REVIEW_REQUIRED') review_count
FROM forced_refund_cases WHERE payment_order_id=$1), chargebacks AS (
SELECT count(*) FILTER (WHERE chargeback.status='REVIEW_REQUIRED') review_count,
       count(*) FILTER (WHERE chargeback.status='CONFIRMED') open_count,
       COALESCE(sum(transaction.amount_fen) FILTER (WHERE chargeback.status='CONFIRMED'),0) lost_amount_fen
FROM chargeback_cases chargeback
JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
WHERE chargeback.payment_order_id=$1), receivables AS (
SELECT count(*) FILTER (WHERE outstanding_microcredits>0) open_count
FROM receivables WHERE payment_order_id=$1), anomalies AS (
SELECT count(*) FILTER (WHERE status='OPEN') open_count
FROM payment_anomaly_cases WHERE payment_order_id=$1)
UPDATE payment_orders payment SET
 payment_summary=jsonb_build_object('paid_count',facts.paid_count,'paid_amount_fen',facts.paid_amount_fen::text,'late_paid_count',facts.late_paid_count,'review_count',facts.review_count),
 refund_summary=jsonb_build_object('succeeded_amount_fen',refunds.succeeded_amount_fen::text,'pending_count',refunds.pending_count,'review_count',refunds.review_count,'forced_review_count',forced_refunds.review_count),
 chargeback_summary=jsonb_build_object('review_count',chargebacks.review_count,'open_count',chargebacks.open_count,'lost_amount_fen',chargebacks.lost_amount_fen::text),
 attention_summary=CASE
   WHEN receivables.open_count>0 THEN 'RECEIVABLE_OPEN'
   WHEN facts.review_count>0 OR refunds.review_count>0 OR forced_refunds.review_count>0 OR chargebacks.review_count>0 OR anomalies.open_count>0 THEN 'REVIEW_REQUIRED'
   WHEN refunds.pending_count>0 OR (payment.status IN ('CREATED','PENDING') AND payment.close_status IN ('CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING')) THEN 'PROCESSING'
   ELSE 'NORMAL' END,
 status=CASE WHEN facts.review_count>0 THEN 'MANUAL_REVIEW' WHEN facts.late_paid_count>0 THEN 'PAID_LATE' WHEN facts.paid_count>0 THEN 'PAID' ELSE payment.status END,
 fulfillment_status=CASE WHEN facts.review_count>0 THEN 'MANUAL_REVIEW' WHEN facts.paid_count>0 AND facts.credited_count=facts.paid_count THEN 'CREDITED' ELSE payment.fulfillment_status END,
 payment_url=CASE WHEN facts.paid_count>0 OR facts.review_count>0 THEN NULL ELSE payment.payment_url END,
 qr_data=CASE WHEN facts.paid_count>0 OR facts.review_count>0 THEN NULL ELSE payment.qr_data END,
 primary_provider_transaction_id=COALESCE(payment.primary_provider_transaction_id,facts.primary_transaction),
 paid_at=COALESCE(payment.paid_at,facts.first_paid_at),summary_version=summary_version+1,updated_at=$2
FROM facts,refunds,forced_refunds,chargebacks,receivables,anomalies WHERE payment.id=$1`, orderID, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	return getPaymentOrderByID(ctx, tx, orderID, false)
}
