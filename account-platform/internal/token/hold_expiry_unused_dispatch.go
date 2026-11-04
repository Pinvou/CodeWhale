package token

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// An unused dispatch is evidence only while its authenticated, usage-bound
// envelope and all acquisition fields remain exactly as read before the lock.
type unusedExpiryDispatch struct {
	ciphertext                    []byte
	keyID, integrity, upstreamKey *string
	acquiredAt, leaseUntil        *time.Time
	leaseOwner                    *string
	idempotent                    bool
	envelope                      DispatchEnvelope
}

const unusedExpiryDispatchSelect = `SELECT encrypted_envelope,key_id,envelope_integrity,upstream_idempotency_key,adapter_supports_idempotency,worker_acquired_at,worker_lease_owner,worker_lease_until FROM usage_dispatch_intents WHERE usage_request_id=$1`

func scanUnusedExpiryDispatch(row rowScanner) (*unusedExpiryDispatch, error) {
	value := new(unusedExpiryDispatch)
	err := row.Scan(&value.ciphertext, &value.keyID, &value.integrity, &value.upstreamKey, &value.idempotent, &value.acquiredAt, &value.leaseOwner, &value.leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return value, err
}

func (value *unusedExpiryDispatch) valid(usageID uuid.UUID) bool {
	return value != nil && len(value.ciphertext) > 0 && value.keyID != nil && strings.TrimSpace(*value.keyID) != "" && value.integrity != nil && *value.integrity == replayDigest(value.ciphertext) && value.upstreamKey != nil && *value.upstreamKey == "pinvou-"+usageID.String() && !value.idempotent && value.acquiredAt == nil && value.leaseOwner == nil && value.leaseUntil == nil
}

func (value *unusedExpiryDispatch) clear() {
	clear(value.envelope.Credential)
	clear(value.envelope.RequestBody)
}

func (r *PostgresRepository) loadUnusedExpiryDispatch(ctx context.Context, usageID uuid.UUID) (*unusedExpiryDispatch, error) {
	value, err := scanUnusedExpiryDispatch(r.pool.QueryRow(ctx, unusedExpiryDispatchSelect, usageID))
	if err != nil || value == nil {
		return value, err
	}
	if !value.valid(usageID) {
		return nil, ErrForbidden
	}
	plain, err := r.secrets.DecryptWithKeyID(*value.keyID, value.ciphertext, replayAAD(usageID, "dispatch", 0))
	if err != nil {
		return nil, ErrForbidden
	}
	defer clear(plain)
	if !expiryJSONFields(plain, []string{"base_url", "provider", "adapter_version", "credential", "request_body", "price", "stream", "maximum_cost", "maximum_output", "context_type"}) || json.Unmarshal(plain, &value.envelope) != nil {
		value.clear()
		return nil, ErrForbidden
	}
	return value, nil
}

func validateUnusedExpiryDispatchTx(ctx context.Context, tx pgx.Tx, usage UsageRequest, checked *unusedExpiryDispatch) error {
	// A plain read is deliberate. Acquire holds intent before usage; usage's
	// lock and execution CAS are the commit fence, not a reversed intent lock.
	current, err := scanUnusedExpiryDispatch(tx.QueryRow(ctx, unusedExpiryDispatchSelect, usage.ID))
	if err != nil {
		return err
	}
	if current == nil && checked == nil {
		return nil
	}
	if checked == nil || !current.valid(usage.ID) || !bytes.Equal(current.ciphertext, checked.ciphertext) || *current.keyID != *checked.keyID || *current.integrity != *checked.integrity || *current.upstreamKey != *checked.upstreamKey {
		return ErrForbidden
	}
	if !unusedExpiryFrozenFactsMatch(usage, checked.envelope) {
		return ErrForbidden
	}
	localStop := usage.DeliveryStatus == "USER_STOPPED"
	if localStop {
		if err = validateExpiryLocalUserStopTx(ctx, tx, usage, checked.envelope.AdapterVersion); err != nil {
			return err
		}
	}
	// A terminal MODEL observation must not invalidate an already released
	// expiry confirmation. The caller still checks the original exact expiry
	// identity; observations never authorize a fresh release or dispatch.
	terminal := usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH" && usage.CancelStatus == "LOCAL_CONFIRMED" && usage.SettlementStatus == "RELEASED"
	var converged []byte
	if terminal {
		converged, err = json.Marshal(emergencyConvergedSnapshot(usage.ID, usage.ExecutionStatus, usage.CancelStatus, usage.SettlementStatus, usage.HoldID, usage.ActualMicrocredits, []byte(usage.UsageSnapshot)))
		if err != nil {
			return err
		}
	}
	var external bool
	err = tx.QueryRow(ctx, `WITH terminal_requests AS (
 SELECT evidence.id,evidence.emergency_cancellation_id,evidence.snapshot,evidence.created_at
 FROM model_emergency_cancellation_evidence evidence
 JOIN model_emergency_cancellations target ON target.id=evidence.emergency_cancellation_id
 JOIN model_emergency_cancellation_policies policy ON policy.id=target.policy_id
 JOIN usage_requests original ON original.id=target.usage_request_id AND original.model_id=policy.model_id
 WHERE $4 AND target.usage_request_id=$1 AND evidence.evidence_type='REQUESTED'
 AND EXISTS(SELECT 1 FROM usage_cancellation_evidence expiry WHERE expiry.usage_request_id=$1 AND expiry.evidence_type='CANCEL_REQUESTED' AND expiry.adapter_version='hold-expiry-v1')
 AND evidence.snapshot->>'capability' IN ('UNSUPPORTED','EXPLICIT_CANCEL','STREAM_ABORT_CONFIRMED_CANCEL')
 AND evidence.snapshot=jsonb_build_object('origin','MODEL_EMERGENCY','capability',evidence.snapshot->>'capability','pre_dispatch',false,'already_terminal',true,'provider_success_inferred',false)
), terminal_attempts AS (
 SELECT evidence.* FROM model_emergency_cancellation_evidence evidence
 JOIN terminal_requests request ON request.emergency_cancellation_id=evidence.emergency_cancellation_id
 WHERE evidence.evidence_type='ATTEMPTED'
 AND evidence.snapshot=jsonb_build_object('capability',request.snapshot->>'capability','local_transport_aborted',false,'provider_cancel_result','ALREADY_TERMINAL','provider_success_inferred',false,'hold_released',false)
), terminal_observations AS (
 SELECT 'REQUESTED' AS evidence_type,snapshot,created_at FROM terminal_requests
 UNION ALL SELECT evidence_type,snapshot,created_at FROM terminal_attempts
 UNION ALL SELECT evidence.evidence_type,evidence.snapshot,evidence.created_at
 FROM model_emergency_cancellation_evidence evidence
 JOIN terminal_attempts attempt ON attempt.emergency_cancellation_id=evidence.emergency_cancellation_id
 WHERE evidence.evidence_type='CONVERGED' AND evidence.snapshot=$5::jsonb
)
SELECT
 EXISTS(SELECT 1 FROM usage_attempts WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_stream_events WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_replay_results WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_replay_retention_cases WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_delivery_confirmations WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_convergence_evidence convergence WHERE usage_request_id=$1 AND NOT (
  EXISTS(SELECT 1 FROM terminal_observations observation WHERE convergence.evidence_type='CANCELLATION' AND convergence.decision='RECORDED' AND convergence.provider_id IS NULL AND convergence.adapter_version IS NULL AND convergence.upstream_reference_digest IS NULL AND convergence.observed_at=observation.created_at AND convergence.sanitized_snapshot=observation.snapshot AND ((observation.evidence_type='REQUESTED' AND convergence.source_type='GATEWAY' AND convergence.evidence_key='model-emergency-requested') OR (observation.evidence_type='CONVERGED' AND convergence.source_type='RECONCILER' AND convergence.evidence_key='model-emergency-converged')))
  OR
  (evidence_type='UNCERTAINTY' AND source_type='GATEWAY' AND decision='REVIEW' AND evidence_key='cancel_hold_recovery_unconfirmed' AND provider_id IS NULL AND adapter_version IS NULL AND upstream_reference_digest IS NULL AND sanitized_snapshot->>'reason' IS NOT DISTINCT FROM 'cancel_hold_recovery_unconfirmed')
  OR ($2 AND evidence_type='CANCELLATION' AND source_type='GATEWAY' AND decision='RECORDED' AND evidence_key='requested:UNSUPPORTED' AND provider_id IS NULL AND adapter_version IS NULL AND upstream_reference_digest IS NULL AND sanitized_snapshot=jsonb_build_object('cancel_status','UNSUPPORTED','capability','UNSUPPORTED','pre_dispatch',false,'provider_success_inferred',false))))
 OR EXISTS(SELECT 1 FROM usage_cancellation_evidence cancellation WHERE usage_request_id=$1 AND NOT (provider_id IS NULL AND upstream_reference_digest IS NULL AND (
  EXISTS(SELECT 1 FROM terminal_observations observation WHERE cancellation.occurred_at=observation.created_at AND cancellation.sanitized_evidence=observation.snapshot AND ((observation.evidence_type='REQUESTED' AND cancellation.evidence_type='MODEL_EMERGENCY_REQUESTED' AND btrim(cancellation.adapter_version)<>'') OR (observation.evidence_type='ATTEMPTED' AND cancellation.evidence_type='MODEL_EMERGENCY_ATTEMPTED' AND cancellation.adapter_version='gateway')))
  OR
  (evidence_type='CANCEL_REQUESTED' AND adapter_version='hold-expiry-v1')
  OR (evidence_type='LOCAL_CONFIRMED' AND adapter_version='local' AND sanitized_evidence=jsonb_build_object('provider_called',false,'hold_released',true))
  OR ($2 AND evidence_type='UNSUPPORTED' AND adapter_version=$3 AND sanitized_evidence=jsonb_build_object('capability','UNSUPPORTED','pre_dispatch',false)))))
 OR EXISTS(SELECT 1 FROM usage_requests WHERE id=$1 AND (trusted_usage_snapshot IS NOT NULL OR trusted_usage_source IS NOT NULL OR provider_cancel_sent_at IS NOT NULL OR provider_cancel_confirmed_at IS NOT NULL))`, usage.ID, localStop, checked.envelope.AdapterVersion, terminal, converged).Scan(&external)
	if err != nil {
		return err
	}
	if external {
		return ErrForbidden
	}
	return nil
}

// RequestCancellation's persisted-intent branch records a local delivery stop,
// not a supplier cancellation. Require the original command and both exact
// immutable local records; a USER_STOPPED label alone grants no recovery.
func validateExpiryLocalUserStopTx(ctx context.Context, tx pgx.Tx, usage UsageRequest, adapter string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT cancellation_origin='USER'
 AND cancel_requested_at=user_stopped_at
 AND EXISTS(SELECT 1 FROM usage_cancel_commands command WHERE command.usage_request_id=usage_requests.id AND command.user_id=usage_requests.user_id AND command.cancellation_version=1 AND command.request_fingerprint=$2 AND command.created_at=usage_requests.user_stopped_at)
 AND EXISTS(SELECT 1 FROM usage_cancellation_evidence evidence WHERE evidence.usage_request_id=usage_requests.id AND evidence.evidence_type='UNSUPPORTED' AND evidence.adapter_version=$3 AND evidence.provider_id IS NULL AND evidence.upstream_reference_digest IS NULL AND evidence.occurred_at=usage_requests.user_stopped_at AND evidence.sanitized_evidence=jsonb_build_object('capability','UNSUPPORTED','pre_dispatch',false))
 AND EXISTS(SELECT 1 FROM usage_convergence_evidence evidence WHERE evidence.usage_request_id=usage_requests.id AND evidence.evidence_type='CANCELLATION' AND evidence.source_type='GATEWAY' AND evidence.decision='RECORDED' AND evidence.evidence_key='requested:UNSUPPORTED' AND evidence.provider_id IS NULL AND evidence.adapter_version IS NULL AND evidence.upstream_reference_digest IS NULL AND evidence.observed_at=usage_requests.user_stopped_at AND evidence.sanitized_snapshot=jsonb_build_object('cancel_status','UNSUPPORTED','capability','UNSUPPORTED','pre_dispatch',false,'provider_success_inferred',false))
 FROM usage_requests WHERE id=$1`, usage.ID, cancellationFingerprint(usage.ID), adapter).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrForbidden
	}
	return nil
}

// These are frozen producer facts, not current catalog/price/credential reads.
// The caller model can be an alias and the operational max-output default is
// applied after fingerprinting, so upstream bytes cannot recreate that original
// logical-body hash. AEAD binds those bytes to the usage ID; the full metering
// and dispatch tuple must independently match the stored request snapshot.
func unusedExpiryFrozenFactsMatch(usage UsageRequest, envelope DispatchEnvelope) bool {
	if usage.CanonicalVersion != 1 || usage.IdempotencyNamespace != "model-inference" || usage.OrganizationID != nil || usage.ModelID == nil || usage.CapabilityVersionID == nil || usage.ProtocolDefinitionID == nil || usage.CatalogVersion == nil || usage.RouteGeneration == nil || usage.PriceVersionID == nil || usage.Model != usage.ModelID.String() || usage.UserID == uuid.Nil || usage.SessionID == "" || usage.TokenAccountID == uuid.Nil || usage.CredentialID == uuid.Nil || !expiryDigest(usage.IdempotencyKeyDigest) || !expiryDigest(usage.RequestFingerprint) {
		return false
	}
	localStop := usage.DeliveryStatus == "USER_STOPPED" && usage.UserStoppedAt != nil && usage.CancelRequestedAt != nil && usage.UserStoppedAt.Equal(*usage.CancelRequestedAt) && (usage.CancelStatus == "UNSUPPORTED" || usage.CancelStatus == "CANCEL_REQUESTED" || usage.CancelStatus == "LOCAL_CONFIRMED")
	if usage.ProviderTerminalAt != nil || usage.FinalUsageObservedAt != nil || usage.ActualMicrocredits != nil || usage.LastPersistedSequence != 0 || usage.LastClientConfirmedSequence != 0 || (usage.DeliveryStatus != "NOT_STARTED" && !localStop) {
		return false
	}
	switch usage.ExecutionStatus {
	case "DISPATCH_INTENT_PERSISTED", "HOLD_RESERVED":
		if usage.Status != "RESERVED" || usage.SettlementStatus != "HOLD_ACTIVE" {
			return false
		}
	case "REVIEW":
		if usage.ErrorClass == nil || *usage.ErrorClass != "cancel_hold_recovery_unconfirmed" || usage.CancelStatus != "CANCEL_REQUESTED" {
			return false
		}
	case "CANCELLED_BEFORE_DISPATCH":
		if usage.Status != "RELEASED" || usage.SettlementStatus != "RELEASED" || usage.CancelStatus != "LOCAL_CONFIRMED" {
			return false
		}
	default:
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(usage.RequestSnapshot, &fields) != nil {
		return false
	}
	for _, field := range []string{"request_sha256", "model_id", "upstream_model_ref", "catalog_version", "model_capability_version_id", "protocol_definition_id", "protocol_code", "protocol_version", "route_generation", "stream", "estimated_input_tokens", "maximum_output_tokens", "estimator", "context_type", "organization_id", "can_purchase_organization_entitlements", "wallet_id", "token_account_id", "credential_id", "formula_version_id", "price_version_id", "pricing_snapshot", "provider_id", "adapter_version", "wallet_owner_type"} {
		if len(fields[field]) == 0 || (field != "organization_id" && bytes.Equal(fields[field], []byte("null"))) {
			return false
		}
	}
	var snapshot struct {
		RequestSHA      string    `json:"request_sha256"`
		ModelID         uuid.UUID `json:"model_id"`
		UpstreamModel   string    `json:"upstream_model_ref"`
		CatalogVersion  int64     `json:"catalog_version"`
		CapabilityID    uuid.UUID `json:"model_capability_version_id"`
		ProtocolID      uuid.UUID `json:"protocol_definition_id"`
		Protocol        string    `json:"protocol_code"`
		ProtocolVersion int       `json:"protocol_version"`
		RouteGeneration int64     `json:"route_generation"`
		Stream          bool      `json:"stream"`
		EstimatedInput  int64     `json:"estimated_input_tokens"`
		MaximumOutput   int64     `json:"maximum_output_tokens"`
		Estimator       struct {
			Code    string `json:"code"`
			Version int    `json:"version"`
		} `json:"estimator"`
		Context        string       `json:"context_type"`
		OrganizationID *uuid.UUID   `json:"organization_id"`
		Purchase       bool         `json:"can_purchase_organization_entitlements"`
		WalletID       uuid.UUID    `json:"wallet_id"`
		TokenAccountID uuid.UUID    `json:"token_account_id"`
		CredentialID   uuid.UUID    `json:"credential_id"`
		FormulaID      uuid.UUID    `json:"formula_version_id"`
		PriceID        uuid.UUID    `json:"price_version_id"`
		Price          PriceVersion `json:"pricing_snapshot"`
		Provider       string       `json:"provider_id"`
		Adapter        string       `json:"adapter_version"`
		WalletOwner    string       `json:"wallet_owner_type"`
	}
	if json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil || !expiryDigest(snapshot.RequestSHA) || snapshot.ModelID != *usage.ModelID || snapshot.CatalogVersion != *usage.CatalogVersion || snapshot.CapabilityID != *usage.CapabilityVersionID || snapshot.ProtocolID != *usage.ProtocolDefinitionID || snapshot.RouteGeneration != *usage.RouteGeneration || snapshot.Protocol != "CHAT" || snapshot.ProtocolVersion != 1 || snapshot.Context != "PERSONAL" || snapshot.OrganizationID != nil || snapshot.WalletID != usage.WalletID || snapshot.TokenAccountID != usage.TokenAccountID || snapshot.CredentialID != usage.CredentialID || snapshot.PriceID != *usage.PriceVersionID || snapshot.PriceID != envelope.Price.ID || snapshot.FormulaID != envelope.Price.FormulaVersionID || snapshot.Estimator.Code != "UTF8_BYTES_DIV4_CEIL" || snapshot.Estimator.Version != 1 || snapshot.WalletOwner != "USER" {
		return false
	}
	storedPrice, _ := json.Marshal(snapshot.Price)
	envelopePrice, _ := json.Marshal(envelope.Price)
	if !expiryJSONFields(fields["pricing_snapshot"], []string{"price_version_id", "model", "input_per_million", "output_per_million", "cached_input_per_million", "scope_type", "protocol_code", "formula_version_id", "formula_snapshot", "credit_unit", "effective_from", "resolved_at", "units"}) || envelope.ContextType != snapshot.Context || envelope.OrganizationID != nil || envelope.CanPurchaseOrganizationEntitlements != snapshot.Purchase || envelope.Stream != snapshot.Stream || envelope.MaximumCost != usage.MaximumMicrocredits || envelope.MaximumOutput != snapshot.MaximumOutput || snapshot.UpstreamModel == "" || strings.TrimSpace(snapshot.Provider) == "" || strings.TrimSpace(snapshot.Adapter) == "" || !strings.EqualFold(envelope.Provider, snapshot.Provider) || envelope.AdapterVersion != snapshot.Adapter || len(envelope.Credential) == 0 || ValidatePriceFormula(envelope.Price) != nil || !expiryJSONEqual(storedPrice, envelopePrice) {
		return false
	}
	endpoint, err := url.Parse(envelope.BaseURL)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" || endpoint.User != nil {
		return false
	}
	var request chatCompletionRequest
	if json.Unmarshal(envelope.RequestBody, &request) != nil || request.Model != snapshot.UpstreamModel || request.Stream != snapshot.Stream {
		return false
	}
	maximum := request.MaxCompletionTokens
	if maximum == 0 {
		maximum = request.MaxTokens
	}
	if maximum != snapshot.MaximumOutput {
		return false
	}
	measured, err := EstimateChatUsage(envelope.RequestBody, maximum)
	if err != nil || measured.InputTokens != snapshot.EstimatedInput {
		return false
	}
	cost, err := Cost(envelope.Price, measured)
	return err == nil && cost == usage.MaximumMicrocredits
}

func expiryDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func expiryJSONFields(payload []byte, required []string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return false
	}
	for _, key := range required {
		if len(fields[key]) == 0 || (key != "units" && bytes.Equal(fields[key], []byte("null"))) {
			return false
		}
	}
	return true
}

func expiryJSONEqual(left, right []byte) bool {
	decode := func(body []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	l, le := decode(left)
	r, re := decode(right)
	return le == nil && re == nil && reflect.DeepEqual(l, r)
}

// Only intact expiry-owned unused dispatches are protected. Legacy MODEL or
// LOCAL_PRE_DISPATCH takeover layouts retain their existing recovery paths.
// This predicate is shared by queue selection and the locked command recheck.
var pendingUnusedExpiryFenceSQL = `COALESCE((
 request.organization_id IS NULL AND request.canonicalization_version=1
 AND request.idempotency_namespace='model-inference' AND request.maximum_microcredits>=0
 AND request.hold_id IS NOT NULL AND request.cancel_requested_at IS NOT NULL
 AND request.cancel_status='CANCEL_REQUESTED'
 AND ((request.execution_status='HOLD_RESERVED' AND request.status='RESERVED' AND request.settlement_status='HOLD_ACTIVE')
   OR (request.execution_status='REVIEW' AND request.status='REVIEW' AND request.settlement_status='REVIEW' AND request.error_class='cancel_hold_recovery_unconfirmed'))
 AND EXISTS(SELECT 1 FROM usage_dispatch_intents intent WHERE intent.usage_request_id=request.id
   AND intent.worker_acquired_at IS NULL AND intent.worker_lease_owner IS NULL AND intent.worker_lease_until IS NULL
   AND NOT intent.adapter_supports_idempotency AND octet_length(intent.encrypted_envelope)>0 AND btrim(intent.key_id)<>''
   AND intent.upstream_idempotency_key='pinvou-'||request.id::text
   AND intent.envelope_integrity=encode(sha256(intent.encrypted_envelope),'hex'))
 AND EXISTS(SELECT 1 FROM LATERAL (
   SELECT first_expiry.* FROM usage_cancellation_evidence first_expiry WHERE first_expiry.usage_request_id=request.id
   AND first_expiry.evidence_type='CANCEL_REQUESTED' AND first_expiry.adapter_version='hold-expiry-v1'
   ORDER BY first_expiry.occurred_at,first_expiry.id LIMIT 1) expiry
   WHERE expiry.provider_id IS NULL AND expiry.upstream_reference_digest IS NULL
   AND jsonb_typeof(expiry.sanitized_evidence->'expires_at')='string'
   AND expiry.sanitized_evidence->>'expires_at' ~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$'
   AND CASE WHEN pg_input_is_valid(expiry.sanitized_evidence->>'expires_at','timestamp with time zone')
     THEN (expiry.sanitized_evidence->>'expires_at')::timestamptz<>timestamptz '0001-01-01 00:00:00+00'
     ELSE false END
   AND expiry.sanitized_evidence=jsonb_build_object('usage_request_id',request.id,'hold_id',request.hold_id,'wallet_id',request.wallet_id,'max_microcredits',request.maximum_microcredits::text,'expires_at',expiry.sanitized_evidence->'expires_at'))
 AND ((request.cancellation_origin='NONE' AND request.delivery_status='NOT_STARTED' AND request.user_stopped_at IS NULL)
 OR (request.cancellation_origin='USER' AND request.delivery_status='USER_STOPPED' AND request.cancel_requested_at=request.user_stopped_at
   AND EXISTS(SELECT 1 FROM usage_cancel_commands command
    JOIN usage_cancellation_evidence stopped ON stopped.usage_request_id=command.usage_request_id
    WHERE command.usage_request_id=request.id AND command.user_id=request.user_id AND command.cancellation_version=1
     AND command.request_fingerprint=encode(sha256(decode('` + hex.EncodeToString([]byte(cancellationFingerprintDomain)) + `','hex')||convert_to(request.id::text,'UTF8')),'hex')
     AND command.created_at=request.user_stopped_at AND stopped.occurred_at=command.created_at
     AND stopped.evidence_type='UNSUPPORTED' AND stopped.adapter_version=request.request_snapshot->>'adapter_version'
     AND stopped.provider_id IS NULL AND stopped.upstream_reference_digest IS NULL
     AND stopped.sanitized_evidence=jsonb_build_object('capability','UNSUPPORTED','pre_dispatch',false))))
),false)`

func pendingUnusedExpiryFenceTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT `+pendingUnusedExpiryFenceSQL+` FROM usage_requests request WHERE request.id=$1`, id).Scan(&pending)
	return pending, err
}

// A USER stop of an intact, never-acquired intent still owns its original
// expiry recovery even before Billing writes its first fence. This predicate
// only defers MODEL takeover; it never grants a financial release proof.
var pendingUserUnusedIntentSQL = `COALESCE((
 request.organization_id IS NULL AND request.canonicalization_version=1
 AND request.idempotency_namespace='model-inference'
 AND request.execution_status='DISPATCH_INTENT_PERSISTED'
 AND request.status='RESERVED' AND request.settlement_status='HOLD_ACTIVE'
 AND request.hold_id IS NOT NULL AND request.maximum_microcredits>=0
 AND request.cancellation_origin='USER' AND request.cancel_status='UNSUPPORTED'
 AND request.delivery_status='USER_STOPPED' AND request.cancel_requested_at=request.user_stopped_at
 AND request.provider_terminal_at IS NULL AND request.final_usage_observed_at IS NULL
 AND request.actual_microcredits IS NULL AND request.last_persisted_sequence=0 AND request.last_client_confirmed_sequence=0
 AND NOT EXISTS(SELECT 1 FROM model_emergency_cancellations cancellation
  JOIN model_emergency_cancellation_evidence evidence ON evidence.emergency_cancellation_id=cancellation.id
  WHERE cancellation.usage_request_id=request.id AND evidence.evidence_type='REQUESTED')
 AND EXISTS(SELECT 1 FROM usage_dispatch_intents intent WHERE intent.usage_request_id=request.id
  AND intent.worker_acquired_at IS NULL AND intent.worker_lease_owner IS NULL AND intent.worker_lease_until IS NULL
  AND NOT intent.adapter_supports_idempotency AND octet_length(intent.encrypted_envelope)>0 AND btrim(intent.key_id)<>''
  AND intent.upstream_idempotency_key='pinvou-'||request.id::text
  AND intent.envelope_integrity=encode(sha256(intent.encrypted_envelope),'hex'))
 AND EXISTS(SELECT 1 FROM usage_cancel_commands command
  JOIN usage_cancellation_evidence stopped ON stopped.usage_request_id=command.usage_request_id
  WHERE command.usage_request_id=request.id AND command.user_id=request.user_id AND command.cancellation_version=1
   AND command.request_fingerprint=encode(sha256(decode('` + hex.EncodeToString([]byte(cancellationFingerprintDomain)) + `','hex')||convert_to(request.id::text,'UTF8')),'hex')
   AND command.created_at=request.user_stopped_at AND stopped.occurred_at=command.created_at
   AND stopped.evidence_type='UNSUPPORTED' AND stopped.adapter_version=request.request_snapshot->>'adapter_version'
   AND stopped.provider_id IS NULL AND stopped.upstream_reference_digest IS NULL
   AND stopped.sanitized_evidence=jsonb_build_object('capability','UNSUPPORTED','pre_dispatch',false))
 AND EXISTS(SELECT 1 FROM usage_convergence_evidence stopped WHERE stopped.usage_request_id=request.id
  AND stopped.evidence_type='CANCELLATION' AND stopped.source_type='GATEWAY' AND stopped.decision='RECORDED'
  AND stopped.evidence_key='requested:UNSUPPORTED' AND stopped.provider_id IS NULL AND stopped.adapter_version IS NULL
  AND stopped.upstream_reference_digest IS NULL AND stopped.observed_at=request.user_stopped_at
  AND stopped.sanitized_snapshot=jsonb_build_object('cancel_status','UNSUPPORTED','capability','UNSUPPORTED','pre_dispatch',false,'provider_success_inferred',false))
),false)`
