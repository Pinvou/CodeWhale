package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Schedule replay returns this invocation's original receipt, never the current
// plan or another scheduling effect. Current ownership still applies before a
// historical receipt is disclosed; active/time bounds admit only new writes.
func (r *PostgresRepository) ScheduleLatePaymentReview(ctx context.Context, caseID, actorID uuid.UUID, nextReviewAt time.Time, reason, key string) (LatePaymentReviewCase, error) {
	reason, key = strings.TrimSpace(reason), strings.TrimSpace(key)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(reason) < 10 || len(reason) > 500 || len(key) < 8 || len(key) > 120 || nextReviewAt.IsZero() {
		return LatePaymentReviewCase{}, ErrConflict
	}
	// Freeze the complete caller instant, including sub-microsecond precision;
	// the returned receipt below is read back at PostgreSQL's actual precision.
	request, err := json.Marshal(struct {
		CaseID          uuid.UUID `json:"case_id"`
		ActorID         uuid.UUID `json:"actor_id"`
		CommandProtocol int       `json:"command_protocol"`
		NextReviewAt    time.Time `json:"next_review_at"`
		Reason          string    `json:"reason"`
	}{caseID, actorID, 1, nextReviewAt.UTC(), reason})
	if err != nil {
		return LatePaymentReviewCase{}, ErrConflict
	}
	digest := sha256.Sum256(request)
	hash := hex.EncodeToString(digest[:])
	var result LatePaymentReviewCase
	// Reuse the bounded financial transaction retry: only a definite PostgreSQL
	// serialization/deadlock rollback is retried; an unknown commit is not.
	err = reconciliationRetry(ctx, func() error {
		var attemptErr error
		result, attemptErr = r.scheduleLatePaymentReviewOnce(ctx, caseID, actorID, nextReviewAt, reason, key, hash, request)
		return attemptErr
	})
	return result, err
}

func (r *PostgresRepository) scheduleLatePaymentReviewOnce(ctx context.Context, caseID, actorID uuid.UUID, nextReviewAt time.Time, reason, key, hash string, request []byte) (LatePaymentReviewCase, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "late-payment-review.schedule", key); err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if !found || item.OwnerID == nil || *item.OwnerID != actorID {
		return LatePaymentReviewCase{}, ErrConflict
	}
	var savedHash string
	var savedActor, savedCase uuid.UUID
	var receipt []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id,case_id,after_snapshot FROM late_payment_review_schedule_commands WHERE idempotency_key=$1`, key).Scan(&savedHash, &savedActor, &savedCase, &receipt)
	if err == nil {
		if savedHash != hash || savedActor != actorID || savedCase != caseID {
			return LatePaymentReviewCase{}, ErrConflict
		}
		var original LatePaymentReviewCase
		if err := json.Unmarshal(receipt, &original); err != nil {
			return LatePaymentReviewCase{}, err
		}
		if original.ID != caseID || original.OwnerID == nil || *original.OwnerID != actorID {
			return LatePaymentReviewCase{}, ErrConflict
		}
		return original, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LatePaymentReviewCase{}, err
	}
	if item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" && item.ReviewStatus != "PROVIDER_BLIND" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	now := r.now()
	// These are admission bounds for a new schedule. An old committed request
	// may be recovered after its original scheduled time without another write.
	if !nextReviewAt.After(now) || nextReviewAt.After(now.Add(8*24*time.Hour)) {
		return LatePaymentReviewCase{}, ErrConflict
	}
	before, err := json.Marshal(item)
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET next_review_at=$2,updated_at=$3 WHERE id=$1`, caseID, nextReviewAt.UTC(), now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET first_response_at=COALESCE(first_response_at,$2),mitigation_at=COALESCE(mitigation_at,$2),updated_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, "REVIEW_SCHEDULED", &actorID, map[string]any{"next_review_at": nextReviewAt.UTC(), "reason": reason}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, found, err = scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return LatePaymentReviewCase{}, err
	}
	after, err := json.Marshal(item)
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO late_payment_review_schedule_commands(id,case_id,actor_id,idempotency_key,request_hash,request_snapshot,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), caseID, actorID, key, hash, json.RawMessage(request), json.RawMessage(before), json.RawMessage(after), now); err != nil {
		return LatePaymentReviewCase{}, err
	}
	return item, tx.Commit(ctx)
}
