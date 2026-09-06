package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrTokenCommandInvalid        = errors.New("token command is invalid")
	ErrTokenCommandResultMismatch = errors.New("token command result does not match the idempotency key")
	ErrTokenCommandResultFinal    = errors.New("token command result is already final")
)

type TokenCommandEnvelope struct {
	CommandType         string          `json:"command_type"`
	Reason              string          `json:"reason"`
	TargetResourceType  string          `json:"target_resource_type"`
	TargetResourceID    string          `json:"target_resource_id"`
	ExpectedVersions    json.RawMessage `json:"expected_versions"`
	PrecheckFingerprint string          `json:"precheck_fingerprint"`
	Command             json.RawMessage `json:"command"`
}

type TokenCommandResult struct {
	CommandReference       uuid.UUID       `json:"command_reference"`
	CommandType            string          `json:"command_type"`
	PolicyVersion          int64           `json:"policy_version"`
	ActorUserID            uuid.UUID       `json:"actor_user_id"`
	ActorPermission        string          `json:"actor_permission"`
	AuthorizationVersion   int64           `json:"authorization_version"`
	IdempotencyFingerprint string          `json:"idempotency_fingerprint"`
	TargetResourceType     string          `json:"target_resource_type"`
	TargetResourceID       string          `json:"target_resource_id"`
	Status                 string          `json:"status"`
	RedactedResult         json.RawMessage `json:"redacted_result,omitempty"`
	StableErrorCode        *string         `json:"stable_error_code,omitempty"`
	ApprovalRequestID      *uuid.UUID      `json:"approval_request_id,omitempty"`
	AuditRequestID         string          `json:"audit_request_id"`
	CreatedAt              time.Time       `json:"created_at"`
	CompletedAt            *time.Time      `json:"completed_at,omitempty"`
	RecoveryState          *string         `json:"recovery_state,omitempty"`
	RecoveryDiagnosticCode *string         `json:"recovery_diagnostic_code,omitempty"`
	RecoveryEpoch          *int64          `json:"recovery_epoch,omitempty"`
}

type tokenCommandSpec struct {
	Permissions   []string
	RecentAuth    bool
	Approval      bool
	RequiredFacts []string
}

var tokenCommandSpecs = map[string]tokenCommandSpec{
	"TOKEN_PROTOCOL_DRAFT_CREATE":      {[]string{"platform.pricing.manage"}, false, false, []string{"protocol_code", "expected_protocol_head_version", "request_fingerprint"}},
	"TOKEN_PROTOCOL_PUBLISH":           {[]string{"platform.pricing.manage"}, true, false, []string{"protocol_definition_id", "expected_protocol_version", "draft_etag", "request_fingerprint"}},
	"TOKEN_FORMULA_DRAFT_CREATE":       {[]string{"platform.pricing.manage"}, false, false, []string{"protocol_definition_id", "expected_protocol_version", "expected_formula_head_version", "request_fingerprint"}},
	"TOKEN_FORMULA_PUBLISH":            {[]string{"platform.pricing.manage"}, true, false, []string{"formula_version_id", "expected_formula_version", "draft_etag", "request_fingerprint"}},
	"TOKEN_MODEL_DRAFT_SAVE":           {[]string{"platform.models.manage"}, false, false, []string{"model_id", "expected_catalog_version", "expected_draft_version", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_MODEL_PUBLISH":              {[]string{"platform.models.manage"}, true, false, []string{"model_id", "expected_catalog_version", "expected_draft_version", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_MODEL_LIFECYCLE_CHANGE":     {[]string{"platform.models.lifecycle.request"}, true, true, nil},
	"TOKEN_MODEL_CATALOG_SYNC":         {[]string{"platform.models.sync"}, false, false, []string{"expected_catalog_version", "adapter_observed_version", "catalog_diff_fingerprint", "request_fingerprint"}},
	"TOKEN_MODEL_CATALOG_ROLLBACK":     {[]string{"platform.models.catalog.rollback.request"}, true, true, nil},
	"TOKEN_ACCOUNT_CREATE":             {[]string{"platform.tokens.manage", "platform.tokens.owner-candidates.read"}, true, false, []string{"owner_type", "owner_id", "owner_authorization_version", "owner_version", "owner_identity_digest", "owner_account_count", "owner_account_cap", "owner_account_cap_policy_version", "creation_version", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_ACCOUNT_STATUS_CHANGE":      {[]string{"platform.tokens.manage"}, true, false, []string{"token_account_id", "expected_account_version", "target_status", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_ROUTE_UPSERT":               {[]string{"platform.tokens.manage"}, true, false, []string{"token_account_id", "expected_account_version", "expected_route_version", "model_id", "protocol_version_id", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_ROUTE_DELETE":               {[]string{"platform.tokens.manage"}, true, false, []string{"token_account_id", "route_id", "expected_account_version", "expected_route_version", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_CREDENTIAL_ENROLL":          {[]string{"platform.tokens.manage"}, true, false, []string{"token_account_id", "expected_account_version", "credential_slot_version", "launch_fingerprint"}},
	"TOKEN_CREDENTIAL_ROTATE":          {[]string{"platform.tokens.manage"}, true, false, []string{"token_account_id", "expected_account_version", "active_credential_version", "launch_fingerprint"}},
	"TOKEN_CREDENTIAL_REVOKE":          {[]string{"platform.tokens.credentials.revoke.request"}, true, true, nil},
	"TOKEN_CREDENTIAL_VALIDATE":        {[]string{"platform.tokens.credentials.validate"}, true, false, []string{"token_account_id", "expected_account_version", "credential_id", "credential_version", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_ROUTE_PROVISIONING_REPAIR":  {[]string{"platform.tokens.manage"}, true, false, []string{"provisioning_id", "expected_provisioning_version", "target_generation", "precheck_fingerprint", "request_fingerprint"}},
	"TOKEN_USAGE_RECONCILIATION_RETRY": {[]string{"platform.usage.reconcile.retry"}, false, false, []string{"usage_request_id", "expected_usage_version", "evidence_high_watermark", "precheck_fingerprint", "request_fingerprint"}},
}

type tokenCommandBegin struct {
	Envelope             TokenCommandEnvelope
	ActorUserID          uuid.UUID
	ActorPermission      string
	AuthorizationVersion int64
	IdempotencyKey       string
	AuditRequestID       string
	BeforeState          json.RawMessage
	ApprovalRequestID    uuid.UUID
	ApprovalFingerprint  string
}

type ApprovedTokenCommandExecutor interface {
	PrecheckApprovedTokenCommand(context.Context, string, TokenCommandEnvelope) (beforeState json.RawMessage, err error)
	ExecuteApprovedTokenCommand(context.Context, string, string, TokenCommandEnvelope) (afterState, redactedResult json.RawMessage, stableErrorCode string, err error)
}

type approvedTokenCommandBody struct {
	ApprovalRequestID        uuid.UUID            `json:"approval_request_id"`
	RequesterUserID          uuid.UUID            `json:"requester_user_id"`
	AuthorizationVersion     int64                `json:"authorization_version"`
	CommandFingerprint       string               `json:"command_fingerprint"`
	ExecutionIdempotencyKey  string               `json:"execution_idempotency_key"`
	CriticalResourceVersions json.RawMessage      `json:"critical_resource_versions,omitempty"`
	FinancialSnapshot        json.RawMessage      `json:"financial_snapshot,omitempty"`
	Envelope                 TokenCommandEnvelope `json:"command"`
}

func exactEmptyFinancialSnapshot(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	return json.Unmarshal(raw, &fields) == nil && fields != nil && len(fields) == 0
}

func (r *PostgresRepository) BeginTokenCommand(ctx context.Context, input tokenCommandBegin) (TokenCommandResult, bool, error) {
	canonical, requestDigest, keyDigest, fingerprint, err := normalizeTokenCommand(input.Envelope, input.IdempotencyKey, input.ApprovalFingerprint)
	if err != nil || input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || strings.TrimSpace(input.ActorPermission) == "" || strings.TrimSpace(input.AuditRequestID) == "" || containsSensitiveCommandField(input.BeforeState) {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	policy, err := approvalpolicy.Lookup(canonical.CommandType)
	if err != nil || (input.ApprovalRequestID == uuid.Nil) != (strings.TrimSpace(input.ApprovalFingerprint) == "") {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	commandReference := uuid.Must(uuid.NewV7())
	result, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,approval_request_id,approval_command_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,'00000000-0000-0000-0000-000000000000')::uuid,NULLIF($16,''),'PENDING',$17,$18) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, commandReference, canonical.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, keyDigest, fingerprint, requestDigest, canonical.TargetResourceType, canonical.TargetResourceID, canonical.Reason, canonical.ExpectedVersions, canonical.PrecheckFingerprint, input.ApprovalRequestID.String(), strings.ToLower(strings.TrimSpace(input.ApprovalFingerprint)), nullableJSONObject(input.BeforeState), input.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	created := result.RowsAffected() == 1
	item, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type=$2 AND idempotency_key_digest=$3`, input.ActorUserID, canonical.CommandType, keyDigest))
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if item.IdempotencyFingerprint != fingerprint {
		return TokenCommandResult{}, false, ErrTokenCommandResultMismatch
	}
	return item, created, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteTokenCommand(ctx context.Context, reference uuid.UUID, status string, afterState, redactedResult json.RawMessage, stableErrorCode string) (TokenCommandResult, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	stableCodeRequired := containsString([]string{"REJECTED", "FAILED", "UNKNOWN"}, status)
	if reference == uuid.Nil || !containsString([]string{"SUCCEEDED", "REJECTED", "FAILED", "UNKNOWN"}, status) || (status == "SUCCEEDED" && stableErrorCode != "") || (stableCodeRequired && !validStableTokenErrorCode(stableErrorCode)) || containsSensitiveCommandField(afterState) || containsSensitiveCommandField(redactedResult) {
		return TokenCommandResult{}, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	item, err := completeTokenCommandTx(ctx, tx, reference, status, afterState, redactedResult, stableErrorCode)
	if err != nil {
		return TokenCommandResult{}, err
	}
	return item, tx.Commit(ctx)
}

func completeTokenCommandTx(ctx context.Context, tx pgx.Tx, reference uuid.UUID, status string, afterState, redactedResult json.RawMessage, stableErrorCode string) (TokenCommandResult, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	stableCodeRequired := containsString([]string{"REJECTED", "FAILED", "UNKNOWN"}, status)
	if reference == uuid.Nil || !containsString([]string{"SUCCEEDED", "REJECTED", "FAILED", "UNKNOWN"}, status) || (status == "SUCCEEDED" && stableErrorCode != "") || (stableCodeRequired && !validStableTokenErrorCode(stableErrorCode)) || containsSensitiveCommandField(afterState) || containsSensitiveCommandField(redactedResult) {
		return TokenCommandResult{}, ErrTokenCommandInvalid
	}
	var commandType, permission, targetType, targetID, reason, keyFingerprint, precheckFingerprint, auditRequestID string
	var policyVersion int64
	var approvalRequestID *uuid.UUID
	var actorID uuid.UUID
	var authorizationVersion int64
	var beforeState json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT command_type,policy_version,actor_user_id,actor_permission,authorization_version,target_resource_type,target_resource_id,reason,idempotency_fingerprint,precheck_fingerprint,before_state,approval_request_id,audit_request_id FROM token_command_results WHERE command_reference=$1 FOR UPDATE`, reference).Scan(&commandType, &policyVersion, &actorID, &permission, &authorizationVersion, &targetType, &targetID, &reason, &keyFingerprint, &precheckFingerprint, &beforeState, &approvalRequestID, &auditRequestID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenCommandResult{}, ErrNotFound
		}
		return TokenCommandResult{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE token_command_results SET status=$2,after_state=$3,redacted_result=$4,stable_error_code=NULLIF($5,''),completed_at=transaction_timestamp() WHERE command_reference=$1 AND status='PENDING'`, reference, status, nullableJSONObject(afterState), nullableJSONObject(redactedResult), strings.TrimSpace(stableErrorCode))
	if err != nil {
		return TokenCommandResult{}, err
	}
	if command.RowsAffected() != 1 {
		item, loadErr := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1`, reference))
		if loadErr != nil {
			return TokenCommandResult{}, loadErr
		}
		if item.Status == status {
			return item, nil
		}
		return TokenCommandResult{}, ErrTokenCommandResultFinal
	}
	resourceID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(targetType+"\x00"+targetID))
	summary, _ := json.Marshal(map[string]any{
		"command_reference": reference, "command_type": commandType, "policy_version": policyVersion, "permission": permission,
		"authorization_version": authorizationVersion, "reason": reason, "idempotency_fingerprint": keyFingerprint,
		"precheck_fingerprint": precheckFingerprint, "before_state": beforeState, "after_state": nullableJSONObject(afterState),
		"result": nullableJSONObject(redactedResult), "status": status, "stable_error_code": strings.TrimSpace(stableErrorCode),
		"approval_request_id": approvalRequestID, "request_id": auditRequestID,
	})
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return TokenCommandResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.Must(uuid.NewV7()), actorID, commandType, targetType, resourceID, summary, now); err != nil {
		return TokenCommandResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'token.management-command.executed.v1',$2,1,$3,$4,$5)`, uuid.Must(uuid.NewV7()), resourceID, summary, auditRequestID, now); err != nil {
		return TokenCommandResult{}, err
	}
	item, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1`, reference))
	if err != nil {
		return TokenCommandResult{}, err
	}
	return item, nil
}

func validStableTokenErrorCode(code string) bool {
	// This fixed domain code contains no provider data. Keep the exception
	// exact so arbitrary credential-bearing diagnostics remain rejected.
	if code == "CREDENTIAL_PROBE_OUTCOME_UNKNOWN" {
		return true
	}
	if len(code) < 1 || len(code) > 120 || code != strings.TrimSpace(code) || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for index := 1; index < len(code); index++ {
		if current := code[index]; (current < 'A' || current > 'Z') && (current < '0' || current > '9') && current != '_' {
			return false
		}
	}
	for _, sensitive := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(code, sensitive) {
			return false
		}
	}
	return true
}

func (r *PostgresRepository) TokenCommandResult(ctx context.Context, reference, actor uuid.UUID) (TokenCommandResult, error) {
	query := tokenCommandResultSelect + ` WHERE command_reference=$1`
	arguments := []any{reference}
	if actor != uuid.Nil {
		query += ` AND actor_user_id=$2`
		arguments = append(arguments, actor)
	}
	item, err := scanTokenCommandResult(r.pool.QueryRow(ctx, query, arguments...))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) ApprovedTokenCommandResult(ctx context.Context, approvalRequestID uuid.UUID, commandType, commandFingerprint, requestDigest, idempotencyFingerprint string, actor uuid.UUID) (TokenCommandResult, error) {
	item, err := scanTokenCommandResult(r.pool.QueryRow(ctx, tokenCommandResultSelect+` WHERE approval_request_id=$1 AND command_type=$2 AND approval_command_fingerprint=$3 AND actor_user_id=$4 AND request_digest=$5 AND idempotency_fingerprint=$6`, approvalRequestID, commandType, strings.ToLower(strings.TrimSpace(commandFingerprint)), actor, requestDigest, idempotencyFingerprint))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	return item, err
}

const tokenCommandResultSelect = `SELECT command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_fingerprint,target_resource_type,target_resource_id,status,redacted_result,stable_error_code,approval_request_id,audit_request_id,created_at,completed_at FROM token_command_results`

func scanTokenCommandResult(row interface{ Scan(...any) error }) (TokenCommandResult, error) {
	var item TokenCommandResult
	return item, row.Scan(&item.CommandReference, &item.CommandType, &item.PolicyVersion, &item.ActorUserID, &item.ActorPermission, &item.AuthorizationVersion, &item.IdempotencyFingerprint, &item.TargetResourceType, &item.TargetResourceID, &item.Status, &item.RedactedResult, &item.StableErrorCode, &item.ApprovalRequestID, &item.AuditRequestID, &item.CreatedAt, &item.CompletedAt)
}

func normalizeTokenCommand(envelope TokenCommandEnvelope, key, approvalFingerprint string) (TokenCommandEnvelope, string, string, string, error) {
	envelope.CommandType = strings.ToUpper(strings.TrimSpace(envelope.CommandType))
	envelope.Reason = strings.TrimSpace(envelope.Reason)
	envelope.TargetResourceType = strings.ToUpper(strings.TrimSpace(envelope.TargetResourceType))
	envelope.TargetResourceID = strings.TrimSpace(envelope.TargetResourceID)
	envelope.PrecheckFingerprint = strings.ToLower(strings.TrimSpace(envelope.PrecheckFingerprint))
	if isProtocolFormulaGovernedCommand(envelope.CommandType) {
		var err error
		envelope, err = canonicalizeProtocolFormulaEnvelope(envelope)
		if err != nil {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if isRouteGovernedCommand(envelope.CommandType) {
		if _, err := parseGovernedRouteCommand(envelope.CommandType, envelope.TargetResourceID, envelope.Command); err != nil {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if isModelAccountGovernedCommand(envelope.CommandType) {
		var err error
		envelope, err = canonicalizeModelAccountEnvelope(envelope)
		if err != nil {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	key = strings.TrimSpace(key)
	approvalFingerprint = strings.ToLower(strings.TrimSpace(approvalFingerprint))
	spec, ok := tokenCommandSpecs[envelope.CommandType]
	policy, policyErr := approvalpolicy.Lookup(envelope.CommandType)
	maximumKeyLength := 128
	if approvalFingerprint != "" {
		maximumKeyLength = 512
	}
	if !ok || policyErr != nil || policy.Mode == approvalpolicy.ApprovalRequired != spec.Approval || utf8.RuneCountInString(envelope.Reason) < 10 || utf8.RuneCountInString(envelope.Reason) > 2000 || len(key) < 16 || len(key) > maximumKeyLength || envelope.TargetResourceType == "" || envelope.TargetResourceID == "" || strings.Contains(strings.ToLower(string(envelope.Command)), "ignore_blocker") || strings.Contains(strings.ToLower(string(envelope.Command)), `"force"`) {
		return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
	}
	if expectedType, governed := directCommandPrecheckTypes[envelope.CommandType]; governed && (envelope.TargetResourceType != expectedType || validateOwnedCommandPrecheckTarget(envelope.CommandType, envelope.TargetResourceID, envelope.Command) != nil) {
		return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
	}
	var facts map[string]any
	if json.Unmarshal(envelope.ExpectedVersions, &facts) != nil || containsSensitiveCommandField(envelope.ExpectedVersions) || containsSensitiveCommandField(envelope.Command) {
		return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
	}
	for _, field := range spec.RequiredFacts {
		if _, present := facts[field]; !present {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if isProtocolFormulaGovernedCommand(envelope.CommandType) {
		requestFingerprint, ok := facts["request_fingerprint"].(string)
		if !ok || requestFingerprint != governedRequestFingerprint(envelope, facts) {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if isModelAccountGovernedCommand(envelope.CommandType) || isRouteGovernedCommand(envelope.CommandType) {
		requestFingerprint, ok := facts["request_fingerprint"].(string)
		if !ok || requestFingerprint != governedRequestFingerprint(envelope, facts) {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if isRouteGovernedCommand(envelope.CommandType) && facts["precheck_fingerprint"] != envelope.PrecheckFingerprint {
		return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
	}
	if envelope.CommandType == "TOKEN_MODEL_PUBLISH" {
		modelID, parseErr := uuid.Parse(envelope.TargetResourceID)
		expected, expectedErr := parseModelPublishExpected(envelope.ExpectedVersions, modelID)
		if parseErr != nil || expectedErr != nil || expected.PrecheckFingerprint != envelope.PrecheckFingerprint || expected.RequestFingerprint != modelPublishRequestFingerprint(envelope, facts) {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	if _, err := hex.DecodeString(envelope.PrecheckFingerprint); err != nil || len(envelope.PrecheckFingerprint) != 64 {
		return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
	}
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return TokenCommandEnvelope{}, "", "", "", err
	}
	requestHash := sha256.Sum256(canonical)
	keyHash := sha256.Sum256([]byte(key))
	if approvalFingerprint != "" {
		if decoded, decodeErr := hex.DecodeString(approvalFingerprint); decodeErr != nil || len(decoded) != sha256.Size {
			return TokenCommandEnvelope{}, "", "", "", ErrTokenCommandInvalid
		}
	}
	fingerprintInput := append(append([]byte("pinvou-token-command-v1\x00"), keyHash[:]...), requestHash[:]...)
	fingerprintInput = append(fingerprintInput, []byte(approvalFingerprint)...)
	fingerprintHash := sha256.Sum256(fingerprintInput)
	return envelope, hex.EncodeToString(requestHash[:]), hex.EncodeToString(keyHash[:]), hex.EncodeToString(fingerprintHash[:]), nil
}

// modelPublishRequestFingerprint mirrors highRiskPayloadKey in the Portal:
// every object key is ordered before JSON encoding and the two transport-only
// proofs are excluded from the frozen authoritative facts.
func modelPublishRequestFingerprint(envelope TokenCommandEnvelope, facts map[string]any) string {
	return governedRequestFingerprint(envelope, facts)
}

func governedRequestFingerprint(envelope TokenCommandEnvelope, facts map[string]any) string {
	domainFacts := make(map[string]any, len(facts))
	for key, value := range facts {
		if key != "request_fingerprint" && key != "precheck_fingerprint" {
			domainFacts[key] = value
		}
	}
	expected, expectedErr := json.Marshal(domainFacts)
	command, commandErr := canonicalJSON(envelope.Command)
	if expectedErr != nil || commandErr != nil {
		return ""
	}
	payload, err := json.Marshal(map[string]any{
		"command": command, "commandType": envelope.CommandType,
		"expectedVersions": json.RawMessage(expected), "targetResourceID": envelope.TargetResourceID,
		"targetResourceType": envelope.TargetResourceType,
	})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func isProtocolFormulaGovernedCommand(commandType string) bool {
	switch commandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE", "TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_DRAFT_CREATE", "TOKEN_FORMULA_PUBLISH":
		return true
	default:
		return false
	}
}

func containsSensitiveCommandField(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var walk func(any) bool
	safeCredentialMetadata := map[string]struct{}{
		"token_account_id": {}, "token_account_version": {}, "credential_id": {}, "credential_version": {},
		"credential_status": {}, "credential_fingerprint": {}, "replacement_credential_version": {},
		"active_credential_version": {}, "credential_slot_version": {},
	}
	walk = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			formulaUnitMaps := validGovernedFormulaUnitMaps(typed)
			for key, child := range typed {
				if formulaUnitMaps && (key == "required_unit_versions" || key == "expression") {
					for unit, mapped := range child.(map[string]any) {
						if !governedTokenMeteringUnit(unit) && sensitiveTokenGovernanceFieldName(normalizeTokenSensitiveFieldName(unit)) || walk(mapped) {
							return true
						}
					}
					continue
				}
				normalized := normalizeTokenSensitiveFieldName(key)
				if normalized == "credential" {
					if !validSafeCredentialProjection(child) {
						return true
					}
					continue
				}
				_, safeCredentialField := safeCredentialMetadata[normalized]
				if safeCredentialField && !validTokenCredentialMetadataValue(normalized, child) {
					return true
				}
				if !safeCredentialField && sensitiveTokenGovernanceFieldName(normalized) {
					return true
				}
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

func validSafeCredentialProjection(value any) bool {
	item, ok := value.(map[string]any)
	if !ok {
		return false
	}
	required := map[string]bool{
		"credential_id": true, "credential_type": true, "fingerprint": true, "version": true,
		"status": true, "validation_status": true, "allowed_models": true,
		"responsible_actor_id": true, "created_at": true, "updated_at": true,
	}
	for key := range item {
		if !required[key] && key != "validated_at" {
			return false
		}
	}
	for key := range required {
		if _, found := item[key]; !found {
			return false
		}
	}
	credentialID, credentialIDOK := item["credential_id"].(string)
	responsibleID, responsibleIDOK := item["responsible_actor_id"].(string)
	credentialType, typeOK := item["credential_type"].(string)
	fingerprint, fingerprintOK := item["fingerprint"].(string)
	version, versionOK := item["version"].(float64)
	status, statusOK := item["status"].(string)
	validation, validationOK := item["validation_status"].(string)
	models, modelsOK := item["allowed_models"].([]any)
	createdAt, createdOK := item["created_at"].(string)
	updatedAt, updatedOK := item["updated_at"].(string)
	if _, err := uuid.Parse(credentialID); !credentialIDOK || err != nil {
		return false
	}
	if _, err := uuid.Parse(responsibleID); !responsibleIDOK || err != nil {
		return false
	}
	if !typeOK || len(credentialType) < 1 || len(credentialType) > 120 || credentialType != strings.TrimSpace(credentialType) || !fingerprintOK || len(fingerprint) != 64 {
		return false
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return false
	}
	if !versionOK || version < 1 || version != float64(int64(version)) || !statusOK || !containsString([]string{"PENDING", "CANDIDATE", "ACTIVE", "RETIRING", "RETIRED", "REVOKED"}, status) || !validationOK || !containsString([]string{"UNKNOWN", "VALID", "INVALID"}, validation) || !modelsOK {
		return false
	}
	seenModels := map[string]bool{}
	for _, raw := range models {
		model, modelOK := raw.(string)
		if !modelOK || len(model) < 1 || len(model) > 160 || model != strings.TrimSpace(model) || seenModels[model] {
			return false
		}
		seenModels[model] = true
	}
	if !createdOK || !updatedOK {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return false
	}
	if validated, found := item["validated_at"]; found {
		value, valueOK := validated.(string)
		if !valueOK {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return false
		}
	}
	return true
}

func normalizeTokenSensitiveFieldName(key string) string {
	var normalized strings.Builder
	var previous rune
	for index, current := range []rune(strings.TrimSpace(key)) {
		if current == '-' || current == '.' || unicode.IsSpace(current) {
			current = '_'
		} else if unicode.IsUpper(current) && index > 0 && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			normalized.WriteRune('_')
		}
		normalized.WriteRune(unicode.ToLower(current))
		previous = current
	}
	return normalized.String()
}

func sensitiveTokenGovernanceFieldName(normalized string) bool {
	compact := strings.ReplaceAll(normalized, "_", "")
	return strings.Contains(compact, "password") || strings.Contains(compact, "secret") || strings.Contains(compact, "cipher") || strings.Contains(compact, "privatekey") || strings.Contains(compact, "credential") || strings.Contains(compact, "endpoint") || strings.Contains(compact, "provideraddress") || strings.Contains(compact, "providerurl") || strings.Contains(compact, "baseurl") || compact == "authorization" || compact == "apikey" || compact == "keyid" || compact == "cookie" || compact == "token" || strings.HasSuffix(compact, "token")
}

func validTokenCredentialMetadataValue(name string, value any) bool {
	switch name {
	case "token_account_id", "credential_id":
		text, ok := value.(string)
		_, err := uuid.Parse(text)
		return ok && err == nil
	case "token_account_version", "credential_version", "replacement_credential_version", "active_credential_version", "credential_slot_version":
		if value == nil && name == "replacement_credential_version" {
			return true
		}
		number, ok := value.(float64)
		return ok && number >= 0 && number == float64(int64(number))
	case "credential_status":
		text, ok := value.(string)
		return ok && containsString([]string{"PENDING", "ACTIVE", "RETIRED", "REVOKED"}, strings.ToUpper(strings.TrimSpace(text)))
	case "credential_fingerprint":
		text, ok := value.(string)
		decoded, err := hex.DecodeString(strings.TrimSpace(text))
		return ok && err == nil && len(decoded) == sha256.Size
	default:
		return false
	}
}

func nullableJSONObject(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Service) adminCreateTokenCommand(w http.ResponseWriter, r *http.Request) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var envelope TokenCommandEnvelope
	if api.DecodeJSON(w, r, &envelope) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	canonicalCommandType := strings.ToUpper(strings.TrimSpace(envelope.CommandType))
	if canonicalCommandType == "TOKEN_ACCOUNT_CREATE" && envelope.CommandType != canonicalCommandType {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command type must be canonical", httpserver.RequestID(r.Context()))
		return
	}
	spec, ok := tokenCommandSpecs[canonicalCommandType]
	if !ok || spec.Approval {
		api.WriteError(w, http.StatusForbidden, "DIRECT_APPROVAL_COMMAND_FORBIDDEN", "submit this command through the approval workflow", httpserver.RequestID(r.Context()))
		return
	}
	if canonicalCommandType == "TOKEN_ACCOUNT_CREATE" && !s.tokenAccountCreateReady {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE", "Token Account creation is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return
	}
	if spec.RecentAuth && (principal.AuthenticatedAt.IsZero() || time.Since(principal.AuthenticatedAt) > 15*time.Minute) {
		api.WriteError(w, http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "authentication within the last 15 minutes is required", httpserver.RequestID(r.Context()))
		return
	}
	actor, authorizationVersion, err := s.authorizedTokenCommandActor(r.Context(), principal, envelope.CommandType)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	canonicalEnvelope, requestDigest, keyDigest, idempotencyFingerprint, canonicalErr := normalizeTokenCommand(envelope, r.Header.Get("Idempotency-Key"), "")
	if canonicalErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	envelope = canonicalEnvelope
	repository, repositoryOK := s.repository.(*PostgresRepository)
	if !repositoryOK {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	begin := tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: strings.Join(spec.Permissions, "+"), AuthorizationVersion: authorizationVersion, IdempotencyKey: r.Header.Get("Idempotency-Key"), AuditRequestID: httpserver.RequestID(r.Context())}
	if isRouteGovernedCommand(envelope.CommandType) {
		s.executeRouteCommandHTTP(w, r, repository, runtimeTokenCommandBegin{tokenCommandBegin: begin, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint})
		return
	}
	// Exact model/account replays are resolved before consulting any mutable
	// owner, catalog, route or Billing authority. This preserves the first
	// immutable result after the domain effect itself changes those facts (for
	// example, publication deletes its draft).
	if isModelAccountGovernedCommand(envelope.CommandType) {
		existing, existingErr := repository.TokenCommandByActorKey(r.Context(), actor, envelope.CommandType, keyDigest, idempotencyFingerprint)
		switch {
		case existingErr == nil:
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, http.StatusOK, existing)
			return
		case errors.Is(existingErr, ErrTokenCommandResultMismatch):
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
			return
		case !errors.Is(existingErr, ErrNotFound):
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
			return
		}
	}
	scheduledModelPublish := false
	if envelope.CommandType == "TOKEN_MODEL_PUBLISH" {
		publishCommand, parseErr := parseModelPublishCommand(envelope.Command)
		if parseErr != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
			return
		}
		scheduledModelPublish = publishCommand.PublishMode == "SCHEDULED"
	}
	if envelope.CommandType == "TOKEN_MODEL_CATALOG_SYNC" {
		item, _, syncErr := s.executeGovernedCatalogSync(r.Context(), repository, runtimeTokenCommandBegin{tokenCommandBegin: begin, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint})
		if errors.Is(syncErr, ErrTokenCommandResultMismatch) {
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(syncErr, ErrTokenCommandInvalid) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(syncErr, ErrCatalogVersionConflict) || errors.Is(syncErr, ErrNotFound) || errors.Is(syncErr, ErrForbidden) {
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(syncErr, ErrTokenRouteActivationChanged) {
			writeTokenCommandGovernanceInactiveHTTP(w, r)
			return
		}
		if syncErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusOK, item)
		return
	}
	if isProtocolFormulaGovernedCommand(envelope.CommandType) {
		item, created, governedErr := s.ExecuteProtocolFormulaTokenCommand(r.Context(), repository, runtimeTokenCommandBegin{tokenCommandBegin: begin, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint})
		// Once Formula Publish has persisted its cross-domain T1 anchor, no
		// downstream or transaction-guard error may be advertised as a proven
		// zero-write response. Preserve the reference so the client only polls the
		// persisted result; recovery must not depend on replaying the POST.
		if governedErr != nil && item.CommandReference != uuid.Nil && item.Status == "PENDING" {
			api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": item.CommandReference, "result_query": "/token/admin/v1/commands/" + item.CommandReference.String()}})
			return
		}
		// A deterministic failure after Formula Publish T1 is itself the durable
		// governed result. Return that closed result rather than a generic error
		// envelope so the client can verify the reference and terminal state.
		if governedErr != nil && item.CommandReference != uuid.Nil && item.Status == "FAILED" {
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, status, item)
			return
		}
		var billingErr *BillingHTTPError
		switch {
		case errors.Is(governedErr, ErrTokenCommandResultMismatch):
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command or Billing binding", httpserver.RequestID(r.Context()))
			return
		case errors.Is(governedErr, ErrTokenCommandInvalid):
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
			return
		case errors.Is(governedErr, ErrCatalogVersionConflict), errors.Is(governedErr, ErrProtocolHeadVersionConflict), errors.Is(governedErr, ErrFormulaHeadVersionConflict), errors.Is(governedErr, ErrProtocolDraftETagMismatch), errors.Is(governedErr, ErrFormulaDraftETagMismatch), errors.Is(governedErr, ErrProtocolFormulaPrecheck), errors.Is(governedErr, ErrNotFound):
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		case errors.Is(governedErr, ErrTokenRouteActivationChanged):
			writeTokenCommandGovernanceInactiveHTTP(w, r)
			return
		case errors.As(governedErr, &billingErr) && billingErr.Status == http.StatusConflict:
			api.WriteError(w, http.StatusConflict, "FORMULA_BINDING_CONFLICT", "Billing already holds a different immutable formula binding", httpserver.RequestID(r.Context()))
			return
		case errors.As(governedErr, &billingErr) && billingErr.Status >= 400 && billingErr.Status < 500:
			api.WriteError(w, http.StatusUnprocessableEntity, "FORMULA_UNIT_VALIDATION_REJECTED", "Billing rejected the protocol or formula evidence", httpserver.RequestID(r.Context()))
			return
		case governedErr != nil:
			if item.CommandReference == uuid.Nil {
				api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RETRYABLE", "no command was persisted; retry with the same idempotency key", httpserver.RequestID(r.Context()))
				return
			}
			details := map[string]any{}
			details["command_reference"] = item.CommandReference
			details["result_query"] = "/token/admin/v1/commands/" + item.CommandReference.String()
			api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: details})
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, status, item)
		return
	}
	if isRuntimeGovernedCommand(envelope.CommandType) {
		item, _, runtimeErr := repository.ExecuteRuntimeTokenCommand(r.Context(), runtimeTokenCommandBegin{tokenCommandBegin: begin, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint})
		if errors.Is(runtimeErr, ErrTokenCommandResultMismatch) {
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(runtimeErr, ErrTokenCommandInvalid) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(runtimeErr, ErrCatalogVersionConflict) || errors.Is(runtimeErr, ErrProtocolFormulaPrecheck) || errors.Is(runtimeErr, ErrNotFound) || errors.Is(runtimeErr, ErrForbidden) {
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		}
		if runtimeErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusOK, item)
		return
	}
	var authoritativeBefore json.RawMessage
	if _, requiresAuthoritativePrecheck := directCommandPrecheckTypes[strings.ToUpper(strings.TrimSpace(envelope.CommandType))]; requiresAuthoritativePrecheck && envelope.CommandType != "TOKEN_ACCOUNT_CREATE" {
		precheck, precheckErr := s.buildTokenCommandPrecheck(r.Context(), TokenCommandPrecheckRequest{
			CommandType: envelope.CommandType, TargetResourceType: envelope.TargetResourceType,
			TargetResourceID: envelope.TargetResourceID, Command: envelope.Command,
		})
		if precheckErr != nil {
			if errors.Is(precheckErr, ErrTokenCommandInvalid) {
				api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
				return
			}
			if errors.Is(precheckErr, ErrCatalogVersionConflict) || errors.Is(precheckErr, ErrProtocolFormulaPrecheck) || errors.Is(precheckErr, ErrNotFound) {
				api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
				return
			}
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_PRECHECK_UNAVAILABLE", "the governed command precheck could not be revalidated", httpserver.RequestID(r.Context()))
			return
		}
		if envelope.CommandType == "TOKEN_ACCOUNT_CREATE" && containsString(precheck.Blockers, "TOKEN_ACCOUNT_CAP_REACHED") {
			api.WriteError(w, http.StatusConflict, "TOKEN_ACCOUNT_CAP_REACHED", "the selected owner has reached the Token Account limit", httpserver.RequestID(r.Context()))
			return
		}
		if envelope.PrecheckFingerprint != precheck.PrecheckFingerprint || !tokenCommandExpectedVersionsMatch(envelope.ExpectedVersions, precheck.ExpectedVersions) || len(precheck.Blockers) > 0 {
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		}
		authoritativeBefore = precheck.ExpectedVersions
	} else if envelope.CommandType == "TOKEN_ACCOUNT_CREATE" {
		// Account creation establishes its one authoritative Account snapshot
		// only after the Token owner/cap locks are held below. Repeating the
		// cross-service snapshot here creates an avoidable TOCTOU and makes a
		// permission change between two reads ambiguous.
		authoritativeBefore = envelope.ExpectedVersions
	}
	begin.BeforeState = authoritativeBefore
	if isModelAccountGovernedCommand(envelope.CommandType) && !scheduledModelPublish {
		item, created, executeErr := s.executeGovernedModelAccountCommand(r.Context(), repository, runtimeTokenCommandBegin{tokenCommandBegin: begin, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint}, authoritativeBefore)
		if executeErr != nil && item.CommandReference != uuid.Nil {
			api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": item.CommandReference, "result_query": "/token/admin/v1/commands/" + item.CommandReference.String()}})
			return
		}
		switch {
		case errors.Is(executeErr, ErrTokenCommandResultMismatch):
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrTokenCommandInvalid):
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrTokenAccountCapReached):
			api.WriteError(w, http.StatusConflict, "TOKEN_ACCOUNT_CAP_REACHED", "the selected owner has reached the Token Account limit", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrTokenAccountPolicyUnavailable):
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_ACCOUNT_POLICY_UNAVAILABLE", "Token Account creation policy is unavailable", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrTokenAccountPolicyConfigMismatch):
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH", "Token Account creation policy is inconsistent", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrCatalogVersionConflict), errors.Is(executeErr, ErrNotFound), errors.Is(executeErr, ErrModelUnavailable), errors.Is(executeErr, ErrForbidden):
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
		case errors.Is(executeErr, ErrTokenRouteActivationChanged):
			writeTokenCommandGovernanceInactiveHTTP(w, r)
		case executeErr != nil:
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RETRYABLE", "no command was persisted; retry with the same idempotency key", httpserver.RequestID(r.Context()))
		default:
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, status, item)
		}
		return
	}
	if scheduledModelPublish {
		item, created, scheduleErr := repository.ScheduleModelPublish(r.Context(), begin)
		switch {
		case errors.Is(scheduleErr, ErrTokenCommandResultMismatch):
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
			return
		case errors.Is(scheduleErr, ErrTokenCommandInvalid):
			api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
			return
		case errors.Is(scheduleErr, ErrCatalogVersionConflict), errors.Is(scheduleErr, ErrNotFound), errors.Is(scheduleErr, ErrProtocolFormulaPrecheck):
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		case errors.Is(scheduleErr, errLifecyclePlanWindowInvalid):
			api.WriteError(w, http.StatusUnprocessableEntity, "LIFECYCLE_PLAN_WINDOW_INVALID", "the lifecycle plan effective time is outside the allowed window", httpserver.RequestID(r.Context()))
			return
		case scheduleErr != nil:
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, status, item)
		return
	}
	item, created, err := repository.BeginTokenCommand(r.Context(), begin)
	if errors.Is(err, ErrTokenCommandResultMismatch) {
		api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrTokenCommandInvalid) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if envelope.CommandType == "TOKEN_CREDENTIAL_VALIDATE" && item.Status == "PENDING" {
		completed, executeErr := s.executeGovernedCredentialValidation(r.Context(), repository, item.CommandReference, actor, envelope)
		if errors.Is(executeErr, ErrCredentialProbeClaimed) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "2")
			api.WriteJSON(w, http.StatusAccepted, completed)
			return
		}
		if executeErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusOK, completed)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	w.Header().Set("Retry-After", "2")
	api.WriteJSON(w, status, item)
}

func writeTokenCommandGovernanceInactiveHTTP(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "Token command governance is not active", httpserver.RequestID(r.Context()))
}

func isRuntimeGovernedCommand(commandType string) bool {
	return commandType == "TOKEN_ROUTE_PROVISIONING_REPAIR" || commandType == "TOKEN_USAGE_RECONCILIATION_RETRY"
}

func (s *Service) adminTokenCommandResult(w http.ResponseWriter, r *http.Request) {
	reference, ok := adminUUID(w, r, "command_reference")
	if !ok {
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	candidate, err := repository.TokenCommandResult(r.Context(), reference, uuid.Nil)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	principal, principalOK := identity.FromContext(r.Context())
	_, specOK := tokenCommandSpecs[candidate.CommandType]
	if !principalOK || !specOK {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	actor, _, authorizationErr := s.authorizedTokenCommandActor(r.Context(), principal, candidate.CommandType)
	if errors.Is(authorizationErr, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	if authorizationErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.TokenCommandResult(r.Context(), reference, actor)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	repository.decorateTokenAccountRecovery(r.Context(), &item)
	if item.CommandType == "TOKEN_FORMULA_PUBLISH" && item.Status == "PENDING" {
		pending, pendingErr := repository.pendingFormulaPublishCommand(r.Context(), reference, actor)
		if pendingErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "the command result could not be recovered", httpserver.RequestID(r.Context()))
			return
		}
		resumed, _, resumeErr := s.resumeGovernedFormulaPublishResult(r.Context(), repository, pending, reference, false)
		if resumed.CommandReference != uuid.Nil {
			item = resumed
		}
		// A transport or authority failure leaves the durable row PENDING. GET is
		// still a safe, actor-scoped observation and the client may poll the same
		// reference; it never needs the original plaintext idempotency key.
		if resumeErr != nil && item.Status != "PENDING" && item.Status != "FAILED" {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "the command result could not be recovered", httpserver.RequestID(r.Context()))
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(item.AuthorizationVersion, 10))
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) authorizedTokenCommandActor(ctx context.Context, principal identity.Principal, commandType string) (uuid.UUID, int64, error) {
	if s.authorizer == nil {
		return uuid.Nil, 0, errors.New("authorization is unavailable")
	}
	spec, ok := tokenCommandSpecs[strings.ToUpper(strings.TrimSpace(commandType))]
	if !ok {
		return uuid.Nil, 0, authorization.ErrPermissionDenied
	}
	return s.authorizer.AuthorizedUserIDAll(ctx, principal, spec.Permissions...)
}

func (s *Service) executeApprovedModelLifecycle(w http.ResponseWriter, r *http.Request) {
	s.executeApprovedTokenCommand(w, r, "TOKEN_MODEL_LIFECYCLE_CHANGE")
}

func (s *Service) executeApprovedCatalogRollback(w http.ResponseWriter, r *http.Request) {
	s.executeApprovedTokenCommand(w, r, "TOKEN_MODEL_CATALOG_ROLLBACK")
}

func (s *Service) executeApprovedCredentialRevoke(w http.ResponseWriter, r *http.Request) {
	s.executeApprovedTokenCommand(w, r, "TOKEN_CREDENTIAL_REVOKE")
}

func (s *Service) executeApprovedTokenCommand(w http.ResponseWriter, r *http.Request, commandType string) {
	workload, authenticated := serviceauth.FromContext(r.Context())
	if !authenticated || workload.ActorType != "SERVICE" || workload.Service != "token-command-executor" || strings.TrimSpace(workload.Subject) == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "the approved Token command requires the Token executor identity", httpserver.RequestID(r.Context()))
		return
	}
	if s.approvedCommandExecutor == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_EXECUTOR_UNAVAILABLE", "the approved Token command executor is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body approvedTokenCommandBody
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	policy, err := approvalpolicy.Lookup(commandType)
	expectedExecutionKey := fmt.Sprintf("approval:%s:%s:%s", body.ApprovalRequestID, commandType, strings.ToLower(strings.TrimSpace(body.CommandFingerprint)))
	if err != nil || !approvalpolicy.Executable(commandType) || policy.Mode != approvalpolicy.ApprovalRequired || policy.ExecutorIdentity != workload.Service || body.Envelope.CommandType != commandType || body.ApprovalRequestID == uuid.Nil || body.RequesterUserID == uuid.Nil || body.AuthorizationVersion < 1 || len(strings.TrimSpace(body.CommandFingerprint)) != 64 || strings.TrimSpace(body.ExecutionIdempotencyKey) != expectedExecutionKey {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	canonicalEnvelope, requestDigest, keyDigest, idempotencyFingerprint, normalizeErr := normalizeTokenCommand(body.Envelope, body.ExecutionIdempotencyKey, body.CommandFingerprint)
	if normalizeErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	body.Envelope = canonicalEnvelope
	if (commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" || commandType == "TOKEN_MODEL_CATALOG_ROLLBACK") && (!jsonSemanticEqual(body.CriticalResourceVersions, body.Envelope.ExpectedVersions) || len(body.FinancialSnapshot) == 0) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command snapshot is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if commandType == "TOKEN_CREDENTIAL_REVOKE" {
		if !tokenCommandExpectedVersionsMatch(body.CriticalResourceVersions, body.Envelope.ExpectedVersions) || !exactEmptyFinancialSnapshot(body.FinancialSnapshot) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command snapshot is invalid", httpserver.RequestID(r.Context()))
			return
		}
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, loadErr := repository.ApprovedTokenCommandResult(r.Context(), body.ApprovalRequestID, commandType, body.CommandFingerprint, requestDigest, idempotencyFingerprint, body.RequesterUserID)
	if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(loadErr, ErrNotFound) {
		// Authoritative lifecycle evidence is intentionally recomputed only for
		// the first execution claim. A repeated Account event must replay the
		// frozen result even when dependencies changed after the plan was
		// scheduled; otherwise event delivery would cease to be idempotent.
		if commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" || commandType == "TOKEN_MODEL_CATALOG_ROLLBACK" {
			projection, projectionErr := s.approvalCommandPrecheck(r.Context(), ApprovalSnapshotRequest{CommandType: commandType, TargetResourceType: body.Envelope.TargetResourceType, TargetResourceID: body.Envelope.TargetResourceID, NormalizedCommand: body.Envelope.Command})
			if projectionErr != nil || projection.PrecheckFingerprint != body.Envelope.PrecheckFingerprint || !jsonSemanticEqual(projection.CriticalResourceVersions, body.CriticalResourceVersions) || !approvalpolicy.SameTokenModelFinancialFacts(projection.FinancialSnapshot, body.FinancialSnapshot) {
				api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
				return
			}
		}
		before, precheckErr := s.approvedCommandExecutor.PrecheckApprovedTokenCommand(r.Context(), commandType, body.Envelope)
		if precheckErr != nil {
			api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
			return
		}
		if commandType == "TOKEN_MODEL_CATALOG_ROLLBACK" {
			executor, supported := s.approvedCommandExecutor.(interface {
				ExecuteApprovedCatalogRollbackCommand(context.Context, runtimeTokenCommandBegin, []GatewayProtocolCapability) (TokenCommandResult, bool, error)
			})
			if !supported {
				api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_EXECUTOR_UNAVAILABLE", "the approved catalog rollback adapter is unavailable", httpserver.RequestID(r.Context()))
				return
			}
			completed, _, executeErr := executor.ExecuteApprovedCatalogRollbackCommand(r.Context(), runtimeTokenCommandBegin{tokenCommandBegin: tokenCommandBegin{
				Envelope: body.Envelope, ActorUserID: body.RequesterUserID, ActorPermission: policy.RequesterPermission,
				AuthorizationVersion: body.AuthorizationVersion, IdempotencyKey: body.ExecutionIdempotencyKey,
				AuditRequestID: httpserver.RequestID(r.Context()), BeforeState: before,
				ApprovalRequestID: body.ApprovalRequestID, ApprovalFingerprint: body.CommandFingerprint,
			}, RequestDigest: requestDigest, KeyDigest: keyDigest, IdempotencyFingerprint: idempotencyFingerprint}, s.upstream.Protocols())
			if completed.CommandReference != uuid.Nil && executeErr != nil && !errors.Is(executeErr, ErrCatalogVersionConflict) && !errors.Is(executeErr, ErrNotFound) && !errors.Is(executeErr, ErrForbidden) && !errors.Is(executeErr, ErrTokenCommandInvalid) {
				api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": completed.CommandReference, "result_query": "/token/admin/v1/commands/" + completed.CommandReference.String()}})
				return
			}
			switch {
			case errors.Is(executeErr, ErrCatalogVersionConflict), errors.Is(executeErr, ErrNotFound), errors.Is(executeErr, ErrForbidden), errors.Is(executeErr, ErrTokenCommandInvalid):
				api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
			case executeErr != nil:
				api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_UNAVAILABLE", "Token command persistence is unavailable", httpserver.RequestID(r.Context()))
			default:
				api.WriteJSON(w, http.StatusOK, completed)
			}
			return
		}
		var beginErr error
		item, _, beginErr = repository.BeginTokenCommand(r.Context(), tokenCommandBegin{
			Envelope: body.Envelope, ActorUserID: body.RequesterUserID, ActorPermission: policy.RequesterPermission,
			AuthorizationVersion: body.AuthorizationVersion, IdempotencyKey: body.ExecutionIdempotencyKey,
			AuditRequestID: httpserver.RequestID(r.Context()), BeforeState: before,
			ApprovalRequestID: body.ApprovalRequestID, ApprovalFingerprint: body.CommandFingerprint,
		})
		if beginErr != nil {
			api.WriteError(w, http.StatusConflict, "APPROVED_COMMAND_CONFLICT", "the approved command could not be claimed", httpserver.RequestID(r.Context()))
			return
		}
	}
	if item.Status != "PENDING" {
		api.WriteJSON(w, http.StatusOK, item)
		return
	}
	if commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		executor, supported := s.approvedCommandExecutor.(interface {
			ExecuteApprovedModelLifecycle(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, json.RawMessage, json.RawMessage, TokenCommandEnvelope) (TokenCommandResult, error)
		})
		if !supported {
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_EXECUTOR_UNAVAILABLE", "the approved lifecycle adapter is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		completed, executeErr := executor.ExecuteApprovedModelLifecycle(r.Context(), item.CommandReference, body.RequesterUserID, body.ApprovalRequestID, body.CommandFingerprint, body.CriticalResourceVersions, body.FinancialSnapshot, body.Envelope)
		if errors.Is(executeErr, ErrCatalogVersionConflict) || errors.Is(executeErr, ErrNotFound) || errors.Is(executeErr, ErrForbidden) || errors.Is(executeErr, ErrTokenCommandInvalid) {
			api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
			return
		}
		if executeErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteJSON(w, http.StatusOK, completed)
		return
	}
	if commandType == "TOKEN_CREDENTIAL_REVOKE" {
		executor, supported := s.approvedCommandExecutor.(interface {
			ExecuteApprovedCredentialRevoke(context.Context, uuid.UUID, uuid.UUID, TokenCommandEnvelope) (TokenCommandResult, error)
		})
		if !supported {
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_EXECUTOR_UNAVAILABLE", "the approved credential revoke adapter is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		completed, executeErr := executor.ExecuteApprovedCredentialRevoke(r.Context(), item.CommandReference, body.RequesterUserID, body.Envelope)
		if errors.Is(executeErr, ErrCatalogVersionConflict) || errors.Is(executeErr, ErrNotFound) || errors.Is(executeErr, ErrForbidden) || errors.Is(executeErr, ErrTokenCommandInvalid) {
			api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
			return
		}
		if executeErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteJSON(w, http.StatusOK, completed)
		return
	}
	after, result, stableCode, executionErr := s.approvedCommandExecutor.ExecuteApprovedTokenCommand(r.Context(), commandType, body.ExecutionIdempotencyKey, body.Envelope)
	status := "SUCCEEDED"
	if executionErr != nil {
		status = "UNKNOWN"
		if stableCode == "" {
			stableCode = "COMMAND_RESULT_UNKNOWN"
		}
	}
	item, err = repository.CompleteTokenCommand(r.Context(), item.CommandReference, status, after, result, stableCode)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
		return
	}
	if status == "UNKNOWN" {
		w.Header().Set("Retry-After", "2")
		api.WriteJSON(w, http.StatusAccepted, item)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
