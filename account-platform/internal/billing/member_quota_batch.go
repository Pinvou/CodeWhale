package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// FreezeMemberQuotaBatch commits the ordered parent facts before any child can
// allocate funds. Child keys deliberately retain their original index and v1
// derivation: identical entries at different indexes remain distinct commands.
func (r *PostgresRepository) FreezeMemberQuotaBatch(ctx context.Context, organizationID, actorID uuid.UUID, key string, items []memberQuotaBatchItem) error {
	key = strings.TrimSpace(key)
	if organizationID == uuid.Nil || actorID == uuid.Nil || len(key) < 16 || len(key) > 128 || len(items) == 0 || len(items) > 200 {
		return ErrConflict
	}
	facts, err := json.Marshal(struct {
		OrganizationID uuid.UUID              `json:"organization_id"`
		ActorID        uuid.UUID              `json:"actor_id"`
		Items          []memberQuotaBatchItem `json:"items"`
	}{organizationID, actorID, items})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(facts)
	fingerprint := hex.EncodeToString(digest[:])
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "member-quota-batch", organizationID.String()+":"+key); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO member_quota_batch_commands(organization_id,idempotency_key,actor_user_id,request_fingerprint,request_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(organization_id,idempotency_key) DO NOTHING`, organizationID, key, actorID, fingerprint, facts, r.now()); err != nil {
		return err
	}
	var originalFingerprint string
	if err := tx.QueryRow(ctx, `SELECT request_fingerprint FROM member_quota_batch_commands WHERE organization_id=$1 AND idempotency_key=$2`, organizationID, key).Scan(&originalFingerprint); err != nil {
		return err
	}
	if originalFingerprint != fingerprint {
		return ErrIdempotencyConflict
	}
	return tx.Commit(ctx)
}
