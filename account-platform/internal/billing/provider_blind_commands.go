package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The existing evidence row is immutable. Only new human commands carry this
// versioned receipt; an older reason-only row cannot prove any command key.
type providerBlindCommandReceipt struct {
	Version         int                   `json:"version"`
	CommandProtocol int                   `json:"command_protocol"`
	IdempotencyKey  string                `json:"idempotency_key"`
	RequestHash     string                `json:"request_hash"`
	ActorID         uuid.UUID             `json:"actor_id"`
	CaseID          uuid.UUID             `json:"case_id"`
	Reason          string                `json:"reason"`
	Response        LatePaymentReviewCase `json:"response"`
}

func providerBlindCommandWriteError(err error) error {
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "23505" && databaseError.ConstraintName == "late_payment_provider_blind_command_key" {
		return ErrConflict
	}
	return err
}

func providerBlindCommandHash(caseID, actorID uuid.UUID, reason string) string {
	facts, _ := json.Marshal(struct {
		CommandProtocol int       `json:"command_protocol"`
		CaseID          uuid.UUID `json:"case_id"`
		ActorID         uuid.UUID `json:"actor_id"`
		Reason          string    `json:"reason"`
	}{1, caseID, actorID, reason})
	digest := sha256.Sum256(facts)
	return hex.EncodeToString(digest[:])
}

func readProviderBlindCommand(ctx context.Context, tx pgx.Tx, caseID, actorID uuid.UUID, key, reason string) (LatePaymentReviewCase, bool, error) {
	var body []byte
	err := tx.QueryRow(ctx, `SELECT payload_snapshot->'provider_blind_command'
FROM late_payment_review_evidence
WHERE evidence_type='PROVIDER_BLIND' AND actor_id=$1
  AND payload_snapshot->'provider_blind_command'->>'version'='1'
  AND payload_snapshot->'provider_blind_command'->>'idempotency_key'=$2`, actorID, key).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return LatePaymentReviewCase{}, false, nil
	}
	if err != nil {
		return LatePaymentReviewCase{}, false, err
	}
	var receipt providerBlindCommandReceipt
	if json.Unmarshal(body, &receipt) != nil || receipt.Version != 1 || receipt.CommandProtocol != 1 || receipt.IdempotencyKey != key || receipt.ActorID != actorID || receipt.CaseID != caseID || receipt.Reason != reason || receipt.RequestHash != providerBlindCommandHash(caseID, actorID, reason) || receipt.Response.ID != caseID || receipt.Response.OwnerID == nil || *receipt.Response.OwnerID != actorID || receipt.Response.ReviewStatus != "PROVIDER_BLIND" || receipt.Response.BlindReason == nil || *receipt.Response.BlindReason != reason {
		return LatePaymentReviewCase{}, false, ErrConflict
	}
	return receipt.Response, true, nil
}
