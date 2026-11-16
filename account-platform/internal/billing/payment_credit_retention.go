package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"

	"github.com/google/uuid"
)

// PurgeExpiredPaymentCreditAttempts keeps deletion and content-free proof in one statement.
// The proof table has its own bounded expiry; idle ticks create no audit rows.
func (r *PostgresRepository) PurgeExpiredPaymentCreditAttempts(ctx context.Context, authority paymentcredit.RetentionAuthority) error {
	// This short autocommit read skips existing locks. Exhaust/close it before
	// any RPC; no Billing transaction or row lock crosses the Account call.
	rows, err := r.pool.Query(ctx, `SELECT id,payer_user_id,payment_order_id,transaction_digest,facts_digest FROM payment_credit_attempts WHERE state<>'PREPARED' AND account_acknowledged_at IS NOT NULL AND legal_hold=false AND retained_until<=clock_timestamp() AND next_retention_check_at<=clock_timestamp() ORDER BY next_retention_check_at,retained_until,id LIMIT 100 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var inputs []paymentcredit.Request
	for rows.Next() {
		var input paymentcredit.Request
		if err = rows.Scan(&input.ID, &input.UserID, &input.OrderID, &input.TransactionDigest, &input.FactsDigest); err != nil {
			rows.Close()
			return err
		}
		inputs = append(inputs, input)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	allowed := []uuid.UUID{}
	var dependencyErr error
	for _, input := range inputs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		decision := paymentcredit.Decision{}
		checkErr := paymentcredit.ErrProtocol
		if authority != nil {
			decision, checkErr = authority.CheckPaymentCreditRetention(ctx, input)
		}
		if checkErr == nil && (!decision.Matches(input) || (decision.State != "RETENTION_RELEASE" && decision.State != "RETENTION_RETAIN")) {
			checkErr = paymentcredit.ErrProtocol
		}
		if checkErr == nil && decision.State == "RETENTION_RELEASE" {
			allowed = append(allowed, input.ID)
			continue
		}
		// A lost final ACK remains recoverable even after three years. Durable
		// due-times keep a waiting old batch from starving later eligible rows.
		if checkErr != nil && dependencyErr == nil {
			dependencyErr = checkErr
		}
		if _, err = r.pool.Exec(ctx, `UPDATE payment_credit_attempts SET next_retention_check_at=clock_timestamp()+interval '5 minutes' WHERE id=$1 AND fingerprint=$2 AND state<>'PREPARED' AND account_acknowledged_at IS NOT NULL AND retained_until<=clock_timestamp()`, input.ID, input.Fingerprint()); err != nil {
			return err
		}
	}
	_, err = r.pool.Exec(ctx, `WITH candidates AS MATERIALIZED(
        SELECT id FROM payment_credit_attempts
        WHERE id=ANY($2::uuid[]) AND state<>'PREPARED' AND account_acknowledged_at IS NOT NULL AND legal_hold=false AND retained_until<=clock_timestamp() AND next_retention_check_at<=clock_timestamp()
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
    FROM batch CROSS JOIN completed WHERE batch.records+batch.proofs>0`, uuid.New(), allowed)
	if err != nil {
		return err
	}
	return dependencyErr
}

func (r *PostgresRepository) RunPaymentCreditRetention(ctx context.Context, authority paymentcredit.RetentionAuthority) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := r.PurgeExpiredPaymentCreditAttempts(ctx, authority); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "payment credit retention pending", "error_code", "PAYMENT_CREDIT_RETENTION_PENDING")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
