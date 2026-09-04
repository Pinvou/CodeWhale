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
	"TOKEN_ACCOUNT_CREATE":             {[]string{"platform.tokens.manage", "platform.tokens.owner-candidates.read"}, true, false, []string{"owner_type", "owner_id", "owner_authorization_version", "creation_version", "precheck_fingerprint", "request_fingerprint"}},
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
	ApprovalRequestID       uuid.UUID            `json:"approval_request_id"`
	RequesterUserID         uuid.UUID            `json:"requester_user_id"`
	AuthorizationVersion    int64                `json:"authorization_version"`
	CommandFingerprint      string               `json:"command_fingerprint"`
	ExecutionIdempotencyKey string               `json:"execution_idempotency_key"`
	Envelope                TokenCommandEnvelope `json:"command"`
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
			return item, tx.Commit(ctx)
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
	return item, tx.Commit(ctx)
}

func validStableTokenErrorCode(code string) bool {
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
			for key, child := range typed {
				normalized := normalizeTokenSensitiveFieldName(key)
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
	spec, ok := tokenCommandSpecs[strings.ToUpper(strings.TrimSpace(envelope.CommandType))]
	if !ok || spec.Approval {
		api.WriteError(w, http.StatusForbidden, "DIRECT_APPROVAL_COMMAND_FORBIDDEN", "submit this command through the approval workflow", httpserver.RequestID(r.Context()))
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
	if _, requiresAuthoritativePrecheck := directCommandPrecheckTypes[strings.ToUpper(strings.TrimSpace(envelope.CommandType))]; requiresAuthoritativePrecheck {
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
		if envelope.PrecheckFingerprint != precheck.PrecheckFingerprint || !tokenCommandExpectedVersionsMatch(envelope.ExpectedVersions, precheck.ExpectedVersions) || len(precheck.Blockers) > 0 {
			api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the governed command no longer matches the authoritative precheck", httpserver.RequestID(r.Context()))
			return
		}
		authoritativeBefore = precheck.ExpectedVersions
	}
	begin.BeforeState = authoritativeBefore
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
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	w.Header().Set("Retry-After", "2")
	api.WriteJSON(w, status, item)
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
	canonicalEnvelope, requestDigest, _, idempotencyFingerprint, normalizeErr := normalizeTokenCommand(body.Envelope, body.ExecutionIdempotencyKey, body.CommandFingerprint)
	if normalizeErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVED_COMMAND", "the approved Token command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	body.Envelope = canonicalEnvelope
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
		before, precheckErr := s.approvedCommandExecutor.PrecheckApprovedTokenCommand(r.Context(), commandType, body.Envelope)
		if precheckErr != nil {
			api.WriteError(w, http.StatusConflict, "PRECHECK_CHANGED", "the approved command no longer matches current resource facts", httpserver.RequestID(r.Context()))
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
