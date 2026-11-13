package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChargebackPrincipalInput is an assertion from the authorized supplier bridge,
// not a value inferred from a payment's gross amount or a points journal.
type ChargebackPrincipalInput struct {
	AmountFen                 string `json:"amount_fen"`
	Currency                  string `json:"currency"`
	ProviderTransactionID     string `json:"provider_transaction_id"`
	SupplierEvidenceReference string `json:"supplier_evidence_reference"`
}

type ChargebackCashPrincipal struct {
	Status               string  `json:"status"`
	AmountFen            *string `json:"amount_fen"`
	Currency             string  `json:"currency"`
	ObservationCount     int64   `json:"observation_count"`
	EvidenceDigest       string  `json:"evidence_digest"`
	ManualReviewRequired bool    `json:"manual_review_required"`
}

func validateChargebackPrincipal(ctx context.Context, input *ChargebackPrincipalInput, payload json.RawMessage) error {
	if input == nil {
		return nil
	}
	identity, ok := serviceauth.FromContext(ctx)
	if !ok || identity.Permission != "internal.billing.chargeback.record" || identity.Service == "" || identity.Subject == "" {
		return ErrConflict
	}
	amount, err := strconv.ParseInt(input.AmountFen, 10, 64)
	var object map[string]json.RawMessage
	if err != nil || amount <= 0 || strconv.FormatInt(amount, 10) != input.AmountFen ||
		strings.TrimSpace(input.Currency) != input.Currency || len(input.Currency) == 0 || len(input.Currency) > 16 ||
		strings.TrimSpace(input.ProviderTransactionID) != input.ProviderTransactionID || len(input.ProviderTransactionID) == 0 || len(input.ProviderTransactionID) > 256 ||
		strings.TrimSpace(input.SupplierEvidenceReference) != input.SupplierEvidenceReference || len(input.SupplierEvidenceReference) == 0 || len(input.SupplierEvidenceReference) > 256 ||
		json.Unmarshal(payload, &object) != nil || object == nil {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) RecordChargebackWithPrincipal(ctx context.Context, transactionID uuid.UUID, providerChargebackID string, payload json.RawMessage, principal *ChargebackPrincipalInput) (ChargebackCase, error) {
	return r.recordConfirmedChargebackForReview(ctx, transactionID, providerChargebackID, payload, principal)
}

func readChargebackCashPrincipal(ctx context.Context, source interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, caseID uuid.UUID) (ChargebackCashPrincipal, error) {
	var result ChargebackCashPrincipal
	err := source.QueryRow(ctx, `SELECT fact FROM chargeback_cash_principals WHERE chargeback_case_id=$1`, caseID).Scan(&result)
	return result, err
}

func (r *PostgresRepository) recordChargebackPrincipal(ctx context.Context, tx pgx.Tx, item *ChargebackCase, input *ChargebackPrincipalInput, payload json.RawMessage) error {
	changed := false
	if input != nil {
		var provider, version, reference, currency string
		if err := tx.QueryRow(ctx, `SELECT provider_id,provider_version,provider_transaction_id,currency FROM payment_transactions WHERE id=$1`, item.PaymentTransactionID).Scan(&provider, &version, &reference, &currency); err != nil {
			return err
		}
		amount, _ := strconv.ParseInt(input.AmountFen, 10, 64) // validated before the transaction
		if input.ProviderTransactionID != reference || input.Currency != currency {
			return ErrConflict
		}
		identity, _ := serviceauth.FromContext(ctx)
		rawDigest := sha256.Sum256(payload)
		payloadDigest := hex.EncodeToString(rawDigest[:])
		claim, _ := json.Marshal(struct {
			CaseID                                                                                         uuid.UUID
			Provider, Version, ChargebackID, Reference, Currency, EvidenceReference, Amount, PayloadDigest string
		}{
			item.ID, provider, version, item.ProviderChargebackID, reference, currency, input.SupplierEvidenceReference, input.AmountFen, payloadDigest,
		})
		digest := sha256.Sum256(claim)
		tag, err := tx.Exec(ctx, `INSERT INTO chargeback_principal_evidence(id,chargeback_case_id,payment_transaction_id,provider_id,provider_version,provider_chargeback_id,provider_transaction_id,amount_fen,currency,supplier_evidence_reference,payload_digest,payload_snapshot,observation_digest,recorded_by_service,recorded_by_subject,received_at)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT(chargeback_case_id,observation_digest) DO NOTHING`,
			uuid.Must(uuid.NewV7()), item.ID, item.PaymentTransactionID, provider, version, item.ProviderChargebackID, reference, amount, currency, input.SupplierEvidenceReference, payloadDigest, payload, hex.EncodeToString(digest[:]), identity.Service, identity.Subject, r.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			changed = true
			if err := tx.QueryRow(ctx, `UPDATE chargeback_cases SET version=version+1 WHERE id=$1 RETURNING version`, item.ID).Scan(&item.Version); err != nil {
				return err
			}
			if err := refreshChargebackCashSummary(ctx, tx, item.PaymentOrderID, r.now()); err != nil {
				return err
			}
		}
	}
	fact, err := readChargebackCashPrincipal(ctx, tx, item.ID)
	if err == nil {
		item.CashPrincipal = &fact
	}
	if err == nil && changed && item.Status != "REVIEW_REQUIRED" && fact.ManualReviewRequired {
		// A late observation must reach an active human queue even when the old
		// chargeback's disposition and its original review receipt are complete.
		_, err = createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "chargeback-principal:"+item.ID.String()+":"+fact.EvidenceDigest,
			paymentAnomalyReference{PaymentOrderID: &item.PaymentOrderID, PaymentTransactionID: &item.PaymentTransactionID, PaymentEvidenceID: &item.PaymentEvidenceID},
			map[string]any{"chargeback_case_id": item.ID, "cash_principal": fact, "reason": "INDEPENDENT_CASH_REQUIRES_REVIEW"}, r.now())
		if err == nil {
			// Include the newly created or deduplicated review in the same commit.
			// A dismissed case no longer contributes a derived principal marker.
			err = refreshChargebackCashSummary(ctx, tx, item.PaymentOrderID, r.now())
		}
	}
	return err
}

// Existing manually authorized whole-payment paths retain legacy MISSING
// handling. New partial/conflicting cash facts cannot authorize their full BASE
// effects. Completed immutable receipts are checked before this fresh-write gate.
func requireWholeChargebackPrincipal(ctx context.Context, source interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, caseID uuid.UUID) error {
	var allowed bool
	err := source.QueryRow(ctx, `SELECT (p.fact->>'status'='MISSING' AND NOT EXISTS(SELECT 1 FROM chargeback_principal_evidence known WHERE known.payment_transaction_id=t.id)) OR (p.fact->>'status'='CONFIRMED' AND (p.fact->>'amount_fen')::bigint=t.amount_fen AND NOT EXISTS(SELECT 1 FROM chargeback_cases peer WHERE peer.payment_transaction_id=t.id AND peer.id<>c.id AND peer.status IN ('REVIEW_REQUIRED','CONFIRMED')))
  FROM chargeback_cases c JOIN payment_transactions t ON t.id=c.payment_transaction_id JOIN chargeback_cash_principals p ON p.chargeback_case_id=c.id WHERE c.id=$1`, caseID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrConflict
	}
	return nil
}

// Unknown cash stays NULL. A separate known subtotal is useful without claiming
// that an incomplete or contradictory cash loss total has been established.
const chargebackCashSummarySelect = `SELECT
 count(*) FILTER(WHERE c.status='REVIEW_REQUIRED') review_count,
 count(*) FILTER(WHERE c.status='CONFIRMED') open_count,
 count(*) FILTER(WHERE c.status IN ('CONFIRMED','REVIEW_REQUIRED') AND p.fact->>'status'<>'CONFIRMED') unverified_count,
	count(*) FILTER(WHERE c.status='CONFIRMED' AND (p.fact->>'manual_review_required')::boolean) principal_review_count,
 COALESCE(sum((p.fact->>'amount_fen')::numeric) FILTER(WHERE c.status IN ('CONFIRMED','REVIEW_REQUIRED') AND p.fact->>'status'='CONFIRMED'),0) verified_lost_amount_fen
 FROM chargeback_cases c JOIN chargeback_cash_principals p ON p.chargeback_case_id=c.id WHERE c.payment_order_id=$1 AND c.status IN ('REVIEW_REQUIRED','CONFIRMED')`

const chargebackCashSummaryJSON = `jsonb_build_object('review_count',chargebacks.review_count,'open_count',chargebacks.open_count,
 'lost_amount_fen',CASE WHEN chargebacks.unverified_count=0 THEN chargebacks.verified_lost_amount_fen::text ELSE NULL END,
 'verified_lost_amount_fen',chargebacks.verified_lost_amount_fen::text,'unverified_count',chargebacks.unverified_count,'principal_review_count',chargebacks.principal_review_count,
 'cash_principal_status',CASE WHEN chargebacks.unverified_count=0 THEN 'CONFIRMED' ELSE 'UNVERIFIED' END)`

const chargebackCashSummaryUpdateSQL = `UPDATE payment_orders payment SET chargeback_summary=` + chargebackCashSummaryJSON + `,
 attention_summary=` + paymentOrderAttentionSQL + `,
 summary_version=summary_version+1,updated_at=$2 FROM (` + chargebackCashSummarySelect + `) chargebacks WHERE payment.id=$1`

func refreshChargebackCashSummary(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) error {
	_, err := tx.Exec(ctx, chargebackCashSummaryUpdateSQL, orderID, now)
	return err
}
