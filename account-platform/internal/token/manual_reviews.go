package token

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type tokenManualReviewCandidate struct {
	caseType, resourceType, severity string
	resourceID                       uuid.UUID
	createdAt                        time.Time
}

type tokenManualReviewOverdue struct {
	id, resourceID            uuid.UUID
	severity, caseType, stage string
	ownerID                   *uuid.UUID
	stopLoss                  bool
}

func (r *PostgresRepository) SyncManualReviewCases(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT case_type,resource_type,resource_id,severity,created_at FROM (
 SELECT 'USAGE_RECONCILIATION_REVIEW' AS case_type,'USAGE_REQUEST' AS resource_type,id AS resource_id,
        CASE WHEN actual_microcredits>maximum_microcredits OR lower(COALESCE(error_class,'')) LIKE '%hold%' THEN 'P1' ELSE 'P2' END AS severity,
        started_at AS created_at
   FROM usage_requests WHERE status='REVIEW' AND (reconciliation_stopped_at IS NOT NULL OR error_class='cancel_hold_recovery_unconfirmed')
 UNION ALL
 SELECT 'ROUTE_PROVISIONING_REPAIR','ROUTE_PROVISIONING',id,'P2',created_at
   FROM route_provisionings WHERE status='REPAIR_REQUIRED'
) source
WHERE NOT EXISTS(SELECT 1 FROM manual_review_cases review WHERE review.domain='TOKEN' AND review.case_type=source.case_type AND review.resource_type=source.resource_type AND review.resource_id=source.resource_id)
ORDER BY created_at,resource_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	candidates := []tokenManualReviewCandidate{}
	for rows.Next() {
		var item tokenManualReviewCandidate
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
		command, err := tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'TOKEN',$2,$3,$4,$5,'OPEN',$6,$7,$8,$9,$10,$11) ON CONFLICT(domain,case_type,resource_type,resource_id) DO NOTHING`, caseID, candidate.caseType, candidate.resourceType, candidate.resourceID, candidate.severity, candidate.createdAt, deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution, now)
		if err == nil && command.RowsAffected() == 1 {
			details, _ := json.Marshal(map[string]any{"severity": candidate.severity, "source_created_at": candidate.createdAt})
			_, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'CREATED',$3,$4)`, uuid.Must(uuid.NewV7()), caseID, details, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			err = events.AppendOutbox(ctx, tx, "token.manual-review.created.v1", caseID, 1, map[string]any{"case_id": caseID, "case_type": candidate.caseType, "resource_type": candidate.resourceType, "resource_id": candidate.resourceID, "severity": candidate.severity})
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
	_, err = r.pool.Exec(ctx, `UPDATE manual_review_cases review SET status='RESOLVED',resolved_at=now(),updated_at=now() WHERE review.domain='TOKEN' AND review.status='OPEN' AND (
 (review.case_type='USAGE_RECONCILIATION_REVIEW' AND EXISTS(SELECT 1 FROM usage_requests source WHERE source.id=review.resource_id AND source.status IN ('COMMITTED','RELEASED'))) OR
 (review.case_type='ROUTE_PROVISIONING_REPAIR' AND EXISTS(SELECT 1 FROM route_provisionings source WHERE source.id=review.resource_id AND source.status='READY'))
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
	items := []tokenManualReviewOverdue{}
	for rows.Next() {
		var item tokenManualReviewOverdue
		if err := rows.Scan(&item.id, &item.severity, &item.ownerID, &item.caseType, &item.resourceID, &item.stage, &item.stopLoss); err != nil {
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
		var level int
		err = tx.QueryRow(ctx, `UPDATE manual_review_cases SET escalation_level=escalation_level+1,last_escalated_at=$2,updated_at=$2 WHERE id=$1 AND status='OPEN' RETURNING escalation_level`, item.id, now).Scan(&level)
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
			err = r.evaluateTokenStopLoss(ctx, tx, item, now)
		}
		if err == nil {
			err = events.AppendOutbox(ctx, tx, "token.manual-review.sla-escalated.v1", item.id, int64(level), map[string]any{"case_id": item.id, "severity": item.severity, "stage": item.stage, "owner_id": map[bool]any{true: owner, false: nil}[assigned]})
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

func (r *PostgresRepository) evaluateTokenStopLoss(ctx context.Context, tx pgx.Tx, item tokenManualReviewOverdue, now time.Time) error {
	if item.caseType != "USAGE_RECONCILIATION_REVIEW" {
		return nil
	}
	var tokenAccountID uuid.UUID
	var model string
	if err := tx.QueryRow(ctx, `SELECT token_account_id,model FROM usage_requests WHERE id=$1`, item.resourceID).Scan(&tokenAccountID, &model); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE routing_policies SET health_status='UNHEALTHY',health_diagnostic_code='MANUAL_REVIEW_REQUIRED',health_observed_at=$3,health_expires_at=NULL,next_probe_at=$3,health_version=health_version+1,updated_at=$3 WHERE token_account_id=$1 AND (model=$2 OR model_id::text=$2) AND health_status<>'UNHEALTHY'`, tokenAccountID, model, now)
	if err != nil {
		return err
	}
	actions := map[string]any{"route_paused": true, "affected_route_count": command.RowsAffected(), "token_account_id": tokenAccountID, "model": model, "automatic_usage_retry_disabled": true}
	payload, _ := json.Marshal(actions)
	insert, err := tx.Exec(ctx, `INSERT INTO manual_review_stop_loss_evaluations(id,case_id,severity,route_reference,high_risk_command_type,action_snapshot,evaluated_at) VALUES($1,$2,$3,$4,'TOKEN_USAGE_RECONCILIATION_RETRY',$5,$6) ON CONFLICT(case_id) DO NOTHING`, uuid.Must(uuid.NewV7()), item.id, item.severity, tokenAccountID.String()+":"+model, payload, now)
	if err != nil || insert.RowsAffected() == 0 {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE manual_review_cases SET mitigation_at=COALESCE(mitigation_at,$2),stop_loss_evaluated_at=COALESCE(stop_loss_evaluated_at,$2),updated_at=$2 WHERE id=$1`, item.id, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'STOP_LOSS_EVALUATED',$3,$4)`, uuid.Must(uuid.NewV7()), item.id, payload, now); err != nil {
		return err
	}
	return events.AppendOutbox(ctx, tx, "token.manual-review.stop-loss-evaluated.v1", item.id, 1, map[string]any{"case_id": item.id, "severity": item.severity, "route_reference": tokenAccountID.String() + ":" + model, "route_paused": true})
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
