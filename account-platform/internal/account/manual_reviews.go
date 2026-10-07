package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type accountManualReviewCandidate struct {
	caseType, resourceType, severity string
	resourceID                       uuid.UUID
	createdAt                        time.Time
}

func (r *PostgresRepository) SyncManualReviewCases(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT case_type,resource_type,resource_id,severity,created_at FROM (
 SELECT 'REGISTRATION_REPAIR' AS case_type,'REGISTRATION_OPERATION' AS resource_type,operation.id AS resource_id,'P2' AS severity,operation.created_at
   FROM registration_operations operation LEFT JOIN wallet_provisioning_jobs wallet ON wallet.registration_operation_id=operation.id
  WHERE operation.status IN ('REPAIR_REQUIRED','COMPENSATION_PENDING') OR wallet.status='REPAIR_REQUIRED'
 UNION ALL
 SELECT 'PROFILE_CHANGE_REPAIR','PROFILE_CHANGE_REQUEST',id,'P2',created_at FROM profile_change_requests WHERE status='REPAIR_REQUIRED'
 UNION ALL
 SELECT 'HIGH_RISK_COMMAND_REPAIR','APPROVAL_REQUEST',id,'P1',created_at FROM approval_requests WHERE status='EXECUTION_FAILED'
) source
WHERE NOT EXISTS(SELECT 1 FROM manual_review_cases review WHERE review.domain='ACCOUNT' AND review.case_type=source.case_type AND review.resource_type=source.resource_type AND review.resource_id=source.resource_id)
ORDER BY created_at,resource_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	candidates := []accountManualReviewCandidate{}
	for rows.Next() {
		var item accountManualReviewCandidate
		if err := rows.Scan(&item.caseType, &item.resourceType, &item.resourceID, &item.severity, &item.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	created := 0
	for _, candidate := range candidates {
		deadlines, err := manualreview.DeadlinesFor(candidate.createdAt, candidate.severity)
		if err != nil {
			return created, err
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return created, err
		}
		now := r.now()
		caseID := uuid.Must(uuid.NewV7())
		command, err := tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'ACCOUNT',$2,$3,$4,$5,'OPEN',$6,$7,$8,$9,$10,$11) ON CONFLICT(domain,case_type,resource_type,resource_id) DO NOTHING`, caseID, candidate.caseType, candidate.resourceType, candidate.resourceID, candidate.severity, candidate.createdAt, deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution, now)
		if err == nil && command.RowsAffected() == 1 {
			details, _ := json.Marshal(map[string]any{"severity": candidate.severity, "source_created_at": candidate.createdAt})
			_, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'CREATED',$3,$4)`, uuid.Must(uuid.NewV7()), caseID, details, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			err = events.AppendOutbox(ctx, tx, "account.manual-review.created.v1", caseID, 1, map[string]any{"case_id": caseID, "case_type": candidate.caseType, "resource_type": candidate.resourceType, "resource_id": candidate.resourceID, "severity": candidate.severity})
		}
		if err != nil {
			tx.Rollback(ctx)
			return created, err
		}
		if err := tx.Commit(ctx); err != nil {
			return created, err
		}
		if command.RowsAffected() == 1 {
			created++
		}
	}
	_, err = r.pool.Exec(ctx, `UPDATE manual_review_cases review SET status='RESOLVED',resolved_at=now(),updated_at=now() WHERE review.domain='ACCOUNT' AND review.status='OPEN' AND (
 (review.case_type='REGISTRATION_REPAIR' AND EXISTS(SELECT 1 FROM registration_operations source WHERE source.id=review.resource_id AND source.status IN ('ACTIVE','COMPENSATED','EXPIRED'))) OR
 (review.case_type='PROFILE_CHANGE_REPAIR' AND EXISTS(SELECT 1 FROM profile_change_requests source WHERE source.id=review.resource_id AND source.status IN ('COMPLETED'))) OR
 (review.case_type='HIGH_RISK_COMMAND_REPAIR' AND EXISTS(SELECT 1 FROM approval_requests source WHERE source.id=review.resource_id AND source.status IN ('EXECUTED','INVALIDATED','EXPIRED','REJECTED','WITHDRAWN')))
)`)
	return created, err
}

func (r *PostgresRepository) EscalateOverdueManualReviews(ctx context.Context, schedule manualreview.OnCallSchedule, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT id,severity,owner_id,case_type,resource_id,CASE WHEN resolution_due_at<=now() THEN 'RESOLUTION_OVERDUE' WHEN mitigation_at IS NULL AND mitigation_due_at<=now() THEN 'MITIGATION_OVERDUE' WHEN first_response_at IS NULL AND first_response_due_at<=now() THEN 'FIRST_RESPONSE_OVERDUE' WHEN claimed_at IS NULL AND claim_due_at<=now() THEN 'CLAIM_OVERDUE' END,(severity IN ('P0','P1') AND mitigation_at IS NULL AND mitigation_due_at<=now()) FROM manual_review_cases WHERE status='OPEN' AND ((claimed_at IS NULL AND claim_due_at<=now()) OR (first_response_at IS NULL AND first_response_due_at<=now()) OR (mitigation_at IS NULL AND mitigation_due_at<=now()) OR resolution_due_at<=now()) AND (last_escalated_at IS NULL OR last_escalated_at<now()-interval '1 hour') ORDER BY CASE severity WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 ELSE 2 END,created_at,id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type overdue struct {
		id, resourceID            uuid.UUID
		severity, caseType, stage string
		ownerID                   *uuid.UUID
		stopLoss                  bool
	}
	items := []overdue{}
	for rows.Next() {
		var item overdue
		if err := rows.Scan(&item.id, &item.severity, &item.ownerID, &item.caseType, &item.resourceID, &item.stage, &item.stopLoss); err != nil {
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
		var level int
		// Recheck under the row lock: two workers may have selected the same
		// overdue case, or its deadlines/response facts may have changed since
		// selection. Only the winning update may append escalation evidence.
		err = tx.QueryRow(ctx, `UPDATE manual_review_cases SET escalation_level=escalation_level+1,last_escalated_at=$2,updated_at=$2
WHERE id=$1 AND status='OPEN' AND (last_escalated_at IS NULL OR last_escalated_at<now()-interval '1 hour')
 AND ((claimed_at IS NULL AND claim_due_at<=now()) OR (first_response_at IS NULL AND first_response_due_at<=now()) OR (mitigation_at IS NULL AND mitigation_due_at<=now()) OR resolution_due_at<=now())
RETURNING escalation_level,owner_id,severity,
 CASE WHEN resolution_due_at<=now() THEN 'RESOLUTION_OVERDUE' WHEN mitigation_at IS NULL AND mitigation_due_at<=now() THEN 'MITIGATION_OVERDUE' WHEN first_response_at IS NULL AND first_response_due_at<=now() THEN 'FIRST_RESPONSE_OVERDUE' WHEN claimed_at IS NULL AND claim_due_at<=now() THEN 'CLAIM_OVERDUE' END,
 (severity IN ('P0','P1') AND mitigation_at IS NULL AND mitigation_due_at<=now())`, item.id, now).Scan(&level, &item.ownerID, &item.severity, &item.stage, &item.stopLoss)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			continue
		}
		owner, assigned := schedule.Next(level - 1)
		if err == nil && assigned {
			_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,last_assigned_at=$3,assignment_revision=assignment_revision+1,updated_at=$3 WHERE id=$1`, item.id, owner, now)
		}
		if err == nil && assigned {
			_, err = tx.Exec(ctx, `INSERT INTO manual_review_assignments(id,case_id,from_owner_id,to_owner_id,escalation_level,reason,assigned_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(case_id,escalation_level) DO NOTHING`, uuid.Must(uuid.NewV7()), item.id, item.ownerID, owner, level, item.stage, now)
		}
		if err == nil {
			details, _ := json.Marshal(map[string]any{"severity": item.severity, "stage": item.stage, "escalation_level": level, "owner_id": map[bool]any{true: owner, false: nil}[assigned]})
			_, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'SLA_ESCALATED',$3,$4)`, uuid.Must(uuid.NewV7()), item.id, details, now)
		}
		if err == nil && item.stopLoss {
			err = evaluateAccountStopLoss(ctx, tx, item, now)
		}
		if err == nil {
			err = events.AppendOutbox(ctx, tx, "account.manual-review.sla-escalated.v1", item.id, int64(level), map[string]any{"case_id": item.id, "severity": item.severity, "stage": item.stage, "owner_id": map[bool]any{true: owner, false: nil}[assigned]})
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

func evaluateAccountStopLoss(ctx context.Context, tx pgx.Tx, item struct {
	id, resourceID            uuid.UUID
	severity, caseType, stage string
	ownerID                   *uuid.UUID
	stopLoss                  bool
}, now time.Time) error {
	if item.caseType != "HIGH_RISK_COMMAND_REPAIR" {
		return nil
	}
	var commandType string
	if err := tx.QueryRow(ctx, `SELECT command_type FROM approval_requests WHERE id=$1`, item.resourceID).Scan(&commandType); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	actions := map[string]any{"automatic_execution_paused": true, "manual_reauthorization_required": true}
	payload, _ := json.Marshal(actions)
	command, err := tx.Exec(ctx, `INSERT INTO manual_review_stop_loss_evaluations(id,case_id,severity,high_risk_command_type,action_snapshot,evaluated_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(case_id) DO NOTHING`, uuid.Must(uuid.NewV7()), item.id, item.severity, commandType, payload, now)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE manual_review_cases SET mitigation_at=COALESCE(mitigation_at,$2),stop_loss_evaluated_at=COALESCE(stop_loss_evaluated_at,$2),updated_at=$2 WHERE id=$1`, item.id, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'STOP_LOSS_EVALUATED',$3,$4)`, uuid.Must(uuid.NewV7()), item.id, payload, now); err != nil {
		return err
	}
	return events.AppendOutbox(ctx, tx, "account.manual-review.stop-loss-evaluated.v1", item.id, 1, map[string]any{"case_id": item.id, "severity": item.severity, "high_risk_command_type": commandType, "automatic_execution_paused": true})
}

type ManualReviewSLAWorker struct {
	repository *PostgresRepository
	schedule   manualreview.OnCallSchedule
	interval   time.Duration
}

func NewManualReviewSLAWorker(repository *PostgresRepository, schedule manualreview.OnCallSchedule, interval time.Duration) *ManualReviewSLAWorker {
	if interval < time.Minute {
		interval = time.Minute
	}
	return &ManualReviewSLAWorker{repository: repository, schedule: schedule, interval: interval}
}

func (worker *ManualReviewSLAWorker) Run(ctx context.Context) {
	run := func() {
		_, _ = worker.repository.SyncManualReviewCases(ctx, 100)
		_, _ = worker.repository.EscalateOverdueManualReviews(ctx, worker.schedule, 100)
	}
	run()
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
