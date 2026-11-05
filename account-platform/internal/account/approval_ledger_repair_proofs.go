package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const ledgerRepairProofBatchLimit = 64
const ledgerRepairProofDurable = "billing-ledger-repair-executor-p1-v1"

type ledgerRepairProofIdentity struct {
	ApprovalRequestID    string `json:"approval_request_id"`
	AuthorizationEventID string `json:"authorization_event_id"`
	ApprovalEpoch        int64  `json:"approval_epoch"`
}
type ledgerRepairProofRequest struct {
	SchemaVersion int                         `json:"schema_version"`
	Items         []ledgerRepairProofIdentity `json:"items"`
}
type ledgerRepairProofItem struct {
	ledgerRepairProofIdentity
	Status                   string          `json:"status"`
	CommandType              string          `json:"command_type,omitempty"`
	PolicyVersion            int64           `json:"policy_version,omitempty"`
	SnapshotSchemaVersion    int             `json:"snapshot_schema_version,omitempty"`
	ExecutionKeyDigest       string          `json:"execution_key_digest,omitempty"`
	CommandDigest            string          `json:"command_digest,omitempty"`
	SnapshotDigest           string          `json:"snapshot_digest,omitempty"`
	DomainRevision           string          `json:"domain_revision,omitempty"`
	NormalizedCommand        json.RawMessage `json:"normalized_command,omitempty"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions,omitempty"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot,omitempty"`
}
type ledgerRepairProofResponse struct {
	SchemaVersion int                     `json:"schema_version"`
	Items         []ledgerRepairProofItem `json:"items"`
}
type ledgerRepairProofReader interface {
	ReadLedgerRepairAuthorizationProofs(context.Context, []ledgerRepairProofIdentity) ([]ledgerRepairProofItem, error)
}

func (s *Service) ledgerRepairAuthorizationProofBatch(w http.ResponseWriter, r *http.Request) {
	caller, ok := serviceauth.FromContext(r.Context())
	if !ok || caller.ActorType != "SERVICE" || caller.Service != "billing-service" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "billing workload is required", httpserver.RequestID(r.Context()))
		return
	}
	if r.URL.Path != "/internal/v1/ledger-repair/authorization-proofs:batch" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid proof request", httpserver.RequestID(r.Context()))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	var body ledgerRepairProofRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || len(raw) > 64<<10 || !uniqueRecoveryJSONKeys(raw) || decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || body.SchemaVersion != 1 || len(body.Items) == 0 || len(body.Items) > ledgerRepairProofBatchLimit {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid proof request", httpserver.RequestID(r.Context()))
		return
	}
	seenRequests, seenEvents := map[string]bool{}, map[string]bool{}
	for _, item := range body.Items {
		requestID, e1 := uuid.Parse(item.ApprovalRequestID)
		eventID, e2 := uuid.Parse(item.AuthorizationEventID)
		if e1 != nil || e2 != nil || requestID == uuid.Nil || eventID == uuid.Nil || requestID.String() != item.ApprovalRequestID || eventID.String() != item.AuthorizationEventID || item.ApprovalEpoch < 1 || seenRequests[item.ApprovalRequestID] || seenEvents[item.AuthorizationEventID] {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid proof request", httpserver.RequestID(r.Context()))
			return
		}
		seenRequests[item.ApprovalRequestID], seenEvents[item.AuthorizationEventID] = true, true
	}
	reader, ok := s.repository.(ledgerRepairProofReader)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROOF_UNAVAILABLE", "approval proof is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := reader.ReadLedgerRepairAuthorizationProofs(r.Context(), body.Items)
	if err != nil || len(items) != len(body.Items) {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROOF_UNAVAILABLE", "approval proof is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, ledgerRepairProofResponse{SchemaVersion: 1, Items: items})
}

// The real historical wire label is fixed across all legacy financial digests.
// Never expose or log the stored execution key itself.
func ledgerRepairHistoricalExecutionKeyDigest(key string) string {
	encoded, err := json.Marshal(map[string]any{"command_type": "LEDGER_REPAIR", "execution_key": strings.TrimSpace(key)})
	if err != nil || strings.TrimSpace(key) == "" {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) ReadLedgerRepairAuthorizationProofs(ctx context.Context, inputs []ledgerRepairProofIdentity) ([]ledgerRepairProofItem, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	result := make([]ledgerRepairProofItem, 0, len(inputs))
	for _, input := range inputs {
		item := ledgerRepairProofItem{ledgerRepairProofIdentity: input, Status: "UNAVAILABLE"}
		// The existing sidecar reader is the least-privilege authoritative check.
		// Its missing binding exception must not abort the rest of this batch.
		nested, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var binding ApprovalAuthorizationBinding
		binding.EventID, _ = uuid.Parse(input.AuthorizationEventID)
		binding.Durable = ledgerRepairProofDurable
		binding.Workload = approvalconsumer.BillingWorkload
		err = nested.QueryRow(ctx, `SELECT approval_request_id,approved_request_version,approval_epoch,serialization_version,provenance,event_digest,digest_kind,command_type,policy_version,executor_identity FROM public.resolve_approval_authorization_binding($1,$2,$3)`, binding.EventID, binding.Durable, binding.Workload).Scan(&binding.ApprovalRequestID, &binding.ApprovedRequestVersion, &binding.ApprovalEpoch, &binding.SerializationVersion, &binding.Provenance, &binding.EventDigest, &binding.DigestKind, &binding.CommandType, &binding.PolicyVersion, &binding.ExecutorIdentity)
		if err != nil {
			_ = nested.Rollback(ctx)
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) && pgerr.Code == "55000" {
				result = append(result, item)
				continue
			}
			return nil, err
		}
		if err = nested.Commit(ctx); err != nil {
			return nil, err
		}
		if !validApprovalAuthorizationBinding(binding) || binding.ApprovalRequestID.String() != input.ApprovalRequestID || binding.ApprovalEpoch != input.ApprovalEpoch || binding.CommandType != "LEDGER_REPAIR" || binding.PolicyVersion != 1 || binding.ExecutorIdentity != approvalconsumer.BillingWorkload || binding.SerializationVersion != approvalencoding.ApprovalSerializationVersion || binding.Provenance != "FROZEN_AT_APPROVAL" || binding.DigestKind != "DELIVERY_BYTES" {
			result = append(result, item)
			continue
		}
		var normalized, critical, financial, payload []byte
		var policy int64
		var schema int
		var targetType, targetID, key, fingerprint, storedExecutor, storedDurable, storedDigest, storedDigestKind, storedSerialization, storedProvenance string
		var storedEvent uuid.UUID
		var storedVersion, storedEpoch int64
		err = tx.QueryRow(ctx, `SELECT r.normalized_command,r.target_resource_type,r.target_resource_id,r.policy_version,s.snapshot_schema_version,e.critical_resource_versions,e.financial_snapshot,e.execution_idempotency_key,encode(e.command_fingerprint,'hex'),e.executor_identity,e.authorization_event_id,e.authorization_durable,e.authorization_approved_request_version,e.authorization_event_digest,e.authorization_digest_kind,e.authorization_serialization_version,e.authorization_provenance,e.authorization_approval_epoch,o.payload FROM public.approval_requests r JOIN public.approval_request_snapshots s ON s.approval_request_id=r.id JOIN public.approval_executions e ON e.approval_request_id=r.id AND e.command_type=r.command_type JOIN public.outbox_events o ON o.event_id=e.authorization_event_id AND o.aggregate_id=r.id WHERE r.id=$1 AND r.command_type='LEDGER_REPAIR' AND o.event_id=$2`, binding.ApprovalRequestID, binding.EventID).Scan(&normalized, &targetType, &targetID, &policy, &schema, &critical, &financial, &key, &fingerprint, &storedExecutor, &storedEvent, &storedDurable, &storedVersion, &storedDigest, &storedDigestKind, &storedSerialization, &storedProvenance, &storedEpoch, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			result = append(result, item)
			continue
		}
		if err != nil {
			return nil, err
		}
		if storedExecutor != binding.ExecutorIdentity || storedEvent != binding.EventID || storedDurable != binding.Durable || storedVersion != binding.ApprovedRequestVersion || storedDigest != binding.EventDigest || storedDigestKind != binding.DigestKind || storedSerialization != binding.SerializationVersion || storedProvenance != binding.Provenance || storedEpoch != binding.ApprovalEpoch || policy != binding.PolicyVersion || schema != 1 || targetType != "LEDGER_POSTING" {
			result = append(result, item)
			continue
		}
		event, err := decodePersistedProofAuthorizationPayload(payload)
		if err != nil || event.ApprovalRequestID != binding.ApprovalRequestID || event.CommandType != binding.CommandType || event.PolicyVersion != policy || event.ExecutorIdentity != storedExecutor || event.ApprovalControlEpoch != storedEpoch || event.CommandFingerprint != fingerprint || event.TargetResourceType != targetType || event.TargetResourceID != targetID || event.ProofSchemaVersion != approvalencoding.ProofSchemaVersion {
			result = append(result, item)
			continue
		}
		artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: binding.CommandType, PolicyVersion: policy, SnapshotSchemaVersion: schema, TargetResourceType: targetType, TargetResourceID: targetID, NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial})
		if err != nil || artifacts.CommandDigest != event.CommandDigest || artifacts.SnapshotDigest != event.SnapshotDigest {
			result = append(result, item)
			continue
		}
		var doc struct {
			NormalizedCommand        json.RawMessage `json:"normalized_command"`
			CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
			FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
		}
		if json.Unmarshal(artifacts.ProofInputJSON, &doc) != nil || !sameJSONObject(doc.NormalizedCommand, event.NormalizedCommand) || !sameJSONObject(doc.CriticalResourceVersions, event.CriticalResourceVersions) || !sameJSONObject(doc.FinancialSnapshot, event.FinancialSnapshot) {
			result = append(result, item)
			continue
		}
		if key != fmt.Sprintf("approval:%s:%s:%s", binding.ApprovalRequestID, binding.CommandType, fingerprint) {
			result = append(result, item)
			continue
		}
		digest := ledgerRepairHistoricalExecutionKeyDigest(key)
		if digest == "" {
			result = append(result, item)
			continue
		}
		item.Status = "VERIFIED"
		item.CommandType = binding.CommandType
		item.PolicyVersion = policy
		item.SnapshotSchemaVersion = schema
		item.ExecutionKeyDigest = digest
		item.CommandDigest = event.CommandDigest
		item.SnapshotDigest = event.SnapshotDigest
		item.DomainRevision = event.DomainRevision
		item.NormalizedCommand = event.NormalizedCommand
		item.CriticalResourceVersions = event.CriticalResourceVersions
		item.FinancialSnapshot = event.FinancialSnapshot
		result = append(result, item)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
