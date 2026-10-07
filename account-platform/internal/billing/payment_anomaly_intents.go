package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PreparePaymentAnomalyResolution binds the complete logical command before
// a separately committed financial action. An uncertain action keeps this
// immutable intent, so a competing key or decision cannot take its place.
func (r *PostgresRepository) PreparePaymentAnomalyResolution(ctx context.Context, caseID, actorID uuid.UUID, key, resolution, reason string) (PaymentAnomalyCase, bool, error) {
	key, resolution, reason = strings.TrimSpace(key), strings.ToUpper(strings.TrimSpace(resolution)), strings.TrimSpace(reason)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(key) < 8 || len(key) > 120 || len([]rune(reason)) < 10 {
		return PaymentAnomalyCase{}, false, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	defer tx.Rollback(ctx)
	if err = lockTransactionKey(ctx, tx, "payment-anomaly.resolution", caseID.String()); err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	if err = lockTransactionKey(ctx, tx, "payment-anomaly.command", caseID.String()+":"+key); err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	hash := paymentAnomalyCommandHash(resolution, reason)
	var savedHash string
	var savedActor uuid.UUID
	var snapshot json.RawMessage
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id,after_snapshot FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, key).Scan(&savedHash, &savedActor, &snapshot)
	if err == nil {
		if savedHash != hash || savedActor != actorID {
			return PaymentAnomalyCase{}, false, ErrConflict
		}
		var completed PaymentAnomalyCase
		if json.Unmarshal(snapshot, &completed) != nil || completed.ID != caseID || completed.Status != "RESOLVED" {
			return PaymentAnomalyCase{}, false, ErrConflict
		}
		return completed, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, false, err
	}
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual`, caseID))
	if err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	if !found {
		return PaymentAnomalyCase{}, false, ErrNotFound
	}
	if item.Status == "RESOLVED" || !validPaymentAnomalyResolution(item.AnomalyType, resolution, anomalyResolutionFinancialStatus(resolution)) {
		return PaymentAnomalyCase{}, false, ErrConflict
	}
	var savedKey string
	err = tx.QueryRow(ctx, `SELECT idempotency_key,request_hash,actor_id FROM payment_anomaly_resolution_intents WHERE payment_anomaly_case_id=$1`, caseID).Scan(&savedKey, &savedHash, &savedActor)
	if err == nil {
		if savedKey != key || savedHash != hash || savedActor != actorID {
			return PaymentAnomalyCase{}, false, ErrConflict
		}
		return item, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, false, err
	}
	snapshot, err = json.Marshal(item)
	if err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_anomaly_resolution_intents(payment_anomaly_case_id,actor_id,idempotency_key,request_hash,resolution,reason,target_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, caseID, actorID, key, hash, resolution, reason, snapshot, r.now())
	if err != nil {
		return PaymentAnomalyCase{}, false, err
	}
	return item, false, tx.Commit(ctx)
}

func anomalyResolutionFinancialStatus(resolution string) string {
	switch resolution {
	case "CONFIRM_CHARGEBACK", "CONFIRM_FORCED_REFUND", "CONFIRM_PAYMENT":
		return "APPLIED"
	case "ESCALATE_FINANCIAL_REPAIR":
		return "PENDING"
	default:
		return "NONE"
	}
}
