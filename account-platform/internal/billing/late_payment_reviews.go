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

const latePaymentReviewGrace = 7 * 24 * time.Hour

type LatePaymentReviewCase struct {
	ID                         uuid.UUID       `json:"case_id"`
	PaymentOrderID             uuid.UUID       `json:"payment_order_id"`
	ReviewStatus               string          `json:"review_status"`
	Severity                   string          `json:"severity"`
	OwnerID                    *uuid.UUID      `json:"owner_id,omitempty"`
	ProviderID                 string          `json:"provider_id"`
	ProviderVersion            string          `json:"provider_version"`
	MerchantOrderNo            string          `json:"merchant_order_no"`
	ProviderCapabilitySnapshot json.RawMessage `json:"provider_capability_snapshot"`
	PolicySnapshot             json.RawMessage `json:"policy_snapshot"`
	EvidenceSummary            json.RawMessage `json:"evidence_summary"`
	ClaimedAt                  *time.Time      `json:"claimed_at,omitempty"`
	FirstResponseAt            *time.Time      `json:"first_response_at,omitempty"`
	ClaimDueAt                 time.Time       `json:"claim_due_at"`
	FirstResponseDueAt         time.Time       `json:"first_response_due_at"`
	MitigationDueAt            time.Time       `json:"mitigation_due_at"`
	ResolutionDueAt            time.Time       `json:"resolution_due_at"`
	EscalationLevel            int             `json:"escalation_level"`
	LastEscalatedAt            *time.Time      `json:"last_escalated_at,omitempty"`
	LastQueryAt                *time.Time      `json:"last_query_at,omitempty"`
	LastQueryResult            *string         `json:"last_query_result,omitempty"`
	NextReviewAt               time.Time       `json:"next_review_at"`
	BlindReason                *string         `json:"blind_reason,omitempty"`
	CreatedAt                  time.Time       `json:"created_at"`
	UpdatedAt                  time.Time       `json:"updated_at"`
	ResolvedAt                 *time.Time      `json:"resolved_at,omitempty"`
}

type LatePaymentReviewFilter struct {
	Status           string
	ProviderID       string
	OwnerID          *uuid.UUID
	EnteredFrom      *time.Time
	EnteredTo        *time.Time
	NextReviewBefore *time.Time
	Overdue          bool
}

type ManualReviewCaseEvent struct {
	ID        uuid.UUID       `json:"event_id"`
	EventType string          `json:"event_type"`
	ActorID   *uuid.UUID      `json:"actor_id,omitempty"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

type LatePaymentReviewEvidence struct {
	ID                uuid.UUID       `json:"evidence_id"`
	EvidenceType      string          `json:"evidence_type"`
	PaymentEvidenceID *uuid.UUID      `json:"payment_evidence_id,omitempty"`
	ActorID           *uuid.UUID      `json:"actor_id,omitempty"`
	PayloadDigest     string          `json:"payload_digest"`
	PayloadSnapshot   json.RawMessage `json:"payload_snapshot"`
	CreatedAt         time.Time       `json:"created_at"`
}

type LatePaymentReviewDetail struct {
	Case     LatePaymentReviewCase       `json:"case"`
	Events   []ManualReviewCaseEvent     `json:"events"`
	Evidence []LatePaymentReviewEvidence `json:"evidence"`
}

func p2ReviewDeadlines(now time.Time) (time.Time, time.Time, time.Time, time.Time) {
	deadlines, _ := manualreview.DeadlinesFor(now, "P2")
	return deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution
}

func addBusinessHours(start time.Time, hours int) time.Time {
	return manualreview.AddBusinessHours(start, hours)
}

func (r *PostgresRepository) EscalateLatePaymentReview(ctx context.Context, orderID uuid.UUID, queryResult string) (LatePaymentReviewCase, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "late-payment.review", orderID.String()); err != nil {
		return LatePaymentReviewCase{}, err
	}
	if existing, found, err := getLatePaymentReviewCaseByOrder(ctx, tx, orderID); err != nil {
		return LatePaymentReviewCase{}, err
	} else if found {
		return existing, tx.Commit(ctx)
	}
	now := r.now()
	var item LatePaymentReviewCase
	var orderStatus, fulfillmentStatus string
	err = tx.QueryRow(ctx, `SELECT id,provider_id,provider_version,merchant_order_no,provider_capability_snapshot,status,fulfillment_status,
		jsonb_build_object('evidence_count',(SELECT count(*) FROM payment_evidence WHERE payment_order_id=payment.id),'latest_received_at',(SELECT max(received_at) FROM payment_evidence WHERE payment_order_id=payment.id))
		FROM payment_orders payment WHERE id=$1 AND archive_status='ACTIVE' FOR UPDATE`, orderID).Scan(&item.PaymentOrderID, &item.ProviderID, &item.ProviderVersion, &item.MerchantOrderNo, &item.ProviderCapabilitySnapshot, &orderStatus, &fulfillmentStatus, &item.EvidenceSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if (orderStatus != "CREATED" && orderStatus != "PENDING") || fulfillmentStatus != "PENDING" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	item.ID = uuid.Must(uuid.NewV7())
	item.ReviewStatus, item.Severity = "REVIEW_REQUIRED_LATE_PAYMENT", "P2"
	item.PolicySnapshot, _ = json.Marshal(map[string]any{"boundary_hours": 72, "initial_review_interval_hours": 24, "initial_review_days": 7, "later_review_interval_days": 7, "reconciliation_grace_seconds": int64(latePaymentReviewGrace.Seconds()), "sla_timezone": "Asia/Shanghai", "sla_business_hours": "09:00-17:00"})
	item.CreatedAt, item.UpdatedAt, item.NextReviewAt = now, now, now.Add(24*time.Hour)
	item.LastQueryAt = &now
	queryResult = strings.TrimSpace(queryResult)
	if queryResult == "" {
		queryResult = "PENDING"
	}
	item.LastQueryResult = &queryResult
	item.ClaimDueAt, item.FirstResponseDueAt, item.MitigationDueAt, item.ResolutionDueAt = p2ReviewDeadlines(now)
	_, err = tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','LATE_PAYMENT','PAYMENT_ORDER',$2,'P2','OPEN',$3,$4,$5,$6,$7,$3)`, item.ID, orderID, now, item.ClaimDueAt, item.FirstResponseDueAt, item.MitigationDueAt, item.ResolutionDueAt)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO late_payment_review_cases(id,payment_order_id,review_status,provider_id,provider_version,merchant_order_no,provider_capability_snapshot,policy_snapshot,evidence_summary,last_query_at,last_query_result,next_review_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$10,$10)`, item.ID, orderID, item.ReviewStatus, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo, item.ProviderCapabilitySnapshot, item.PolicySnapshot, item.EvidenceSummary, now, queryResult, item.NextReviewAt)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, item.ID, "CREATED", nil, map[string]any{"review_status": item.ReviewStatus, "last_query_result": queryResult}, now)
	}
	if err == nil {
		payload, _ := json.Marshal(map[string]any{"result": queryResult, "queried_at": now, "boundary": "72_HOURS"})
		digest := sha256.Sum256(payload)
		_, err = tx.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'BOUNDARY_QUERY',$3,$4,$5)`, uuid.Must(uuid.NewV7()), item.ID, hex.EncodeToString(digest[:]), payload, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE payment_orders SET status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW',attention_summary='REVIEW_REQUIRED',reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, orderID, now)
	}
	if err == nil {
		err = events.AppendOutbox(ctx, tx, "billing.late-payment.review-required.v1", item.ID, 1, map[string]any{"case_id": item.ID, "payment_order_id": orderID, "severity": "P2", "next_review_at": item.NextReviewAt})
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	return item, tx.Commit(ctx)
}

func appendManualReviewEvent(ctx context.Context, tx pgx.Tx, caseID uuid.UUID, eventType string, actorID *uuid.UUID, details any, now time.Time) error {
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,actor_id,details,created_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), caseID, eventType, actorID, payload, now)
	return err
}

func getLatePaymentReviewCaseByOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) (LatePaymentReviewCase, bool, error) {
	return scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.payment_order_id=$1`, orderID))
}

const latePaymentReviewSelect = `SELECT late.id,late.payment_order_id,late.review_status,manual.severity,manual.owner_id,late.provider_id,late.provider_version,late.merchant_order_no,late.provider_capability_snapshot,late.policy_snapshot,late.evidence_summary,manual.claimed_at,manual.first_response_at,manual.claim_due_at,manual.first_response_due_at,manual.mitigation_due_at,manual.resolution_due_at,manual.escalation_level,manual.last_escalated_at,late.last_query_at,late.last_query_result,late.next_review_at,late.blind_reason,late.created_at,late.updated_at,late.resolved_at FROM late_payment_review_cases late JOIN manual_review_cases manual ON manual.id=late.id`

func scanLatePaymentReviewCase(row pgx.Row) (LatePaymentReviewCase, bool, error) {
	var item LatePaymentReviewCase
	err := row.Scan(&item.ID, &item.PaymentOrderID, &item.ReviewStatus, &item.Severity, &item.OwnerID, &item.ProviderID, &item.ProviderVersion, &item.MerchantOrderNo, &item.ProviderCapabilitySnapshot, &item.PolicySnapshot, &item.EvidenceSummary, &item.ClaimedAt, &item.FirstResponseAt, &item.ClaimDueAt, &item.FirstResponseDueAt, &item.MitigationDueAt, &item.ResolutionDueAt, &item.EscalationLevel, &item.LastEscalatedAt, &item.LastQueryAt, &item.LastQueryResult, &item.NextReviewAt, &item.BlindReason, &item.CreatedAt, &item.UpdatedAt, &item.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

func (r *PostgresRepository) AddLatePaymentClaim(ctx context.Context, orderID, requesterID uuid.UUID, statement string) (LatePaymentReviewCase, error) {
	statement = strings.TrimSpace(statement)
	if orderID == uuid.Nil || requesterID == uuid.Nil || len(statement) < 10 || len(statement) > 1000 {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT requester_id FROM payment_orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&owner); err != nil || owner != requesterID {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	item, found, err := getLatePaymentReviewCaseByOrder(ctx, tx, orderID)
	if err != nil || !found || item.ReviewStatus == "ARCHIVED_CONFIRMED_UNPAID" || item.ReviewStatus == "RESOLVED_PAID" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	now := r.now()
	digest := sha256.Sum256([]byte(statement))
	command, err := tx.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'USER_PAYMENT_CLAIM',$3,jsonb_build_object('statement',$4),$5) ON CONFLICT(case_id,evidence_type,payload_digest) DO NOTHING`, uuid.Must(uuid.NewV7()), item.ID, hex.EncodeToString(digest[:]), statement, now)
	if err == nil && command.RowsAffected() == 0 {
		return item, tx.Commit(ctx)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET next_review_at=$2,updated_at=$2 WHERE id=$1`, item.ID, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, item.ID, "USER_PAYMENT_CLAIMED", nil, map[string]any{"payload_digest": hex.EncodeToString(digest[:])}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item.NextReviewAt, item.UpdatedAt = now, now
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminLatePaymentReviews(ctx context.Context, filter LatePaymentReviewFilter, limit int) ([]LatePaymentReviewCase, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	status, providerID := strings.TrimSpace(filter.Status), strings.TrimSpace(filter.ProviderID)
	rows, err := r.pool.Query(ctx, latePaymentReviewSelect+` WHERE ($1='' OR late.review_status=$1) AND ($2='' OR late.provider_id=$2) AND ($3::uuid IS NULL OR manual.owner_id=$3) AND ($4::timestamptz IS NULL OR late.created_at>=$4) AND ($5::timestamptz IS NULL OR late.created_at<$5) AND ($6::timestamptz IS NULL OR late.next_review_at<=$6) AND (NOT $7 OR manual.status='OPEN' AND (late.next_review_at<=now() OR manual.claim_due_at<=now() AND manual.claimed_at IS NULL OR manual.first_response_due_at<=now() AND manual.first_response_at IS NULL OR manual.mitigation_due_at<=now() AND manual.mitigation_at IS NULL OR manual.resolution_due_at<=now())) ORDER BY late.next_review_at,late.created_at,late.id LIMIT $8`, status, providerID, filter.OwnerID, filter.EnteredFrom, filter.EnteredTo, filter.NextReviewBefore, filter.Overdue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []LatePaymentReviewCase{}
	for rows.Next() {
		item, _, err := scanLatePaymentReviewCase(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) LatePaymentReview(ctx context.Context, caseID uuid.UUID) (LatePaymentReviewCase, error) {
	item, found, err := scanLatePaymentReviewCase(r.pool.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil || !found {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	return item, nil
}

func (r *PostgresRepository) LatePaymentReviewDetail(ctx context.Context, caseID uuid.UUID) (LatePaymentReviewDetail, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LatePaymentReviewDetail{}, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil || !found {
		return LatePaymentReviewDetail{}, ErrNotFound
	}
	detail := LatePaymentReviewDetail{Case: item, Events: []ManualReviewCaseEvent{}, Evidence: []LatePaymentReviewEvidence{}}
	eventRows, err := tx.Query(ctx, `SELECT id,event_type,actor_id,details,created_at FROM manual_review_case_events WHERE case_id=$1 ORDER BY created_at,id`, caseID)
	if err != nil {
		return LatePaymentReviewDetail{}, err
	}
	for eventRows.Next() {
		var event ManualReviewCaseEvent
		if err := eventRows.Scan(&event.ID, &event.EventType, &event.ActorID, &event.Details, &event.CreatedAt); err != nil {
			eventRows.Close()
			return LatePaymentReviewDetail{}, err
		}
		detail.Events = append(detail.Events, event)
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		return LatePaymentReviewDetail{}, err
	}
	eventRows.Close()
	evidenceRows, err := tx.Query(ctx, `SELECT id,evidence_type,payment_evidence_id,actor_id,payload_digest,payload_snapshot,created_at FROM late_payment_review_evidence WHERE case_id=$1 ORDER BY created_at,id`, caseID)
	if err != nil {
		return LatePaymentReviewDetail{}, err
	}
	for evidenceRows.Next() {
		var evidence LatePaymentReviewEvidence
		if err := evidenceRows.Scan(&evidence.ID, &evidence.EvidenceType, &evidence.PaymentEvidenceID, &evidence.ActorID, &evidence.PayloadDigest, &evidence.PayloadSnapshot, &evidence.CreatedAt); err != nil {
			evidenceRows.Close()
			return LatePaymentReviewDetail{}, err
		}
		detail.Evidence = append(detail.Evidence, evidence)
	}
	if err := evidenceRows.Err(); err != nil {
		evidenceRows.Close()
		return LatePaymentReviewDetail{}, err
	}
	evidenceRows.Close()
	return detail, tx.Commit(ctx)
}

func (r *PostgresRepository) ClaimLatePaymentReview(ctx context.Context, caseID, ownerID uuid.UUID) (LatePaymentReviewCase, error) {
	if caseID == uuid.Nil || ownerID == uuid.Nil {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var existingOwner *uuid.UUID
	var status string
	if err := tx.QueryRow(ctx, `SELECT owner_id,status FROM manual_review_cases WHERE id=$1 FOR UPDATE`, caseID).Scan(&existingOwner, &status); err != nil {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	if status != "OPEN" || existingOwner != nil && *existingOwner != ownerID {
		return LatePaymentReviewCase{}, ErrConflict
	}
	if existingOwner != nil {
		item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
		if err != nil || !found {
			return LatePaymentReviewCase{}, ErrNotFound
		}
		return item, tx.Commit(ctx)
	}
	command, err := tx.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=COALESCE(claimed_at,$3),updated_at=$3 WHERE id=$1 AND status='OPEN' AND (owner_id IS NULL OR owner_id=$2)`, caseID, ownerID, now)
	if err != nil || command.RowsAffected() != 1 {
		return LatePaymentReviewCase{}, ErrConflict
	}
	if err := appendManualReviewEvent(ctx, tx, caseID, "CLAIMED", &ownerID, map[string]any{"owner_id": ownerID}, now); err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil || !found {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) ScheduleLatePaymentReview(ctx context.Context, caseID, actorID uuid.UUID, nextReviewAt time.Time, reason string) (LatePaymentReviewCase, error) {
	reason = strings.TrimSpace(reason)
	now := r.now()
	if caseID == uuid.Nil || actorID == uuid.Nil || len(reason) < 10 || len(reason) > 500 || !nextReviewAt.After(now) || nextReviewAt.After(now.Add(8*24*time.Hour)) {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil || !found || item.OwnerID == nil || *item.OwnerID != actorID || (item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" && item.ReviewStatus != "PROVIDER_BLIND") {
		return LatePaymentReviewCase{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET next_review_at=$2,updated_at=$3 WHERE id=$1`, caseID, nextReviewAt.UTC(), now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET first_response_at=COALESCE(first_response_at,$2),mitigation_at=COALESCE(mitigation_at,$2),updated_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, "REVIEW_SCHEDULED", &actorID, map[string]any{"next_review_at": nextReviewAt.UTC(), "reason": reason}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item.NextReviewAt, item.UpdatedAt = nextReviewAt.UTC(), now
	if item.FirstResponseAt == nil {
		item.FirstResponseAt = &now
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) RecordLatePaymentReviewQuery(ctx context.Context, caseID, actorID uuid.UUID, order PaymentOrder, queryErr error) (LatePaymentReviewCase, error) {
	if caseID == uuid.Nil || actorID == uuid.Nil {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil || !found {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	if item.OwnerID == nil || *item.OwnerID != actorID || (item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" && item.ReviewStatus != "PROVIDER_BLIND") {
		return LatePaymentReviewCase{}, ErrConflict
	}
	now := r.now()
	result := order.Status
	if queryErr != nil {
		result = safePaymentReconciliationError(queryErr)
	}
	if result == "" {
		result = "UNKNOWN"
	}
	payload, _ := json.Marshal(map[string]any{"result": result, "order_status": order.Status, "fulfillment_status": order.FulfillmentStatus})
	digest := sha256.Sum256(payload)
	_, err = tx.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,actor_id,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'MANUAL_QUERY',$3,$4,$5,$6) ON CONFLICT(case_id,evidence_type,payload_digest) DO NOTHING`, uuid.Must(uuid.NewV7()), caseID, actorID, hex.EncodeToString(digest[:]), payload, now)
	resolvedPaid := queryErr == nil && (order.Status == "PAID" || order.Status == "PAID_LATE") && order.FulfillmentStatus == "CREDITED"
	nextReviewAt := nextLatePaymentReviewAt(item.CreatedAt, now)
	if err == nil && resolvedPaid {
		_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='RESOLVED_PAID',last_query_at=$2,last_query_result=$3,updated_at=$2,resolved_at=$2 WHERE id=$1`, caseID, now, result)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET first_response_at=COALESCE(first_response_at,$2),status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1`, caseID, now)
		}
	} else if err == nil {
		_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET last_query_at=$2,last_query_result=$3,next_review_at=$4,updated_at=$2 WHERE id=$1`, caseID, now, result, nextReviewAt)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET first_response_at=COALESCE(first_response_at,$2),updated_at=$2 WHERE id=$1`, caseID, now)
		}
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, map[bool]string{true: "PAYMENT_CONFIRMED", false: "PROVIDER_QUERY_COMPLETED"}[resolvedPaid], &actorID, map[string]any{"result": result, "next_review_at": nextReviewAt}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, _, err = scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	return item, tx.Commit(ctx)
}

func nextLatePaymentReviewAt(createdAt, now time.Time) time.Time {
	if now.Before(createdAt.Add(7 * 24 * time.Hour)) {
		return now.Add(24 * time.Hour)
	}
	return now.Add(7 * 24 * time.Hour)
}

func (r *PostgresRepository) ArchiveLatePaymentReview(ctx context.Context, caseID, actorID uuid.UUID) (LatePaymentReviewCase, error) {
	if caseID == uuid.Nil || actorID == uuid.Nil {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil || !found || item.OwnerID == nil || *item.OwnerID != actorID || item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	now := r.now()
	var eligibleAt time.Time
	var valid bool
	err = tx.QueryRow(ctx, `SELECT final_archive_eligible_at,
		$2>=final_archive_eligible_at AND last_successful_query_at>=final_archive_eligible_at AND last_authoritative_unpaid_at>=final_archive_eligible_at
		AND NOT EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=payment.id AND status IN ('PAID','LATE_PAID','REVIEW_REQUIRED'))
		AND NOT EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=payment.id AND merge_result='REVIEW_REQUIRED')
		AND NOT EXISTS(SELECT 1 FROM late_payment_review_evidence claim WHERE claim.case_id=$1 AND claim.evidence_type='USER_PAYMENT_CLAIM' AND NOT EXISTS(SELECT 1 FROM late_payment_review_evidence query WHERE query.case_id=claim.case_id AND query.evidence_type IN ('MANUAL_QUERY','ARCHIVE_QUERY') AND query.created_at>claim.created_at))
		FROM payment_orders payment WHERE id=$3 FOR UPDATE`, caseID, now, item.PaymentOrderID).Scan(&eligibleAt, &valid)
	if err != nil || !valid {
		return LatePaymentReviewCase{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET archive_status='ARCHIVED_CONFIRMED_UNPAID',archived_at=$2,payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,updated_at=$2 WHERE id=$1`, item.PaymentOrderID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='ARCHIVED_CONFIRMED_UNPAID',updated_at=$2,resolved_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, "ARCHIVED_CONFIRMED_UNPAID", &actorID, map[string]any{"final_archive_eligible_at": eligibleAt}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item.ReviewStatus, item.ResolvedAt, item.UpdatedAt = "ARCHIVED_CONFIRMED_UNPAID", &now, now
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) EscalateProviderBlind(ctx context.Context, caseID, actorID uuid.UUID, reason string) (LatePaymentReviewCase, error) {
	reason = strings.TrimSpace(reason)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(reason) < 10 || len(reason) > 1000 {
		return LatePaymentReviewCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil || !found || item.OwnerID == nil || *item.OwnerID != actorID || item.ReviewStatus == "ARCHIVED_CONFIRMED_UNPAID" || item.ReviewStatus == "RESOLVED_PAID" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	now := r.now()
	_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='PROVIDER_BLIND',blind_reason=$2,next_review_at=$3,updated_at=$3 WHERE id=$1`, caseID, reason, now.Add(24*time.Hour))
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,updated_at) VALUES($1,$2,FALSE,$3,$4) ON CONFLICT(provider_id,provider_version) DO UPDATE SET new_orders_enabled=FALSE,reason=EXCLUDED.reason,version=payment_provider_controls.version+1,updated_at=EXCLUDED.updated_at`, item.ProviderID, item.ProviderVersion, reason, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, "PROVIDER_BLIND", &actorID, map[string]any{"reason": reason, "new_orders_enabled": false}, now)
	}
	if err == nil {
		payload, _ := json.Marshal(map[string]any{"reason": reason, "new_orders_enabled": false})
		digest := sha256.Sum256(payload)
		_, err = tx.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,actor_id,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'PROVIDER_BLIND',$3,$4,$5,$6) ON CONFLICT(case_id,evidence_type,payload_digest) DO NOTHING`, uuid.Must(uuid.NewV7()), caseID, actorID, hex.EncodeToString(digest[:]), payload, now)
	}
	if err == nil {
		err = events.AppendOutbox(ctx, tx, "billing.provider-blind.detected.v1", caseID, 1, map[string]any{"case_id": caseID, "payment_order_id": item.PaymentOrderID, "provider_id": item.ProviderID, "provider_version": item.ProviderVersion, "reason": reason, "new_orders_enabled": false})
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item.ReviewStatus, item.BlindReason, item.UpdatedAt = "PROVIDER_BLIND", &reason, now
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) EscalateOverdueManualReviews(ctx context.Context, limit int, schedules ...manualreview.OnCallSchedule) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var schedule manualreview.OnCallSchedule
	if len(schedules) > 0 {
		schedule = schedules[0]
	}
	rows, err := r.pool.Query(ctx, `SELECT id,severity,owner_id,CASE WHEN resolution_due_at<=now() THEN 'RESOLUTION_OVERDUE' WHEN mitigation_at IS NULL AND mitigation_due_at<=now() THEN 'MITIGATION_OVERDUE' WHEN first_response_at IS NULL AND first_response_due_at<=now() THEN 'FIRST_RESPONSE_OVERDUE' WHEN claimed_at IS NULL AND claim_due_at<=now() THEN 'CLAIM_OVERDUE' END,(severity IN ('P0','P1') AND mitigation_at IS NULL AND mitigation_due_at<=now()) FROM manual_review_cases WHERE status='OPEN' AND ((claimed_at IS NULL AND claim_due_at<=now()) OR (first_response_at IS NULL AND first_response_due_at<=now()) OR (mitigation_at IS NULL AND mitigation_due_at<=now()) OR resolution_due_at<=now()) AND (last_escalated_at IS NULL OR last_escalated_at<now()-interval '1 hour') ORDER BY CASE severity WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 ELSE 2 END,created_at,id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type overdue struct {
		id       uuid.UUID
		severity string
		ownerID  *uuid.UUID
		stage    string
		stopLoss bool
	}
	items := []overdue{}
	for rows.Next() {
		var item overdue
		if err := rows.Scan(&item.id, &item.severity, &item.ownerID, &item.stage, &item.stopLoss); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return count, err
		}
		now := r.now()
		var escalationLevel int
		err = tx.QueryRow(ctx, `UPDATE manual_review_cases SET escalation_level=escalation_level+1,last_escalated_at=$2,updated_at=$2 WHERE id=$1 AND status='OPEN' RETURNING escalation_level`, item.id, now).Scan(&escalationLevel)
		toOwner, hasOwner := schedule.Next(escalationLevel - 1)
		if err == nil && hasOwner {
			_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,last_assigned_at=$3,assignment_revision=assignment_revision+1,updated_at=$3 WHERE id=$1`, item.id, toOwner, now)
		}
		if err == nil && hasOwner {
			_, err = tx.Exec(ctx, `INSERT INTO manual_review_assignments(id,case_id,from_owner_id,to_owner_id,escalation_level,reason,assigned_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(case_id,escalation_level) DO NOTHING`, uuid.Must(uuid.NewV7()), item.id, item.ownerID, toOwner, escalationLevel, item.stage, now)
		}
		if err == nil {
			details := map[string]any{"stage": item.stage, "severity": item.severity, "escalation_level": escalationLevel, "assignment_changed": hasOwner}
			if hasOwner {
				details["to_owner_id"] = toOwner
			}
			err = appendManualReviewEvent(ctx, tx, item.id, "SLA_ESCALATED", nil, details, now)
		}
		if err == nil && item.stopLoss {
			err = r.evaluateManualReviewStopLoss(ctx, tx, item.id, item.severity, now)
		}
		if err == nil {
			err = events.AppendOutbox(ctx, tx, "billing.manual-review.sla-escalated.v1", item.id, int64(escalationLevel), map[string]any{"case_id": item.id, "stage": item.stage, "severity": item.severity, "owner_id": map[bool]any{true: toOwner, false: nil}[hasOwner]})
		}
		if err != nil {
			tx.Rollback(ctx)
			return count, err
		}
		if err := tx.Commit(ctx); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (r *PostgresRepository) evaluateManualReviewStopLoss(ctx context.Context, tx pgx.Tx, caseID uuid.UUID, severity string, now time.Time) error {
	var providerID, providerVersion, productID *string
	err := tx.QueryRow(ctx, `
WITH target_order AS (
    SELECT manual.id AS case_id,late.payment_order_id FROM manual_review_cases manual JOIN late_payment_review_cases late ON late.id=manual.id WHERE manual.id=$1
    UNION ALL
    SELECT manual.id,COALESCE(anomaly.payment_order_id,transaction.payment_order_id,refund.payment_order_id,forced.payment_order_id,chargeback.payment_order_id)
      FROM manual_review_cases manual JOIN payment_anomaly_cases anomaly ON anomaly.id=manual.id
      LEFT JOIN payment_transactions transaction ON transaction.id=anomaly.payment_transaction_id
      LEFT JOIN refunds refund ON refund.id=anomaly.refund_id
      LEFT JOIN forced_refund_cases forced ON forced.id=anomaly.forced_refund_case_id
      LEFT JOIN chargeback_cases chargeback ON chargeback.id=anomaly.chargeback_case_id
     WHERE manual.id=$1
    UNION ALL
    SELECT manual.id,reconciliation.payment_order_id FROM manual_review_cases manual JOIN reconciliation_cases reconciliation ON reconciliation.manual_review_case_id=manual.id WHERE manual.id=$1
)
SELECT payment.provider_id,payment.provider_version,payment.product_id
  FROM target_order target JOIN payment_orders payment ON payment.id=target.payment_order_id
 LIMIT 1`, caseID).Scan(&providerID, &providerVersion, &productID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	actions := map[string]any{"automatic": true, "provider_paused": false, "payment_product_paused": false, "route_pause_requested": true, "high_risk_command_pause_requested": true}
	if providerID != nil && providerVersion != nil {
		reason := "P0/P1 manual review mitigation SLA exceeded"
		if _, err = tx.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,updated_at) VALUES($1,$2,FALSE,$3,$4) ON CONFLICT(provider_id,provider_version) DO UPDATE SET new_orders_enabled=FALSE,reason=EXCLUDED.reason,version=payment_provider_controls.version+1,updated_at=EXCLUDED.updated_at`, *providerID, *providerVersion, reason, now); err != nil {
			return err
		}
		actions["provider_paused"] = true
	}
	if productID != nil {
		if _, err = tx.Exec(ctx, `UPDATE recharge_products SET enabled=FALSE WHERE product_id=$1 AND enabled`, *productID); err != nil {
			return err
		}
		actions["payment_product_paused"] = true
	}
	payload, _ := json.Marshal(actions)
	command, err := tx.Exec(ctx, `INSERT INTO manual_review_stop_loss_evaluations(id,case_id,severity,provider_id,provider_version,payment_product_id,route_reference,high_risk_command_type,action_snapshot,evaluated_at) VALUES($1,$2,$3,$4,$5,$6,'RELATED_MODEL_ROUTES','RELATED_HIGH_RISK_COMMANDS',$7,$8) ON CONFLICT(case_id) DO NOTHING`, uuid.Must(uuid.NewV7()), caseID, severity, providerID, providerVersion, productID, payload, now)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE manual_review_cases SET mitigation_at=COALESCE(mitigation_at,$2),stop_loss_evaluated_at=COALESCE(stop_loss_evaluated_at,$2),updated_at=$2 WHERE id=$1`, caseID, now); err != nil {
		return err
	}
	if err = appendManualReviewEvent(ctx, tx, caseID, "STOP_LOSS_EVALUATED", nil, actions, now); err != nil {
		return err
	}
	return events.AppendOutbox(ctx, tx, "billing.manual-review.stop-loss-evaluated.v1", caseID, 1, map[string]any{"case_id": caseID, "severity": severity, "provider_id": providerID, "provider_version": providerVersion, "payment_product_id": productID, "route_pause_requested": true, "high_risk_command_pause_requested": true})
}

func (r *PostgresRepository) EscalateArchiveBlindCases(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT late.id,late.provider_id,late.provider_version FROM late_payment_review_cases late JOIN payment_orders payment ON payment.id=late.payment_order_id WHERE late.review_status='REVIEW_REQUIRED_LATE_PAYMENT' AND payment.final_archive_eligible_at<=now() AND (payment.last_successful_query_at IS NULL OR payment.last_successful_query_at<payment.final_archive_eligible_at OR payment.last_authoritative_unpaid_at IS NULL OR payment.last_authoritative_unpaid_at<payment.final_archive_eligible_at) ORDER BY payment.final_archive_eligible_at,late.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id                          uuid.UUID
		providerID, providerVersion string
	}
	items := []candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.providerID, &item.providerVersion); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	rows.Close()
	count := 0
	for _, item := range items {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return count, err
		}
		now := r.now()
		reason := "archive deadline reached without a successful authoritative unpaid query"
		nextReviewAt := now.Add(24 * time.Hour)
		command, err := tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='PROVIDER_BLIND',blind_reason=$2,next_review_at=$3,updated_at=$4 WHERE id=$1 AND review_status='REVIEW_REQUIRED_LATE_PAYMENT'`, item.id, reason, nextReviewAt, now)
		if err == nil && command.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,updated_at) VALUES($1,$2,FALSE,$3,$4) ON CONFLICT(provider_id,provider_version) DO UPDATE SET new_orders_enabled=FALSE,reason=EXCLUDED.reason,version=payment_provider_controls.version+1,updated_at=EXCLUDED.updated_at`, item.providerID, item.providerVersion, reason, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			err = appendManualReviewEvent(ctx, tx, item.id, "PROVIDER_BLIND", nil, map[string]any{"reason": reason, "new_orders_enabled": false}, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			payload, _ := json.Marshal(map[string]any{"reason": reason, "new_orders_enabled": false})
			digest := sha256.Sum256(payload)
			_, err = tx.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'PROVIDER_BLIND',$3,$4,$5) ON CONFLICT(case_id,evidence_type,payload_digest) DO NOTHING`, uuid.Must(uuid.NewV7()), item.id, hex.EncodeToString(digest[:]), payload, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			err = events.AppendOutbox(ctx, tx, "billing.provider-blind.detected.v1", item.id, 1, map[string]any{"case_id": item.id, "provider_id": item.providerID, "provider_version": item.providerVersion, "reason": reason, "new_orders_enabled": false})
		}
		if err != nil {
			tx.Rollback(ctx)
			return count, err
		}
		if err := tx.Commit(ctx); err != nil {
			return count, err
		}
		if command.RowsAffected() == 1 {
			count++
		}
	}
	return count, nil
}

type ManualReviewSLAWorker struct {
	repository *PostgresRepository
	interval   time.Duration
	schedule   manualreview.OnCallSchedule
}

func NewManualReviewSLAWorker(repository *PostgresRepository, interval time.Duration, schedules ...manualreview.OnCallSchedule) *ManualReviewSLAWorker {
	if interval < time.Minute {
		interval = time.Minute
	}
	var schedule manualreview.OnCallSchedule
	if len(schedules) > 0 {
		schedule = schedules[0]
	}
	return &ManualReviewSLAWorker{repository: repository, interval: interval, schedule: schedule}
}

func (worker *ManualReviewSLAWorker) Run(ctx context.Context) {
	_, _ = worker.repository.EscalateOverdueManualReviews(ctx, 100, worker.schedule)
	_, _ = worker.repository.EscalateArchiveBlindCases(ctx, 100)
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = worker.repository.EscalateOverdueManualReviews(ctx, 100, worker.schedule)
			_, _ = worker.repository.EscalateArchiveBlindCases(ctx, 100)
		}
	}
}
