package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	lifecyclePlanMinimumWindow = 5 * time.Minute
	lifecyclePlanApprovalTTL   = 24 * time.Hour
	lifecyclePlanLeaseTTL      = 5 * time.Minute
)

var (
	errLifecyclePlanWindowInvalid = errors.New("lifecycle plan effective time is outside the allowed window")
	errModelServingUnavailable    = errors.New("model has no authoritative serving coverage")
)

type LifecyclePlan struct {
	ID                       uuid.UUID  `json:"lifecycle_plan_id"`
	ModelID                  uuid.UUID  `json:"model_id"`
	PlanVersion              int64      `json:"lifecycle_plan_version"`
	ExpectedCatalogVersion   int64      `json:"expected_catalog_version"`
	CurrentLifecycle         string     `json:"current_lifecycle"`
	TargetLifecycle          string     `json:"target_lifecycle"`
	EffectiveAt              time.Time  `json:"effective_at"`
	ExistingSessionCutoffAt  *time.Time `json:"existing_session_cutoff_at"`
	ReplacementModelID       *uuid.UUID `json:"replacement_model_id"`
	ForceCancelInflight      bool       `json:"force_cancel_inflight"`
	Status                   string     `json:"status"`
	Reason                   string     `json:"reason"`
	ApprovalRequestID        *uuid.UUID `json:"approval_request_id,omitempty"`
	GovernedCommandReference *uuid.UUID `json:"governed_command_reference,omitempty"`
	ExecutionLeaseUntil      *time.Time `json:"execution_lease_until,omitempty"`
	ExecutionAttempt         int        `json:"execution_attempt"`
	ResultCatalogVersion     *int64     `json:"result_catalog_version,omitempty"`
	StableErrorCode          *string    `json:"stable_error_code,omitempty"`
	CreatedBy                uuid.UUID  `json:"created_by"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	CompletedAt              *time.Time `json:"completed_at,omitempty"`
	// Frozen approval facts remain Token-domain persistence and are only
	// consumed by the governed executor. They are deliberately excluded from
	// the admin projection; the impact endpoint is the sole public view.
	CriticalResourceVersions json.RawMessage `json:"-"`
	FinancialSnapshot        json.RawMessage `json:"-"`
}

type LifecyclePlanInput struct {
	ExpectedCatalogVersion  int64      `json:"expected_catalog_version"`
	ExpectedPlanVersion     int64      `json:"expected_plan_version"`
	TargetLifecycle         string     `json:"target_lifecycle"`
	EffectiveAt             time.Time  `json:"effective_at"`
	ExistingSessionCutoffAt *time.Time `json:"existing_session_cutoff_at"`
	ReplacementModelID      *uuid.UUID `json:"replacement_model_id"`
	ForceCancelInflight     bool       `json:"force_cancel_inflight"`
	Reason                  string     `json:"reason"`
}

type lifecyclePlanCommand struct {
	ModelID                 uuid.UUID  `json:"model_id"`
	LifecyclePlanID         uuid.UUID  `json:"lifecycle_plan_id"`
	LifecyclePlanVersion    int64      `json:"lifecycle_plan_version"`
	TargetLifecycle         string     `json:"target_lifecycle"`
	EffectiveAt             time.Time  `json:"effective_at"`
	ExistingSessionCutoffAt *time.Time `json:"existing_session_cutoff_at"`
	ReplacementModelID      *uuid.UUID `json:"replacement_model_id"`
}

type lifecyclePlanCritical struct {
	ModelID                       uuid.UUID  `json:"model_id"`
	CatalogVersion                int64      `json:"catalog_version"`
	CurrentLifecycle              string     `json:"current_lifecycle"`
	TargetLifecycle               string     `json:"target_lifecycle"`
	LifecyclePlanVersion          int64      `json:"lifecycle_plan_version"`
	EffectiveAt                   time.Time  `json:"effective_at"`
	ExistingSessionCutoffAt       *time.Time `json:"existing_session_cutoff_at"`
	ReplacementModelID            *uuid.UUID `json:"replacement_model_id"`
	BillingPriceVersion           int64      `json:"billing_price_version"`
	RouteSetFingerprint           string     `json:"route_set_fingerprint"`
	ProvisioningSetFingerprint    string     `json:"provisioning_set_fingerprint"`
	InflightRequestSetFingerprint string     `json:"inflight_request_set_fingerprint"`
	PolicyVersion                 int64      `json:"policy_version"`
}

type scheduledModelPublishExpected struct {
	ModelID                    uuid.UUID `json:"model_id"`
	ExpectedCatalogVersion     int64     `json:"expected_catalog_version"`
	ExpectedDraftVersion       int64     `json:"expected_draft_version"`
	DraftFingerprint           string    `json:"draft_fingerprint"`
	ProtocolDefinitionID       uuid.UUID `json:"protocol_definition_id"`
	ProtocolVersion            int64     `json:"protocol_version"`
	StableRouteCount           int64     `json:"stable_route_count"`
	StableRouteSetFingerprint  string    `json:"stable_route_set_fingerprint"`
	ProvisioningCount          int64     `json:"provisioning_count"`
	ReadyRouteCount            int64     `json:"ready_route_count"`
	ProvisioningSetFingerprint string    `json:"provisioning_set_fingerprint"`
	FinancialFactRevision      int64     `json:"financial_fact_revision"`
	PriceSetFingerprint        string    `json:"price_set_fingerprint"`
	PrecheckFingerprint        string    `json:"precheck_fingerprint"`
	RequestFingerprint         string    `json:"request_fingerprint"`
}

func parseModelPublishExpected(raw json.RawMessage, modelID uuid.UUID) (scheduledModelPublishExpected, error) {
	var expected scheduledModelPublishExpected
	if decodeCommand(raw, &expected) != nil || expected.ModelID != modelID || expected.ExpectedCatalogVersion < 1 || expected.ExpectedDraftVersion < 1 || expected.ProtocolDefinitionID == uuid.Nil || expected.ProtocolVersion < 1 || expected.StableRouteCount < 1 || expected.ProvisioningCount < 1 || expected.ReadyRouteCount < expected.StableRouteCount || expected.FinancialFactRevision < 1 {
		return scheduledModelPublishExpected{}, ErrTokenCommandInvalid
	}
	for _, value := range []string{expected.DraftFingerprint, expected.StableRouteSetFingerprint, expected.ProvisioningSetFingerprint, expected.PriceSetFingerprint, expected.PrecheckFingerprint, expected.RequestFingerprint} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size || value != strings.ToLower(value) {
			return scheduledModelPublishExpected{}, ErrTokenCommandInvalid
		}
	}
	return expected, nil
}

func lifecyclePlanDigest(modelID uuid.UUID, input LifecyclePlanInput) string {
	raw, _ := json.Marshal(struct {
		ModelID uuid.UUID `json:"model_id"`
		LifecyclePlanInput
	}{modelID, input})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func lifecyclePlanKeyDigest(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) < 16 || len(key) > 128 {
		return "", ErrTokenCommandInvalid
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func normalizeLifecyclePlanInput(modelID uuid.UUID, input LifecyclePlanInput) (LifecyclePlanInput, error) {
	input.TargetLifecycle = strings.ToUpper(strings.TrimSpace(input.TargetLifecycle))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EffectiveAt = input.EffectiveAt.UTC()
	if input.ExistingSessionCutoffAt != nil {
		value := input.ExistingSessionCutoffAt.UTC()
		input.ExistingSessionCutoffAt = &value
	}
	if modelID == uuid.Nil || input.ExpectedCatalogVersion < 1 || input.ExpectedPlanVersion < 0 || len([]rune(input.Reason)) < 10 || len([]rune(input.Reason)) > 2000 || input.EffectiveAt.IsZero() || input.ForceCancelInflight || (input.TargetLifecycle != "DEPRECATED" && input.TargetLifecycle != "REMOVED") || input.ReplacementModelID != nil && (*input.ReplacementModelID == uuid.Nil || *input.ReplacementModelID == modelID) {
		return LifecyclePlanInput{}, ErrTokenCommandInvalid
	}
	if input.TargetLifecycle != "REMOVED" && input.ReplacementModelID != nil {
		return LifecyclePlanInput{}, ErrTokenCommandInvalid
	}
	if input.TargetLifecycle == "REMOVED" && input.ReplacementModelID == nil {
		return LifecyclePlanInput{}, ErrModelUnavailable
	}
	if input.TargetLifecycle == "REMOVED" && (input.ExistingSessionCutoffAt == nil || !input.ExistingSessionCutoffAt.Equal(input.EffectiveAt)) {
		return LifecyclePlanInput{}, ErrTokenCommandInvalid
	}
	if input.TargetLifecycle != "REMOVED" && input.ExistingSessionCutoffAt != nil && input.ExistingSessionCutoffAt.Before(input.EffectiveAt) {
		return LifecyclePlanInput{}, ErrTokenCommandInvalid
	}
	return input, nil
}

func scanLifecyclePlan(row pgx.Row) (LifecyclePlan, error) {
	var item LifecyclePlan
	var leaseOwner *string
	err := row.Scan(&item.ID, &item.ModelID, &item.PlanVersion, &item.ExpectedCatalogVersion, &item.CurrentLifecycle, &item.TargetLifecycle,
		&item.EffectiveAt, &item.ExistingSessionCutoffAt, &item.ReplacementModelID, &item.ForceCancelInflight, &item.Status, &item.Reason,
		&item.ApprovalRequestID, &item.GovernedCommandReference, &leaseOwner, &item.ExecutionLeaseUntil, &item.ExecutionAttempt, &item.ResultCatalogVersion, &item.StableErrorCode,
		&item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &item.CompletedAt, &item.CriticalResourceVersions, &item.FinancialSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return LifecyclePlan{}, ErrNotFound
	}
	return item, err
}

const lifecyclePlanSelect = `SELECT id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id,force_cancel_inflight,status,reason,approval_request_id,governed_command_reference,execution_lease_owner,execution_lease_until,execution_attempt,result_catalog_version,stable_error_code,created_by,created_at,updated_at,completed_at,critical_resource_versions,financial_snapshot FROM model_lifecycle_plans`

func (r *PostgresRepository) CreateLifecyclePlan(ctx context.Context, modelID uuid.UUID, rawInput LifecyclePlanInput, actor uuid.UUID, idempotencyKey string) (LifecyclePlan, bool, error) {
	input, err := normalizeLifecyclePlanInput(modelID, rawInput)
	if err != nil || actor == uuid.Nil {
		return LifecyclePlan{}, false, ErrTokenCommandInvalid
	}
	keyDigest, err := lifecyclePlanKeyDigest(idempotencyKey)
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	digest := lifecyclePlanDigest(modelID, input)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	defer tx.Rollback(ctx)
	// The per-model advisory lock serializes first-plan creation as well as
	// reschedules without depending on an already existing mutable row.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, modelID.String()); err != nil {
		return LifecyclePlan{}, false, err
	}
	existing, loadErr := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE created_by=$1 AND idempotency_key_digest=$2`, actor, keyDigest))
	if loadErr == nil {
		if existing.ModelID != modelID || existing.PlanVersion < 1 {
			return LifecyclePlan{}, false, ErrIdempotencyConflict
		}
		var storedDigest string
		if err = tx.QueryRow(ctx, `SELECT request_digest FROM model_lifecycle_plans WHERE id=$1`, existing.ID).Scan(&storedDigest); err != nil || storedDigest != digest {
			return LifecyclePlan{}, false, ErrIdempotencyConflict
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(loadErr, ErrNotFound) {
		return LifecyclePlan{}, false, loadErr
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return LifecyclePlan{}, false, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return LifecyclePlan{}, false, err
	}
	now = now.UTC()
	if input.EffectiveAt.Before(now.Add(lifecyclePlanMinimumWindow)) {
		return LifecyclePlan{}, false, errLifecyclePlanWindowInvalid
	}
	var catalogVersion int64
	var current string
	if err = tx.QueryRow(ctx, `SELECT entry.catalog_version,entry.lifecycle_status FROM model_catalog_entries entry WHERE entry.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND entry.model_id=$1`, modelID).Scan(&catalogVersion, &current); errors.Is(err, pgx.ErrNoRows) {
		return LifecyclePlan{}, false, ErrNotFound
	} else if err != nil {
		return LifecyclePlan{}, false, err
	}
	validTransition := current == "PUBLISHED" && (input.TargetLifecycle == "DEPRECATED" || input.TargetLifecycle == "REMOVED") || current == "DEPRECATED" && input.TargetLifecycle == "REMOVED"
	if catalogVersion != input.ExpectedCatalogVersion || !validTransition {
		return LifecyclePlan{}, false, ErrCatalogVersionConflict
	}
	if input.ReplacementModelID != nil {
		ready, readyErr := replacementModelReadyInTx(ctx, tx, catalogVersion, *input.ReplacementModelID)
		if readyErr != nil || !ready {
			if readyErr != nil {
				return LifecyclePlan{}, false, readyErr
			}
			return LifecyclePlan{}, false, ErrModelUnavailable
		}
	}
	var previousID uuid.UUID
	var previousVersion int64
	previousErr := tx.QueryRow(ctx, `SELECT id,plan_version FROM model_lifecycle_plans WHERE model_id=$1 AND status IN ('PENDING_APPROVAL','SCHEDULED','EXECUTING','UNKNOWN') FOR UPDATE`, modelID).Scan(&previousID, &previousVersion)
	if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
		return LifecyclePlan{}, false, previousErr
	}
	if (input.ExpectedPlanVersion == 0 && previousErr == nil) || (input.ExpectedPlanVersion > 0 && (previousErr != nil || previousVersion != input.ExpectedPlanVersion)) {
		return LifecyclePlan{}, false, ErrCatalogVersionConflict
	}
	if previousErr == nil {
		var previousStatus string
		if err = tx.QueryRow(ctx, `SELECT status FROM model_lifecycle_plans WHERE id=$1`, previousID).Scan(&previousStatus); err != nil || previousStatus == "EXECUTING" || previousStatus == "UNKNOWN" {
			return LifecyclePlan{}, false, ErrCatalogVersionConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='CANCELLED',cancelled_at=$2,completed_at=$2,updated_at=$2,stable_error_code='PLAN_SUPERSEDED' WHERE id=$1`, previousID, now); err != nil {
			return LifecyclePlan{}, false, err
		}
		if err = appendLifecyclePlanEvent(ctx, tx, previousID, previousVersion, "CANCELLED", "USER", actor.String(), input.Reason, "PLAN_SUPERSEDED", map[string]any{"replacement_plan_version": previousVersion + 1}, now); err != nil {
			return LifecyclePlan{}, false, err
		}
	}
	var nextVersion int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(plan_version),0)+1 FROM model_lifecycle_plans WHERE model_id=$1`, modelID).Scan(&nextVersion); err != nil {
		return LifecyclePlan{}, false, err
	}
	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING_APPROVAL',$11,$12,$13,$14,$15,$15)`, id, modelID, nextVersion, catalogVersion, current, input.TargetLifecycle, input.EffectiveAt, input.ExistingSessionCutoffAt, input.ReplacementModelID, input.ForceCancelInflight, input.Reason, digest, keyDigest, actor, now)
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	eventType := "CREATED"
	if previousErr == nil {
		eventType = "RESCHEDULED"
	}
	if err = appendLifecyclePlanEvent(ctx, tx, id, nextVersion, eventType, "USER", actor.String(), input.Reason, "", map[string]any{"expected_catalog_version": catalogVersion, "effective_at": input.EffectiveAt}, now); err != nil {
		return LifecyclePlan{}, false, err
	}
	item, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1`, id))
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	return item, true, r.commitGovernedMutation(ctx, tx)
}

// ScheduleModelPublish atomically records the governed audit-only command and
// its immutable due plan. It rechecks every Token-owned publication fact under
// the same catalog/model locks; the Billing revision/fingerprint remains the
// separately authoritative projection already frozen in the precheck.
func (r *PostgresRepository) ScheduleModelPublish(ctx context.Context, begin tokenCommandBegin) (TokenCommandResult, bool, error) {
	canonical, requestDigest, keyDigest, fingerprint, err := normalizeTokenCommand(begin.Envelope, begin.IdempotencyKey, "")
	if err != nil || begin.ActorUserID == uuid.Nil || begin.AuthorizationVersion < 1 || strings.TrimSpace(begin.ActorPermission) == "" || strings.TrimSpace(begin.AuditRequestID) == "" {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	command, err := parseModelPublishCommand(canonical.Command)
	if err != nil || command.PublishMode != "SCHEDULED" || command.LifecyclePlanID == nil || command.LifecyclePlanVersion == nil || command.EffectiveAt == nil || command.ModelID.String() != canonical.TargetResourceID {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	expected, err := parseModelPublishExpected(canonical.ExpectedVersions, command.ModelID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if expected.PrecheckFingerprint != canonical.PrecheckFingerprint {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	policy, err := approvalpolicy.Lookup("TOKEN_MODEL_PUBLISH")
	if err != nil || policy.Mode != approvalpolicy.AuditOnly {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if err = lockCatalogPublication(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, command.ModelID.String()); err != nil {
		return TokenCommandResult{}, false, err
	}
	existing, loadErr := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type='TOKEN_MODEL_PUBLISH' AND idempotency_key_digest=$2`, begin.ActorUserID, keyDigest))
	if loadErr == nil {
		if existing.IdempotencyFingerprint != fingerprint {
			return TokenCommandResult{}, false, ErrTokenCommandResultMismatch
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(loadErr, pgx.ErrNoRows) {
		return TokenCommandResult{}, false, loadErr
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return TokenCommandResult{}, false, err
	}
	now = now.UTC()
	if command.EffectiveAt.Before(now.Add(lifecyclePlanMinimumWindow)) {
		return TokenCommandResult{}, false, errLifecyclePlanWindowInvalid
	}
	// DRAFT is the independent model_catalog_drafts state, never a catalog
	// entry lifecycle. Publication still requires the discovered catalog base;
	// the exact draft and its base version are revalidated immediately below.
	var catalogModelExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2)`, expected.ExpectedCatalogVersion, command.ModelID).Scan(&catalogModelExists); err != nil || !catalogModelExists {
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	local, err := loadCommandPrecheckSnapshot(ctx, tx, "TOKEN_MODEL_PUBLISH", command.ModelID.String(), canonical.Command)
	if err != nil || len(local.Blockers) > 0 {
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	var frozen map[string]any
	if json.Unmarshal(canonical.ExpectedVersions, &frozen) != nil {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	for key, value := range local.ExpectedVersions {
		candidate, ok := frozen[key]
		left, _ := json.Marshal(value)
		right, _ := json.Marshal(candidate)
		if !ok || !jsonSemanticEqual(left, right) {
			return TokenCommandResult{}, false, ErrCatalogVersionConflict
		}
	}
	var nextVersion int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(plan_version),0)+1 FROM model_lifecycle_plans WHERE model_id=$1`, command.ModelID).Scan(&nextVersion); err != nil || nextVersion != *command.LifecyclePlanVersion {
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_lifecycle_plans WHERE model_id=$1 AND status IN ('PENDING_APPROVAL','SCHEDULED','EXECUTING','UNKNOWN'))`, command.ModelID).Scan(&active); err != nil || active {
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	commandReference := uuid.Must(uuid.NewV7())
	inserted, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,'TOKEN_MODEL_PUBLISH',$2,$3,$4,$5,$6,$7,$8,'MODEL',$9,$10,$11,$12,'PENDING',$11,$13) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, commandReference, policy.PolicyVersion, begin.ActorUserID, begin.ActorPermission, begin.AuthorizationVersion, keyDigest, fingerprint, requestDigest, command.ModelID.String(), canonical.Reason, canonical.ExpectedVersions, canonical.PrecheckFingerprint, begin.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if inserted.RowsAffected() != 1 {
		item, replayErr := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type='TOKEN_MODEL_PUBLISH' AND idempotency_key_digest=$2`, begin.ActorUserID, keyDigest))
		if replayErr != nil {
			return TokenCommandResult{}, false, replayErr
		}
		if item.IdempotencyFingerprint != fingerprint {
			return TokenCommandResult{}, false, ErrTokenCommandResultMismatch
		}
		return item, false, r.commitGovernedMutation(ctx, tx)
	}
	financial := json.RawMessage(`{}`)
	_, err = tx.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,governed_command_reference,critical_resource_versions,financial_snapshot,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,'DRAFT','PUBLISHED',$5,NULL,NULL,false,'SCHEDULED',$6,$7,$8,$9,$10,$11,$12,$13,$13)`, *command.LifecyclePlanID, command.ModelID, *command.LifecyclePlanVersion, expected.ExpectedCatalogVersion, *command.EffectiveAt, canonical.Reason, requestDigest, keyDigest, commandReference, canonical.ExpectedVersions, financial, begin.ActorUserID, now)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err = appendLifecyclePlanEvent(ctx, tx, *command.LifecyclePlanID, *command.LifecyclePlanVersion, "CREATED", "USER", begin.ActorUserID.String(), canonical.Reason, "", map[string]any{"governed_command_reference": commandReference, "effective_at": command.EffectiveAt}, now); err != nil {
		return TokenCommandResult{}, false, err
	}
	resultJSON, _ := json.Marshal(map[string]any{"command_reference": commandReference, "command_type": "TOKEN_MODEL_PUBLISH", "lifecycle_plan_id": command.LifecyclePlanID, "lifecycle_plan_version": command.LifecyclePlanVersion, "status": "SCHEDULED", "effective_at": command.EffectiveAt})
	item, err := completeTokenCommandTx(ctx, tx, commandReference, "SUCCEEDED", resultJSON, resultJSON, "")
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return item, true, r.commitGovernedMutation(ctx, tx)
}

func appendLifecyclePlanEvent(ctx context.Context, tx pgx.Tx, planID uuid.UUID, version int64, eventType, actorType, actorID, reason, stableCode string, evidence any, occurredAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_lifecycle_plan_events(id,plan_id,plan_version,event_type,actor_type,actor_id,reason,stable_error_code,evidence,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10)`, uuid.Must(uuid.NewV7()), planID, version, eventType, actorType, actorID, reason, stableCode, evidence, occurredAt)
	return err
}

func (r *PostgresRepository) CancelLifecyclePlan(ctx context.Context, modelID, planID uuid.UUID, expectedVersion int64, actor uuid.UUID, reason, idempotencyKey string) (LifecyclePlan, error) {
	reason = strings.TrimSpace(reason)
	keyDigest, err := lifecyclePlanKeyDigest(idempotencyKey)
	if modelID == uuid.Nil || planID == uuid.Nil || actor == uuid.Nil || expectedVersion < 1 || len([]rune(reason)) < 10 || len([]rune(reason)) > 2000 || err != nil {
		return LifecyclePlan{}, ErrTokenCommandInvalid
	}
	digestBytes := sha256.Sum256([]byte(planID.String() + "\x00" + strconv.FormatInt(expectedVersion, 10) + "\x00" + reason))
	digest := hex.EncodeToString(digestBytes[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LifecyclePlan{}, err
	}
	defer tx.Rollback(ctx)
	item, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1 AND model_id=$2 FOR UPDATE`, planID, modelID))
	if err != nil {
		return LifecyclePlan{}, err
	}
	var storedKey, storedDigest *string
	var cancelActor *uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT cancel_idempotency_key_digest,cancel_request_digest,cancel_actor_id FROM model_lifecycle_plans WHERE id=$1`, planID).Scan(&storedKey, &storedDigest, &cancelActor); err != nil {
		return LifecyclePlan{}, err
	}
	if storedKey != nil {
		if *storedKey != keyDigest || storedDigest == nil || *storedDigest != digest || cancelActor == nil || *cancelActor != actor {
			return LifecyclePlan{}, ErrIdempotencyConflict
		}
		return item, tx.Commit(ctx)
	}
	if item.PlanVersion != expectedVersion || (item.Status != "PENDING_APPROVAL" && item.Status != "SCHEDULED") {
		return LifecyclePlan{}, ErrCatalogVersionConflict
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return LifecyclePlan{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return LifecyclePlan{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='CANCELLED',cancelled_at=$2,completed_at=$2,updated_at=$2,cancel_idempotency_key_digest=$3,cancel_request_digest=$4,cancel_actor_id=$5,stable_error_code='PLAN_CANCELLED' WHERE id=$1`, planID, now, keyDigest, digest, actor); err != nil {
		return LifecyclePlan{}, err
	}
	if err = appendLifecyclePlanEvent(ctx, tx, planID, item.PlanVersion, "CANCELLED", "USER", actor.String(), reason, "PLAN_CANCELLED", map[string]any{"previous_status": item.Status}, now); err != nil {
		return LifecyclePlan{}, err
	}
	item, err = scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1`, planID))
	if err != nil {
		return LifecyclePlan{}, err
	}
	return item, r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) LifecyclePlan(ctx context.Context, modelID, planID uuid.UUID) (LifecyclePlan, error) {
	return scanLifecyclePlan(r.pool.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1 AND model_id=$2`, planID, modelID))
}

func (r *PostgresRepository) adminLifecyclePlansByIDs(ctx context.Context, modelID uuid.UUID, resourceIDs []string) ([]LifecyclePlan, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []LifecyclePlan{}, err
	}
	rows, err := r.pool.Query(ctx, lifecyclePlanSelect+` WHERE model_id=$1 AND id=ANY($2::uuid[])`, modelID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]LifecyclePlan, len(ids))
	for rows.Next() {
		item, scanErr := scanLifecyclePlan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func lifecyclePlanSnapshotSource(modelID uuid.UUID, filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	status, err := filter.enum("status", "PENDING_APPROVAL", "SCHEDULED", "EXECUTING", "APPLIED", "CANCELLED", "EXPIRED", "INVALIDATED", "FAILED", "UNKNOWN")
	if err != nil {
		return nil, err
	}
	target, err := filter.enum("target_lifecycle", "PUBLISHED", "DEPRECATED", "REMOVED")
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, _ time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT plan.id::text AS resource_id,row_number() OVER (ORDER BY plan.plan_version DESC,plan.id DESC)-1 AS position FROM model_lifecycle_plans plan WHERE plan.model_id=$3 AND ($4='' OR plan.status=$4) AND ($5='' OR plan.target_lifecycle=$5)`, modelID, status, target)
	}, nil
}

func (s *Service) adminLifecyclePlans(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), []string{"status", "target_lifecycle"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := lifecyclePlanSnapshotSource(modelID, filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repository, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "lifecycle plans are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "model-lifecycle-plans", "model:"+modelID.String(), "plan_version DESC,id DESC", 25, 100, source, func(ctx context.Context, ids []string) (any, error) {
		return repository.adminLifecyclePlansByIDs(ctx, modelID, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) adminLifecyclePlan(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	planID, ok := adminUUID(w, r, "lifecycle_plan_id")
	if !ok {
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, 503, "LIFECYCLE_PLAN_UNAVAILABLE", "lifecycle plans are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.LifecyclePlan(r.Context(), modelID, planID)
	if err != nil {
		writeLifecyclePlanError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminCreateLifecyclePlan(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, 503, "LIFECYCLE_PLAN_UNAVAILABLE", "lifecycle plans are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var input LifecyclePlanInput
	if api.DecodeJSON(w, r, &input) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_LIFECYCLE_PLAN", "the lifecycle plan input is invalid", httpserver.RequestID(r.Context()))
		return
	}
	item, created, err := repository.CreateLifecyclePlan(r.Context(), modelID, input, tokenAdminActor(r), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeLifecyclePlanError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, status, item)
}

func (s *Service) adminCancelLifecyclePlan(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	planID, ok := adminUUID(w, r, "lifecycle_plan_id")
	if !ok {
		return
	}
	var input struct {
		ExpectedPlanVersion int64  `json:"expected_plan_version"`
		Reason              string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &input) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_LIFECYCLE_PLAN", "the lifecycle plan input is invalid", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, 503, "LIFECYCLE_PLAN_UNAVAILABLE", "lifecycle plans are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	// The immutable plan target selects the exact permission. The outer
	// RequireAny check reveals no plan existence to an actor holding neither
	// capability; this second check prevents either capability from cancelling
	// the other command family without another Account call or a TOCTOU-prone
	// client hint.
	plan, err := repository.LifecyclePlan(r.Context(), modelID, planID)
	if err != nil {
		// A caller holding only one command-family permission must not be able
		// to distinguish a plan owned by the other family from a random ID.
		// Only a caller authorized for both immutable target families receives
		// the ordinary not-found projection.
		if errors.Is(err, ErrNotFound) && !(authorization.GrantedPermission(r.Context(), "platform.models.manage") && authorization.GrantedPermission(r.Context(), "platform.models.lifecycle.request")) {
			api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
			return
		}
		writeLifecyclePlanError(w, r, err)
		return
	}
	requiredPermission := "platform.models.lifecycle.request"
	if plan.TargetLifecycle == "PUBLISHED" {
		requiredPermission = "platform.models.manage"
	}
	if !authorization.GrantedPermission(r.Context(), requiredPermission) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.CancelLifecyclePlan(r.Context(), modelID, planID, input.ExpectedPlanVersion, tokenAdminActor(r), input.Reason, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeLifecyclePlanError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func writeLifecyclePlanError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusServiceUnavailable, "LIFECYCLE_PLAN_UNAVAILABLE", "the lifecycle plan service is unavailable"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "LIFECYCLE_PLAN_NOT_FOUND", "the lifecycle plan or model was not found"
	case errors.Is(err, ErrTokenCommandInvalid):
		status, code, message = http.StatusBadRequest, "INVALID_LIFECYCLE_PLAN", "the lifecycle plan input is invalid"
	case errors.Is(err, ErrCatalogVersionConflict):
		status, code, message = http.StatusConflict, "LIFECYCLE_PLAN_VERSION_CONFLICT", "the model catalog or lifecycle plan version changed"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "IDEMPOTENCY_CONFLICT", "the idempotency key was already used with different input"
	case errors.Is(err, ErrModelUnavailable):
		status, code, message = http.StatusUnprocessableEntity, "REPLACEMENT_MODEL_UNAVAILABLE", "the required published replacement model is unavailable"
	case errors.Is(err, errLifecyclePlanWindowInvalid):
		status, code, message = http.StatusUnprocessableEntity, "LIFECYCLE_PLAN_WINDOW_INVALID", "the lifecycle plan effective time is outside the allowed window"
	case errors.Is(err, errModelServingUnavailable):
		status, code, message = http.StatusUnprocessableEntity, "MODEL_SERVING_COVERAGE_MISSING", "the model has no complete serving coverage"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code, message = http.StatusGatewayTimeout, "LIFECYCLE_PLAN_TIMEOUT", "the lifecycle plan operation timed out"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func parseLifecyclePlanEnvelope(envelope TokenCommandEnvelope) (lifecyclePlanCommand, lifecyclePlanCritical, error) {
	var command lifecyclePlanCommand
	var expected lifecyclePlanCritical
	if envelope.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" || envelope.TargetResourceType != "MODEL" || decodeCommand(envelope.Command, &command) != nil || decodeCommand(envelope.ExpectedVersions, &expected) != nil || command.ModelID == uuid.Nil || command.ModelID.String() != envelope.TargetResourceID || command.LifecyclePlanID == uuid.Nil || command.LifecyclePlanVersion < 1 || expected.ModelID != command.ModelID || expected.LifecyclePlanVersion != command.LifecyclePlanVersion || expected.TargetLifecycle != command.TargetLifecycle || expected.EffectiveAt.UTC() != command.EffectiveAt.UTC() || !sameOptionalTime(expected.ExistingSessionCutoffAt, command.ExistingSessionCutoffAt) || !sameOptionalUUID(expected.ReplacementModelID, command.ReplacementModelID) || expected.PolicyVersion != 2 {
		return lifecyclePlanCommand{}, lifecyclePlanCritical{}, ErrTokenCommandInvalid
	}
	validTransition := expected.CurrentLifecycle == "PUBLISHED" && (command.TargetLifecycle == "DEPRECATED" || command.TargetLifecycle == "REMOVED") || expected.CurrentLifecycle == "DEPRECATED" && command.TargetLifecycle == "REMOVED"
	if !validTransition || command.TargetLifecycle != "REMOVED" && command.ReplacementModelID != nil || command.TargetLifecycle == "PUBLISHED" && command.ExistingSessionCutoffAt != nil || command.TargetLifecycle == "REMOVED" && (command.ReplacementModelID == nil || command.ExistingSessionCutoffAt == nil || !command.ExistingSessionCutoffAt.Equal(command.EffectiveAt)) || command.TargetLifecycle == "DEPRECATED" && command.ExistingSessionCutoffAt != nil && command.ExistingSessionCutoffAt.Before(command.EffectiveAt) {
		return lifecyclePlanCommand{}, lifecyclePlanCritical{}, ErrTokenCommandInvalid
	}
	return command, expected, nil
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UTC() == b.UTC()
}
func sameOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (e *PostgresApprovedCredentialExecutor) precheckApprovedLifecyclePlan(ctx context.Context, envelope TokenCommandEnvelope) (json.RawMessage, error) {
	command, expected, err := parseLifecyclePlanEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	plan, err := e.repository.LifecyclePlan(ctx, command.ModelID, command.LifecyclePlanID)
	if err != nil {
		return nil, err
	}
	if !containsString([]string{"PENDING_APPROVAL", "SCHEDULED", "EXECUTING"}, plan.Status) || plan.PlanVersion != command.LifecyclePlanVersion || plan.ExpectedCatalogVersion != expected.CatalogVersion || plan.CurrentLifecycle != expected.CurrentLifecycle || plan.TargetLifecycle != command.TargetLifecycle || !plan.EffectiveAt.Equal(command.EffectiveAt.UTC()) || !sameOptionalTime(plan.ExistingSessionCutoffAt, command.ExistingSessionCutoffAt) || !sameOptionalUUID(plan.ReplacementModelID, command.ReplacementModelID) {
		return nil, ErrCatalogVersionConflict
	}
	evidence, err := e.repository.ModelLifecycleImpact(ctx, command.ModelID, command.ReplacementModelID)
	if err != nil || evidence.CurrentVersion.CatalogVersion == nil || *evidence.CurrentVersion.CatalogVersion != expected.CatalogVersion || evidence.CurrentStatus != expected.CurrentLifecycle || evidence.RouteSetFingerprint != expected.RouteSetFingerprint || evidence.ProvisioningSetFingerprint != expected.ProvisioningSetFingerprint || evidence.InflightSetFingerprint != expected.InflightRequestSetFingerprint {
		return nil, ErrCatalogVersionConflict
	}
	if command.TargetLifecycle == "REMOVED" && (command.ReplacementModelID == nil || evidence.ReplacementReady == nil || !*evidence.ReplacementReady) {
		return nil, ErrCatalogVersionConflict
	}
	return json.Marshal(expected)
}

// ExecuteApprovedModelLifecycle schedules an approved immutable plan and
// completes the governed command in the same Token transaction. Applying the
// catalog transition is deliberately left to the due-plan worker; APT-138 is
// responsible for activating delivery of approved Account events.
func (e *PostgresApprovedCredentialExecutor) ExecuteApprovedModelLifecycle(ctx context.Context, reference, actorID, approvalID uuid.UUID, commandFingerprint string, critical, financial json.RawMessage, envelope TokenCommandEnvelope) (TokenCommandResult, error) {
	command, expected, err := parseLifecyclePlanEnvelope(envelope)
	if err != nil || approvalID == uuid.Nil || len(strings.TrimSpace(commandFingerprint)) != 64 || !jsonSemanticEqual(critical, envelope.ExpectedVersions) || len(financial) == 0 {
		return TokenCommandResult{}, ErrTokenCommandInvalid
	}
	tx, err := e.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	claimed, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1 FOR UPDATE`, reference))
	if err != nil {
		return TokenCommandResult{}, err
	}
	if claimed.ActorUserID != actorID || claimed.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" || claimed.ApprovalRequestID == nil || *claimed.ApprovalRequestID != approvalID {
		return TokenCommandResult{}, ErrForbidden
	}
	if claimed.Status != "PENDING" {
		return claimed, tx.Commit(ctx)
	}
	if err = e.repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, err
	}
	plan, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1 AND model_id=$2 FOR UPDATE`, command.LifecyclePlanID, command.ModelID))
	if err != nil {
		return TokenCommandResult{}, err
	}
	if plan.Status == "SCHEDULED" && plan.ApprovalRequestID != nil && *plan.ApprovalRequestID == approvalID {
		result, _ := json.Marshal(map[string]any{"lifecycle_plan_id": plan.ID, "lifecycle_plan_version": plan.PlanVersion, "status": plan.Status, "effective_at": plan.EffectiveAt})
		completed, completeErr := completeTokenCommandTx(ctx, tx, reference, "SUCCEEDED", result, result, "")
		if completeErr != nil {
			return TokenCommandResult{}, completeErr
		}
		return completed, e.repository.commitGovernedMutation(ctx, tx)
	}
	if plan.Status != "PENDING_APPROVAL" || plan.PlanVersion != expected.LifecyclePlanVersion || plan.ExpectedCatalogVersion != expected.CatalogVersion || plan.CurrentLifecycle != expected.CurrentLifecycle || plan.TargetLifecycle != expected.TargetLifecycle || !plan.EffectiveAt.Equal(expected.EffectiveAt.UTC()) || !sameOptionalTime(plan.ExistingSessionCutoffAt, expected.ExistingSessionCutoffAt) || !sameOptionalUUID(plan.ReplacementModelID, expected.ReplacementModelID) {
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	evidence, err := modelImpactInTx(ctx, tx, command.ModelID, command.ReplacementModelID)
	if err != nil || evidence.CurrentVersion.CatalogVersion == nil || *evidence.CurrentVersion.CatalogVersion != expected.CatalogVersion || evidence.CurrentStatus != expected.CurrentLifecycle || evidence.RouteSetFingerprint != expected.RouteSetFingerprint || evidence.ProvisioningSetFingerprint != expected.ProvisioningSetFingerprint || evidence.InflightSetFingerprint != expected.InflightRequestSetFingerprint {
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	if command.ReplacementModelID != nil {
		ready, readyErr := replacementModelReadyInTx(ctx, tx, expected.CatalogVersion, *command.ReplacementModelID)
		if readyErr != nil || !ready {
			return TokenCommandResult{}, ErrCatalogVersionConflict
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return TokenCommandResult{}, err
	}
	mutation, err := tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot=$5,updated_at=$6 WHERE id=$1 AND status='PENDING_APPROVAL'`, plan.ID, approvalID, strings.ToLower(strings.TrimSpace(commandFingerprint)), critical, financial, now)
	if err != nil || mutation.RowsAffected() != 1 {
		if err != nil {
			return TokenCommandResult{}, err
		}
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	if err = appendLifecyclePlanEvent(ctx, tx, plan.ID, plan.PlanVersion, "APPROVED", "SERVICE", "token-command-executor", plan.Reason, "", map[string]any{"approval_request_id": approvalID, "command_fingerprint": strings.ToLower(strings.TrimSpace(commandFingerprint))}, now); err != nil {
		return TokenCommandResult{}, err
	}
	after, _ := json.Marshal(map[string]any{"lifecycle_plan_id": plan.ID, "lifecycle_plan_version": plan.PlanVersion, "status": "SCHEDULED", "effective_at": plan.EffectiveAt})
	completed, err := completeTokenCommandTx(ctx, tx, reference, "SUCCEEDED", after, after, "")
	if err != nil {
		return TokenCommandResult{}, err
	}
	return completed, e.repository.commitGovernedMutation(ctx, tx)
}

func jsonSemanticEqual(left, right json.RawMessage) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && string(mustCanonicalJSON(a)) == string(mustCanonicalJSON(b))
}

func mustCanonicalJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
