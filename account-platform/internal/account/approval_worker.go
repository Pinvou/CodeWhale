package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) ExpireApprovalRequests(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT id FROM approval_requests WHERE (status='SUBMITTED' AND expires_at<=now()) OR (status IN ('APPROVED','EXECUTION_FAILED') AND execution_deadline<=now()) ORDER BY COALESCE(execution_deadline,expires_at),id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	changed := 0
	for _, id := range ids {
		updated, err := r.expireApprovalRequest(ctx, id)
		if err != nil {
			return changed, err
		}
		if updated {
			changed++
		}
	}
	return changed, nil
}

func (r *PostgresRepository) expireApprovalRequest(ctx context.Context, id uuid.UUID) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var status string
	var expiresAt time.Time
	var executionDeadline *time.Time
	err = tx.QueryRow(ctx, `SELECT status,expires_at,execution_deadline FROM approval_requests WHERE id=$1 FOR UPDATE`, id).Scan(&status, &expiresAt, &executionDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := r.now()
	to, code := "", ""
	if status == "SUBMITTED" && !expiresAt.After(now) {
		to, code = "EXPIRED", "REQUEST_EXPIRED"
	} else if (status == "APPROVED" || status == "EXECUTION_FAILED") && executionDeadline != nil && !executionDeadline.After(now) {
		to, code = "INVALIDATED", "EXECUTION_WINDOW_EXPIRED"
	} else {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,invalidation_code=CASE WHEN $2='INVALIDATED' THEN $3 ELSE invalidation_code END,invalidation_details=CASE WHEN $2='INVALIDATED' THEN '{}' ELSE invalidation_details END,version=version+1,updated_at=$4 WHERE id=$1`, id, to, code, now); err != nil {
		return false, err
	}
	if err := appendApprovalStateEvent(ctx, tx, id, status, to, "SYSTEM", "approval-expiry-worker", code, map[string]any{}, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type ApprovalExpiryWorker struct {
	repository *PostgresRepository
	interval   time.Duration
}

func NewApprovalExpiryWorker(repository *PostgresRepository, interval time.Duration) *ApprovalExpiryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &ApprovalExpiryWorker{repository: repository, interval: interval}
}

func (worker *ApprovalExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		_, _ = worker.repository.ExpireApprovalRequests(ctx, 100)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
