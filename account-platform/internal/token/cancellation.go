package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (r *PostgresRepository) RequestCancellation(ctx context.Context, usageID, userID uuid.UUID, keyDigest, fingerprint, adapterVersion string) (CancellationResult, bool, error) {
	if usageID == uuid.Nil || userID == uuid.Nil || len(keyDigest) != 64 || len(fingerprint) != 64 || strings.TrimSpace(adapterVersion) == "" {
		return CancellationResult{}, false, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CancellationResult{}, false, err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID), &usage); errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, false, ErrNotFound
	} else if err != nil {
		return CancellationResult{}, false, err
	}
	// Personal cancellation is scoped to the persisted resource, under the
	// same row lock that serializes dispatch. A personal permission never
	// authorizes cancellation of an organization invocation.
	if usage.UserID != userID || usage.OrganizationID != nil {
		return CancellationResult{}, false, ErrNotFound
	}
	var storedFingerprint string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM usage_cancel_commands WHERE usage_request_id=$1`, usageID).Scan(&storedFingerprint)
	if err == nil {
		if storedFingerprint != fingerprint {
			return CancellationResult{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CancellationResult{}, false, err
		}
		return cancellationSnapshot(usage), usage.CancelStatus == "CANCEL_REQUESTED" && (usage.ExecutionStatus == "RECEIVED" || usage.ExecutionStatus == "HOLD_RESERVING" || usage.ExecutionStatus == "HOLD_RESERVED"), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CancellationResult{}, false, err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancel_commands(id,usage_request_id,user_id,idempotency_key_digest,request_fingerprint,cancellation_version,created_at) VALUES($1,$2,$3,$4,$5,1,$6)`, uuid.Must(uuid.NewV7()), usageID, userID, keyDigest, fingerprint, now); err != nil {
		if isCancellationKeyConflict(err) {
			return CancellationResult{}, false, ErrIdempotencyConflict
		}
		return CancellationResult{}, false, err
	}
	terminal := usage.ExecutionStatus == "SUCCEEDED" || usage.ExecutionStatus == "FAILED" || usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH"
	if terminal {
		if _, err := tx.Exec(ctx, `UPDATE usage_requests SET cancel_status='ALREADY_TERMINAL',cancel_requested_at=$2,updated_at=$2 WHERE id=$1`, usageID, now); err != nil {
			return CancellationResult{}, false, err
		}
		usage.CancelStatus, usage.CancelRequestedAt = "ALREADY_TERMINAL", &now
		return cancellationSnapshot(usage), false, tx.Commit(ctx)
	}
	preDispatch := usage.ExecutionStatus == "RECEIVED" || usage.ExecutionStatus == "HOLD_RESERVING" || usage.ExecutionStatus == "HOLD_RESERVED"
	cancelStatus := "UNSUPPORTED"
	if preDispatch {
		cancelStatus = "CANCEL_REQUESTED"
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET cancel_status=$2,delivery_status='USER_STOPPED',cancellation_origin='USER',cancel_requested_at=$3,user_stopped_at=$3,cancel_reconciliation_deadline=$3::timestamptz + make_interval(secs=>cancel_reconciliation_seconds),updated_at=$3 WHERE id=$1`, usageID, cancelStatus, now); err != nil {
		return CancellationResult{}, false, err
	}
	evidence := map[string]any{"capability": "UNSUPPORTED", "pre_dispatch": preDispatch}
	if preDispatch {
		evidence["capability"] = "LOCAL_PRE_DISPATCH"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), usageID, cancelStatus, strings.TrimSpace(adapterVersion), evidence, now); err != nil {
		return CancellationResult{}, false, err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "CANCELLATION", "GATEWAY", "RECORDED", "requested:"+cancelStatus, map[string]any{"cancel_status": cancelStatus, "capability": evidence["capability"], "pre_dispatch": preDispatch, "provider_success_inferred": false}, now); err != nil {
		return CancellationResult{}, false, err
	}
	usage.CancelStatus, usage.DeliveryStatus, usage.CancelRequestedAt, usage.UserStoppedAt = cancelStatus, "USER_STOPPED", &now, &now
	if err := tx.Commit(ctx); err != nil {
		return CancellationResult{}, false, err
	}
	return cancellationSnapshot(usage), preDispatch, nil
}

func isCancellationKeyConflict(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505" &&
		databaseError.TableName == "usage_cancel_commands" &&
		databaseError.ConstraintName == "usage_cancel_commands_user_id_idempotency_key_digest_key"
}

func cancellationFingerprint(usageID uuid.UUID) string {
	digest := sha256.Sum256([]byte("pinvou-usage-cancel-v1\x00" + usageID.String()))
	return hex.EncodeToString(digest[:])
}
