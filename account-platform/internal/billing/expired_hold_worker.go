package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"time"
)

type ExpiredHoldRecoveryWorker struct {
	repository *PostgresRepository
	proof      ExpiredHoldProofClient
	interval   time.Duration
}

func NewExpiredHoldRecoveryWorker(r *PostgresRepository, proof ExpiredHoldProofClient, interval time.Duration) *ExpiredHoldRecoveryWorker {
	if interval < time.Minute {
		interval = time.Minute
	}
	return &ExpiredHoldRecoveryWorker{r, proof, interval}
}
func (w *ExpiredHoldRecoveryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := w.RunOnce(bounded)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "expired hold recovery failed", "component", "expired-hold-recovery", "error_code", "HOLD_RECOVERY_RETRY_PENDING")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (w *ExpiredHoldRecoveryWorker) RunOnce(ctx context.Context) error {
	rows, err := w.repository.pool.Query(ctx, `SELECT h.usage_request_id FROM usage_holds h LEFT JOIN usage_hold_expiry_recovery r ON r.hold_id=h.id WHERE h.expires_at<=clock_timestamp() AND ((h.status IN ('ACTIVE','REVIEW') AND (r.hold_id IS NULL OR (r.state='MANUAL_REVIEW' AND r.next_attempt_at<=clock_timestamp()))) OR (h.status='RELEASED' AND r.state='MANUAL_REVIEW' AND r.next_attempt_at<=clock_timestamp())) ORDER BY COALESCE(r.next_attempt_at,h.expires_at),h.id LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var first error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = w.recoverOne(bounded, id)
		cancel()
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}
func (w *ExpiredHoldRecoveryWorker) recoverOne(ctx context.Context, usageID uuid.UUID) error {
	conn, err := w.repository.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var claimed bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('hold.expiry.'||$1::text,0))`, usageID.String()).Scan(&claimed); err != nil || !claimed {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := conn.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended('hold.expiry.'||$1::text,0))`, usageID.String()); e != nil {
			conn.Conn().Close(context.Background())
		}
	}()
	hold, err := getHoldByUsage(ctx, conn, usageID, false)
	if err != nil {
		return err
	}
	if hold.ExpiresAt.After(w.repository.now()) || (hold.Status != "ACTIVE" && hold.Status != "REVIEW" && hold.Status != "RELEASED") {
		return nil
	}
	var state string
	var frozenIdentity []byte
	err = conn.QueryRow(ctx, `SELECT state,identity FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state, &frozenIdentity)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if state == "COMPLETED" {
		return nil
	}
	identity := ExpiredHoldIdentity{UsageRequestID: usageID, HoldID: hold.ID, WalletID: hold.WalletID, MaxMicrocredits: hold.MaxMicrocredits, ExpiresAt: hold.ExpiresAt}
	if state != "" {
		var saved ExpiredHoldIdentity
		if json.Unmarshal(frozenIdentity, &saved) != nil || !saved.Matches(identity) {
			return ErrConflict
		}
	}
	identityJSON, _ := json.Marshal(identity)
	// Persist recovery before any RPC can release funds. A lost response or
	// cancelled process must leave a candidate even after the hold is RELEASED.
	intent, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err = intent.Exec(ctx, `INSERT INTO usage_hold_expiry_recovery(hold_id,state,identity,next_attempt_at,updated_at) VALUES($1,'MANUAL_REVIEW',$2,clock_timestamp(),clock_timestamp()) ON CONFLICT(hold_id) DO NOTHING`, hold.ID, identityJSON); err != nil {
		intent.Rollback(context.Background())
		return err
	}
	if state == "" {
		if _, err = intent.Exec(ctx, `INSERT INTO usage_hold_expiry_evidence(id,hold_id,decision,occurred_at) VALUES($1,$2,'DISPATCH_UNPROVEN',clock_timestamp())`, uuid.Must(uuid.NewV7()), hold.ID); err != nil {
			intent.Rollback(context.Background())
			return err
		}
	}
	if err = intent.Commit(ctx); err != nil {
		return err
	}
	state = "MANUAL_REVIEW"
	var proof ExpiredHoldProof
	if w.proof != nil {
		proof, err = w.proof.FenceExpiredHold(ctx, identity)
	} else {
		err = errors.New("trusted hold proof unavailable")
	}
	valid := err == nil && proof.Matches(identity)
	current, readErr := getHoldByUsage(ctx, conn, usageID, false)
	if readErr != nil {
		return readErr
	}
	complete := valid && current.ID == hold.ID && current.Status == "RELEASED"
	decision, nextState := "DISPATCH_UNPROVEN", "MANUAL_REVIEW"
	if complete {
		decision, nextState = "CONFIRMED", "COMPLETED"
	}
	var raw any
	if valid {
		b, _ := json.Marshal(proof)
		raw = b
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET state=$2,proof=$3,next_attempt_at=clock_timestamp()+interval '1 minute',updated_at=clock_timestamp() WHERE hold_id=$1`, hold.ID, nextState, raw); err != nil {
		return err
	}
	if state != nextState {
		if _, err = tx.Exec(ctx, `INSERT INTO usage_hold_expiry_evidence(id,hold_id,decision,proof,occurred_at) VALUES($1,$2,$3,$4,clock_timestamp())`, uuid.Must(uuid.NewV7()), hold.ID, decision, raw); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
