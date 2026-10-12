package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/holdexpiry"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
)

// The same usage row lock serializes cancellation with persisted dispatch.
// Absence of a future producer is never treated as evidence of no dispatch.
func (r *PostgresRepository) fenceExpiredHold(ctx context.Context, identity holdexpiry.Identity) (holdexpiry.Proof, error) {
	if !identity.Valid() || identity.ExpiresAt.After(r.now()) {
		return holdexpiry.Proof{}, ErrForbidden
	}
	// Authentication/decryption may use a remote key service. Do it before
	// acquiring the usage lock, then compare the exact sealed row under that lock.
	unused, err := r.loadUnusedExpiryDispatch(ctx, identity.UsageRequestID)
	if err != nil {
		return holdexpiry.Proof{}, err
	}
	if unused != nil {
		defer unused.clear()
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return holdexpiry.Proof{}, err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err = scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, identity.UsageRequestID), &usage); err != nil {
		return holdexpiry.Proof{}, err
	}
	if usage.CanonicalVersion <= 0 || usage.HoldID == nil || *usage.HoldID != identity.HoldID || usage.WalletID != identity.WalletID || usage.MaximumMicrocredits != identity.MaxMicrocredits {
		return holdexpiry.Proof{}, ErrForbidden
	}
	if err = validateUnusedExpiryDispatchTx(ctx, tx, usage, unused); err != nil {
		return holdexpiry.Proof{}, err
	}
	var saved []byte
	var proofID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id,sanitized_evidence FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='CANCEL_REQUESTED' AND adapter_version='hold-expiry-v1' ORDER BY occurred_at,id LIMIT 1`, usage.ID).Scan(&proofID, &saved)
	if err == nil {
		var previous holdexpiry.Identity
		if json.Unmarshal(saved, &previous) != nil || !previous.Matches(identity) || (usage.CancelStatus != "CANCEL_REQUESTED" && usage.CancelStatus != "LOCAL_CONFIRMED") {
			return holdexpiry.Proof{}, ErrForbidden
		}
		if usage.ExecutionStatus == "REVIEW" && usage.ErrorClass != nil && *usage.ErrorClass == "cancel_hold_recovery_unconfirmed" && usage.CancelStatus == "CANCEL_REQUESTED" {
			if err = transitionExecutionTx(ctx, tx, usage.ID, []string{"REVIEW"}, "HOLD_RESERVED", "retry fenced expiry release", r.now()); err != nil {
				return holdexpiry.Proof{}, err
			}
			if _, err = tx.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',settlement_status='HOLD_ACTIVE',updated_at=$2 WHERE id=$1`, usage.ID, r.now()); err != nil {
				return holdexpiry.Proof{}, err
			}
			usage.ExecutionStatus = "HOLD_RESERVED"
		}
		if usage.ExecutionStatus != "HOLD_RESERVING" && usage.ExecutionStatus != "HOLD_RESERVED" && usage.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" {
			return holdexpiry.Proof{}, ErrForbidden
		}
	} else {
		if !errors.Is(err, pgx.ErrNoRows) {
			return holdexpiry.Proof{}, err
		}
		if unused != nil && usage.ExecutionStatus != "DISPATCH_INTENT_PERSISTED" {
			return holdexpiry.Proof{}, ErrForbidden
		}
		if unused != nil && usage.ExecutionStatus == "DISPATCH_INTENT_PERSISTED" {
			// AcquireDispatch cannot commit/return an envelope until it has
			// transitioned this same locked usage row. Do not lock the intent
			// here: Acquire takes intent then usage, so that would invert order.
			if err = transitionExecutionTx(ctx, tx, usage.ID, []string{"DISPATCH_INTENT_PERSISTED"}, "HOLD_RESERVED", "unused expired dispatch fenced", r.now()); err != nil {
				return holdexpiry.Proof{}, err
			}
			usage.ExecutionStatus = "HOLD_RESERVED"
		}
		if usage.ExecutionStatus != "HOLD_RESERVING" && usage.ExecutionStatus != "HOLD_RESERVED" {
			return holdexpiry.Proof{}, ErrForbidden
		}
		if usage.CancelStatus != "NOT_REQUESTED" && usage.CancelStatus != "CANCEL_REQUESTED" && !(unused != nil && usage.DeliveryStatus == "USER_STOPPED" && usage.CancelStatus == "UNSUPPORTED") {
			return holdexpiry.Proof{}, ErrForbidden
		}
		proofID = uuid.Must(uuid.NewV7())
		body, _ := json.Marshal(identity)
		if _, err = tx.Exec(ctx, `UPDATE usage_requests SET cancel_status='CANCEL_REQUESTED',cancel_requested_at=COALESCE(cancel_requested_at,$2),updated_at=$2 WHERE id=$1`, usage.ID, r.now()); err != nil {
			return holdexpiry.Proof{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','hold-expiry-v1',$3,$4)`, proofID, usage.ID, body, r.now()); err != nil {
			return holdexpiry.Proof{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return holdexpiry.Proof{}, err
	}
	return holdexpiry.Proof{Identity: identity, ProofID: proofID, Status: "PRE_DISPATCH_FENCED"}, nil
}
func (s *Service) expiredHoldProof(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := serviceauth.FromContext(r.Context())
	if !ok || actor.Service != "billing-service" {
		api.WriteError(w, 403, "PERMISSION_DENIED", "billing workload required", httpserver.RequestID(r.Context()))
		return
	}
	var identity holdexpiry.Identity
	if api.DecodeJSON(w, r, &identity) != nil || !identity.Valid() {
		api.WriteError(w, 400, "INVALID_REQUEST", "invalid hold identity", httpserver.RequestID(r.Context()))
		return
	}
	proof, err := s.recoverExpiredHold(r.Context(), identity)
	if err != nil {
		api.WriteError(w, 503, "HOLD_RECOVERY_UNKNOWN", "hold release requires retry or manual review", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, 200, proof)
}
func (s *Service) recoverExpiredHold(ctx context.Context, identity holdexpiry.Identity) (holdexpiry.Proof, error) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || repository == nil {
		return holdexpiry.Proof{}, ErrForbidden
	}
	proof, err := repository.fenceExpiredHold(ctx, identity)
	if err != nil {
		return holdexpiry.Proof{}, err
	}
	// The cancellation fence has committed. Release uses the installed protocol;
	// no usage or financial row lock crosses this RPC boundary.
	result, err := s.finishPreDispatchCancellation(ctx, identity.UsageRequestID)
	if err != nil || result.CancelStatus != "LOCAL_CONFIRMED" || result.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || result.SettlementStatus != "RELEASED" {
		return holdexpiry.Proof{}, ErrForbidden
	}
	proof.Status = "LOCAL_CONFIRMED"
	return proof, nil
}
