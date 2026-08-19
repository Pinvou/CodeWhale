package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type MallUserClient interface {
	CreateUser(context.Context, string) error
}

type MallSyncJob struct {
	UserID   uuid.UUID
	Mobile   string
	Attempts int
}

type MallSyncStore interface {
	ClaimMallSyncJob(context.Context, time.Duration) (*MallSyncJob, error)
	CompleteMallSyncJob(context.Context, uuid.UUID) error
	RetryMallSyncJob(context.Context, uuid.UUID, string, time.Time) error
}

type MallSyncWorker struct {
	store    MallSyncStore
	client   MallUserClient
	interval time.Duration
}

func NewMallSyncWorker(store MallSyncStore, client MallUserClient, interval time.Duration) *MallSyncWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &MallSyncWorker{store: store, client: client, interval: interval}
}

func (w *MallSyncWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.runOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *MallSyncWorker) runOne(ctx context.Context) {
	job, err := w.store.ClaimMallSyncJob(ctx, 30*time.Second)
	if err != nil || job == nil {
		return
	}
	if err := w.client.CreateUser(ctx, job.Mobile); err != nil {
		delay := time.Duration(1<<min(job.Attempts, 8)) * time.Minute
		_ = w.store.RetryMallSyncJob(context.WithoutCancel(ctx), job.UserID, fmt.Sprintf("mall_user_sync_failed: %T", err), time.Now().UTC().Add(delay))
		return
	}
	_ = w.store.CompleteMallSyncJob(context.WithoutCancel(ctx), job.UserID)
}

func (r *PostgresRepository) ClaimMallSyncJob(ctx context.Context, lease time.Duration) (*MallSyncJob, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var job MallSyncJob
	err = tx.QueryRow(ctx, `
SELECT user_id,mobile_e164,attempts FROM mall_user_sync_jobs
WHERE (status IN ('PENDING','RETRY') AND next_attempt_at<=$1) OR (status='LEASED' AND lease_until<$1)
ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`, r.now()).Scan(&job.UserID, &job.Mobile, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='LEASED',attempts=attempts+1,lease_until=$2,updated_at=$1 WHERE user_id=$3`, r.now(), r.now().Add(lease), job.UserID)
	if err != nil {
		return nil, err
	}
	job.Attempts++
	return &job, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteMallSyncJob(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='SYNCED',lease_until=NULL,last_error=NULL,synced_at=$2,updated_at=$2 WHERE user_id=$1 AND status='LEASED'`, userID, r.now())
	return err
}

func (r *PostgresRepository) RetryMallSyncJob(ctx context.Context, userID uuid.UUID, reason string, next time.Time) error {
	if len(reason) > 240 {
		reason = reason[:240]
	}
	_, err := r.pool.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='RETRY',lease_until=NULL,last_error=$2,next_attempt_at=$3,updated_at=$4 WHERE user_id=$1 AND status='LEASED'`, userID, reason, next, r.now())
	return err
}
