package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PaymentTransaction struct {
	ID                    uuid.UUID  `json:"payment_transaction_id"`
	PaymentOrderID        uuid.UUID  `json:"payment_order_id"`
	ProviderID            string     `json:"provider_id"`
	ProviderVersion       string     `json:"provider_version"`
	ProviderTransactionID string     `json:"provider_transaction_id"`
	Channel               string     `json:"channel"`
	Status                string     `json:"status"`
	FulfillmentStatus     string     `json:"fulfillment_status"`
	AmountFen             int64      `json:"amount_fen"`
	Currency              string     `json:"currency"`
	ProviderStatus        string     `json:"provider_status"`
	PaidAt                *time.Time `json:"paid_at,omitempty"`
	VerifiedAt            time.Time  `json:"verified_at"`
	FulfilledAt           *time.Time `json:"fulfilled_at,omitempty"`
}

type PaymentEvidence struct {
	ID                    uuid.UUID       `json:"payment_evidence_id"`
	PaymentOrderID        uuid.UUID       `json:"payment_order_id"`
	PaymentTransactionID  *uuid.UUID      `json:"payment_transaction_id,omitempty"`
	EvidenceType          string          `json:"evidence_type"`
	ProviderID            string          `json:"provider_id"`
	ProviderVersion       string          `json:"provider_version"`
	EvidenceKey           string          `json:"evidence_key"`
	ProviderTransactionID *string         `json:"provider_transaction_id,omitempty"`
	AmountFen             *int64          `json:"amount_fen,omitempty"`
	Currency              string          `json:"currency,omitempty"`
	ProviderStatus        string          `json:"provider_status,omitempty"`
	ProviderEventAt       *time.Time      `json:"provider_event_at,omitempty"`
	ReceivedAt            time.Time       `json:"received_at"`
	PayloadDigest         string          `json:"payload_digest"`
	PayloadSnapshot       json.RawMessage `json:"payload_snapshot"`
	MergeResult           string          `json:"merge_result"`
}

func providerEvidencePayload(provider ProviderOrder, payload []byte) ([]byte, string, error) {
	if len(payload) == 0 {
		var err error
		payload, err = json.Marshal(provider)
		if err != nil {
			return nil, "", err
		}
	}
	digest := sha256.Sum256(payload)
	return payload, hex.EncodeToString(digest[:]), nil
}

func providerEvidenceKey(evidenceType string, provider ProviderOrder) string {
	transactionID := "none"
	if provider.ChannelTransactionID != nil {
		transactionID = *provider.ChannelTransactionID
	}
	eventAt := "none"
	if provider.PaidAt != nil {
		eventAt = provider.PaidAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s", evidenceType, provider.MerchantOrderNo, transactionID, provider.ProviderStatus, eventAt)
}

func (p *PaymentCoordinator) insertPaymentEvidence(ctx context.Context, tx pgx.Tx, order PaymentOrder, transactionID *uuid.UUID, evidenceType, evidenceKey, mergeResult string, provider ProviderOrder, payload []byte, receivedAt time.Time) (uuid.UUID, error) {
	payload, digest, err := providerEvidencePayload(provider, payload)
	if err != nil {
		return uuid.Nil, err
	}
	if evidenceKey == "" {
		evidenceKey = providerEvidenceKey(evidenceType, provider)
	}
	evidenceID := uuid.Must(uuid.NewV7())
	command, err := tx.Exec(ctx, `INSERT INTO payment_evidence(
id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,
received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$15,$16,$17)
ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING`,
		evidenceID, order.ID, transactionID, evidenceType, order.ProviderID, order.ProviderVersion, evidenceKey,
		provider.MerchantOrderNo, provider.ChannelTransactionID, provider.AmountFen, provider.Currency,
		provider.ProviderStatus, provider.PaidAt, receivedAt, digest, json.RawMessage(payload), mergeResult)
	if err != nil {
		return uuid.Nil, err
	}
	if command.RowsAffected() == 0 {
		err = tx.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id=$1 AND provider_version=$2 AND evidence_type=$3 AND evidence_key=$4 AND payload_digest=$5`, order.ProviderID, order.ProviderVersion, evidenceType, evidenceKey, digest).Scan(&evidenceID)
	}
	return evidenceID, err
}

func (p *PaymentCoordinator) recordProviderObservation(ctx context.Context, orderID uuid.UUID, evidenceType string, provider ProviderOrder) error {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if err != nil {
		return err
	}
	valid := provider.MerchantOrderNo == order.MerchantOrderNo && provider.Channel == order.Channel && provider.AmountFen == order.AmountFen && provider.Currency == order.Currency
	mergeResult := "RECORDED"
	if !valid {
		mergeResult = "REVIEW_REQUIRED"
	}
	now := p.now()
	evidenceID, err := p.insertPaymentEvidence(ctx, tx, order, nil, evidenceType, providerEvidenceKey(evidenceType, provider), mergeResult, provider, nil, now)
	if err != nil {
		return err
	}
	if !valid {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary='REVIEW_REQUIRED',status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW',summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return err
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "merchant_order_no": provider.MerchantOrderNo, "amount_fen": provider.AmountFen, "currency": provider.Currency}, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}

// mergeSuccessfulPaymentWithinTransaction is the single convergence path used
// by callbacks, queries and audited repairs. Every distinct provider money
// movement owns its transaction and, when fulfilled, its own entitlement package.
func (p *PaymentCoordinator) mergeSuccessfulPaymentWithinTransaction(ctx context.Context, tx pgx.Tx, order PaymentOrder, evidenceType, evidenceKey string, payload []byte, provider ProviderOrder, repair bool, now time.Time) (PaymentOrder, uuid.UUID, error) {
	if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil {
		return order, uuid.Nil, ErrConflict
	}
	valid := provider.MerchantOrderNo == order.MerchantOrderNo && provider.Channel == order.Channel && provider.AmountFen == order.AmountFen && provider.Currency == order.Currency
	if !valid {
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, nil, evidenceType, evidenceKey, "REVIEW_REQUIRED", provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary='REVIEW_REQUIRED',status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW',summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return order, uuid.Nil, err
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "order channel amount or currency mismatch"}, now); err != nil {
			return order, uuid.Nil, err
		}
		return order, evidenceID, ErrConflict
	}

	var transaction PaymentTransaction
	err := tx.QueryRow(ctx, `SELECT id,payment_order_id,provider_id,provider_version,provider_transaction_id,channel,status,fulfillment_status,amount_fen,currency,COALESCE(provider_status,''),provider_paid_at,verified_at,fulfilled_at
FROM payment_transactions WHERE provider_transaction_id=$1 FOR UPDATE`, *provider.ChannelTransactionID).Scan(
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
		order, err = projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now)
		return order, evidenceID, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
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
	order, err = projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now)
	return order, evidenceID, err
}

func reopenArchivedLatePaymentCase(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) error {
	var caseID uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE late_payment_review_cases SET review_status='REVIEW_REQUIRED_LATE_PAYMENT',blind_reason='stronger payment evidence arrived after unpaid archive',next_review_at=$2,updated_at=$2,resolved_at=NULL WHERE payment_order_id=$1 AND review_status='ARCHIVED_CONFIRMED_UNPAID' RETURNING id`, orderID, now).Scan(&caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET archive_status='ACTIVE',archived_at=NULL,attention_summary='REVIEW_REQUIRED',summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, orderID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE manual_review_cases SET status='OPEN',resolved_at=NULL,updated_at=$2 WHERE id=$1`, caseID, now); err != nil {
		return err
	}
	return appendManualReviewEvent(ctx, tx, caseID, "ARCHIVE_REOPENED_BY_PAYMENT_EVIDENCE", nil, map[string]any{"payment_order_id": orderID}, now)
}

func classifyPaymentTransaction(expiresAt, paidAt time.Time, policy PaymentPolicy, priorTransactions int, repair bool) (string, string, bool) {
	return classifyPaymentTransactionAfterClose(expiresAt, paidAt, "OPEN", policy, priorTransactions, repair)
}

func classifyPaymentTransactionAfterClose(expiresAt, paidAt time.Time, closeStatus string, policy PaymentPolicy, priorTransactions int, repair bool) (string, string, bool) {
	status := "PAID"
	if !expiresAt.After(paidAt) || (closeStatus != "" && closeStatus != "OPEN") {
		status = "LATE_PAID"
	}
	requiresReview := (policy.RequiresReview(expiresAt, paidAt) || priorTransactions > 0) && !repair
	if requiresReview {
		return "REVIEW_REQUIRED", "REVIEW_REQUIRED", true
	}
	return status, "PENDING", false
}

func projectPaymentOrderWithinTransaction(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) (PaymentOrder, error) {
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
SELECT count(*) FILTER (WHERE status='REVIEW_REQUIRED') review_count,
       count(*) FILTER (WHERE status='CONFIRMED') open_count,
       COALESCE(sum(transaction.amount_fen) FILTER (WHERE chargeback.status='CONFIRMED'),0) lost_amount_fen
FROM chargeback_cases chargeback
JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
WHERE chargeback.payment_order_id=$1), receivables AS (
SELECT count(*) FILTER (WHERE outstanding_microcredits>0) open_count
FROM receivables WHERE payment_order_id=$1), anomalies AS (
SELECT count(*) FILTER (WHERE status='OPEN') open_count
FROM payment_anomaly_cases WHERE payment_order_id=$1)
UPDATE payment_orders payment SET
 payment_summary=jsonb_build_object('paid_count',facts.paid_count,'paid_amount_fen',facts.paid_amount_fen,'late_paid_count',facts.late_paid_count,'review_count',facts.review_count),
 refund_summary=jsonb_build_object('succeeded_amount_fen',refunds.succeeded_amount_fen,'pending_count',refunds.pending_count,'review_count',refunds.review_count,'forced_review_count',forced_refunds.review_count),
 chargeback_summary=jsonb_build_object('review_count',chargebacks.review_count,'open_count',chargebacks.open_count,'lost_amount_fen',chargebacks.lost_amount_fen),
 attention_summary=CASE
   WHEN receivables.open_count>0 THEN 'RECEIVABLE_OPEN'
   WHEN facts.review_count>0 OR refunds.review_count>0 OR forced_refunds.review_count>0 OR chargebacks.review_count>0 OR anomalies.open_count>0 THEN 'REVIEW_REQUIRED'
   WHEN refunds.pending_count>0 OR payment.close_status IN ('CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING') THEN 'PROCESSING'
   ELSE 'NORMAL' END,
 status=CASE WHEN facts.review_count>0 THEN 'MANUAL_REVIEW' WHEN facts.late_paid_count>0 THEN 'PAID_LATE' WHEN facts.paid_count>0 THEN 'PAID' ELSE payment.status END,
 fulfillment_status=CASE WHEN facts.review_count>0 THEN 'MANUAL_REVIEW' WHEN facts.paid_count>0 AND facts.credited_count=facts.paid_count THEN 'CREDITED' ELSE payment.fulfillment_status END,
 primary_provider_transaction_id=COALESCE(payment.primary_provider_transaction_id,facts.primary_transaction),
 paid_at=COALESCE(payment.paid_at,facts.first_paid_at),summary_version=summary_version+1,updated_at=$2
FROM facts,refunds,forced_refunds,chargebacks,receivables,anomalies WHERE payment.id=$1`, orderID, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	return getPaymentOrderByID(ctx, tx, orderID, false)
}
