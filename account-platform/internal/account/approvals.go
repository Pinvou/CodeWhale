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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrApprovalSelfDecision        = errors.New("approval requester cannot decide their own request")
	ErrApprovalInvalidated         = errors.New("approval request is invalidated")
	ErrApprovalExpired             = errors.New("approval request is expired")
	ErrTokenGovernanceInactive     = errors.New("token command governance is not active")
	ErrApprovalSnapshotUnavailable = errors.New("approval snapshot is unavailable")
	ErrApprovalSnapshotTimeout     = errors.New("approval snapshot timed out")
	ErrApprovalSnapshotRejected    = errors.New("approval snapshot request was rejected")
	ErrInvalidApprovalInput        = errors.New("approval request input is invalid")
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
	SnapshotSchemaVersion         int             `json:"-"`
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
	ApprovalRequestID             uuid.UUID       `json:"approval_request_id"`
	CommandType                   string          `json:"command_type"`
	PolicyVersion                 int64           `json:"policy_version"`
	CommandFingerprint            string          `json:"command_fingerprint"`
	NormalizedCommand             json.RawMessage `json:"normalized_command"`
	TargetResourceType            string          `json:"target_resource_type"`
	TargetResourceID              string          `json:"target_resource_id"`
	CriticalResourceVersions      json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot             json.RawMessage `json:"financial_snapshot"`
	RequesterUserID               uuid.UUID       `json:"requester_user_id"`
	RequesterAuthorizationVersion int64           `json:"requester_authorization_version"`
	RequesterReason               string          `json:"requester_reason"`
	FirstClaim                    bool            `json:"first_claim"`
	ApproverUserID                uuid.UUID       `json:"approver_user_id"`
	DecisionReason                string          `json:"decision_reason"`
	ExecutionIdempotencyKey       string          `json:"execution_idempotency_key"`
	ExecutorIdentity              string          `json:"executor_identity"`
	Status                        string          `json:"status"`
	AuthorizationEventID          *uuid.UUID      `json:"authorization_event_id,omitempty"`
	AuthorizationDurable          string          `json:"authorization_durable,omitempty"`
	AuthorizationApprovedVersion  int64           `json:"authorization_approved_request_version,omitempty"`
	AuthorizationEventDigest      string          `json:"authorization_event_digest,omitempty"`
	AuthorizationDigestKind       string          `json:"authorization_digest_kind,omitempty"`
	AuthorizationSerialization    string          `json:"authorization_serialization_version,omitempty"`
	AuthorizationProvenance       string          `json:"authorization_provenance,omitempty"`
	AuthorizationApprovalEpoch    int64           `json:"authorization_approval_epoch,omitempty"`
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
	policy, originalFingerprint, err := validateApprovalDraftRequest(draft)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if policy.Mode != approvalpolicy.ApprovalRequired {
		return ApprovalRequest{}, ErrConflict
	}
	// The actor-scoped advisory lock below serializes an idempotency key. READ
	// COMMITTED is intentional: a waiter must observe the winner's committed
	// row after acquiring the lock. Authorization rows are held with SHARE
	// locks by strongPlatformAuthorization until this transaction commits.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
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
	lockScope := actorID.String() + "\x00" + policy.CommandType + "\x00" + strings.TrimSpace(draft.IdempotencyKey)
	// Keep unambiguous scope boundaries without sending NUL bytes as SQL text.
	lockDigest := sha256.Sum256([]byte(lockScope))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, hex.EncodeToString(lockDigest[:])); err != nil {
		return ApprovalRequest{}, err
	}
	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM approval_requests WHERE requester_user_id=$1 AND command_type=$2 AND idempotency_key=$3`, *actorID, policy.CommandType, strings.TrimSpace(draft.IdempotencyKey)).Scan(&existingID)
	if err == nil {
		existing, loadErr := approvalRequestByID(ctx, tx, existingID)
		if loadErr != nil {
			return ApprovalRequest{}, loadErr
		}
		existingFingerprint, fingerprintErr := approvalRequestOriginalFingerprint(existing)
		if fingerprintErr != nil || existingFingerprint != originalFingerprint {
			return ApprovalRequest{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ApprovalRequest{}, err
	}
	// APT-138 owns activation. Existing same-request replays above remain
	// readable, while a new executable Token approval fails closed only after
	// policy, actor and strong authorization have been established.
	if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
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
	draft, err = r.authoritativeApprovalDraft(ctx, draft)
	if err != nil {
		return ApprovalRequest{}, err
	}
	resolvedPolicy, normalized, fingerprint, err := normalizeApprovalDraft(draft)
	if err != nil || resolvedPolicy.CommandType != policy.CommandType || resolvedPolicy.PolicyVersion != policy.PolicyVersion {
		return ApprovalRequest{}, ErrConflict
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
	policy, _, err := validateApprovalDraftRequest(draft)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if policy.Mode != approvalpolicy.ApprovalRequired {
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
	if _, err := approvalRequestVisibilityForActor(ctx, tx, *actorID, requestID); err != nil {
		return ApprovalRequest{}, err
	}
	current, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, approvalVisibleRequestReadError(err)
	}
	if current.RequesterUserID != *actorID || current.Status != "DRAFT" || current.Version != expectedVersion || current.CommandType != policy.CommandType {
		return ApprovalRequest{}, ErrConflict
	}
	currentPolicy, policyErr := approvalPolicyForItem(current)
	if policyErr != nil || currentPolicy.PolicyVersion != policy.PolicyVersion || currentPolicy.SnapshotSchemaVersion != policy.SnapshotSchemaVersion {
		return ApprovalRequest{}, ErrApprovalInvalidated
	}
	if _, _, _, err := strongPlatformAuthorization(ctx, tx, *actorID, policy.RequesterPermission); err != nil {
		return ApprovalRequest{}, err
	}
	if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
		return ApprovalRequest{}, err
	}
	draft, err = r.authoritativeApprovalDraft(ctx, draft)
	if err != nil {
		return ApprovalRequest{}, err
	}
	resolvedPolicy, normalized, fingerprint, err := normalizeApprovalDraft(draft)
	if err != nil || resolvedPolicy.CommandType != policy.CommandType || resolvedPolicy.PolicyVersion != policy.PolicyVersion {
		return ApprovalRequest{}, ErrConflict
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
	if _, err := approvalRequestVisibilityForActor(ctx, tx, *actorID, requestID); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, approvalVisibleRequestReadError(err)
	}
	if item.RequesterUserID != *actorID {
		return ApprovalRequest{}, ErrForbidden
	}
	if to == "WITHDRAWN" {
		if _, _, _, err := strongPlatformAuthorization(ctx, tx, *actorID, item.RequesterPermission); err != nil {
			return ApprovalRequest{}, err
		}
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
		policy, lookupErr := approvalPolicyForItem(item)
		if lookupErr != nil {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		bindingID, authVersion, _, authErr := strongPlatformAuthorization(ctx, tx, *actorID, policy.RequesterPermission)
		if authErr != nil || bindingID != item.RequesterBindingID || authVersion != item.RequesterAuthorizationVersion {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
			return ApprovalRequest{}, err
		}
		if approvalSnapshotCommand(item.CommandType) {
			if r.approvalSnapshots == nil {
				return ApprovalRequest{}, ErrApprovalInvalidated
			}
			projection, precheckErr := r.precheckApprovalSnapshot(ctx, ApprovalSnapshotRequest{CommandType: item.CommandType, TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID, NormalizedCommand: item.NormalizedCommand})
			if precheckErr != nil {
				return ApprovalRequest{}, precheckErr
			}
			if !sameJSONObject(projection.CriticalResourceVersions, item.CriticalResourceVersions) || !sameApprovalFinancialFacts(item.CommandType, projection.FinancialSnapshot, item.FinancialSnapshot) {
				return r.invalidateApproval(ctx, tx, item, *actorID, "INVALIDATED", "AUTHORITATIVE_SNAPSHOT_CHANGED", map[string]any{}, now, ErrApprovalInvalidated)
			}
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
	if strings.EqualFold(strings.TrimSpace(decision), "APPROVED") {
		return r.approveApprovalRequest(ctx, principal, requestID, expectedVersion, reason, observedVersions)
	}
	return r.decideApprovalRequestWithoutAuthorization(ctx, principal, requestID, expectedVersion, decision, reason, observedVersions)
}

func (r *PostgresRepository) decideApprovalRequestWithoutAuthorization(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64, decision, reason string, observedVersions json.RawMessage) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	decision, reason = strings.ToUpper(strings.TrimSpace(decision)), strings.TrimSpace(reason)
	if decision != "REJECTED" || len(reason) < 3 || len(reason) > 2000 {
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
	if _, err := approvalRequestVisibilityForActor(ctx, tx, *actorID, requestID); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, approvalVisibleRequestReadError(err)
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
			} else if errors.Is(authErr, ErrForbidden) {
				return ApprovalRequest{}, ErrApprovalRequestNotFound
			} else {
				return ApprovalRequest{}, authErr
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
		if errors.Is(err, ErrForbidden) {
			return ApprovalRequest{}, ErrApprovalRequestNotFound
		}
		return ApprovalRequest{}, err
	}
	authSnapshot, _ := json.Marshal(map[string]any{"user_id": actorID, "binding_id": bindingID, "authorization_version": authorizationVersion, "valid_until": validUntil})
	if _, err := tx.Exec(ctx, `INSERT INTO approval_decisions(id,approval_request_id,approver_user_id,approver_binding_id,approver_authorization_version,decision,reason,authorization_snapshot,decided_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), requestID, *actorID, bindingID, authorizationVersion, decision, reason, authSnapshot, now); err != nil {
		return ApprovalRequest{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,execution_deadline=NULL,decided_at=$3,version=version+1,updated_at=$3 WHERE id=$1`, requestID, decision, now); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEvent(ctx, tx, requestID, "SUBMITTED", decision, "USER", actorID.String(), "INDEPENDENT_DECISION", map[string]any{"reason": reason}, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err = approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) approveApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64, reason string, observedVersions json.RawMessage) (ApprovalRequest, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalRequest{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 || len(reason) > 2000 || requestID == uuid.Nil || expectedVersion < 1 {
		return ApprovalRequest{}, ErrConflict
	}
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if _, err := approvalRequestVisibilityForActor(ctx, r.pool, actorID, requestID); err != nil {
		return ApprovalRequest{}, err
	}
	control, err := r.ApprovalActionControl(ctx)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if err := approvalApproveGateError(control); err != nil {
		return ApprovalRequest{}, err
	}
	if control.Mode == AdminApprovalModeProofRequired {
		// The writer is a prerequisite for a useful domain proof. Check it
		// before spending the owner's bounded read budget.
		if err := r.ApprovalWriterReady(ctx); err != nil {
			return ApprovalRequest{}, ErrApprovalWriterUnavailable
		}
	}
	var checkpoint *approvalProofCheckpoint
	var proofResult *ApprovalProofResult
	if control.Mode == AdminApprovalModeProofRequired {
		observed, err := r.readApprovalProofCheckpoint(ctx, principal, requestID, expectedVersion)
		if err != nil {
			return ApprovalRequest{}, err
		}
		if r.approvalProofSource == nil {
			return ApprovalRequest{}, ErrApprovalProofUnavailable
		}
		policy, err := approvalPolicyForItem(observed.Request)
		if err != nil {
			return ApprovalRequest{}, ErrApprovalInvalidated
		}
		if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
			return ApprovalRequest{}, err
		}
		result, err := r.approvalProofSource.Proof(ctx, requestID, observed.Artifacts)
		if err != nil {
			return ApprovalRequest{}, err
		}
		checkpoint, proofResult = &observed, &result
	}
	// A missing or already-known unusable writer must not take the request row
	// lock or fall back to the broad runtime credential.
	if err := r.ApprovalWriterReady(ctx); err != nil {
		return ApprovalRequest{}, ErrApprovalWriterUnavailable
	}
	decisionID := uuid.Must(uuid.NewV7())
	stateEventID := uuid.Must(uuid.NewV7())
	authorizationEventID := uuid.Must(uuid.NewV7())
	for attempt := 1; attempt <= 3; attempt++ {
		item, committed, rolledBack, err := r.approveApprovalRequestAttempt(
			ctx, principal, requestID, expectedVersion, reason, observedVersions,
			decisionID, stateEventID, authorizationEventID, checkpoint, proofResult,
		)
		if err == nil || committed {
			return item, err
		}
		if !rolledBack {
			return item, approvalAuthorizationPublicError(ctx, err)
		}
		if retryErr := approvalAuthorizationRetryDelay(ctx, attempt, err, nil); retryErr != nil {
			return item, approvalAuthorizationPublicError(ctx, retryErr)
		}
	}
	return ApprovalRequest{}, ErrApprovalWriterUnavailable
}

func (r *PostgresRepository) approveApprovalRequestAttempt(
	ctx context.Context,
	principal identity.Principal,
	requestID uuid.UUID,
	expectedVersion int64,
	reason string,
	observedVersions json.RawMessage,
	decisionID uuid.UUID,
	stateEventID uuid.UUID,
	authorizationEventID uuid.UUID,
	checkpoint *approvalProofCheckpoint,
	proofResult *ApprovalProofResult,
) (ApprovalRequest, bool, bool, error) {
	tx, err := r.approvalWriterPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalRequest{}, false, false, ErrApprovalWriterUnavailable
	}
	rollback := func(result ApprovalRequest, cause error) (ApprovalRequest, bool, bool, error) {
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		rollbackErr := tx.Rollback(rollbackContext)
		return result, false, rollbackErr == nil || errors.Is(rollbackErr, pgx.ErrTxClosed), cause
	}
	control, err := lockApprovalAuthorizationControl(ctx, tx)
	if err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	if checkpoint == nil {
		if control.Mode != AdminApprovalModeLegacy {
			return rollback(ApprovalRequest{}, ErrApprovalAuthorizationControlChanged)
		}
	} else if proofResult == nil || control.Mode != AdminApprovalModeProofRequired ||
		control.ApprovalEpoch != checkpoint.Control.ApprovalEpoch ||
		control.ApprovalWireVersion != checkpoint.Control.ApprovalWireVersion || !control.ProofRequired {
		return rollback(ApprovalRequest{}, ErrApprovalRequestChanged)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	if actorID == nil {
		return rollback(ApprovalRequest{}, ErrForbidden)
	}
	if checkpoint != nil && *actorID != checkpoint.ActorID {
		return rollback(ApprovalRequest{}, ErrApprovalRequestChanged)
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return rollback(ApprovalRequest{}, approvalVisibleRequestReadError(err))
	}
	if item.Status != "SUBMITTED" || item.Version != expectedVersion {
		if checkpoint != nil {
			return rollback(ApprovalRequest{}, ErrApprovalRequestChanged)
		}
		if policy, lookupErr := approvalPolicyForItem(item); lookupErr == nil {
			if _, _, _, authErr := strongApprovalWriterAuthorization(ctx, tx, *actorID, policy.ApproverPermission); authErr == nil {
				if err := appendApprovalStateEventWithID(ctx, tx, stateEventID, requestID, item.Status, item.Status, "USER", actorID.String(), "DECISION_CONFLICT", map[string]any{"expected_version": expectedVersion, "current_version": item.Version}, now); err != nil {
					return rollback(ApprovalRequest{}, err)
				}
				_, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, item, stateEventID, "DECISION_CONFLICT")
				if commitErr != nil {
					return ApprovalRequest{}, committed, rolledBack, commitErr
				}
				return ApprovalRequest{}, true, false, ErrConflict
			} else if errors.Is(authErr, ErrForbidden) {
				return rollback(ApprovalRequest{}, ErrApprovalRequestNotFound)
			} else {
				return rollback(ApprovalRequest{}, authErr)
			}
		}
		return rollback(ApprovalRequest{}, ErrConflict)
	}
	if item.RequesterUserID == *actorID {
		if err := appendApprovalStateEventWithID(ctx, tx, stateEventID, requestID, item.Status, item.Status, "USER", actorID.String(), "SELF_DECISION_REJECTED", map[string]any{}, now); err != nil {
			return rollback(ApprovalRequest{}, err)
		}
		_, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, item, stateEventID, "SELF_DECISION_REJECTED")
		if commitErr != nil {
			return ApprovalRequest{}, committed, rolledBack, commitErr
		}
		return ApprovalRequest{}, true, false, ErrApprovalSelfDecision
	}
	if !item.ExpiresAt.After(now) {
		invalidated, err := r.invalidateApprovalInTransaction(ctx, tx, item, *actorID, "EXPIRED", "REQUEST_EXPIRED", map[string]any{}, now, stateEventID)
		if err != nil {
			return rollback(ApprovalRequest{}, err)
		}
		invalidated, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, invalidated, stateEventID, "REQUEST_EXPIRED")
		if commitErr != nil {
			return ApprovalRequest{}, committed, rolledBack, commitErr
		}
		return invalidated, true, false, ErrApprovalExpired
	}
	policy, err := approvalPolicyForItem(item)
	if err != nil {
		invalidated, invalidationErr := r.invalidateApprovalInTransaction(ctx, tx, item, *actorID, "INVALIDATED", "POLICY_VERSION_CHANGED", map[string]any{}, now, stateEventID)
		if invalidationErr != nil {
			return rollback(ApprovalRequest{}, invalidationErr)
		}
		invalidated, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, invalidated, stateEventID, "POLICY_VERSION_CHANGED")
		if commitErr != nil {
			return ApprovalRequest{}, committed, rolledBack, commitErr
		}
		return invalidated, true, false, ErrApprovalInvalidated
	}
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		invalidated, invalidationErr := r.invalidateApprovalInTransaction(ctx, tx, item, *actorID, "INVALIDATED", "POLICY_VERSION_CHANGED", map[string]any{}, now, stateEventID)
		if invalidationErr != nil {
			return rollback(ApprovalRequest{}, invalidationErr)
		}
		invalidated, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, invalidated, stateEventID, "POLICY_VERSION_CHANGED")
		if commitErr != nil {
			return ApprovalRequest{}, committed, rolledBack, commitErr
		}
		return invalidated, true, false, ErrApprovalInvalidated
	}
	bindingID, authorizationVersion, validUntil, err := strongApprovalWriterAuthorization(ctx, tx, *actorID, policy.ApproverPermission)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return rollback(ApprovalRequest{}, ErrApprovalRequestNotFound)
		}
		return rollback(ApprovalRequest{}, err)
	}
	if checkpoint != nil {
		if bindingID != checkpoint.BindingID || authorizationVersion != checkpoint.AuthorizationVersion ||
			approvalProofNeedsRecentAuthentication(item.CommandType) && !recentlyAuthenticated(principal.AuthenticatedAt, now) {
			return rollback(ApprovalRequest{}, ErrApprovalRequestChanged)
		}
		artifacts, conversionErr := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
			CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, SnapshotSchemaVersion: item.SnapshotSchemaVersion,
			TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID,
			NormalizedCommand: item.NormalizedCommand, CriticalResourceVersions: item.CriticalResourceVersions,
			FinancialSnapshot: item.FinancialSnapshot,
		})
		if conversionErr != nil || !sameProofArtifacts(artifacts, checkpoint.Artifacts) ||
			!approvalencoding.ProofResponseBound(proofResult.Response, requestID, artifacts) ||
			!approvalencoding.ProofResponseFresh(proofResult.Response, now.UTC()) || proofResult.Response.Verdict != "ALLOW" {
			return rollback(ApprovalRequest{}, ErrApprovalProofInvalid)
		}
		derived, deriveErr := approvalencoding.DeriveProofDigest(proofResult.Response)
		if deriveErr != nil || hex.EncodeToString(derived[:]) != proofResult.Digest {
			return rollback(ApprovalRequest{}, ErrApprovalProofInvalid)
		}
	}
	if err := r.requireApprovalAdvancement(ctx, policy); err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	if checkpoint == nil && !sameJSONObject(observedVersions, item.CriticalResourceVersions) {
		invalidated, invalidationErr := r.invalidateApprovalInTransaction(ctx, tx, item, *actorID, "INVALIDATED", "RESOURCE_VERSION_CHANGED", map[string]any{"observed": jsonOrDefault(observedVersions, `{}`)}, now, stateEventID)
		if invalidationErr != nil {
			return rollback(ApprovalRequest{}, invalidationErr)
		}
		invalidated, committed, rolledBack, commitErr := r.commitApprovalWriterTerminal(ctx, tx, invalidated, stateEventID, "RESOURCE_VERSION_CHANGED")
		if commitErr != nil {
			return ApprovalRequest{}, committed, rolledBack, commitErr
		}
		return invalidated, true, false, ErrApprovalInvalidated
	}
	executionDeadline := now.Add(policy.ExecutionWindow)
	authSnapshot, _ := json.Marshal(map[string]any{"user_id": actorID, "binding_id": bindingID, "authorization_version": authorizationVersion, "valid_until": validUntil})
	if _, err := tx.Exec(ctx, `INSERT INTO approval_decisions(id,approval_request_id,approver_user_id,approver_binding_id,approver_authorization_version,decision,reason,authorization_snapshot,decided_at) VALUES($1,$2,$3,$4,$5,'APPROVED',$6,$7,$8)`, decisionID, requestID, *actorID, bindingID, authorizationVersion, reason, authSnapshot, now); err != nil {
		return rollback(ApprovalRequest{}, mapConstraintError(err))
	}
	command, err := tx.Exec(ctx, `UPDATE approval_requests SET status='APPROVED',execution_deadline=$2,decided_at=$3,version=version+1,updated_at=$3 WHERE id=$1 AND status='SUBMITTED' AND version=$4`, requestID, executionDeadline, now, expectedVersion)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return rollback(ApprovalRequest{}, err)
	}
	if err := appendApprovalStateEventWithID(ctx, tx, stateEventID, requestID, "SUBMITTED", "APPROVED", "USER", actorID.String(), "INDEPENDENT_DECISION", map[string]any{"reason": reason}, now); err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	item, err = approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	var authorizationRecord ApprovalAuthorizationRecord
	if checkpoint != nil {
		authorizationRecord, err = appendProofApprovalAuthorization(ctx, tx, control, requestID, authorizationEventID,
			ApprovalAuthorizationProof{Artifacts: checkpoint.Artifacts, Response: proofResult.Response})
	} else {
		authorizationRecord, err = appendLegacyApprovalAuthorization(ctx, tx, control, requestID, authorizationEventID)
	}
	if err != nil {
		return rollback(ApprovalRequest{}, err)
	}
	committed, rolledBack, commitErr := r.commitApprovalWriterTransaction(ctx, tx)
	if commitErr != nil {
		if rolledBack {
			return ApprovalRequest{}, false, true, commitErr
		}
		// Commit errors are outcome-unknown and must never be retried. A fresh
		// connection may only recognize the already-committed closed fact set.
		verificationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var committed bool
		verificationErr := r.approvalWriterPool.QueryRow(verificationContext,
			`SELECT account_approval_authorization.authorization_commit_outcome($1,$2,$3,$4,$5,$6,$7,$8)`,
			authorizationRecord.EventID, requestID, authorizationRecord.AggregateVersion, authorizationRecord.EventDigest,
			authorizationRecord.SerializationVersion, authorizationRecord.ApprovalControlEpoch, decisionID, stateEventID,
		).Scan(&committed)
		if verificationErr == nil && committed {
			verified, readErr := approvalRequestByID(verificationContext, r.approvalWriterPool, requestID)
			if readErr == nil {
				return verified, true, false, nil
			}
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return ApprovalRequest{}, false, false, contextErr
		}
		return ApprovalRequest{}, false, false, ErrApprovalWriterUnavailable
	}
	return item, committed, false, nil
}

func (r *PostgresRepository) commitApprovalWriterTerminal(
	ctx context.Context,
	tx pgx.Tx,
	expected ApprovalRequest,
	stateEventID uuid.UUID,
	reasonCode string,
) (ApprovalRequest, bool, bool, error) {
	committed, rolledBack, err := r.commitApprovalWriterTransaction(ctx, tx)
	if err == nil || rolledBack {
		return expected, committed, rolledBack, err
	}
	verificationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var found bool
	verificationErr := r.approvalWriterPool.QueryRow(verificationContext,
		`SELECT account_approval_authorization.approval_terminal_commit_outcome($1,$2,$3,$4,$5)`,
		expected.ID, expected.Version, expected.Status, stateEventID, reasonCode,
	).Scan(&found)
	if verificationErr == nil && found {
		verified, readErr := approvalRequestByID(verificationContext, r.approvalWriterPool, expected.ID)
		if readErr == nil {
			return verified, true, false, nil
		}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return ApprovalRequest{}, false, false, contextErr
	}
	return ApprovalRequest{}, false, false, ErrApprovalWriterUnavailable
}

func (r *PostgresRepository) invalidateApproval(ctx context.Context, tx pgx.Tx, item ApprovalRequest, actorID uuid.UUID, status, code string, details map[string]any, now time.Time, result error) (ApprovalRequest, error) {
	item, err := r.invalidateApprovalInTransaction(ctx, tx, item, actorID, status, code, details, now, uuid.Must(uuid.NewV7()))
	if err != nil {
		return ApprovalRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalRequest{}, err
	}
	return item, result
}

func (r *PostgresRepository) invalidateApprovalInTransaction(ctx context.Context, tx pgx.Tx, item ApprovalRequest, actorID uuid.UUID, status, code string, details map[string]any, now time.Time, stateEventID uuid.UUID) (ApprovalRequest, error) {
	payload, _ := json.Marshal(details)
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status=$2,invalidation_code=$3,invalidation_details=$4,version=version+1,updated_at=$5 WHERE id=$1`, item.ID, status, code, payload, now); err != nil {
		return ApprovalRequest{}, err
	}
	if err := appendApprovalStateEventWithID(ctx, tx, stateEventID, item.ID, item.Status, status, "USER", actorID.String(), code, details, now); err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, tx, item.ID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return item, nil
}

func (r *PostgresRepository) ListApprovalRequests(ctx context.Context, principal identity.Principal, filter ApprovalFilter) ([]ApprovalRequest, error) {
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return nil, err
	}
	authorizationVersion, err := platformAuthorizationVersionForActor(ctx, r.pool, actorID)
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
	canAdvance := r.approvalAdvancementProjection(ctx)
	writerReady := r.ApprovalWriterReady(ctx) == nil
	for rows.Next() {
		item, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		canRequest, canApprove, err := r.approvalCapabilities(ctx, actorID, item)
		if err != nil {
			return nil, err
		}
		// A permission can be revoked after the set-based visibility query but
		// before projection. Never append a non-owned row that fails the fresh
		// authorization check.
		if item.RequesterUserID != actorID && !canApprove {
			continue
		}
		policy, policyErr := approvalPolicyForItem(item)
		advanceAllowed := false
		if policyErr == nil && (canRequest || canApprove) {
			advanceAllowed = canAdvance(policy)
		}
		item.AllowedActions = approvalAllowedActions(item, actorID, canRequest, canApprove, advanceAllowed, writerReady)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := ensureApprovalAuthorizationVersion(ctx, r.pool, actorID, authorizationVersion); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *PostgresRepository) ApprovalRequest(ctx context.Context, principal identity.Principal, requestID uuid.UUID) (ApprovalRequest, error) {
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return ApprovalRequest{}, err
	}
	visibility, err := approvalRequestVisibilityForActor(ctx, r.pool, actorID, requestID)
	if err != nil {
		return ApprovalRequest{}, err
	}
	item, err := approvalRequestByID(ctx, r.pool, requestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ApprovalRequest{}, ErrApprovalRequestNotFound
		}
		return ApprovalRequest{}, err
	}
	if visibility.Owned {
		canRequest, _, err := r.approvalCapabilities(ctx, actorID, item)
		if err != nil {
			return ApprovalRequest{}, err
		}
		policy, policyErr := approvalPolicyForItem(item)
		advanceAllowed := false
		if canRequest && policyErr == nil {
			advanceAllowed = r.approvalAdvancementProjection(ctx)(policy)
		}
		item.AllowedActions = approvalAllowedActions(item, actorID, canRequest, false, advanceAllowed, false)
		if err := ensureApprovalAuthorizationVersion(ctx, r.pool, actorID, visibility.AuthorizationVersion); err != nil {
			return ApprovalRequest{}, err
		}
		return item, nil
	}
	_, canApprove, err := r.approvalCapabilities(ctx, actorID, item)
	if err != nil {
		return ApprovalRequest{}, err
	}
	if !canApprove {
		return ApprovalRequest{}, ErrApprovalRequestNotFound
	}
	policy, policyErr := approvalPolicyForItem(item)
	item.AllowedActions = approvalAllowedActions(item, actorID, false, true, policyErr == nil && r.approvalAdvancementProjection(ctx)(policy), r.ApprovalWriterReady(ctx) == nil)
	if err := ensureApprovalAuthorizationVersion(ctx, r.pool, actorID, visibility.AuthorizationVersion); err != nil {
		return ApprovalRequest{}, err
	}
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

func approvalAllowedActions(item ApprovalRequest, actorID uuid.UUID, canRequest, canApprove, canAdvance, writerReady bool) []string {
	actions := make([]string, 0, 3)
	if item.RequesterUserID == actorID && canRequest {
		if item.Status == "DRAFT" && canAdvance && approvalpolicy.Executable(item.CommandType) {
			actions = append(actions, "EDIT", "SUBMIT")
		}
		if item.Status == "SUBMITTED" {
			actions = append(actions, "WITHDRAW")
		}
	}
	if canApprove && item.RequesterUserID != actorID && item.Status == "SUBMITTED" {
		if canAdvance && writerReady && approvalpolicy.Executable(item.CommandType) {
			actions = append(actions, "APPROVE")
		}
		actions = append(actions, "REJECT")
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

func (r *PostgresRepository) tokenApprovalCreateEnabledForTest(commandType string) bool {
	return r.tokenApprovalCreateGate != nil && r.tokenApprovalCreateGate(strings.ToUpper(strings.TrimSpace(commandType)))
}

func approvalPolicyForItem(item ApprovalRequest) (approvalpolicy.Policy, error) {
	policy, err := approvalpolicy.Lookup(item.CommandType)
	if err != nil || item.PolicyVersion != policy.PolicyVersion || item.SnapshotSchemaVersion != policy.SnapshotSchemaVersion || policy.Mode != approvalpolicy.ApprovalRequired || item.RequesterPermission != policy.RequesterPermission || item.ApproverPermission != policy.ApproverPermission {
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
	criticalVersions, err := canonicalApprovalCriticalVersions(policy, jsonOrDefault(draft.CriticalResourceVersions, `{}`))
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	financialSnapshot, err := canonicalApprovalFinancialSnapshot(policy, jsonOrDefault(draft.FinancialSnapshot, `{}`))
	if err != nil {
		return approvalpolicy.Policy{}, nil, [32]byte{}, err
	}
	if approvalSnapshotCommand(policy.CommandType) && !tokenApprovalSnapshotsAligned(criticalVersions, financialSnapshot) {
		return approvalpolicy.Policy{}, nil, [32]byte{}, ErrConflict
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

// validateApprovalDraftRequest validates only caller-owned fields. Authoritative
// resource snapshots are deliberately excluded so an idempotent replay can be
// proven from the original request without another downstream read.
func validateApprovalDraftRequest(draft ApprovalDraft) (approvalpolicy.Policy, [32]byte, error) {
	policy, err := approvalpolicy.Lookup(strings.ToUpper(strings.TrimSpace(draft.CommandType)))
	if err != nil || policy.Mode != approvalpolicy.ApprovalRequired || !approvalpolicy.Executable(policy.CommandType) {
		return approvalpolicy.Policy{}, [32]byte{}, ErrConflict
	}
	if len(strings.TrimSpace(draft.IdempotencyKey)) < 8 || len(draft.IdempotencyKey) > 120 || len(strings.TrimSpace(draft.TargetResourceType)) < 1 || len(strings.TrimSpace(draft.TargetResourceType)) > 80 || len(strings.TrimSpace(draft.TargetResourceID)) < 1 || len(strings.TrimSpace(draft.TargetResourceID)) > 160 || len(strings.TrimSpace(draft.Reason)) < 10 || len(strings.TrimSpace(draft.Reason)) > 2000 {
		return approvalpolicy.Policy{}, [32]byte{}, ErrConflict
	}
	// Credential revocation never has a Billing snapshot. Requiring the
	// caller to send the exact empty object keeps null/default coercion from
	// weakening the closed approval contract before any downstream read or
	// persistence is attempted.
	if policy.CommandType == "TOKEN_CREDENTIAL_REVOKE" {
		financial, financialErr := canonicalSafeJSONObject(draft.FinancialSnapshot)
		if financialErr != nil || string(financial) != "{}" {
			return approvalpolicy.Policy{}, [32]byte{}, ErrInvalidApprovalInput
		}
	}
	fingerprint, err := approvalDraftOriginalFingerprint(policy.CommandType, draft)
	if err != nil {
		return approvalpolicy.Policy{}, [32]byte{}, err
	}
	return policy, fingerprint, nil
}

func approvalRequestOriginalFingerprint(item ApprovalRequest) ([32]byte, error) {
	return approvalDraftOriginalFingerprint(item.CommandType, ApprovalDraft{
		NormalizedCommand: item.NormalizedCommand, TargetResourceType: item.TargetResourceType,
		TargetResourceID: item.TargetResourceID, Reason: item.Reason, RedactedEvidence: item.RedactedEvidence,
		ExpectedImpact: item.ExpectedImpact, CriticalResourceVersions: item.CriticalResourceVersions,
		FinancialSnapshot: item.FinancialSnapshot, SupersedesRequestID: item.SupersedesRequestID,
	})
}

func approvalDraftOriginalFingerprint(commandType string, draft ApprovalDraft) ([32]byte, error) {
	normalized, err := canonicalSafeJSON(draft.NormalizedCommand)
	if err != nil {
		return [32]byte{}, err
	}
	evidence, err := canonicalSafeJSONValue(jsonOrDefault(draft.RedactedEvidence, `[]`), "object", "array")
	if err != nil {
		return [32]byte{}, err
	}
	impact, err := canonicalSafeJSONObject(jsonOrDefault(draft.ExpectedImpact, `{}`))
	if err != nil {
		return [32]byte{}, err
	}
	parts := [][]byte{[]byte(strings.ToUpper(strings.TrimSpace(commandType))), normalized, []byte(strings.ToUpper(strings.TrimSpace(draft.TargetResourceType))), []byte(strings.TrimSpace(draft.TargetResourceID)), []byte(strings.TrimSpace(draft.Reason)), evidence, impact}
	if draft.SupersedesRequestID != nil {
		parts = append(parts, []byte(draft.SupersedesRequestID.String()))
	} else {
		parts = append(parts, nil)
	}
	if !approvalSnapshotCommand(commandType) {
		critical, criticalErr := canonicalSafeJSONObject(jsonOrDefault(draft.CriticalResourceVersions, `{}`))
		financial, financialErr := canonicalSafeJSONObject(jsonOrDefault(draft.FinancialSnapshot, `{}`))
		if criticalErr != nil || financialErr != nil {
			return [32]byte{}, ErrConflict
		}
		parts = append(parts, critical, financial)
	}
	return sha256.Sum256(bytes.Join(parts, []byte{0})), nil
}

func tokenApprovalSnapshotsAligned(critical, financial []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(critical))
	decoder.UseNumber()
	var criticalFields map[string]any
	if decoder.Decode(&criticalFields) != nil {
		return false
	}
	decoder = json.NewDecoder(bytes.NewReader(financial))
	decoder.UseNumber()
	var financialFields map[string]any
	if decoder.Decode(&financialFields) != nil {
		return false
	}
	if _, credentialSnapshot := criticalFields["credential_id"]; credentialSnapshot {
		return len(financialFields) == 0
	}
	return fmt.Sprint(criticalFields["billing_price_version"]) == fmt.Sprint(financialFields["billing_price_revision"])
}

func canonicalApprovalFinancialSnapshot(policy approvalpolicy.Policy, raw json.RawMessage) ([]byte, error) {
	if !approvalSnapshotCommand(policy.CommandType) {
		return canonicalSafeJSONObject(raw)
	}
	if policy.CommandType == "TOKEN_CREDENTIAL_REVOKE" {
		canonical, err := canonicalSafeJSONObject(raw)
		if err != nil || string(canonical) != "{}" {
			return nil, ErrConflict
		}
		return canonical, nil
	}
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil || fields == nil || len(fields) != 6 {
		return nil, ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrConflict
	}
	revision, ok := fields["billing_price_revision"].(json.Number)
	if !ok {
		return nil, ErrConflict
	}
	parsedRevision, err := strconv.ParseInt(revision.String(), 10, 64)
	if err != nil || parsedRevision < 1 {
		return nil, ErrConflict
	}
	for _, name := range []string{"models_checked", "models_covered"} {
		number, ok := fields[name].(json.Number)
		if !ok {
			return nil, ErrConflict
		}
		value, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil || value < 0 || name == "models_checked" && value < 1 {
			return nil, ErrConflict
		}
	}
	checked, _ := strconv.ParseInt(fields["models_checked"].(json.Number).String(), 10, 64)
	covered, _ := strconv.ParseInt(fields["models_covered"].(json.Number).String(), 10, 64)
	fullyCovered, ok := fields["fully_covered"].(bool)
	if !ok || !fullyCovered || covered != checked {
		return nil, ErrConflict
	}
	fingerprint, ok := fields["price_set_fingerprint"].(string)
	if !ok || len(fingerprint) != 64 {
		return nil, ErrConflict
	}
	if decoded, err := hex.DecodeString(fingerprint); err != nil || len(decoded) != sha256.Size {
		return nil, ErrConflict
	}
	asOf, ok := fields["billing_as_of"].(string)
	if !ok {
		return nil, ErrConflict
	}
	if _, err := time.Parse(time.RFC3339Nano, asOf); err != nil {
		return nil, ErrConflict
	}
	if containsSecretFieldExceptCredentialMetadata(fields) {
		return nil, ErrConflict
	}
	return json.Marshal(fields)
}

func canonicalApprovalCriticalVersions(policy approvalpolicy.Policy, raw json.RawMessage) ([]byte, error) {
	if !approvalpolicy.TokenExecutableApproval(policy.CommandType) {
		return canonicalSafeJSONObject(raw)
	}
	if len(raw) == 0 || len(raw) > 256<<10 {
		return nil, ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil || fields == nil {
		return nil, ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrConflict
	}
	if len(fields) != len(policy.CriticalResourceFields) {
		return nil, ErrConflict
	}
	for _, name := range policy.CriticalResourceFields {
		value, ok := fields[name]
		if !ok || !validTokenApprovalCriticalValue(policy, name, value) {
			return nil, ErrConflict
		}
	}
	if containsSecretFieldExceptCredentialMetadata(fields) {
		return nil, ErrConflict
	}
	return json.Marshal(fields)
}

func validTokenApprovalCriticalValue(policy approvalpolicy.Policy, name string, value any) bool {
	if value == nil {
		switch name {
		case "existing_session_cutoff_at", "replacement_model_id", "replacement_credential_version":
			return true
		default:
			return false
		}
	}
	if strings.HasSuffix(name, "_version") || name == "catalog_version" || name == "source_catalog_version" || name == "current_catalog_version" {
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		parsed, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return false
		}
		if name == "policy_version" {
			return parsed == policy.PolicyVersion
		}
		if name == "lifecycle_plan_version" {
			// Zero is the N-1 immediate lifecycle approval sentinel. Positive
			// versions are immutable APT-134 lifecycle plan revisions.
			return parsed >= 0
		}
		return parsed >= 1
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" || len(text) > 512 {
		return false
	}
	if strings.HasSuffix(name, "_fingerprint") {
		decoded, err := hex.DecodeString(strings.TrimSpace(text))
		return err == nil && len(decoded) == sha256.Size
	}
	if strings.HasSuffix(name, "_at") {
		_, err := time.Parse(time.RFC3339Nano, text)
		return err == nil
	}
	if strings.HasSuffix(name, "_id") {
		parsed, err := uuid.Parse(text)
		return err == nil && parsed != uuid.Nil
	}
	if name == "credential_status" {
		return slices.Contains([]string{"PENDING", "ACTIVE", "RETIRED", "REVOKED"}, strings.ToUpper(text))
	}
	if name == "current_lifecycle" || name == "target_lifecycle" {
		return slices.Contains([]string{"DRAFT", "PUBLISHED", "DEPRECATED", "REMOVED"}, strings.ToUpper(text))
	}
	return true
}

func containsSecretFieldExceptCredentialMetadata(value any) bool {
	safeCredentialMetadata := map[string]struct{}{
		"token_account_id": {}, "token_account_version": {}, "credential_id": {}, "credential_version": {},
		"credential_status": {}, "credential_fingerprint": {}, "replacement_credential_version": {},
	}
	var walk func(any) bool
	walk = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := normalizeSensitiveFieldName(key)
				if _, safe := safeCredentialMetadata[normalized]; safe {
					if !validCredentialMetadataValue(normalized, child) {
						return true
					}
					continue
				} else if sensitiveGovernanceFieldName(normalized) {
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

func normalizeSensitiveFieldName(key string) string {
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

func validCredentialMetadataValue(name string, value any) bool {
	switch name {
	case "token_account_id", "credential_id":
		text, ok := value.(string)
		_, err := uuid.Parse(text)
		return ok && err == nil
	case "token_account_version", "credential_version", "replacement_credential_version", "active_credential_version", "credential_slot_version":
		if value == nil && name == "replacement_credential_version" {
			return true
		}
		switch number := value.(type) {
		case json.Number:
			parsed, err := strconv.ParseInt(number.String(), 10, 64)
			return err == nil && parsed >= 0
		case float64:
			return number >= 0 && number == float64(int64(number))
		default:
			return false
		}
	case "credential_status":
		text, ok := value.(string)
		return ok && slices.Contains([]string{"PENDING", "ACTIVE", "RETIRED", "REVOKED"}, strings.ToUpper(strings.TrimSpace(text)))
	case "credential_fingerprint":
		text, ok := value.(string)
		decoded, err := hex.DecodeString(strings.TrimSpace(text))
		return ok && err == nil && len(decoded) == sha256.Size
	default:
		return false
	}
}

func sensitiveGovernanceFieldName(normalized string) bool {
	compact := strings.ReplaceAll(normalized, "_", "")
	return strings.Contains(compact, "password") || strings.Contains(compact, "secret") || strings.Contains(compact, "cipher") || strings.Contains(compact, "privatekey") || strings.Contains(compact, "credential") || strings.Contains(compact, "endpoint") || strings.Contains(compact, "provideraddress") || strings.Contains(compact, "providerurl") || strings.Contains(compact, "baseurl") || compact == "authorization" || compact == "apikey" || compact == "keyid" || compact == "cookie" || compact == "token" || strings.HasSuffix(compact, "token")
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
	if containsSecretFieldExceptCredentialMetadata(value) {
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
			normalized := normalizeSensitiveFieldName(key)
			if sensitiveGovernanceFieldName(normalized) {
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
	err := database.QueryRow(ctx, `SELECT ra.id,ra.expires_at FROM users u JOIN role_assignments ra ON ra.user_id=u.id AND ra.organization_id IS NULL JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$2 WHERE u.id=$1 AND u.status='ACTIVE' AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now()) ORDER BY ra.expires_at NULLS LAST,ra.id LIMIT 1 FOR SHARE OF u,ra`, userID, permission).Scan(&bindingID, &validUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, nil, ErrForbidden
	}
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	var version int64
	if err := database.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid FOR SHARE`, userID).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
		version = 1
	} else if err != nil {
		return uuid.Nil, 0, nil, err
	}
	return bindingID, version, validUntil, nil
}

// strongApprovalWriterAuthorization preserves the existing authorization-row
// locks without granting the dedicated approval writer mutable access to user,
// role-assignment, or authorization-version facts. The fixed SECURITY DEFINER
// function verifies session_user and exposes only this closed result tuple.
func strongApprovalWriterAuthorization(ctx context.Context, database queryable, userID uuid.UUID, permission string) (uuid.UUID, int64, *time.Time, error) {
	var bindingID uuid.UUID
	var authorizationVersion int64
	var validUntil *time.Time
	err := database.QueryRow(ctx, `SELECT * FROM public.lock_approval_writer_authorization($1,$2)`, userID, permission).Scan(
		&bindingID, &authorizationVersion, &validUntil,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, nil, ErrForbidden
	}
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	return bindingID, authorizationVersion, validUntil, nil
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

const approvalSelect = `SELECT ar.id,ar.command_type,ar.policy_version,p.approval_mode,p.risk_class,ar.requester_user_id,ar.requester_binding_id,ar.requester_authorization_version,ar.normalized_command,encode(ar.command_fingerprint,'hex'),ar.target_resource_type,ar.target_resource_id,ar.reason,ar.redacted_evidence,ar.expected_impact,ar.status,ar.version,ar.supersedes_request_id,ar.expires_at,ar.execution_deadline,ar.invalidation_code,ar.invalidation_details,ar.submitted_at,ar.decided_at,ar.created_at,ar.updated_at,s.critical_resource_versions,s.financial_snapshot,s.policy_snapshot,s.snapshot_schema_version,p.requester_permission,COALESCE(p.approver_permission,'') FROM approval_requests ar JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version JOIN approval_request_snapshots s ON s.approval_request_id=ar.id`

type rowScanner interface{ Scan(...any) error }

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanApprovalRequest(row rowScanner) (ApprovalRequest, error) {
	var item ApprovalRequest
	err := row.Scan(&item.ID, &item.CommandType, &item.PolicyVersion, &item.ApprovalMode, &item.RiskClass, &item.RequesterUserID, &item.RequesterBindingID, &item.RequesterAuthorizationVersion, &item.NormalizedCommand, &item.CommandFingerprint, &item.TargetResourceType, &item.TargetResourceID, &item.Reason, &item.RedactedEvidence, &item.ExpectedImpact, &item.Status, &item.Version, &item.SupersedesRequestID, &item.ExpiresAt, &item.ExecutionDeadline, &item.InvalidationCode, &item.InvalidationDetails, &item.SubmittedAt, &item.DecidedAt, &item.CreatedAt, &item.UpdatedAt, &item.CriticalResourceVersions, &item.FinancialSnapshot, &item.PolicySnapshot, &item.SnapshotSchemaVersion, &item.RequesterPermission, &item.ApproverPermission)
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
	return appendApprovalStateEventWithID(ctx, tx, uuid.Must(uuid.NewV7()), requestID, from, to, actorType, actorID, reason, details, now)
}

func appendApprovalStateEventWithID(ctx context.Context, tx pgx.Tx, eventID, requestID uuid.UUID, from, to, actorType, actorID, reason string, details map[string]any, now time.Time) error {
	payload, _ := json.Marshal(details)
	var fromValue *string
	if from != "" {
		fromValue = &from
	}
	_, err := tx.Exec(ctx, `INSERT INTO approval_state_events(id,approval_request_id,from_status,to_status,actor_type,actor_id,reason_code,details,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, eventID, requestID, fromValue, to, actorType, actorID, reason, payload, now)
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
