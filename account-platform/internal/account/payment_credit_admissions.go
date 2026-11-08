package account

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AdmitPaymentCredit serializes admission with the actual cleanup transition.
// Updating user.version invalidates claims captured before domain assessment.
// No Account transaction or subject lock crosses a Billing RPC.
func (r *PostgresRepository) AdmitPaymentCredit(ctx context.Context, input paymentcredit.Request) (paymentcredit.Decision, error) {
	decision := paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: "SUBJECT_UNAVAILABLE"}
	if !input.Valid() {
		return paymentcredit.Decision{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return paymentcredit.Decision{}, err
	}
	defer tx.Rollback(ctx)
	var status string
	var anonymized bool
	err = tx.QueryRow(ctx, `SELECT status,anonymized_at IS NOT NULL FROM users WHERE id=$1 FOR UPDATE`, input.UserID).Scan(&status, &anonymized)
	if errors.Is(err, pgx.ErrNoRows) {
		return decision, nil
	}
	if err != nil {
		return paymentcredit.Decision{}, err
	}
	var fingerprint, state string
	err = tx.QueryRow(ctx, `SELECT fingerprint,state FROM account_payment_credit_admissions WHERE id=$1`, input.ID).Scan(&fingerprint, &state)
	if err == nil {
		if fingerprint != input.Fingerprint() {
			return paymentcredit.Decision{}, ErrConflict
		}
		decision.State = state
		if state == "PENDING" {
			decision.State = "ADMITTED"
		}
		return decision, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return paymentcredit.Decision{}, err
	}
	var cleanup bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_closure_requests WHERE user_id=$1 AND stage IN ('IDENTITY_CLEANUP','ANONYMIZED','COMPLETED'))`, input.UserID).Scan(&cleanup); err != nil {
		return paymentcredit.Decision{}, err
	}
	if anonymized || status == "DELETED" || cleanup {
		decision.State = "CLOSED"
		return decision, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO account_payment_credit_admissions(id,user_id,payment_order_id,transaction_digest,facts_digest,fingerprint) VALUES($1,$2,$3,$4,$5,$6)`, input.ID, input.UserID, input.OrderID, input.TransactionDigest, input.FactsDigest, input.Fingerprint()); err != nil {
		return paymentcredit.Decision{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET version=version+1,updated_at=clock_timestamp() WHERE id=$1`, input.UserID); err != nil {
		return paymentcredit.Decision{}, err
	}
	decision.State = "ADMITTED"
	return decision, tx.Commit(ctx)
}

// ReconcilePaymentCredits asks Billing only outside Account transactions. The
// resolver must establish APPLIED or a retained ABORTED tombstone under the
// original order lock; an unavailable/uncertain response only schedules retry.
func (r *PostgresRepository) ReconcilePaymentCredits(ctx context.Context, resolver paymentcredit.Resolver) error {
	if resolver == nil {
		return paymentcredit.ErrProtocol
	}
	rows, err := r.pool.Query(ctx, `SELECT id,user_id,payment_order_id,transaction_digest,facts_digest,state FROM account_payment_credit_admissions WHERE billing_acknowledged_at IS NULL AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,id LIMIT 100`)
	if err != nil {
		return err
	}
	type recoveryItem struct {
		input paymentcredit.Request
		state string
	}
	var inputs []recoveryItem
	for rows.Next() {
		var item recoveryItem
		input := &item.input
		if err = rows.Scan(&input.ID, &input.UserID, &input.OrderID, &input.TransactionDigest, &input.FactsDigest, &item.state); err != nil {
			rows.Close()
			return err
		}
		inputs = append(inputs, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range inputs {
		input, state := item.input, item.state
		if state == "PENDING" {
			decision, resolveErr := resolver.Resolve(ctx, input)
			if resolveErr != nil || !decision.Matches(input) || (decision.State != "APPLIED" && decision.State != "ABORTED") {
				if err = r.retryPaymentCredit(ctx, input); err != nil {
					return err
				}
				continue
			}
			if err = r.resolvePaymentCredit(ctx, input, decision); err != nil {
				return err
			}
			state = decision.State
		}
		// Tell Billing only after Account durably knows the financial outcome.
		// Until both acknowledgements persist, automatic retention keeps a hold.
		ack, ackErr := resolver.Acknowledge(ctx, input, state)
		if ackErr != nil || !ack.Matches(input) || ack.State != state {
			if err = r.retryPaymentCredit(ctx, input); err != nil {
				return err
			}
			continue
		}
		if _, err = r.pool.Exec(ctx, `UPDATE account_payment_credit_admissions SET billing_acknowledged_at=COALESCE(billing_acknowledged_at,clock_timestamp()) WHERE id=$1 AND fingerprint=$2 AND state=$3`, input.ID, input.Fingerprint(), state); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) retryPaymentCredit(ctx context.Context, input paymentcredit.Request) error {
	// Persist backoff so a failed oldest operation cannot starve subsequent batches.
	_, err := r.pool.Exec(ctx, `UPDATE account_payment_credit_admissions SET attempts=LEAST(attempts+1,30),next_attempt_at=clock_timestamp()+make_interval(secs=>LEAST(300,5*power(2,LEAST(attempts,6)))::double precision) WHERE id=$1 AND fingerprint=$2 AND billing_acknowledged_at IS NULL`, input.ID, input.Fingerprint())
	return err
}

func (r *PostgresRepository) resolvePaymentCredit(ctx context.Context, input paymentcredit.Request, decision paymentcredit.Decision) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Same subject-first order as admission; resolver network call finished.
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR UPDATE`, input.UserID).Scan(&locked); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE account_payment_credit_admissions SET state=$3,resolved_at=clock_timestamp() WHERE id=$1 AND fingerprint=$2 AND state='PENDING'`, input.ID, input.Fingerprint(), decision.State)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		// Even a quickly resolved effect must invalidate stale Ready assessments.
		if _, err = tx.Exec(ctx, `UPDATE users SET version=version+1,updated_at=clock_timestamp() WHERE id=$1`, input.UserID); err != nil {
			return err
		}
	} else {
		var stored string
		if err = tx.QueryRow(ctx, `SELECT state FROM account_payment_credit_admissions WHERE id=$1 AND fingerprint=$2`, input.ID, input.Fingerprint()).Scan(&stored); err != nil {
			return err
		}
		if stored != decision.State {
			return paymentcredit.ErrProtocol
		}
	}
	return tx.Commit(ctx)
}

// RunPaymentCreditRecovery remains separate from closure claims: admitted
// credits must converge even when their user never requested closure.
func (r *PostgresRepository) RunPaymentCreditRecovery(ctx context.Context, resolver paymentcredit.Resolver) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := r.PurgeExpiredPaymentCreditAdmissions(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "payment credit retention pending", "error_code", "PAYMENT_CREDIT_RETENTION_PENDING")
		}
		if err := r.ReconcilePaymentCredits(ctx, resolver); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "payment credit recovery pending", "error_code", "PAYMENT_CREDIT_RECOVERY_PENDING")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PurgeExpiredPaymentCreditAdmissions keeps deletion and content-free proof in one statement.
// The proof table has its own bounded expiry; idle ticks create no audit rows.
func (r *PostgresRepository) PurgeExpiredPaymentCreditAdmissions(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `WITH candidates AS MATERIALIZED(
        SELECT id FROM account_payment_credit_admissions
        WHERE state<>'PENDING' AND billing_acknowledged_at IS NOT NULL AND legal_hold=false AND retained_until<=clock_timestamp()
        ORDER BY retained_until,id LIMIT 100 FOR UPDATE SKIP LOCKED
    ), removed AS (
        DELETE FROM account_payment_credit_admissions record USING candidates candidate WHERE record.id=candidate.id RETURNING record.retained_until
    ), proof_candidates AS MATERIALIZED(
        SELECT id FROM account_payment_credit_retention_batches WHERE legal_hold=false AND retained_until<=clock_timestamp()
        ORDER BY retained_until,id LIMIT 100 FOR UPDATE SKIP LOCKED
    ), removed_proofs AS (
        DELETE FROM account_payment_credit_retention_batches proof USING proof_candidates candidate WHERE proof.id=candidate.id RETURNING proof.completed_at
    ), cleared_times AS (
        SELECT retained_until AS at FROM removed UNION ALL SELECT completed_at FROM removed_proofs
    ), batch AS (
        SELECT (SELECT count(*) FROM removed) AS records,(SELECT count(*) FROM removed_proofs) AS proofs,
               min(at) AS first_at,max(at) AS last_at FROM cleared_times
    ), completed AS MATERIALIZED(SELECT clock_timestamp() AS at)
    INSERT INTO account_payment_credit_retention_batches(id,policy_version,purged_record_count,purged_batch_count,range_start,range_end,result,completed_at,retained_until)
    SELECT $1,1,batch.records,batch.proofs,batch.first_at,batch.last_at,'PURGED',completed.at,completed.at+interval '3 years'
    FROM batch CROSS JOIN completed WHERE batch.records+batch.proofs>0`, uuid.New())
	return err
}
