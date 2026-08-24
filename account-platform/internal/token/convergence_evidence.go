package token

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

type convergenceEvidenceMetadata struct {
	ProviderID              *string
	AdapterVersion          *string
	UpstreamReferenceDigest *string
}

func appendUsageConvergenceEvidenceTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, evidenceType, sourceType, decision, evidenceKey string, snapshot map[string]any, now time.Time) error {
	evidenceType = strings.ToUpper(strings.TrimSpace(evidenceType))
	sourceType = strings.ToUpper(strings.TrimSpace(sourceType))
	decision = strings.ToUpper(strings.TrimSpace(decision))
	evidenceKey = strings.TrimSpace(evidenceKey)
	if usageID == uuid.Nil || evidenceKey == "" || snapshot == nil {
		return ErrForbidden
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	metadata := convergenceEvidenceMetadata{}
	err = tx.QueryRow(ctx, `SELECT provider_id,adapter_version,upstream_reference_digest FROM usage_attempts WHERE usage_request_id=$1`, usageID).Scan(&metadata.ProviderID, &metadata.AdapterVersion, &metadata.UpstreamReferenceDigest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_convergence_evidence(
id,usage_request_id,evidence_type,source_type,decision,evidence_key,provider_id,adapter_version,
upstream_reference_digest,payload_digest,sanitized_snapshot,observed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT(usage_request_id,evidence_type,evidence_key,payload_digest) DO NOTHING`,
		uuid.Must(uuid.NewV7()), usageID, evidenceType, sourceType, decision, evidenceKey,
		metadata.ProviderID, metadata.AdapterVersion, metadata.UpstreamReferenceDigest,
		hex.EncodeToString(digest[:]), json.RawMessage(encoded), now)
	return err
}

func (r *PostgresRepository) RecordConvergenceEvidence(ctx context.Context, usageID uuid.UUID, evidenceType, sourceType, decision, evidenceKey string, snapshot map[string]any) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, evidenceType, sourceType, decision, evidenceKey, snapshot, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
