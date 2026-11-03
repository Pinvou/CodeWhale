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
	return r.requestCancellation(ctx, usageID, userID, nil, keyDigest, fingerprint, adapterVersion)
}

// Organization cancellation belongs to the original invocation, not the user's
// current membership or active context. Keep the personal entry point separate
// and recheck this explicit organization under the same lock as dispatch.
func (r *PostgresRepository) RequestOrganizationCancellation(ctx context.Context, usageID, userID, organizationID uuid.UUID, keyDigest, fingerprint, adapterVersion string) (CancellationResult, bool, error) {
	if organizationID == uuid.Nil {
		return CancellationResult{}, false, ErrNotFound
	}
	return r.requestCancellation(ctx, usageID, userID, &organizationID, keyDigest, fingerprint, adapterVersion)
}

func (r *PostgresRepository) requestCancellation(ctx context.Context, usageID, userID uuid.UUID, organizationID *uuid.UUID, keyDigest, fingerprint, adapterVersion string) (CancellationResult, bool, error) {
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
	if !cancellationOwnerMatches(usage, userID) || !sameCancellationOrganization(usage.OrganizationID, organizationID) {
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
	// A Billing expiry fence can precede the user's first command. Retain
	// its pending cancellation facts rather than replacing them with
	// UNSUPPORTED; only the existing proof/recovery owner may confirm release.
	if usage.ExecutionStatus == "REVIEW" && usage.ErrorClass != nil && *usage.ErrorClass == "cancel_hold_recovery_unconfirmed" && usage.CancelStatus == "CANCEL_REQUESTED" {
		return cancellationSnapshot(usage), false, tx.Commit(ctx)
	}
	// A MODEL cancellation or expiry recovery may already have confirmed release
	// before the user's first command. Keep its immutable confirmation so the
	// original worker can record its attempt or replay its expiry proof.
	if usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH" && usage.CancelStatus == "LOCAL_CONFIRMED" && usage.SettlementStatus == "RELEASED" {
		return cancellationSnapshot(usage), false, tx.Commit(ctx)
	}
	// The existing expiry fence already stopped this original dispatch. Record
	// the user's command, retaining its proof and the usual same-hold finish.
	if usage.ExecutionStatus == "HOLD_RESERVED" && usage.CancelStatus == "CANCEL_REQUESTED" {
		pending, err := pendingUnusedExpiryFenceTx(ctx, tx, usage.ID)
		if err != nil {
			return CancellationResult{}, false, err
		}
		if pending {
			return cancellationSnapshot(usage), true, tx.Commit(ctx)
		}
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

// Public cancellation/recovery exposes only the caller's own original resource.
// Its persisted organization is authoritative; current membership, context and
// optional historical snapshot fields cannot change the original ownership.
func cancellationOwnerMatches(usage UsageRequest, userID uuid.UUID) bool {
	return userID != uuid.Nil && usage.UserID == userID && (usage.OrganizationID == nil || *usage.OrganizationID != uuid.Nil)
}

func sameCancellationOrganization(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func isCancellationKeyConflict(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505" &&
		databaseError.TableName == "usage_cancel_commands" &&
		databaseError.ConstraintName == "usage_cancel_commands_user_id_idempotency_key_digest_key"
}

const cancellationFingerprintDomain = "pinvou-usage-cancel-v1\x00"

func cancellationFingerprint(usageID uuid.UUID) string {
	digest := sha256.Sum256([]byte(cancellationFingerprintDomain + usageID.String()))
	return hex.EncodeToString(digest[:])
}
