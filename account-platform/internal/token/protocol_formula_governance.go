package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExecuteProtocolFormulaTokenCommand is the sole human-write adapter for the
// four protocol/formula commands after governance activation. Historical
// resource-specific write endpoints are not registered; the governed command
// dispatcher reaches these transaction adapters directly.
func (s *Service) ExecuteProtocolFormulaTokenCommand(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	if repository == nil || !isProtocolFormulaGovernedCommand(input.Envelope.CommandType) || input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || strings.TrimSpace(input.ActorPermission) == "" || strings.TrimSpace(input.AuditRequestID) == "" || len(input.RequestDigest) != 64 || len(input.KeyDigest) != 64 || len(input.IdempotencyFingerprint) != 64 {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	if input.Envelope.CommandType == "TOKEN_FORMULA_PUBLISH" {
		return s.executeGovernedFormulaPublish(ctx, repository, input)
	}
	return s.executeGovernedProtocolFormulaSingleTx(ctx, repository, input)
}

func (s *Service) executeGovernedProtocolFormulaSingleTx(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err := lockProtocolFormulaCommandTarget(ctx, tx, input.Envelope); err != nil {
		return TokenCommandResult{}, false, err
	}
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	before, err := s.protocolFormulaPrecheckInTx(ctx, tx, input.Envelope)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err := repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	reference := uuid.Must(uuid.NewV7())
	if err := insertProtocolFormulaPending(ctx, tx, input, reference, before); err != nil {
		return TokenCommandResult{}, false, err
	}
	after, redacted, err := repository.applyProtocolFormulaDomainTx(ctx, tx, input.Envelope, nil)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, input.Envelope, reference, input.IdempotencyFingerprint, before, after, redacted)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return item, true, tx.Commit(ctx)
}

func (s *Service) executeGovernedFormulaPublish(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	// Persist the canonical identity before crossing into Billing. This is the
	// T1 recovery anchor: a retry can only resume the same actor/type/key and
	// exact payload, and Billing's immutable binding PUT is existing-first.
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		if existing.Status != "PENDING" {
			return existing, false, tx.Commit(ctx)
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenCommandResult{}, false, err
		}
		return s.resumeGovernedFormulaPublishResult(ctx, repository, input, existing.CommandReference, false)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err := lockProtocolFormulaCommandTarget(ctx, tx, input.Envelope); err != nil {
		return TokenCommandResult{}, false, err
	}
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return TokenCommandResult{}, false, err
		}
		if existing.Status != "PENDING" {
			return existing, false, nil
		}
		return s.resumeGovernedFormulaPublishResult(ctx, repository, input, existing.CommandReference, false)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	before, err := s.protocolFormulaPrecheckInTx(ctx, tx, input.Envelope)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err := repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	reference := uuid.Must(uuid.NewV7())
	if err := insertProtocolFormulaPending(ctx, tx, input, reference, before); err != nil {
		return TokenCommandResult{}, false, err
	}
	if err := repository.commitProtocolFormulaT1(ctx, tx); err != nil {
		// ErrTxCommitRollback is PostgreSQL's explicit proof that the transaction
		// did not commit. Every other commit error is ambiguous: retain the
		// generated reference so the caller can only recover by actor-scoped GET.
		if errors.Is(err, pgx.ErrTxCommitRollback) {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{CommandReference: reference, Status: "PENDING"}, true, err
	}
	return s.resumeGovernedFormulaPublishResult(ctx, repository, input, reference, true)
}

func (r *PostgresRepository) commitProtocolFormulaT1(ctx context.Context, tx pgx.Tx) error {
	if r.protocolFormulaT1Commit != nil {
		return r.protocolFormulaT1Commit(ctx, tx)
	}
	return tx.Commit(ctx)
}

func (s *Service) resumeGovernedFormulaPublishResult(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin, reference uuid.UUID, created bool) (TokenCommandResult, bool, error) {
	item, resumedCreated, err := s.resumeGovernedFormulaPublish(ctx, repository, input, reference, created)
	if err == nil {
		return item, resumedCreated, nil
	}
	pending, loadErr := repository.TokenCommandResult(ctx, reference, input.ActorUserID)
	if loadErr != nil {
		return TokenCommandResult{}, false, err
	}
	if stableCode, deterministic := protocolFormulaTerminalFailure(err); deterministic && pending.Status == "PENDING" {
		final, finalErr := repository.CompleteTokenCommand(ctx, reference, "FAILED", nil, nil, stableCode)
		if finalErr == nil {
			return final, created, err
		}
	}
	return pending, created, err
}

func protocolFormulaTerminalFailure(err error) (string, bool) {
	var billingErr *BillingHTTPError
	switch {
	case errors.As(err, &billingErr) && billingErr.Status == http.StatusConflict:
		return "BILLING_BINDING_CONFLICT", true
	case errors.As(err, &billingErr) && (billingErr.Status == http.StatusBadRequest || billingErr.Status == http.StatusNotFound || billingErr.Status == http.StatusUnprocessableEntity):
		return "FORMULA_UNIT_VALIDATION_REJECTED", true
	case errors.Is(err, ErrCatalogVersionConflict), errors.Is(err, ErrProtocolHeadVersionConflict), errors.Is(err, ErrFormulaHeadVersionConflict), errors.Is(err, ErrProtocolDraftETagMismatch), errors.Is(err, ErrFormulaDraftETagMismatch), errors.Is(err, ErrProtocolFormulaPrecheck), errors.Is(err, ErrNotFound):
		return "COMMAND_PRECHECK_CHANGED", true
	default:
		return "", false
	}
}

func (r *PostgresRepository) pendingFormulaPublishCommand(ctx context.Context, reference, actor uuid.UUID) (runtimeTokenCommandBegin, error) {
	var input runtimeTokenCommandBegin
	var status string
	err := r.pool.QueryRow(ctx, `SELECT command_type,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,audit_request_id,status FROM token_command_results WHERE command_reference=$1 AND actor_user_id=$2`, reference, actor).Scan(
		&input.Envelope.CommandType, &input.ActorUserID, &input.ActorPermission, &input.AuthorizationVersion, &input.KeyDigest, &input.IdempotencyFingerprint, &input.RequestDigest, &input.Envelope.TargetResourceType, &input.Envelope.TargetResourceID, &input.Envelope.Reason, &input.Envelope.ExpectedVersions, &input.Envelope.PrecheckFingerprint, &input.AuditRequestID, &status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeTokenCommandBegin{}, ErrNotFound
	}
	if err != nil {
		return runtimeTokenCommandBegin{}, err
	}
	if input.Envelope.CommandType != "TOKEN_FORMULA_PUBLISH" || status != "PENDING" {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	var expected struct {
		FormulaVersionID       uuid.UUID `json:"formula_version_id"`
		ExpectedFormulaVersion int64     `json:"expected_formula_version"`
		DraftETag              string    `json:"draft_etag"`
		RequestFingerprint     string    `json:"request_fingerprint"`
	}
	if decodeCommand(input.Envelope.ExpectedVersions, &expected) != nil || expected.FormulaVersionID == uuid.Nil || expected.FormulaVersionID.String() != input.Envelope.TargetResourceID || expected.ExpectedFormulaVersion < 1 || !validCommandETag(expected.DraftETag) || expected.RequestFingerprint == "" {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	input.Envelope.Command, _ = json.Marshal(map[string]any{"expected_formula_version": expected.ExpectedFormulaVersion, "draft_etag": expected.DraftETag})
	canonical, err := canonicalizeProtocolFormulaEnvelope(input.Envelope)
	if err != nil {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	input.Envelope = canonical
	var facts map[string]any
	if decodeExactCommandJSON(input.Envelope.ExpectedVersions, &facts) != nil || facts == nil || governedRequestFingerprint(input.Envelope, facts) != expected.RequestFingerprint {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	canonicalRequest, err := json.Marshal(input.Envelope)
	keyDigest, keyErr := hex.DecodeString(input.KeyDigest)
	if err != nil || keyErr != nil || len(keyDigest) != sha256.Size {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	requestHash := sha256.Sum256(canonicalRequest)
	if hex.EncodeToString(requestHash[:]) != input.RequestDigest {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	fingerprintInput := append(append([]byte("pinvou-token-command-v1\x00"), keyDigest...), requestHash[:]...)
	fingerprintHash := sha256.Sum256(fingerprintInput)
	if hex.EncodeToString(fingerprintHash[:]) != input.IdempotencyFingerprint {
		return runtimeTokenCommandBegin{}, ErrTokenCommandInvalid
	}
	return input, nil
}

func canonicalizeProtocolFormulaCommand(commandType string, raw json.RawMessage) (json.RawMessage, error) {
	var command any
	switch commandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		var value struct {
			ProtocolCode  string          `json:"protocol_code"`
			RequestSchema json.RawMessage `json:"request_schema"`
		}
		if decodeCommand(raw, &value) != nil {
			return nil, ErrTokenCommandInvalid
		}
		value.ProtocolCode = strings.ToUpper(strings.TrimSpace(value.ProtocolCode))
		schema, ok := canonicalJSONObject(value.RequestSchema)
		if value.ProtocolCode == "" || !ok {
			return nil, ErrTokenCommandInvalid
		}
		value.RequestSchema = schema
		command = value
	case "TOKEN_PROTOCOL_PUBLISH":
		var value struct {
			ExpectedProtocolVersion int64  `json:"expected_protocol_version"`
			DraftETag               string `json:"draft_etag"`
		}
		if decodeCommand(raw, &value) != nil || value.ExpectedProtocolVersion < 1 || !validCommandETag(value.DraftETag) {
			return nil, ErrTokenCommandInvalid
		}
		value.DraftETag = strings.ToLower(strings.TrimSpace(value.DraftETag))
		command = value
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var value struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(raw, &value) != nil {
			return nil, ErrTokenCommandInvalid
		}
		required, expression, err := normalizeSafeFormula(value.RequiredUnitVersions, value.Expression)
		if err != nil {
			return nil, ErrTokenCommandInvalid
		}
		value.RequiredUnitVersions, value.Expression = required, expression
		command = value
	case "TOKEN_FORMULA_PUBLISH":
		var value struct {
			ExpectedFormulaVersion int64  `json:"expected_formula_version"`
			DraftETag              string `json:"draft_etag"`
		}
		if decodeCommand(raw, &value) != nil || value.ExpectedFormulaVersion < 1 || !validCommandETag(value.DraftETag) {
			return nil, ErrTokenCommandInvalid
		}
		value.DraftETag = strings.ToLower(strings.TrimSpace(value.DraftETag))
		command = value
	default:
		return nil, ErrTokenCommandInvalid
	}
	canonicalCommand, err := json.Marshal(command)
	if err != nil {
		return nil, ErrTokenCommandInvalid
	}
	return canonicalCommand, nil
}

func canonicalizeProtocolFormulaEnvelope(envelope TokenCommandEnvelope) (TokenCommandEnvelope, error) {
	if !isProtocolFormulaGovernedCommand(envelope.CommandType) {
		return envelope, nil
	}
	canonicalCommand, err := canonicalizeProtocolFormulaCommand(envelope.CommandType, envelope.Command)
	if err != nil {
		return TokenCommandEnvelope{}, err
	}
	envelope.Command = canonicalCommand

	requestFingerprint := ""
	expectedTarget := ""
	var facts any
	switch envelope.CommandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		var value struct {
			ProtocolCode                string `json:"protocol_code"`
			ExpectedProtocolHeadVersion int64  `json:"expected_protocol_head_version"`
			RequestFingerprint          string `json:"request_fingerprint"`
		}
		if decodeCommand(envelope.ExpectedVersions, &value) != nil || value.ExpectedProtocolHeadVersion < 0 {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		value.ProtocolCode = strings.ToUpper(strings.TrimSpace(value.ProtocolCode))
		expectedTarget = value.ProtocolCode
		requestFingerprint = strings.ToLower(strings.TrimSpace(value.RequestFingerprint))
		value.RequestFingerprint = requestFingerprint
		facts = value
	case "TOKEN_PROTOCOL_PUBLISH":
		var value struct {
			ProtocolDefinitionID     uuid.UUID `json:"protocol_definition_id"`
			ExpectedProtocolVersion  int64     `json:"expected_protocol_version"`
			DraftETag                string    `json:"draft_etag"`
			RequestFingerprint       string    `json:"request_fingerprint"`
			FormulaSetSHA256         string    `json:"formula_set_sha256"`
			ModelDraftSetSHA256      string    `json:"model_draft_set_sha256"`
			ModelDraftReferenceCount int64     `json:"model_draft_reference_count"`
		}
		if decodeCommand(envelope.ExpectedVersions, &value) != nil || value.ProtocolDefinitionID == uuid.Nil || value.ExpectedProtocolVersion < 1 || !validCommandETag(value.DraftETag) || value.ModelDraftReferenceCount < 0 {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		value.DraftETag = strings.ToLower(strings.TrimSpace(value.DraftETag))
		expectedTarget = value.ProtocolDefinitionID.String()
		value.FormulaSetSHA256 = strings.ToLower(strings.TrimSpace(value.FormulaSetSHA256))
		value.ModelDraftSetSHA256 = strings.ToLower(strings.TrimSpace(value.ModelDraftSetSHA256))
		if formulaDigest, formulaErr := hex.DecodeString(value.FormulaSetSHA256); formulaErr != nil || len(formulaDigest) != sha256.Size {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		if draftDigest, draftErr := hex.DecodeString(value.ModelDraftSetSHA256); draftErr != nil || len(draftDigest) != sha256.Size {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		requestFingerprint = strings.ToLower(strings.TrimSpace(value.RequestFingerprint))
		value.RequestFingerprint = requestFingerprint
		facts = value
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var value struct {
			ProtocolDefinitionID       uuid.UUID `json:"protocol_definition_id"`
			ExpectedProtocolVersion    int64     `json:"expected_protocol_version"`
			ExpectedFormulaHeadVersion int64     `json:"expected_formula_head_version"`
			RequestFingerprint         string    `json:"request_fingerprint"`
		}
		if decodeCommand(envelope.ExpectedVersions, &value) != nil || value.ProtocolDefinitionID == uuid.Nil || value.ExpectedProtocolVersion < 1 || value.ExpectedFormulaHeadVersion < 0 {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		expectedTarget = value.ProtocolDefinitionID.String()
		requestFingerprint = strings.ToLower(strings.TrimSpace(value.RequestFingerprint))
		value.RequestFingerprint = requestFingerprint
		facts = value
	case "TOKEN_FORMULA_PUBLISH":
		var value struct {
			FormulaVersionID       uuid.UUID `json:"formula_version_id"`
			ExpectedFormulaVersion int64     `json:"expected_formula_version"`
			DraftETag              string    `json:"draft_etag"`
			RequestFingerprint     string    `json:"request_fingerprint"`
		}
		if decodeCommand(envelope.ExpectedVersions, &value) != nil || value.FormulaVersionID == uuid.Nil || value.ExpectedFormulaVersion < 1 || !validCommandETag(value.DraftETag) {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		value.DraftETag = strings.ToLower(strings.TrimSpace(value.DraftETag))
		expectedTarget = value.FormulaVersionID.String()
		requestFingerprint = strings.ToLower(strings.TrimSpace(value.RequestFingerprint))
		value.RequestFingerprint = requestFingerprint
		facts = value
	}
	requestDigest, requestErr := hex.DecodeString(requestFingerprint)
	if requestErr != nil || len(requestDigest) != sha256.Size || expectedTarget != envelope.TargetResourceID {
		return TokenCommandEnvelope{}, ErrTokenCommandInvalid
	}
	envelope.ExpectedVersions, err = json.Marshal(facts)
	if err != nil {
		return TokenCommandEnvelope{}, ErrTokenCommandInvalid
	}
	return envelope, nil
}

func (s *Service) resumeGovernedFormulaPublish(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin, reference uuid.UUID, created bool) (TokenCommandResult, bool, error) {
	id, err := uuid.Parse(input.Envelope.TargetResourceID)
	if err != nil {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	draft, err := repository.FormulaDraft(ctx, id)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	required, snapshotDigest := formulaBindingSnapshot(draft)
	snapshot, _ := json.Marshal(map[string]any{"evaluator_version": draft.EvaluatorVersion, "expression": draft.Expression})
	client, ok := s.billing.(FormulaBindingClient)
	if !ok {
		return TokenCommandResult{}, false, errors.New("billing formula binding unavailable")
	}
	receipt, err := client.RegisterFormulaBinding(ctx, id, draft.ProtocolCode, required, snapshot)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if receipt.FormulaVersionID != id || receipt.Status != "REGISTERED" || strings.ToUpper(strings.TrimSpace(receipt.ProtocolCode)) != draft.ProtocolCode || receipt.FormulaSnapshotSHA256 != snapshotDigest || receipt.PublishedAt.IsZero() || !uuidSlicesEqual(required, receipt.RequiredMeteringUnitDefinitionIDs) {
		return TokenCommandResult{}, false, ErrTokenCommandResultMismatch
	}
	binding := &FormulaBindingSync{FormulaVersionID: receipt.FormulaVersionID, BillingStatus: receipt.Status, ProtocolCode: receipt.ProtocolCode, RequiredUnitVersions: receipt.RequiredMeteringUnitDefinitionIDs, FormulaSnapshotSHA256: receipt.FormulaSnapshotSHA256, PublishedAt: receipt.PublishedAt.UTC()}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	existing, err := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if existing.CommandReference != reference {
		return TokenCommandResult{}, false, ErrTokenCommandResultMismatch
	}
	if existing.Status != "PENDING" {
		return existing, false, tx.Commit(ctx)
	}
	if err := lockProtocolFormulaCommandTarget(ctx, tx, input.Envelope); err != nil {
		return TokenCommandResult{}, false, err
	}
	before, err := s.protocolFormulaPrecheckInTx(ctx, tx, input.Envelope)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err := repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	after, redacted, err := repository.applyProtocolFormulaDomainTx(ctx, tx, input.Envelope, binding)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, input.Envelope, reference, input.IdempotencyFingerprint, before, after, redacted)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return item, created, tx.Commit(ctx)
}

func insertProtocolFormulaPending(ctx context.Context, tx pgx.Tx, input runtimeTokenCommandBegin, reference uuid.UUID, before json.RawMessage) error {
	policy, err := approvalpolicy.Lookup(input.Envelope.CommandType)
	if err != nil || policy.Mode != approvalpolicy.AuditOnly {
		return ErrTokenCommandInvalid
	}
	result, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',$15,$16) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, reference, input.Envelope.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, input.KeyDigest, input.IdempotencyFingerprint, input.RequestDigest, input.Envelope.TargetResourceType, input.Envelope.TargetResourceID, input.Envelope.Reason, input.Envelope.ExpectedVersions, input.Envelope.PrecheckFingerprint, before, input.AuditRequestID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrTokenCommandResultMismatch
	}
	return nil
}

func lockProtocolFormulaCommandTarget(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope) error {
	switch envelope.CommandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		return lockTokenKey(ctx, tx, "protocol", envelope.TargetResourceID)
	case "TOKEN_PROTOCOL_PUBLISH":
		id, err := uuid.Parse(envelope.TargetResourceID)
		if err != nil {
			return ErrTokenCommandInvalid
		}
		var locked uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE id=$1 FOR UPDATE`, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status IN ('DRAFT','PUBLISHED') ORDER BY id FOR SHARE`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ignored uuid.UUID
			if err := rows.Scan(&ignored); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var code string
		var version int64
		if err := tx.QueryRow(ctx, `SELECT protocol_code,version FROM protocol_definitions WHERE id=$1`, id).Scan(&code, &version); err != nil {
			return err
		}
		modelRows, err := tx.Query(ctx, `SELECT model_id FROM model_catalog_drafts WHERE protocol_code=$1 AND protocol_version=$2 ORDER BY model_id FOR SHARE`, code, version)
		if err != nil {
			return err
		}
		defer modelRows.Close()
		for modelRows.Next() {
			var ignored uuid.UUID
			if err := modelRows.Scan(&ignored); err != nil {
				return err
			}
		}
		return modelRows.Err()
	case "TOKEN_FORMULA_DRAFT_CREATE":
		id, err := uuid.Parse(envelope.TargetResourceID)
		if err != nil {
			return ErrTokenCommandInvalid
		}
		if err := lockTokenKey(ctx, tx, "formula", id.String()); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE id=$1 FOR SHARE`, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else {
			return err
		}
	case "TOKEN_FORMULA_PUBLISH":
		id, err := uuid.Parse(envelope.TargetResourceID)
		if err != nil {
			return ErrTokenCommandInvalid
		}
		var locked, protocolID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id,protocol_definition_id FROM protocol_formula_versions WHERE id=$1 FOR UPDATE`, id).Scan(&locked, &protocolID); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE id=$1 FOR SHARE`, protocolID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else {
			return err
		}
	default:
		return ErrTokenCommandInvalid
	}
}

type protocolPublicationFacts struct {
	Protocol                 ProtocolDefinition
	Formulas                 []ProtocolFormulaVersion
	FormulaSetSHA256         string
	ModelDraftSetSHA256      string
	ModelDraftReferenceCount int64
}

func loadProtocolPublicationFacts(ctx context.Context, tx pgx.Tx, id uuid.UUID) (protocolPublicationFacts, error) {
	var facts protocolPublicationFacts
	if err := tx.QueryRow(ctx, `SELECT id,protocol_code,version,status,request_schema,created_at,published_at FROM protocol_definitions WHERE id=$1`, id).Scan(&facts.Protocol.ID, &facts.Protocol.ProtocolCode, &facts.Protocol.Version, &facts.Protocol.Status, &facts.Protocol.RequestSchema, &facts.Protocol.CreatedAt, &facts.Protocol.PublishedAt); errors.Is(err, pgx.ErrNoRows) {
		return facts, ErrNotFound
	} else if err != nil {
		return facts, err
	}
	if schema, ok := canonicalJSONObject(facts.Protocol.RequestSchema); !ok {
		return facts, ErrProtocolFormulaPrecheck
	} else {
		facts.Protocol.RequestSchema = schema
	}
	facts.Protocol.ETag = protocolDefinitionETag(facts.Protocol)
	rows, err := tx.Query(ctx, `SELECT id,protocol_definition_id,$2,version,status,required_unit_versions,expression,evaluator_version,created_at,published_at FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status IN ('DRAFT','PUBLISHED') ORDER BY id`, id, facts.Protocol.ProtocolCode)
	if err != nil {
		return facts, err
	}
	for rows.Next() {
		var formula ProtocolFormulaVersion
		var required, expression []byte
		if err := rows.Scan(&formula.ID, &formula.ProtocolDefinitionID, &formula.ProtocolCode, &formula.Version, &formula.Status, &required, &expression, &formula.EvaluatorVersion, &formula.CreatedAt, &formula.PublishedAt); err != nil {
			rows.Close()
			return facts, err
		}
		if json.Unmarshal(required, &formula.RequiredUnitVersions) != nil || json.Unmarshal(expression, &formula.Expression) != nil || formula.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || validateSafeFormula(formula.RequiredUnitVersions, formula.Expression) != nil {
			rows.Close()
			return facts, ErrProtocolFormulaPrecheck
		}
		facts.Formulas = append(facts.Formulas, formula)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return facts, err
	}
	rows.Close()
	facts.FormulaSetSHA256 = protocolPublicationFormulaDigest(facts.Formulas)
	modelRows, err := tx.Query(ctx, `SELECT model_id FROM model_catalog_drafts WHERE protocol_code=$1 AND protocol_version=$2 ORDER BY model_id`, facts.Protocol.ProtocolCode, facts.Protocol.Version)
	if err != nil {
		return facts, err
	}
	modelIDs := make([]string, 0)
	for modelRows.Next() {
		var modelID uuid.UUID
		if err := modelRows.Scan(&modelID); err != nil {
			modelRows.Close()
			return facts, err
		}
		modelIDs = append(modelIDs, modelID.String())
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return facts, err
	}
	modelRows.Close()
	facts.ModelDraftReferenceCount = int64(len(modelIDs))
	digest := sha256.Sum256([]byte(strings.Join(modelIDs, "\x00")))
	facts.ModelDraftSetSHA256 = hex.EncodeToString(digest[:])
	return facts, nil
}

func loadFormulaPublicationFacts(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ProtocolFormulaVersion, error) {
	var item ProtocolFormulaVersion
	var protocolStatus string
	var required, expression []byte
	err := tx.QueryRow(ctx, `SELECT f.id,f.protocol_definition_id,p.protocol_code,p.status,f.version,f.status,f.required_unit_versions,f.expression,f.evaluator_version,f.created_at,f.published_at FROM protocol_formula_versions f JOIN protocol_definitions p ON p.id=f.protocol_definition_id WHERE f.id=$1`, id).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &protocolStatus, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if protocolStatus != "PUBLISHED" || item.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || json.Unmarshal(required, &item.RequiredUnitVersions) != nil || json.Unmarshal(expression, &item.Expression) != nil || validateSafeFormula(item.RequiredUnitVersions, item.Expression) != nil {
		return item, ErrProtocolFormulaPrecheck
	}
	item.ETag = protocolFormulaETag(item)
	return item, nil
}

func (s *Service) protocolFormulaPrecheckInTx(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope) (json.RawMessage, error) {
	snapshot, err := loadCommandPrecheckSnapshot(ctx, tx, envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Blockers) > 0 {
		return nil, ErrProtocolFormulaPrecheck
	}
	_, billingValues, err := s.validateProtocolFormulaBillingPrecheckInTx(ctx, tx, envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil {
		return nil, err
	}
	snapshot.EvidenceValues = append(snapshot.EvidenceValues, billingValues...)
	expected, fingerprint, err := commandPrecheckFingerprint(envelope.CommandType, envelope.TargetResourceType, envelope.TargetResourceID, envelope.Command, snapshot)
	if err != nil {
		return nil, err
	}
	if envelope.PrecheckFingerprint != fingerprint || !tokenCommandExpectedVersionsMatch(envelope.ExpectedVersions, expected) {
		return nil, ErrCatalogVersionConflict
	}
	return expected, nil
}

// validateProtocolFormulaBillingPrecheckInTx derives every Token-owned unit
// binding from the same transaction that holds the command target. Billing is
// still an independent authority, but a second Token connection cannot observe
// a different formula set while the command is being revalidated.
func (s *Service) validateProtocolFormulaBillingPrecheckInTx(ctx context.Context, tx pgx.Tx, commandType, targetID string, raw json.RawMessage) (*time.Time, []string, error) {
	unitSets := []map[string]uuid.UUID{}
	switch commandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		return nil, nil, nil
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var command struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(raw, &command) != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		required, _, err := normalizeSafeFormula(command.RequiredUnitVersions, command.Expression)
		if err != nil {
			return nil, nil, ErrProtocolFormulaPrecheck
		}
		unitSets = append(unitSets, required)
	case "TOKEN_FORMULA_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil || id == uuid.Nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		var required map[string]uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT required_unit_versions FROM protocol_formula_versions WHERE id=$1`, id).Scan(&required); errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		} else if err != nil {
			return nil, nil, err
		}
		unitSets = append(unitSets, required)
	case "TOKEN_PROTOCOL_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil || id == uuid.Nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		rows, err := tx.Query(ctx, `SELECT required_unit_versions FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status IN ('DRAFT','PUBLISHED') ORDER BY id`, id)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var required map[string]uuid.UUID
			if err := rows.Scan(&required); err != nil {
				return nil, nil, err
			}
			unitSets = append(unitSets, required)
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, ErrTokenCommandInvalid
	}
	return s.validateFormulaUnitSets(ctx, unitSets)
}

func (r *PostgresRepository) applyProtocolFormulaDomainTx(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope, binding *FormulaBindingSync) (json.RawMessage, json.RawMessage, error) {
	var after map[string]any
	switch envelope.CommandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		var command struct {
			ProtocolCode  string          `json:"protocol_code"`
			RequestSchema json.RawMessage `json:"request_schema"`
		}
		if decodeCommand(envelope.Command, &command) != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		command.ProtocolCode = strings.ToUpper(strings.TrimSpace(command.ProtocolCode))
		canonicalSchema, ok := canonicalJSONObject(command.RequestSchema)
		if !ok {
			return nil, nil, ErrProtocolFormulaPrecheck
		}
		command.RequestSchema = canonicalSchema
		var head int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM protocol_definitions WHERE protocol_code=$1`, command.ProtocolCode).Scan(&head); err != nil {
			return nil, nil, err
		}
		id := uuid.Must(uuid.NewV7())
		now := r.protocolCommandNow()
		if _, err := tx.Exec(ctx, `INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5)`, id, command.ProtocolCode, head+1, command.RequestSchema, now); err != nil {
			return nil, nil, err
		}
		item := ProtocolDefinition{ID: id, ProtocolCode: command.ProtocolCode, Version: head + 1, Status: "DRAFT", RequestSchema: command.RequestSchema, CreatedAt: now}
		after = map[string]any{"protocol_definition_id": id, "protocol_code": command.ProtocolCode, "version": head + 1, "status": "DRAFT", "etag": protocolDefinitionETag(item)}
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var command struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(envelope.Command, &command) != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		required, expression, err := normalizeSafeFormula(command.RequiredUnitVersions, command.Expression)
		if err != nil {
			return nil, nil, ErrProtocolFormulaPrecheck
		}
		protocolID, _ := uuid.Parse(envelope.TargetResourceID)
		var code string
		var protocolVersion int64
		var status string
		if err := tx.QueryRow(ctx, `SELECT protocol_code,version,status FROM protocol_definitions WHERE id=$1`, protocolID).Scan(&code, &protocolVersion, &status); err != nil {
			return nil, nil, err
		}
		var head int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM protocol_formula_versions WHERE protocol_definition_id=$1`, protocolID).Scan(&head); err != nil {
			return nil, nil, err
		}
		id := uuid.Must(uuid.NewV7())
		now := r.protocolCommandNow()
		requiredJSON, _ := json.Marshal(required)
		expressionJSON, _ := json.Marshal(expression)
		if _, err := tx.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5,'SAFE_PARAMETER_MAP_V1',$6)`, id, protocolID, head+1, requiredJSON, expressionJSON, now); err != nil {
			return nil, nil, err
		}
		item := ProtocolFormulaVersion{ID: id, ProtocolDefinitionID: protocolID, ProtocolCode: code, Version: head + 1, Status: "DRAFT", RequiredUnitVersions: required, Expression: expression, EvaluatorVersion: "SAFE_PARAMETER_MAP_V1", CreatedAt: now}
		after = map[string]any{"formula_version_id": id, "protocol_definition_id": protocolID, "version": head + 1, "status": "DRAFT", "etag": protocolFormulaETag(item), "verified_unit_count": len(required)}
	case "TOKEN_PROTOCOL_PUBLISH":
		id, _ := uuid.Parse(envelope.TargetResourceID)
		facts, err := loadProtocolPublicationFacts(ctx, tx, id)
		if err != nil {
			return nil, nil, err
		}
		item := facts.Protocol
		now := r.protocolCommandNow()
		result, err := tx.Exec(ctx, `UPDATE protocol_definitions SET status='PUBLISHED',published_at=$2 WHERE id=$1 AND status='DRAFT'`, id, now)
		if err != nil || result.RowsAffected() != 1 {
			if err != nil {
				return nil, nil, err
			}
			return nil, nil, ErrCatalogVersionConflict
		}
		item.Status, item.PublishedAt = "PUBLISHED", &now
		after = map[string]any{"protocol_definition_id": id, "protocol_code": item.ProtocolCode, "version": item.Version, "status": "PUBLISHED", "etag": protocolDefinitionETag(item)}
	case "TOKEN_FORMULA_PUBLISH":
		if binding == nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		id, _ := uuid.Parse(envelope.TargetResourceID)
		item, err := loadFormulaPublicationFacts(ctx, tx, id)
		if err != nil {
			return nil, nil, err
		}
		required, digest := formulaBindingSnapshot(item)
		if binding.FormulaVersionID != id || binding.BillingStatus != "REGISTERED" || strings.ToUpper(strings.TrimSpace(binding.ProtocolCode)) != item.ProtocolCode || binding.FormulaSnapshotSHA256 != digest || !uuidSlicesEqual(required, binding.RequiredUnitVersions) || binding.PublishedAt.IsZero() {
			return nil, nil, ErrTokenCommandResultMismatch
		}
		result, err := tx.Exec(ctx, `UPDATE protocol_formula_versions SET status='PUBLISHED',published_at=$2 WHERE id=$1 AND status='DRAFT'`, id, binding.PublishedAt)
		if err != nil || result.RowsAffected() != 1 {
			if err != nil {
				return nil, nil, err
			}
			return nil, nil, ErrCatalogVersionConflict
		}
		item.Status, item.PublishedAt = "PUBLISHED", &binding.PublishedAt
		ids := append([]uuid.UUID(nil), binding.RequiredUnitVersions...)
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		after = map[string]any{"formula_version_id": id, "protocol_definition_id": item.ProtocolDefinitionID, "version": item.Version, "status": "PUBLISHED", "etag": protocolFormulaETag(item), "billing_sync": map[string]any{"status": "SYNCHRONIZED", "formula_version_id": id, "protocol_code": item.ProtocolCode, "required_metering_unit_definition_ids": ids, "formula_snapshot_sha256": binding.FormulaSnapshotSHA256, "published_at": binding.PublishedAt.UTC()}}
	default:
		return nil, nil, ErrTokenCommandInvalid
	}
	redacted, err := json.Marshal(after)
	if err != nil {
		return nil, nil, err
	}
	return redacted, redacted, nil
}
