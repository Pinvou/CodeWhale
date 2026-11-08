package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// PurgeExpiredPaymentCreditAttempts keeps deletion and content-free proof in one statement.
// The proof table has its own bounded expiry; idle ticks create no audit rows.
func (r *PostgresRepository) PurgeExpiredPaymentCreditAttempts(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `WITH candidates AS MATERIALIZED(
        SELECT id FROM payment_credit_attempts
        WHERE state<>'PREPARED' AND account_acknowledged_at IS NOT NULL AND legal_hold=false AND retained_until<=clock_timestamp()
        ORDER BY retained_until,id LIMIT 100 FOR UPDATE SKIP LOCKED
    ), removed AS (
        DELETE FROM payment_credit_attempts record USING candidates candidate WHERE record.id=candidate.id RETURNING record.retained_until
    ), proof_candidates AS MATERIALIZED(
        SELECT id FROM payment_credit_retention_batches WHERE legal_hold=false AND retained_until<=clock_timestamp()
        ORDER BY retained_until,id LIMIT 100 FOR UPDATE SKIP LOCKED
    ), removed_proofs AS (
        DELETE FROM payment_credit_retention_batches proof USING proof_candidates candidate WHERE proof.id=candidate.id RETURNING proof.completed_at
    ), cleared_times AS (
        SELECT retained_until AS at FROM removed UNION ALL SELECT completed_at FROM removed_proofs
    ), batch AS (
        SELECT (SELECT count(*) FROM removed) AS records,(SELECT count(*) FROM removed_proofs) AS proofs,
               min(at) AS first_at,max(at) AS last_at FROM cleared_times
    ), completed AS MATERIALIZED(SELECT clock_timestamp() AS at)
    INSERT INTO payment_credit_retention_batches(id,policy_version,purged_record_count,purged_batch_count,range_start,range_end,result,completed_at,retained_until)
    SELECT $1,1,batch.records,batch.proofs,batch.first_at,batch.last_at,'PURGED',completed.at,completed.at+interval '3 years'
    FROM batch CROSS JOIN completed WHERE batch.records+batch.proofs>0`, uuid.New())
	return err
}

func (r *PostgresRepository) RunPaymentCreditRetention(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := r.PurgeExpiredPaymentCreditAttempts(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "payment credit retention pending", "error_code", "PAYMENT_CREDIT_RETENTION_PENDING")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
