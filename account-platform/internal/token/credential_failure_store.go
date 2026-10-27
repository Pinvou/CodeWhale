package token

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// FailCredentialBeforeDispatch closes only the untouched original acceptance.
// It must never release a peer's hold or overwrite a peer's dispatch/result.
// The exact error body, transition, convergence evidence and retention clocks
// commit together; a failed/unknown acknowledgement authorizes only a read.
func (r *PostgresRepository) FailCredentialBeforeDispatch(ctx context.Context, id uuid.UUID, result ReplayResult) (bool, error) {
	var receipt struct {
		Code    string `json:"code"`
		Details struct {
			ID         uuid.UUID `json:"usage_request_id"`
			Execution  string    `json:"execution_status"`
			Settlement string    `json:"settlement_status"`
			URL        string    `json:"status_url"`
		} `json:"details"`
	}
	if id == uuid.Nil || result.Kind != "JSON" || result.HTTPStatus != http.StatusServiceUnavailable || result.ContentType != "application/json" || result.ResponseReceived || result.FinalUsage != nil || result.ActualMicrocredits != nil || result.SettlementSnapshot != nil || result.LastSequence != 0 || json.Unmarshal(result.Payload, &receipt) != nil || receipt.Code != "PROVIDER_CREDENTIAL_UNAVAILABLE" || receipt.Details.ID != id || receipt.Details.Execution != "FAILED" || receipt.Details.Settlement != "RELEASED" || receipt.Details.URL != "/v1/usage-requests/"+id.String() {
		return false, ErrForbidden
	}
	ciphertext, err := r.secrets.Encrypt(result.Payload, replayAAD(id, "result:JSON", 0))
	if err != nil {
		return false, err
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockReplayUsageTx(ctx, tx, id); err != nil {
		return false, err
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		return false, err
	}
	var untouched bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM usage_requests request WHERE request.id=$1
 AND request.status='CREATED' AND request.execution_status='RECEIVED' AND request.execution_version=1
 AND request.settlement_status='HOLD_PENDING' AND request.error_class IS NULL
 AND request.hold_id IS NULL AND request.price_version_id IS NULL AND request.actual_microcredits IS NULL
 AND request.provider_terminal_at IS NULL AND request.final_usage_observed_at IS NULL
 AND request.cancel_requested_at IS NULL AND request.cancel_status='NOT_REQUESTED'
 AND request.last_persisted_sequence=0 AND request.delivery_status='NOT_STARTED'
 AND request.idempotency_key_digest IS NOT NULL AND request.canonicalization_version=1
 AND EXISTS(SELECT 1 FROM usage_execution_transitions accepted WHERE accepted.usage_request_id=request.id
 AND accepted.from_status='NONE' AND accepted.to_status='RECEIVED' AND accepted.from_version=0 AND accepted.to_version=1 AND accepted.reason='request accepted')
 AND NOT EXISTS(SELECT 1 FROM usage_dispatch_preparations WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_dispatch_intents WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_attempts WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_stream_events WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_replay_results WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_idempotency_tombstones WHERE usage_request_id=request.id)
 )`, id).Scan(&untouched)
	if err != nil {
		return false, err
	}
	if !untouched {
		return false, nil
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',settlement_status='RELEASED',error_class='credential_unavailable',terminal_at=$2,updated_at=$2,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1 AND status='CREATED' AND execution_status='RECEIVED' AND execution_version=1`, id, now)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, ErrForbidden
	}
	if err := transitionExecutionTx(ctx, tx, id, []string{"RECEIVED"}, "FAILED", "credential_unavailable", now); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_replay_results(usage_request_id,response_kind,http_status,content_type,encrypted_payload,key_id,payload_integrity,evidence_payload_digest,last_sequence,created_at,expires_at) VALUES($1,'JSON',$2,$3,$4,$5,$6,$7,0,$8,$9)`, id, result.HTTPStatus, result.ContentType, ciphertext, r.secrets.KeyID(), replayDigest(ciphertext), replayDigest(result.Payload), now, now.Add(resultRetention))
	if err != nil {
		return false, err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, id, "RESULT_OBSERVED", "GATEWAY", "RECORDED", "JSON:503:0", map[string]any{"response_kind": "JSON", "http_status": result.HTTPStatus, "content_type": result.ContentType, "response_payload_digest": replayDigest(result.Payload), "final_usage_present": false, "settlement_snapshot_present": false, "last_sequence": 0}, now); err != nil {
		return false, err
	}
	if err := addReplayTombstone(ctx, tx, id, "FAILED", now); err != nil {
		return false, err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, id, "CUSTOMER_HOLD_RELEASE", "BILLING", "RELEASE", "credential_unavailable", map[string]any{"reason": "credential_unavailable", "external_success_inferred": false}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
