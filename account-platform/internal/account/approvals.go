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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrApprovalSelfDecision = errors.New("approval requester cannot decide their own request")
	ErrApprovalInvalidated  = errors.New("approval request is invalidated")
	ErrApprovalExpired      = errors.New("approval request is expired")
)

type ApprovalRequest struct {
	ID                            uuid.UUID       `json:"approval_request_id"`
	CommandType                   string          `json:"command_type"`
	PolicyVersion                 int64           `json:"policy_version"`
	ApprovalMode                  string          `json:"approval_mode"`
	RiskClass                     string          `json:"risk_class"`
	RequesterUserID               uuid.UUID       `json:"requester_user_id"`
	RequesterBindingID            uuid.UUID       `json:"requester_binding_id"`
	RequesterAuthorizationVersion int64           `json:"requester_authorization_version"`
	NormalizedCommand             json.RawMessage `json:"normalized_command"`
	CommandFingerprint            string          `json:"command_fingerprint"`
	TargetResourceType            string          `json:"target_resource_type"`
	TargetResourceID              string          `json:"target_resource_id"`
	Reason                        string          `json:"reason"`
	RedactedEvidence              json.RawMessage `json:"redacted_evidence"`
	ExpectedImpact                json.RawMessage `json:"expected_impact"`
	Status                        string          `json:"status"`
	Version                       int64           `json:"version"`
	SupersedesRequestID           *uuid.UUID      `json:"supersedes_request_id,omitempty"`
	ExpiresAt                     time.Time       `json:"expires_at"`
	ExecutionDeadline             *time.Time      `json:"execution_deadline,omitempty"`
	InvalidationCode              *string         `json:"invalidation_code,omitempty"`
	InvalidationDetails           json.RawMessage `json:"invalidation_details,omitempty"`
	SubmittedAt                   *time.Time      `json:"submitted_at,omitempty"`
	DecidedAt                     *time.Time      `json:"decided_at,omitempty"`
	CreatedAt                     time.Time       `json:"created_at"`
	UpdatedAt                     time.Time       `json:"updated_at"`
	CriticalResourceVersions      json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot             json.RawMessage `json:"financial_snapshot"`
	PolicySnapshot                json.RawMessage `json:"policy_snapshot"`
	AllowedActions                []string        `json:"allowed_actions"`
	RequesterPermission           string          `json:"-"`
	ApproverPermission            string          `json:"-"`
}

type ApprovalDraft struct {
	CommandType              string
	IdempotencyKey           string
	NormalizedCommand        json.RawMessage
	TargetResourceType       string
	TargetResourceID         string
	Reason                   string
	RedactedEvidence         json.RawMessage
	ExpectedImpact           json.RawMessage
	CriticalResourceVersions json.RawMessage
	FinancialSnapshot        json.RawMessage
	SupersedesRequestID      *uuid.UUID
}

type ApprovalFilter struct {
	CommandType   string
	Status        string
	RiskClass     string
	Requester     *uuid.UUID
	Target        string
	SubmittedFrom *time.Time
	SubmittedTo   *time.Time
	ExpiresFrom   *time.Time
	ExpiresTo     *time.Time
	Limit         int
}

type ApprovalExecution struct {
	ApprovalRequestID        uuid.UUID       `json:"approval_request_id"`
	CommandType              string          `json:"command_type"`
	PolicyVersion            int64           `json:"policy_version"`
	CommandFingerprint       string          `json:"command_fingerprint"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	RequesterUserID          uuid.UUID       `json:"requester_user_id"`
	ApproverUserID           uuid.UUID       `json:"approver_user_id"`
	DecisionReason           string          `json:"decision_reason"`
	ExecutionIdempotencyKey  string          `json:"execution_idempotency_key"`
	ExecutorIdentity         string          `json:"executor_identity"`
	Status                   string          `json:"status"`
}

func (r *PostgresRepository) ApprovalPolicies(context.Context) []approvalpolicy.Policy {
	items := approvalpolicy.All()
	sort.Slice(items, func(i, j int) bool { return items[i].CommandType < items[j].CommandType })
	return items
}

func (r *PostgresRepository) CreateApprovalDraft(ctx context.Context, principal identity.Principal, draft ApprovalDraft) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	policy, normalized, fingerprint, err := normalizeApprovalDraft(draft)
	if err != nil || policy.Mode != approvalpolicy.ApprovalRequired {
		if err == nil {
			err = ErrConflict
		}
		return ApprovalRequest{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalRequest{}, err
	}
	defer tx.Rollback(ctx)
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		return ApprovalRequest{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		return ApprovalRequest{}, ErrForbidden
	}
	bindingID, authorizationVersion, validUntil, err := strongPlatformAuthorization(ctx, tx, *actorID, policy.RequesterPermission)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if draft.SupersedesRequestID != nil {
		var previousRequester uuid.UUID
		var previousStatus string
		if err := tx.QueryRow(ctx, `SELECT requester_user_id,status FROM approval_requests WHERE id=$1`, *draft.SupersedesRequestID).Scan(&previousRequester, &previousStatus); errors.Is(err, pgx.ErrNoRows) {
			return ApprovalRequest{}, ErrNotFound
		} else if err != nil {
			return ApprovalRequest{}, err
		}
		if previousRequester != *actorID || !slices.Contains([]string{"REJECTED", "WITHDRAWN", "EXPIRED", "INVALIDATED", "EXECUTED", "EXECUTION_FAILED"}, previousStatus) {
			return ApprovalRequest{}, ErrConflict
		}
	}
	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM approval_requests WHERE requester_user_id=$1 AND command_type=$2 AND idempotency_key=$3`, *actorID, policy.CommandType, strings.TrimSpace(draft.IdempotencyKey)).Scan(&existingID)
	if err == nil {
		existing, loadErr := approvalRequestByID(ctx, tx, existingID)
		if loadErr != nil {
			return ApprovalRequest{}, loadErr
		}
		if existing.CommandFingerprint != hex.EncodeToString(fingerprint[:]) {
			return ApprovalRequest{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ApprovalRequest{}, err
	}
	now := r.now()
	id := uuid.Must(uuid.NewV7())
	expiresAt := now.Add(policy.ExpiresAfter)
	policySnapshot, _ := json.Marshal(map[string]any{"policy_version": policy.PolicyVersion, "requester_permission": policy.RequesterPermission, "approver_permission": policy.ApproverPermission, "approval_count": policy.ApprovalCount, "executor_identity": policy.ExecutorIdentity, "valid_until": validUntil})
	if _, err := tx.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,version,supersedes_request_id,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'DRAFT',1,$15,$16,$17,$17)`, id, policy.CommandType, policy.PolicyVersion, *actorID, bindingID, authorizationVersion, strings.TrimSpace(draft.IdempotencyKey), normalized, fingerprint[:], strings.TrimSpace(draft.TargetResourceType), strings.TrimSpace(draft.TargetResourceID), strings.TrimSpace(draft.Reason), jsonOrDefault(draft.RedactedEvidence, `[]`), jsonOrDefault(draft.ExpectedImpact, `{}`), draft.SupersedesRequestID, expiresAt, now); err != nil {
		return ApprovalRequest{}, mapConstraintError(err)
	}
	requesterSnapshot, _ := json.Marshal(map[string]any{"user_id": actorID, "binding_id": bindingID, "authorization_version": authorizationVersion, "valid_until": validUntil})
	if _, err := tx.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, policy.SnapshotSchemaVersion, jsonOrDefault(draft.CriticalResourceVersions, `{}`), jsonOrDefault(draft.FinancialSnapshot, `{}`), policySnapshot, requesterSnapshot, now); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, id, "", "DRAFT", "USER", actorID.String(), "CREATED", map[string]any{"command_fingerprint": hex.EncodeToString(fingerprint[:])}, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, tx, id)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) UpdateApprovalDraft(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64, draft ApprovalDraft) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	policy, normalized, fingerprint, err := normalizeApprovalDraft(draft)
	if err != nil || policy.Mode != approvalpolicy.ApprovalRequired {
		return ApprovalRequest{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalRequest{}, err
	}
	defer tx.Rollback(ctx)
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		return ApprovalRequest{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		return ApprovalRequest{}, ErrForbidden
	}
	current, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if current.RequesterUserID != *actorID || current.Status != "DRAFT" || current.Version != expectedVersion || current.CommandType != policy.CommandType {
		return ApprovalRequest{}, ErrConflict
	}
	if _, _, _, err := strongPlatformAuthorization(ctx, tx, *actorID, policy.RequesterPermission); err != nil {
		return ApprovalRequest{}, err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE approval_requests SET normalized_command=$2,command_fingerprint=$3,target_resource_type=$4,target_resource_id=$5,reason=$6,redacted_evidence=$7,expected_impact=$8,version=version+1,updated_at=$9 WHERE id=$1 AND status='DRAFT' AND version=$10`, requestID, normalized, fingerprint[:], strings.TrimSpace(draft.TargetResourceType), strings.TrimSpace(draft.TargetResourceID), strings.TrimSpace(draft.Reason), jsonOrDefault(draft.RedactedEvidence, `[]`), jsonOrDefault(draft.ExpectedImpact, `{}`), now, expectedVersion)
	if err != nil || command.RowsAffected() != 1 {
		return ApprovalRequest{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_request_snapshots SET critical_resource_versions=$2,financial_snapshot=$3 WHERE approval_request_id=$1`, requestID, jsonOrDefault(draft.CriticalResourceVersions, `{}`), jsonOrDefault(draft.FinancialSnapshot, `{}`)); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, requestID, "DRAFT", "DRAFT", "USER", actorID.String(), "DRAFT_UPDATED", map[string]any{"command_fingerprint": hex.EncodeToString(fingerprint[:])}, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) SubmitApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64) (ApprovalRequest, error) {
	return r.transitionOwnApproval(ctx, principal, requestID, expectedVersion, "DRAFT", "SUBMITTED", "SUBMITTED")
}

func (r *PostgresRepository) WithdrawApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64) (ApprovalRequest, error) {
	return r.transitionOwnApproval(ctx, principal, requestID, expectedVersion, "SUBMITTED", "WITHDRAWN", "WITHDRAWN_BY_REQUESTER")
}

func (r *PostgresRepository) transitionOwnApproval(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64, from, to, reason string) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalRequest{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		return ApprovalRequest{}, ErrForbidden
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if item.RequesterUserID != *actorID {
		return ApprovalRequest{}, ErrForbidden
	}
	if item.Status != from || item.Version != expectedVersion {
		now := r.now()
		if err := appendApprovalStateEvent(ctx, tx, requestID, item.Status, item.Status, "USER", actorID.String(), "TRANSITION_CONFLICT", map[string]any{"requested_transition": to, "expected_version": expectedVersion, "current_version": item.Version}, now); err != nil {
			return ApprovalRequest{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApprovalRequest{}, err
		}
		return ApprovalRequest{}, ErrConflict
	}
	now := r.now()
	submittedExpiresAt := item.ExpiresAt
	if from == "DRAFT" {
		policy, lookupErr := approvalpolicy.Lookup(item.CommandType)
		if lookupErr != nil || policy.PolicyVersion != item.PolicyVersion {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		bindingID, authVersion, _, authErr := strongPlatformAuthorization(ctx, tx, *actorID, policy.RequesterPermission)
		if authErr != nil || bindingID != item.RequesterBindingID || authVersion != item.RequesterAuthorizationVersion {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		submittedExpiresAt = now.Add(policy.ExpiresAfter)
	}
	command, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$4,submitted_at=CASE WHEN $4='SUBMITTED' THEN $5 ELSE submitted_at END,expires_at=CASE WHEN $4='SUBMITTED' THEN $7 ELSE expires_at END,version=version+1,updated_at=$5 WHERE id=$1 AND requester_user_id=$2 AND status=$3 AND version=$6`, requestID, *actorID, from, to, now, expectedVersion, submittedExpiresAt)
	if err != nil || command.RowsAffected() != 1 {
		return ApprovalRequest{}, ErrConflict
	}
	if err := appendApprovalStateEvent(ctx, tx, requestID, from, to, "USER", actorID.String(), reason, map[string]any{}, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err = approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalRequest{}, err
	}
	return item, nil
}

func (r *PostgresRepository) DecideApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64, decision, reason string, observedVersions json.RawMessage) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	decision, reason = strings.ToUpper(strings.TrimSpace(decision)), strings.TrimSpace(reason)
	if (decision != "APPROVED" && decision != "REJECTED") || len(reason) < 3 || len(reason) > 2000 {
		return ApprovalRequest{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalRequest{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		return ApprovalRequest{}, ErrForbidden
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if item.Status != "SUBMITTED" || item.Version != expectedVersion {
		if policy, lookupErr := approvalPolicyForItem(item); lookupErr == nil {
			if _, _, _, authErr := strongPlatformAuthorization(ctx, tx, *actorID, policy.ApproverPermission); authErr == nil {
				now := r.now()
				if err := appendApprovalStateEvent(ctx, tx, requestID, item.Status, item.Status, "USER", actorID.String(), "DECISION_CONFLICT", map[string]any{"expected_version": expectedVersion, "current_version": item.Version}, now); err != nil {
					return ApprovalRequest{}, err
				}
				if err := tx.Commit(ctx); err != nil {
					return ApprovalRequest{}, err
				}
			}
		}
		return ApprovalRequest{}, ErrConflict
	}
	if item.RequesterUserID == *actorID {
		now := r.now()
		if err := appendApprovalStateEvent(ctx, tx, requestID, item.Status, item.Status, "USER", actorID.String(), "SELF_DECISION_REJECTED", map[string]any{}, now); err != nil {
			return ApprovalRequest{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApprovalRequest{}, err
		}
		return ApprovalRequest{}, ErrApprovalSelfDecision
	}
	now := r.now()
	if !item.ExpiresAt.After(now) {
		return r.invalidateApproval(ctx, tx, item, *actorID, "EXPIRED", "REQUEST_EXPIRED", map[string]any{}, now, ErrApprovalExpired)
	}
	policy, err := approvalPolicyForItem(item)
	if err != nil {
		return r.invalidateApproval(ctx, tx, item, *actorID, "INVALIDATED", "POLICY_VERSION_CHANGED", map[string]any{}, now, ErrApprovalInvalidated)
	}
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		return r.invalidateApproval(ctx, tx, item, *actorID, "INVALIDATED", "POLICY_VERSION_CHANGED", map[string]any{}, now, ErrApprovalInvalidated)
	}
	bindingID, authorizationVersion, validUntil, err := strongPlatformAuthorization(ctx, tx, *actorID, policy.ApproverPermission)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if decision == "APPROVED" && !sameJSONObject(observedVersions, item.CriticalResourceVersions) {
		return r.invalidateApproval(ctx, tx, item, *actorID, "INVALIDATED", "RESOURCE_VERSION_CHANGED", map[string]any{"observed": jsonOrDefault(observedVersions, `{}`)}, now, ErrApprovalInvalidated)
	}
	executionDeadline := now.Add(policy.ExecutionWindow)
	if decision == "REJECTED" {
		executionDeadline = time.Time{}
	}
	authSnapshot, _ := json.Marshal(map[string]any{"user_id": actorID, "binding_id": bindingID, "authorization_version": authorizationVersion, "valid_until": validUntil})
	if _, err := tx.Exec(ctx, `INSERT INTO approval_decisions(id,approval_request_id,approver_user_id,approver_binding_id,approver_authorization_version,decision,reason,authorization_snapshot,decided_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), requestID, *actorID, bindingID, authorizationVersion, decision, reason, authSnapshot, now); err != nil {
		return ApprovalRequest{}, mapConstraintError(err)
	}
	var deadline *time.Time
	if decision == "APPROVED" {
		deadline = &executionDeadline
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,execution_deadline=$3,decided_at=$4,version=version+1,updated_at=$4 WHERE id=$1`, requestID, decision, deadline, now); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, requestID, "SUBMITTED", decision, "USER", actorID.String(), "INDEPENDENT_DECISION", map[string]any{"reason": reason}, now); err != nil {
		return ApprovalRequest{}, err
	}
	if decision == "APPROVED" {
		payload := map[string]any{"approval_request_id": requestID, "command_type": item.CommandType, "command_fingerprint": item.CommandFingerprint, "policy_version": item.PolicyVersion, "executor_identity": policy.ExecutorIdentity, "execution_deadline": executionDeadline, "normalized_command": item.NormalizedCommand, "target_resource_type": item.TargetResourceType, "target_resource_id": item.TargetResourceID, "critical_resource_versions": item.CriticalResourceVersions}
		if err := appendOutbox(ctx, tx, "account.approval-command.authorized.v1", requestID, item.Version+1, payload); err != nil {
			return ApprovalRequest{}, err
		}
	}
	item, err = approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) invalidateApproval(ctx context.Context, tx pgx.Tx, item ApprovalRequest, actorID uuid.UUID, status, code string, details map[string]any, now time.Time, result error) (ApprovalRequest, error) {
	payload, _ := json.Marshal(details)
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,invalidation_code=$3,invalidation_details=$4,version=version+1,updated_at=$5 WHERE id=$1`, item.ID, status, code, payload, now); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, item.ID, item.Status, status, "USER", actorID.String(), code, details, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, tx, item.ID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalRequest{}, err
	}
	return item, result
}

func (r *PostgresRepository) ListApprovalRequests(ctx context.Context, principal identity.Principal, filter ApprovalFilter) ([]ApprovalRequest, error) {
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, approvalSelect+` WHERE ($1='' OR ar.command_type=$1) AND ($2='' OR ar.status=$2) AND ($3='' OR p.risk_class=$3) AND ($4::uuid IS NULL OR ar.requester_user_id=$4) AND ($5='' OR ar.target_resource_id=$5) AND ($6::timestamptz IS NULL OR ar.submitted_at>=$6) AND ($7::timestamptz IS NULL OR ar.submitted_at<$7) AND ($8::timestamptz IS NULL OR ar.expires_at>=$8) AND ($9::timestamptz IS NULL OR ar.expires_at<$9) AND (ar.requester_user_id=$10 OR EXISTS(SELECT 1 FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p.approver_permission WHERE ra.user_id=$10 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now()))) ORDER BY ar.updated_at DESC,ar.id LIMIT $11`, strings.ToUpper(strings.TrimSpace(filter.CommandType)), strings.ToUpper(strings.TrimSpace(filter.Status)), strings.ToUpper(strings.TrimSpace(filter.RiskClass)), filter.Requester, strings.TrimSpace(filter.Target), filter.SubmittedFrom, filter.SubmittedTo, filter.ExpiresFrom, filter.ExpiresTo, actorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ApprovalRequest, 0)
	for rows.Next() {
		item, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		canRequest, canApprove, err := r.approvalCapabilities(ctx, actorID, item)
		if err != nil {
			return nil, err
		}
		item.AllowedActions = approvalAllowedActions(item, actorID, canRequest, canApprove)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) ApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID) (ApprovalRequest, error) {
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, r.pool, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if item.RequesterUserID == actorID {
		canRequest, _, err := r.approvalCapabilities(ctx, actorID, item)
		if err != nil {
			return ApprovalRequest{}, err
		}
		item.AllowedActions = approvalAllowedActions(item, actorID, canRequest, false)
		return item, nil
	}
	_, canApprove, err := r.approvalCapabilities(ctx, actorID, item)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if !canApprove {
		return ApprovalRequest{}, ErrForbidden
	}
	item.AllowedActions = approvalAllowedActions(item, actorID, false, true)
	return item, nil
}

func (r *PostgresRepository) approvalCapabilities(ctx context.Context, actorID uuid.UUID, item ApprovalRequest) (bool, bool, error) {
	canRequest, canApprove := false, false
	if item.RequesterUserID == actorID {
		if _, _, _, err := strongPlatformAuthorization(ctx, r.pool, actorID, item.RequesterPermission); err == nil {
			canRequest = true
		} else if !errors.Is(err, ErrForbidden) {
			return false, false, err
		}
	}
	if item.RequesterUserID != actorID && item.ApproverPermission != "" {
		if _, _, _, err := strongPlatformAuthorization(ctx, r.pool, actorID, item.ApproverPermission); err == nil {
			canApprove = true
		} else if !errors.Is(err, ErrForbidden) {
			return false, false, err
		}
	}
	return canRequest, canApprove, nil
}

func approvalAllowedActions(item ApprovalRequest, actorID uuid.UUID, canRequest, canApprove bool) []string {
	actions := make([]string, 0, 3)
	if item.RequesterUserID == actorID && canRequest {
		if item.Status == "DRAFT" {
			actions = append(actions, "EDIT", "SUBMIT")
		}
		if item.Status == "SUBMITTED" {
			actions = append(actions, "WITHDRAW")
		}
	}
	if canApprove && item.RequesterUserID != actorID && item.Status == "SUBMITTED" {
		actions = append(actions, "APPROVE", "REJECT")
	}
	return actions
}

func (r *PostgresRepository) userIDByPrincipal(ctx context.Context, principal identity.Principal) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT u.id FROM identities i JOIN users u ON u.id=i.user_id AND u.status='ACTIVE' WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return userID, err
}

func humanApprovalPrincipal(principal identity.Principal) bool {
	return strings.TrimSpace(principal.Issuer) != "" && strings.TrimSpace(principal.Subject) != "" && strings.TrimSpace(principal.SessionID) != "" && !principal.AuthenticatedAt.IsZero()
}

func approvalPolicyForItem(item ApprovalRequest) (approvalpolicy.Policy, error) {
	policy, err := approvalpolicy.Lookup(item.CommandType)
	if err != nil || item.PolicyVersion != policy.PolicyVersion || policy.Mode != approvalpolicy.ApprovalRequired || item.RequesterPermission != policy.RequesterPermission || item.ApproverPermission != policy.ApproverPermission {
		return approvalpolicy.Policy{}, ErrApprovalInvalidated
	}
	return policy, nil
}

func normalizeApprovalDraft(draft ApprovalDraft) (approvalpolicy.Policy, []byte, [32]byte, error) {
	policy, err := approvalpolicy.Lookup(draft.CommandType)
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	if policy.Mode == approvalpolicy.ApprovalRequired && !approvalpolicy.Executable(policy.CommandType) {
		return approvalpolicy.Policy{}, nil, [32]byte{}, ErrConflict
	}
	if len(strings.TrimSpace(draft.IdempotencyKey)) < 8 || len(draft.IdempotencyKey) > 120 || len(strings.TrimSpace(draft.TargetResourceType)) < 1 || len(strings.TrimSpace(draft.TargetResourceType)) > 80 || len(strings.TrimSpace(draft.TargetResourceID)) < 1 || len(strings.TrimSpace(draft.TargetResourceID)) > 160 || len(strings.TrimSpace(draft.Reason)) < 10 || len(strings.TrimSpace(draft.Reason)) > 2000 {
		return approvalpolicy.Policy{}, nil, [32]byte{}, ErrConflict
	}
	normalized, err := canonicalSafeJSON(draft.NormalizedCommand)
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	criticalVersions, err := canonicalSafeJSONObject(jsonOrDefault(draft.CriticalResourceVersions, `{}`))
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	financialSnapshot, err := canonicalSafeJSONObject(jsonOrDefault(draft.FinancialSnapshot, `{}`))
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	if _, err := canonicalSafeJSONObject(jsonOrDefault(draft.ExpectedImpact, `{}`)); err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	if _, err := canonicalSafeJSONValue(jsonOrDefault(draft.RedactedEvidence, `[]`), "object", "array"); err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	fingerprint := sha256.Sum256(bytes.Join([][]byte{[]byte(policy.CommandType), []byte(fmt.Sprint(policy.PolicyVersion)), normalized, []byte(strings.TrimSpace(draft.TargetResourceType)), []byte(strings.TrimSpace(draft.TargetResourceID)), criticalVersions, financialSnapshot}, []byte{0}))
	return policy, normalized, fingerprint, nil
}

func canonicalSafeJSON(raw json.RawMessage) ([]byte, error) {
	canonical, err := canonicalSafeJSONValue(raw, "object")
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func canonicalSafeJSONValue(raw json.RawMessage, allowedTypes ...string) ([]byte, error) {
	if len(raw) == 0 || len(raw) > 256<<10 {
		return nil, ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrConflict
	}
	if containsSecretField(value) {
		return nil, ErrConflict
	}
	valueType := ""
	switch value.(type) {
	case map[string]any:
		valueType = "object"
	case []any:
		valueType = "array"
	}
	if !slices.Contains(allowedTypes, valueType) {
		return nil, ErrConflict
	}
	return json.Marshal(value)
}

func containsSecretField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
			if strings.Contains(normalized, "password") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "cookie") || normalized == "authorization" || normalized == "api_key" || strings.HasSuffix(normalized, "_token") || normalized == "token" {
				return true
			}
			if containsSecretField(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsSecretField(child) {
				return true
			}
		}
	}
	return false
}

func strongPlatformAuthorization(ctx context.Context, database queryable, userID uuid.UUID, permission string) (uuid.UUID, int64, *time.Time, error) {
	var bindingID uuid.UUID
	var validUntil *time.Time
	err := database.QueryRow(ctx, `SELECT ra.id,ra.expires_at FROM users u JOIN role_assignments ra ON ra.user_id=u.id AND ra.organization_id IS NULL JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$2 WHERE u.id=$1 AND u.status='ACTIVE' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now()) ORDER BY ra.expires_at NULLS LAST,ra.id LIMIT 1`, userID, permission).Scan(&bindingID, &validUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, nil, ErrForbidden
	}
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	var version int64
	if err := database.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1)`, userID).Scan(&version); err != nil {
		return uuid.Nil, 0, nil, err
	}
	return bindingID, version, validUntil, nil
}

func assertStoredApprovalPolicy(ctx context.Context, database queryable, policy approvalpolicy.Policy) error {
	var mode, requesterPermission, riskClass, executorIdentity string
	var approverPermission *string
	var approvalCount, snapshotSchemaVersion int
	var selfApprovalForbidden bool
	var expiresSeconds, executionSeconds float64
	var criticalFieldsJSON json.RawMessage
	err := database.QueryRow(ctx, `SELECT approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,executor_identity,EXTRACT(EPOCH FROM expires_after),EXTRACT(EPOCH FROM execution_window) FROM approval_command_policies WHERE command_type=$1 AND policy_version=$2`, policy.CommandType, policy.PolicyVersion).Scan(&mode, &requesterPermission, &approverPermission, &riskClass, &approvalCount, &selfApprovalForbidden, &snapshotSchemaVersion, &criticalFieldsJSON, &executorIdentity, &expiresSeconds, &executionSeconds)
	if err != nil {
		return err
	}
	storedApprover := ""
	if approverPermission != nil {
		storedApprover = *approverPermission
	}
	var criticalFields []string
	if json.Unmarshal(criticalFieldsJSON, &criticalFields) != nil || mode != string(policy.Mode) || requesterPermission != policy.RequesterPermission || storedApprover != policy.ApproverPermission || riskClass != policy.RiskClass || approvalCount != policy.ApprovalCount || selfApprovalForbidden != policy.SelfApprovalForbidden || snapshotSchemaVersion != policy.SnapshotSchemaVersion || !slices.Equal(criticalFields, policy.CriticalResourceFields) || executorIdentity != policy.ExecutorIdentity || int64(expiresSeconds) != int64(policy.ExpiresAfter/time.Second) || int64(executionSeconds) != int64(policy.ExecutionWindow/time.Second) {
		return approvalpolicy.ErrPolicyNotRegistered
	}
	return nil
}

const approvalSelect = `SELECT ar.id,ar.command_type,ar.policy_version,p.approval_mode,p.risk_class,ar.requester_user_id,ar.requester_binding_id,ar.requester_authorization_version,ar.normalized_command,encode(ar.command_fingerprint,'hex'),ar.target_resource_type,ar.target_resource_id,ar.reason,ar.redacted_evidence,ar.expected_impact,ar.status,ar.version,ar.supersedes_request_id,ar.expires_at,ar.execution_deadline,ar.invalidation_code,ar.invalidation_details,ar.submitted_at,ar.decided_at,ar.created_at,ar.updated_at,s.critical_resource_versions,s.financial_snapshot,s.policy_snapshot,p.requester_permission,COALESCE(p.approver_permission,'') FROM approval_requests ar JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version JOIN approval_request_snapshots s ON s.approval_request_id=ar.id`

type rowScanner interface{ Scan(...any) error }

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanApprovalRequest(row rowScanner) (ApprovalRequest, error) {
	var item ApprovalRequest
	err := row.Scan(&item.ID, &item.CommandType, &item.PolicyVersion, &item.ApprovalMode, &item.RiskClass, &item.RequesterUserID, &item.RequesterBindingID, &item.RequesterAuthorizationVersion, &item.NormalizedCommand, &item.CommandFingerprint, &item.TargetResourceType, &item.TargetResourceID, &item.Reason, &item.RedactedEvidence, &item.ExpectedImpact, &item.Status, &item.Version, &item.SupersedesRequestID, &item.ExpiresAt, &item.ExecutionDeadline, &item.InvalidationCode, &item.InvalidationDetails, &item.SubmittedAt, &item.DecidedAt, &item.CreatedAt, &item.UpdatedAt, &item.CriticalResourceVersions, &item.FinancialSnapshot, &item.PolicySnapshot, &item.RequesterPermission, &item.ApproverPermission)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalRequest{}, ErrNotFound
	}
	return item, err
}

func approvalRequestByID(ctx context.Context, database queryable, id uuid.UUID) (ApprovalRequest, error) {
	return scanApprovalRequest(database.QueryRow(ctx, approvalSelect+` WHERE ar.id=$1`, id))
}

func approvalRequestByIDForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ApprovalRequest, error) {
	return scanApprovalRequest(tx.QueryRow(ctx, approvalSelect+` WHERE ar.id=$1 FOR UPDATE OF ar`, id))
}

func appendApprovalStateEvent(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, from, to, actorType, actorID, reason string, details map[string]any, now time.Time) error {
	payload, _ := json.Marshal(details)
	var fromValue *string
	if from != "" {
		fromValue = &from
	}
	_, err := tx.Exec(ctx, `INSERT INTO approval_state_events(id,approval_request_id,from_status,to_status,actor_type,actor_id,reason_code,details,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), requestID, fromValue, to, actorType, actorID, reason, payload, now)
	return err
}

func jsonOrDefault(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}

func sameJSONObject(left, right json.RawMessage) bool {
	canonical := func(raw json.RawMessage) ([]byte, error) {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(jsonOrDefault(raw, `{}`)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	a, leftErr := canonical(left)
	b, rightErr := canonical(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(a, b)
}
