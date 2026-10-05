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
	UserID     uuid.UUID
	Mobile     string
	Attempts   int
	LeaseUntil time.Time
}

type MallSyncStore interface {
	ClaimMallSyncJob(context.Context, time.Duration) (*MallSyncJob, error)
	CompleteMallSyncJob(context.Context, MallSyncJob) error
	RetryMallSyncJob(context.Context, MallSyncJob, string, time.Time) error
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
	if !job.LeaseUntil.After(time.Now()) {
		return
	}
	jobCtx, cancel := context.WithDeadline(ctx, job.LeaseUntil)
	defer cancel()
	if err := w.client.CreateUser(jobCtx, job.Mobile); err != nil {
		delay := time.Duration(1<<min(job.Attempts, 8)) * time.Minute
		_ = w.store.RetryMallSyncJob(jobCtx, *job, fmt.Sprintf("mall_user_sync_failed: %T", err), time.Now().UTC().Add(delay))
		return
	}
	_ = w.store.CompleteMallSyncJob(jobCtx, *job)
}

func (r *PostgresRepository) ClaimMallSyncJob(ctx context.Context, lease time.Duration) (*MallSyncJob, error) {
	if lease <= 0 || lease > 10*time.Minute {
		return nil, ErrConflict
	}
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
	now := r.now()
	job.LeaseUntil = now.Add(lease)
	_, err = tx.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='LEASED',attempts=attempts+1,lease_until=$2,updated_at=$1 WHERE user_id=$3`, now, job.LeaseUntil, job.UserID)
	if err != nil {
		return nil, err
	}
	job.Attempts++
	return &job, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteMallSyncJob(ctx context.Context, job MallSyncJob) error {
	command, err := r.pool.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='SYNCED',lease_until=NULL,last_error=NULL,synced_at=$2,updated_at=$2 WHERE user_id=$1 AND status='LEASED' AND lease_until=$3 AND attempts=$4 AND lease_until>$2`, job.UserID, r.now(), job.LeaseUntil, job.Attempts)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) RetryMallSyncJob(ctx context.Context, job MallSyncJob, reason string, next time.Time) error {
	if len(reason) > 240 {
		reason = reason[:240]
	}
	command, err := r.pool.Exec(ctx, `UPDATE mall_user_sync_jobs SET status='RETRY',lease_until=NULL,last_error=$2,next_attempt_at=$3,updated_at=$4 WHERE user_id=$1 AND status='LEASED' AND lease_until=$5 AND attempts=$6 AND lease_until>$4`, job.UserID, reason, next, r.now(), job.LeaseUntil, job.Attempts)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
