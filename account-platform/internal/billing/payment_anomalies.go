package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PaymentAnomalyCase struct {
	ID                    uuid.UUID       `json:"case_id"`
	AnomalyType           string          `json:"anomaly_type"`
	Severity              string          `json:"severity"`
	Status                string          `json:"status"`
	OwnerID               *uuid.UUID      `json:"owner_id,omitempty"`
	PaymentOrderID        *uuid.UUID      `json:"payment_order_id,omitempty"`
	PaymentTransactionID  *uuid.UUID      `json:"payment_transaction_id,omitempty"`
	PaymentEvidenceID     *uuid.UUID      `json:"payment_evidence_id,omitempty"`
	RefundID              *uuid.UUID      `json:"refund_id,omitempty"`
	ForcedRefundCaseID    *uuid.UUID      `json:"forced_refund_case_id,omitempty"`
	ChargebackCaseID      *uuid.UUID      `json:"chargeback_case_id,omitempty"`
	ResolutionCode        *string         `json:"resolution_code,omitempty"`
	ResolutionReason      *string         `json:"resolution_reason,omitempty"`
	FinancialActionStatus string          `json:"financial_action_status"`
	ResourceSnapshot      json.RawMessage `json:"resource_snapshot"`
	ClaimedAt             *time.Time      `json:"claimed_at,omitempty"`
	ClaimDueAt            time.Time       `json:"claim_due_at"`
	FirstResponseDueAt    time.Time       `json:"first_response_due_at"`
	MitigationDueAt       time.Time       `json:"mitigation_due_at"`
	ResolutionDueAt       time.Time       `json:"resolution_due_at"`
	Version               int64           `json:"version"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	ResolvedAt            *time.Time      `json:"resolved_at,omitempty"`
}

type PaymentAnomalyFilter struct {
	AnomalyType string
	Status      string
	Severity    string
	OwnerID     *uuid.UUID
}

type PaymentAnomalyDetail struct {
	Case   PaymentAnomalyCase      `json:"case"`
	Events []ManualReviewCaseEvent `json:"events"`
}

type paymentAnomalyReference struct {
	PaymentOrderID       *uuid.UUID
	PaymentTransactionID *uuid.UUID
	PaymentEvidenceID    *uuid.UUID
	RefundID             *uuid.UUID
	ForcedRefundCaseID   *uuid.UUID
	ChargebackCaseID     *uuid.UUID
}

func paymentAnomalyDeadlines(now time.Time, severity string) (time.Time, time.Time, time.Time, time.Time) {
	deadlines, _ := manualreview.DeadlinesFor(now, severity)
	return deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution
}

func createPaymentAnomalyWithinTransaction(ctx context.Context, tx pgx.Tx, anomalyType, severity, dedupeKey string, reference paymentAnomalyReference, snapshot any, now time.Time) (PaymentAnomalyCase, error) {
	anomalyType = strings.ToUpper(strings.TrimSpace(anomalyType))
	severity = strings.ToUpper(strings.TrimSpace(severity))
	dedupeKey = strings.TrimSpace(dedupeKey)
	if anomalyType == "" || dedupeKey == "" || (severity != "P0" && severity != "P1" && severity != "P2") {
		return PaymentAnomalyCase{}, ErrConflict
	}
	if existing, found, err := getPaymentAnomalyByDedupeKey(ctx, tx, dedupeKey); err != nil {
		return PaymentAnomalyCase{}, err
	} else if found {
		return existing, nil
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(`{}`)
	}
	item := PaymentAnomalyCase{
		ID: uuid.Must(uuid.NewV7()), AnomalyType: anomalyType, Severity: severity, Status: "OPEN",
		PaymentOrderID: reference.PaymentOrderID, PaymentTransactionID: reference.PaymentTransactionID,
		PaymentEvidenceID: reference.PaymentEvidenceID, RefundID: reference.RefundID,
		ForcedRefundCaseID: reference.ForcedRefundCaseID, ChargebackCaseID: reference.ChargebackCaseID,
		FinancialActionStatus: "NONE", ResourceSnapshot: payload, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	item.ClaimDueAt, item.FirstResponseDueAt, item.MitigationDueAt, item.ResolutionDueAt = paymentAnomalyDeadlines(now, severity)
	_, err = tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','PAYMENT_ANOMALY','PAYMENT_ANOMALY',$1,$2,'OPEN',$3,$4,$5,$6,$7,$3)`, item.ID, severity, now, item.ClaimDueAt, item.FirstResponseDueAt, item.MitigationDueAt, item.ResolutionDueAt)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,refund_id,forced_refund_case_id,chargeback_case_id,status,resource_snapshot,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'OPEN',$10,$11,$11)`, item.ID, anomalyType, dedupeKey, reference.PaymentOrderID, reference.PaymentTransactionID, reference.PaymentEvidenceID, reference.RefundID, reference.ForcedRefundCaseID, reference.ChargebackCaseID, payload, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, item.ID, "CREATED", nil, map[string]any{"anomaly_type": anomalyType, "severity": severity, "dedupe_key": dedupeKey}, now)
	}
	if err == nil {
		err = events.AppendOutbox(ctx, tx, "billing.payment-anomaly.review-required.v1", item.ID, 1, map[string]any{"case_id": item.ID, "anomaly_type": anomalyType, "severity": severity, "payment_order_id": reference.PaymentOrderID})
	}
	return item, err
}

const paymentAnomalySelect = `SELECT anomaly.id,anomaly.anomaly_type,manual.severity,anomaly.status,manual.owner_id,anomaly.payment_order_id,anomaly.payment_transaction_id,anomaly.payment_evidence_id,anomaly.refund_id,anomaly.forced_refund_case_id,anomaly.chargeback_case_id,anomaly.resolution_code,anomaly.resolution_reason,anomaly.financial_action_status,anomaly.resource_snapshot,manual.claimed_at,manual.claim_due_at,manual.first_response_due_at,manual.mitigation_due_at,manual.resolution_due_at,anomaly.version,anomaly.created_at,anomaly.updated_at,anomaly.resolved_at FROM payment_anomaly_cases anomaly JOIN manual_review_cases manual ON manual.id=anomaly.id`

func scanPaymentAnomaly(row billingRowScanner) (PaymentAnomalyCase, bool, error) {
	var item PaymentAnomalyCase
	err := row.Scan(&item.ID, &item.AnomalyType, &item.Severity, &item.Status, &item.OwnerID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.PaymentEvidenceID, &item.RefundID, &item.ForcedRefundCaseID, &item.ChargebackCaseID, &item.ResolutionCode, &item.ResolutionReason, &item.FinancialActionStatus, &item.ResourceSnapshot, &item.ClaimedAt, &item.ClaimDueAt, &item.FirstResponseDueAt, &item.MitigationDueAt, &item.ResolutionDueAt, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

func getPaymentAnomalyByDedupeKey(ctx context.Context, tx pgx.Tx, dedupeKey string) (PaymentAnomalyCase, bool, error) {
	return scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.dedupe_key=$1`, dedupeKey))
}

func (r *PostgresRepository) AdminPaymentAnomalies(ctx context.Context, filter PaymentAnomalyFilter, limit int) ([]PaymentAnomalyCase, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, paymentAnomalySelect+` WHERE ($1='' OR anomaly.anomaly_type=$1) AND ($2='' OR anomaly.status=$2) AND ($3='' OR manual.severity=$3) AND ($4::uuid IS NULL OR manual.owner_id=$4) ORDER BY CASE manual.severity WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 ELSE 2 END,anomaly.created_at,anomaly.id LIMIT $5`, strings.ToUpper(strings.TrimSpace(filter.AnomalyType)), strings.ToUpper(strings.TrimSpace(filter.Status)), strings.ToUpper(strings.TrimSpace(filter.Severity)), filter.OwnerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PaymentAnomalyCase{}
	for rows.Next() {
		item, _, err := scanPaymentAnomaly(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) PaymentAnomalyDetail(ctx context.Context, caseID uuid.UUID) (PaymentAnomalyDetail, error) {
	item, found, err := scanPaymentAnomaly(r.pool.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1`, caseID))
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return PaymentAnomalyDetail{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id,event_type,actor_id,details,created_at FROM manual_review_case_events WHERE case_id=$1 ORDER BY created_at,id`, caseID)
	if err != nil {
		return PaymentAnomalyDetail{}, err
	}
	defer rows.Close()
	eventsList := []ManualReviewCaseEvent{}
	for rows.Next() {
		var event ManualReviewCaseEvent
		if err := rows.Scan(&event.ID, &event.EventType, &event.ActorID, &event.Details, &event.CreatedAt); err != nil {
			return PaymentAnomalyDetail{}, err
		}
		eventsList = append(eventsList, event)
	}
	return PaymentAnomalyDetail{Case: item, Events: eventsList}, rows.Err()
}

func paymentAnomalyCommandHash(action, reason string) string {
	digest := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(action)) + "\n" + strings.TrimSpace(reason)))
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) ClaimPaymentAnomaly(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey string) (PaymentAnomalyCase, error) {
	return r.updatePaymentAnomaly(ctx, caseID, actorID, idempotencyKey, "CLAIM", "claim case", func(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, now time.Time) (PaymentAnomalyCase, error) {
		if item.Status != "OPEN" || (item.OwnerID != nil && *item.OwnerID != actorID) {
			return item, ErrConflict
		}
		_, err := tx.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=COALESCE(claimed_at,$3),first_response_at=COALESCE(first_response_at,$3),updated_at=$3 WHERE id=$1`, caseID, actorID, now)
		if err == nil {
			item.OwnerID, item.ClaimedAt, item.UpdatedAt = &actorID, &now, now
		}
		return item, err
	})
}

func (r *PostgresRepository) ResolvePaymentAnomaly(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey, resolution, reason, financialStatus string) (PaymentAnomalyCase, error) {
	resolution = strings.ToUpper(strings.TrimSpace(resolution))
	financialStatus = strings.ToUpper(strings.TrimSpace(financialStatus))
	if resolution == "" || len([]rune(strings.TrimSpace(reason))) < 10 || (financialStatus != "NONE" && financialStatus != "PENDING" && financialStatus != "APPLIED" && financialStatus != "FAILED") {
		return PaymentAnomalyCase{}, ErrConflict
	}
	if financialStatus != anomalyResolutionFinancialStatus(resolution) {
		return PaymentAnomalyCase{}, ErrConflict
	}
	prepared, completed, err := r.PreparePaymentAnomalyResolution(ctx, caseID, actorID, idempotencyKey, resolution, reason)
	if err != nil || completed {
		return prepared, err
	}
	return r.updatePaymentAnomaly(ctx, caseID, actorID, idempotencyKey, resolution, reason, func(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, now time.Time) (PaymentAnomalyCase, error) {
		if !validPaymentAnomalyResolution(item.AnomalyType, resolution, financialStatus) {
			return item, ErrConflict
		}
		if item.Status == "RESOLVED" {
			if item.ResolutionCode != nil && *item.ResolutionCode == resolution {
				return item, nil
			}
			return item, ErrConflict
		}
		_, err := tx.Exec(ctx, `UPDATE payment_anomaly_cases SET status='RESOLVED',resolution_code=$2,resolution_reason=$3,financial_action_status=$4,version=version+1,updated_at=$5,resolved_at=$5 WHERE id=$1`, caseID, resolution, strings.TrimSpace(reason), financialStatus, now)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status=CASE WHEN $2='PENDING' THEN 'LONG_TERM_PLAN' ELSE 'RESOLVED' END,owner_id=COALESCE(owner_id,$3),claimed_at=COALESCE(claimed_at,$4),first_response_at=COALESCE(first_response_at,$4),mitigation_at=COALESCE(mitigation_at,$4),resolved_at=$4,updated_at=$4 WHERE id=$1`, caseID, financialStatus, actorID, now)
		}
		if err == nil && item.ChargebackCaseID != nil {
			status := "REVIEW_REQUIRED"
			if resolution == "CONFIRM_CHARGEBACK" {
				status = "CONFIRMED"
			} else if resolution == "DISMISS" {
				status = "DISMISSED"
			}
			_, err = tx.Exec(ctx, `UPDATE chargeback_cases SET status=$2,resolution_code=$3,resolution_reason=$4,resolved_at=$5,version=version+1 WHERE id=$1`, *item.ChargebackCaseID, status, resolution, strings.TrimSpace(reason), now)
		}
		if err == nil && item.ForcedRefundCaseID != nil {
			status := "REVIEW_REQUIRED"
			if resolution == "CONFIRM_FORCED_REFUND" || resolution == "DISMISS" {
				status = "RESOLVED"
			}
			_, err = tx.Exec(ctx, `UPDATE forced_refund_cases SET status=$2,resolution_code=$3,resolution_reason=$4,resolved_at=$5,version=version+1 WHERE id=$1`, *item.ForcedRefundCaseID, status, resolution, strings.TrimSpace(reason), now)
		}
		if err == nil && resolution == "DISMISS" {
			err = restoreDismissedPaymentAnomalyEntitlements(ctx, tx, item, now)
		}
		if err == nil && item.PaymentOrderID != nil {
			_, err = tx.Exec(ctx, `UPDATE payment_orders payment SET
chargeback_summary=chargeback_summary||jsonb_build_object('review_count',(SELECT count(*) FROM chargeback_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),
refund_summary=refund_summary||jsonb_build_object('forced_review_count',(SELECT count(*) FROM forced_refund_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),
attention_summary=CASE
 WHEN EXISTS(SELECT 1 FROM receivables WHERE payment_order_id=$1 AND outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN'
 WHEN EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$1 AND status='OPEN')
   OR EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1 AND (status='REVIEW_REQUIRED' OR fulfillment_status='REVIEW_REQUIRED'))
   OR EXISTS(SELECT 1 FROM refunds WHERE payment_order_id=$1 AND status='MANUAL_REVIEW') THEN 'REVIEW_REQUIRED'
 ELSE 'NORMAL' END,
summary_version=summary_version+1,updated_at=$2 WHERE payment.id=$1`, *item.PaymentOrderID, now)
		}
		if err != nil {
			return item, err
		}
		item.Status, item.ResolutionCode, item.ResolutionReason, item.FinancialActionStatus, item.UpdatedAt, item.ResolvedAt = "RESOLVED", &resolution, stringPointer(strings.TrimSpace(reason)), financialStatus, now, &now
		item.Version++
		return item, nil
	})
}

func (r *PostgresRepository) ConfirmProviderFailed(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey, reason string, expectedCaseVersion int64, queriedAt time.Time, provider ProviderOrder) (PaymentAnomalyCase, error) {
	reason = strings.TrimSpace(reason)
	payload, digest, err := providerEvidencePayload(provider, nil)
	if err != nil || expectedCaseVersion < 1 || queriedAt.IsZero() || len([]rune(reason)) < 10 {
		return PaymentAnomalyCase{}, ErrConflict
	}
	return r.updatePaymentAnomaly(ctx, caseID, actorID, idempotencyKey, "CONFIRM_PROVIDER_FAILED", reason, func(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, now time.Time) (PaymentAnomalyCase, error) {
		if item.AnomalyType != "ILLEGAL_TRANSITION" || item.Status != "OPEN" || item.Version != expectedCaseVersion || item.PaymentOrderID == nil || item.PaymentEvidenceID == nil || item.OwnerID == nil || *item.OwnerID != actorID {
			return item, ErrConflict
		}
		order, orderErr := getPaymentOrderByID(ctx, tx, *item.PaymentOrderID, true)
		if orderErr != nil {
			return item, orderErr
		}
		if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" || !providerFailureMatchesOrder(provider, order) || order.ProviderID == "" || order.ProviderVersion == "" {
			return item, ErrConflict
		}
		var evidenceStatus string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(provider_status,'') FROM payment_evidence WHERE id=$1 AND payment_order_id=$2 AND provider_id=$3 AND provider_version=$4`, *item.PaymentEvidenceID, order.ID, order.ProviderID, order.ProviderVersion).Scan(&evidenceStatus); err != nil || !strings.EqualFold(strings.TrimSpace(evidenceStatus), "FAILED") {
			return item, ErrConflict
		}
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND received_at>$2)
			OR EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1)
			OR EXISTS(SELECT 1 FROM entitlement_packages WHERE payment_order_id=$1)
			OR EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$1 AND id<>$3 AND status='OPEN')
			OR EXISTS(SELECT 1 FROM late_payment_review_cases review JOIN late_payment_review_evidence evidence ON evidence.case_id=review.id WHERE review.payment_order_id=$1 AND evidence.evidence_type='USER_PAYMENT_CLAIM')
			OR EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND (payment_transaction_id IS NOT NULL OR provider_transaction_id IS NOT NULL OR UPPER(COALESCE(provider_status,'')) IN ('PAID','PAY_SUCCESS','SUCCESS')))`, order.ID, queriedAt, caseID).Scan(&blocked); err != nil {
			return item, err
		}
		if blocked {
			return item, ErrConflict
		}
		confirmationID := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_failure_confirmations(id,payment_anomaly_case_id,payment_order_id,provider_id,provider_version,provider_status,provider_payload_digest,provider_snapshot,queried_at,actor_id,reason,created_at) VALUES($1,$2,$3,$4,$5,'FAILED',$6,$7,$8,$9,$10,$11)`, confirmationID, caseID, order.ID, order.ProviderID, order.ProviderVersion, digest, json.RawMessage(payload), queriedAt, actorID, reason, now); err != nil {
			return item, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET status='FAILED',fulfillment_status='PENDING',close_status='EXPIRED',payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,provider_status='FAILED',last_successful_query_at=$2,last_authoritative_unpaid_at=$2,updated_at=$3 WHERE id=$1 AND status='MANUAL_REVIEW'`, order.ID, queriedAt, now); err != nil {
			return item, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_anomaly_cases SET status='RESOLVED',resolution_code='CONFIRM_PROVIDER_FAILED',resolution_reason=$2,financial_action_status='NONE',version=version+1,updated_at=$3,resolved_at=$3 WHERE id=$1`, caseID, reason, now); err != nil {
			return item, err
		}
		if _, err := tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',first_response_at=COALESCE(first_response_at,$2),mitigation_at=COALESCE(mitigation_at,$2),resolved_at=$2,updated_at=$2 WHERE id=$1`, caseID, now); err != nil {
			return item, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary=CASE WHEN EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$1 AND status='OPEN') THEN 'REVIEW_REQUIRED' ELSE 'NORMAL' END,summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return item, err
		}
		if err := events.AppendOutbox(ctx, tx, "billing.payment-provider.failure-confirmed.v1", order.ID, item.Version+1, map[string]any{"case_id": caseID, "payment_order_id": order.ID, "confirmation_id": confirmationID, "provider_id": order.ProviderID, "provider_version": order.ProviderVersion}); err != nil {
			return item, err
		}
		resolution := "CONFIRM_PROVIDER_FAILED"
		item.Status, item.ResolutionCode, item.ResolutionReason, item.FinancialActionStatus = "RESOLVED", &resolution, stringPointer(reason), "NONE"
		item.Version++
		item.UpdatedAt, item.ResolvedAt = now, &now
		return item, nil
	})
}

func validPaymentAnomalyResolution(anomalyType, resolution, financialStatus string) bool {
	if resolution == "ESCALATE_FINANCIAL_REPAIR" {
		return financialStatus == "PENDING"
	}
	if financialStatus == "PENDING" || financialStatus == "FAILED" {
		return false
	}
	switch resolution {
	case "CONFIRM_CHARGEBACK":
		return anomalyType == "CHARGEBACK" && financialStatus == "APPLIED"
	case "CONFIRM_FORCED_REFUND":
		return anomalyType == "FORCED_REFUND" && financialStatus == "APPLIED"
	case "CONFIRM_PAYMENT":
		return (anomalyType == "DUPLICATE_PAYMENT" || anomalyType == "LATE_PAYMENT_CONFLICT") && financialStatus == "APPLIED"
	case "CONFIRM_PROVIDER_FAILED":
		return anomalyType == "ILLEGAL_TRANSITION" && financialStatus == "NONE"
	case "RECONCILE_REFUND":
		return anomalyType == "REFUND_FAILURE" && financialStatus == "APPLIED"
	case "DISMISS":
		return financialStatus == "NONE" && (anomalyType == "CHARGEBACK" || anomalyType == "FORCED_REFUND" || anomalyType == "PAYMENT_FACT_CONFLICT" || anomalyType == "REFUND_FAILURE")
	default:
		return false
	}
}

func stringPointer(value string) *string { return &value }

func restoreDismissedPaymentAnomalyEntitlements(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, now time.Time) error {
	var packageID uuid.UUID
	switch {
	case item.ChargebackCaseID != nil:
		if err := tx.QueryRow(ctx, `SELECT entitlement_package_id FROM chargeback_cases WHERE id=$1`, *item.ChargebackCaseID).Scan(&packageID); err != nil {
			return err
		}
	case item.ForcedRefundCaseID != nil:
		if err := tx.QueryRow(ctx, `SELECT entitlement_package_id FROM forced_refund_cases WHERE id=$1`, *item.ForcedRefundCaseID).Scan(&packageID); err != nil {
			return err
		}
	default:
		return nil
	}
	var walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT wallet_id FROM entitlement_packages WHERE id=$1`, packageID).Scan(&walletID); err != nil {
		return err
	}
	wallet, err := getWallet(ctx, tx, walletID, true)
	if err != nil {
		return err
	}
	var packageStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1 AND wallet_id=$2 FOR UPDATE`, packageID, walletID).Scan(&packageStatus); err != nil {
		return err
	}
	if wallet.Status != "ACTIVE" || packageStatus != "FROZEN" {
		return nil
	}
	pending, err := packageFinancialFreezeBlocked(ctx, tx, packageID, walletID, uuid.Nil, item.ID)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	var freezeSnapshot json.RawMessage
	// Follow only overlapping, now-disproved financial freezes in this episode.
	// An unrelated old dismissal must not authorize a newer administrative freeze.
	err = tx.QueryRow(ctx, `WITH RECURSIVE sources AS (
 SELECT source.manual_review_case_id case_id,source.created_at,source.resolved_at,source.entitlement_snapshot snapshot FROM forced_refund_cases source WHERE source.entitlement_package_id=$1
 UNION ALL
 SELECT source.manual_review_case_id,source.created_at,source.resolved_at,anomaly.resource_snapshot->'entitlement_snapshot' FROM chargeback_cases source JOIN payment_anomaly_cases anomaly ON anomaly.id=source.manual_review_case_id WHERE source.entitlement_package_id=$1
 ), episode AS (
 SELECT * FROM sources WHERE case_id=$2
 UNION
 SELECT prior.* FROM sources prior JOIN episode current ON prior.created_at<=current.created_at AND prior.resolved_at>=current.created_at
 ) SELECT snapshot FROM episode ORDER BY created_at,case_id LIMIT 1`, packageID, item.ID).Scan(&freezeSnapshot)
	if err != nil {
		return err
	}
	// Historical evidence that did not record the freeze origin is retained.
	// Closing a review is not proof that every frozen right was originally active.
	if !validFinancialFreezeSnapshot(freezeSnapshot, packageID, walletID) {
		return nil
	}
	// A financial suspension may have hidden a newer membership projection.
	// Missing organization evidence keeps the financial fence intact.
	if wallet.OwnerType == "ORGANIZATION" {
		var unknown bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member_quota_grants g LEFT JOIN organization_member_lifecycle_states m ON m.organization_id=g.organization_id AND m.user_id=g.member_user_id WHERE g.package_id=$1 AND g.status='SUSPENDED' AND g.suspension_reason IN ('CHARGEBACK_REVIEW','FORCED_REFUND_REVIEW') AND (m.user_id IS NULL OR m.membership_version<g.membership_version))`, packageID).Scan(&unknown); err != nil {
			return err
		}
		if unknown {
			return nil
		}
		rows, err := tx.Query(ctx, `UPDATE member_quota_grants g SET suspension_reason='MEMBER_REMOVED',membership_version=m.membership_version,allocation_version=g.allocation_version+1,updated_at=$2 FROM organization_member_lifecycle_states m WHERE g.package_id=$1 AND g.status='SUSPENDED' AND g.suspension_reason IN ('CHARGEBACK_REVIEW','FORCED_REFUND_REVIEW') AND m.organization_id=g.organization_id AND m.user_id=g.member_user_id AND m.status IN ('REVOKED','SUSPENDED') AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'grants') prior WHERE (prior->>'id')::uuid=g.id AND prior->>'status'='ACTIVE') RETURNING g.id`, packageID, now, freezeSnapshot)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			grant, _, err := loadMemberQuotaGrant(ctx, tx, id, false)
			if err != nil {
				return err
			}
			if err := appendMemberQuotaAudit(ctx, tx, grant, "SUSPENDED", nil, 0, now); err != nil {
				return err
			}
			if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.suspended.v1", now); err != nil {
				return err
			}
		}
	}
	var restoredOrganizationGrants []uuid.UUID
	if wallet.OwnerType == "ORGANIZATION" {
		rows, err := tx.Query(ctx, `SELECT g.id FROM member_quota_grants g JOIN entitlement_components c ON c.id=g.component_id JOIN organization_member_lifecycle_states m ON m.organization_id=g.organization_id AND m.user_id=g.member_user_id WHERE g.package_id=$1 AND g.status='SUSPENDED' AND g.suspension_reason IN ('CHARGEBACK_REVIEW','FORCED_REFUND_REVIEW') AND g.expires_at>$2 AND c.status='FROZEN' AND c.expires_at>$2 AND m.status='ACTIVE' AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'grants') prior WHERE (prior->>'id')::uuid=g.id AND prior->>'status'='ACTIVE') AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'components') prior WHERE (prior->>'id')::uuid=c.id AND prior->>'status'='ACTIVE') ORDER BY g.id`, packageID, now, freezeSnapshot)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			restoredOrganizationGrants = append(restoredOrganizationGrants, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	// Restore only the financial-review suspensions. Administrative suspension,
	// expiry and revocation remain intact, and no credit is minted or extended.
	if _, err = tx.Exec(ctx, `WITH restored AS (
 UPDATE member_quota_grants member_grant SET status='ACTIVE',suspension_reason=NULL,membership_version=CASE WHEN $4='ORGANIZATION' THEN (SELECT m.membership_version FROM organization_member_lifecycle_states m WHERE m.organization_id=member_grant.organization_id AND m.user_id=member_grant.member_user_id) ELSE member_grant.membership_version END,allocation_version=allocation_version+1,updated_at=$2
 FROM entitlement_components component
 WHERE member_grant.package_id=$1 AND member_grant.status='SUSPENDED' AND member_grant.suspension_reason IN ('CHARGEBACK_REVIEW','FORCED_REFUND_REVIEW') AND member_grant.expires_at>$2
 AND component.id=member_grant.component_id AND component.status='FROZEN' AND component.expires_at>$2
 AND ($4<>'ORGANIZATION' OR EXISTS(SELECT 1 FROM organization_member_lifecycle_states m WHERE m.organization_id=member_grant.organization_id AND m.user_id=member_grant.member_user_id AND m.status='ACTIVE'))
 AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'grants') prior WHERE (prior->>'id')::uuid=member_grant.id AND prior->>'status'='ACTIVE')
 AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'components') prior WHERE (prior->>'id')::uuid=component.id AND prior->>'status'='ACTIVE')
 RETURNING member_grant.component_id,member_grant.available_microcredits
), totals AS (SELECT component_id,sum(available_microcredits)::bigint amount FROM restored GROUP BY component_id)
 UPDATE entitlement_components component SET active_grant_microcredits=component.active_grant_microcredits+totals.amount,suspended_grant_microcredits=component.suspended_grant_microcredits-totals.amount,updated_at=$2
 FROM totals WHERE component.id=totals.component_id`, packageID, now, freezeSnapshot, wallet.OwnerType); err != nil {
		return err
	}
	for _, id := range restoredOrganizationGrants {
		grant, _, err := loadMemberQuotaGrant(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := appendMemberQuotaAudit(ctx, tx, grant, "RESTORED", nil, grant.AvailableMicrocredits, now); err != nil {
			return err
		}
		if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.restored.v1", now); err != nil {
			return err
		}
	}
	// Clearing a disproved financial fence does not issue or extend rights.
	// Time-expired rights remain unusable and can enter the ordinary expiry worker;
	// already EXPIRED/REVOKED states are never resurrected.
	if _, err = tx.Exec(ctx, `UPDATE entitlement_components component SET status='ACTIVE',updated_at=$2 WHERE package_id=$1 AND status='FROZEN' AND EXISTS(SELECT 1 FROM jsonb_array_elements($3::jsonb->'components') prior WHERE (prior->>'id')::uuid=component.id AND prior->>'status'='ACTIVE')`, packageID, now, freezeSnapshot); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE entitlement_packages SET status='ACTIVE',updated_at=$2 WHERE id=$1 AND status='FROZEN' AND $3::jsonb->'package'->>'status'='ACTIVE' AND NOT EXISTS(SELECT 1 FROM entitlement_components WHERE package_id=$1 AND status='FROZEN')`, packageID, now, freezeSnapshot)
	return err
}

type paymentAnomalyMutation func(context.Context, pgx.Tx, PaymentAnomalyCase, time.Time) (PaymentAnomalyCase, error)

func (r *PostgresRepository) updatePaymentAnomaly(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey, action, reason string, mutate paymentAnomalyMutation) (PaymentAnomalyCase, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		return PaymentAnomalyCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-anomaly.command", caseID.String()+":"+idempotencyKey); err != nil {
		return PaymentAnomalyCase{}, err
	}
	hash := paymentAnomalyCommandHash(action, reason)
	var existingHash string
	var existingActor uuid.UUID
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, idempotencyKey).Scan(&existingHash, &existingActor)
	if err == nil {
		if existingHash != hash || existingActor != actorID {
			return PaymentAnomalyCase{}, ErrConflict
		}
		item, found, loadErr := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1`, caseID))
		if loadErr != nil || !found {
			return PaymentAnomalyCase{}, loadErr
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, err
	}
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id FROM payment_anomaly_resolution_intents WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, idempotencyKey).Scan(&existingHash, &existingActor)
	if err == nil && (existingHash != hash || existingActor != actorID) {
		return PaymentAnomalyCase{}, ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, err
	}
	// Order projection and entitlement restoration share the wallet-first gate.
	var orderID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT payment_order_id FROM payment_anomaly_cases WHERE id=$1`, caseID).Scan(&orderID); errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, ErrNotFound
	} else if err != nil {
		return PaymentAnomalyCase{}, err
	}
	if orderID != nil {
		if _, err := getPaymentOrderByID(ctx, tx, *orderID, true); err != nil {
			return PaymentAnomalyCase{}, err
		}
	}
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual`, caseID))
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return PaymentAnomalyCase{}, err
	}
	if !equalUUID(item.PaymentOrderID, orderID) {
		return PaymentAnomalyCase{}, ErrConflict
	}
	before, _ := json.Marshal(item)
	now := r.now()
	updated, err := mutate(ctx, tx, item, now)
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	after, _ := json.Marshal(updated)
	_, err = tx.Exec(ctx, `INSERT INTO payment_anomaly_commands(id,payment_anomaly_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), caseID, actorID, idempotencyKey, hash, strings.ToUpper(strings.TrimSpace(action)), strings.TrimSpace(reason), before, after, now)
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, strings.ToUpper(strings.TrimSpace(action)), &actorID, map[string]any{"reason": strings.TrimSpace(reason), "financial_action_status": updated.FinancialActionStatus}, now)
	}
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	return updated, tx.Commit(ctx)
}
