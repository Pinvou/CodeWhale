package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
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
	switch severity {
	case "P0":
		return now.Add(15 * time.Minute), now.Add(30 * time.Minute), now.Add(time.Hour), now.Add(4 * time.Hour)
	case "P1":
		return now.Add(time.Hour), now.Add(2 * time.Hour), now.Add(4 * time.Hour), now.Add(8 * time.Hour)
	default:
		return p2ReviewDeadlines(now)
	}
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

func scanPaymentAnomaly(row pgx.Row) (PaymentAnomalyCase, bool, error) {
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
	case "DISMISS":
		return financialStatus == "NONE" && (anomalyType == "CHARGEBACK" || anomalyType == "FORCED_REFUND" || anomalyType == "PAYMENT_FACT_CONFLICT" || anomalyType == "REFUND_FAILURE")
	default:
		return false
	}
}

func stringPointer(value string) *string { return &value }

func restoreDismissedPaymentAnomalyEntitlements(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, now time.Time) error {
	var packageID uuid.UUID
	var suspensionReason string
	switch {
	case item.ChargebackCaseID != nil:
		suspensionReason = "CHARGEBACK_REVIEW"
		if err := tx.QueryRow(ctx, `SELECT entitlement_package_id FROM chargeback_cases WHERE id=$1`, *item.ChargebackCaseID).Scan(&packageID); err != nil {
			return err
		}
	case item.ForcedRefundCaseID != nil:
		suspensionReason = "FORCED_REFUND_REVIEW"
		if err := tx.QueryRow(ctx, `SELECT entitlement_package_id FROM forced_refund_cases WHERE id=$1`, *item.ForcedRefundCaseID).Scan(&packageID); err != nil {
			return err
		}
	default:
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT component_id,available_microcredits FROM member_quota_grants WHERE package_id=$1 AND status='SUSPENDED' AND suspension_reason=$2 ORDER BY component_id,id FOR UPDATE`, packageID, suspensionReason)
	if err != nil {
		return err
	}
	type restoration struct {
		componentID uuid.UUID
		amount      int64
	}
	restoreByComponent := map[uuid.UUID]int64{}
	for rows.Next() {
		var componentID uuid.UUID
		var amount int64
		if err := rows.Scan(&componentID, &amount); err != nil {
			rows.Close()
			return err
		}
		restoreByComponent[componentID] += amount
	}
	rows.Close()
	restores := make([]restoration, 0, len(restoreByComponent))
	for componentID, amount := range restoreByComponent {
		restores = append(restores, restoration{componentID: componentID, amount: amount})
	}
	sort.Slice(restores, func(i, j int) bool { return restores[i].componentID.String() < restores[j].componentID.String() })
	for _, value := range restores {
		if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET active_grant_microcredits=active_grant_microcredits+$2,suspended_grant_microcredits=suspended_grant_microcredits-$2,status='ACTIVE',updated_at=$3 WHERE id=$1 AND suspended_grant_microcredits>=$2`, value.componentID, value.amount, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET status='ACTIVE',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$3 WHERE package_id=$1 AND status='SUSPENDED' AND suspension_reason=$2`, packageID, suspensionReason, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE entitlement_packages SET status='ACTIVE',updated_at=$2 WHERE id=$1 AND status='FROZEN'`, packageID, now)
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
	err = tx.QueryRow(ctx, `SELECT request_hash FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, idempotencyKey).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
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
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual`, caseID))
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return PaymentAnomalyCase{}, err
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
