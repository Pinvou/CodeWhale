package token

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type LifecyclePlanWorker struct {
	service    *Service
	repository *PostgresRepository
	workerID   string
}

func NewLifecyclePlanWorker(service *Service, repository *PostgresRepository, workerID string) *LifecyclePlanWorker {
	return &LifecyclePlanWorker{service: service, repository: repository, workerID: strings.TrimSpace(workerID)}
}

// RunOnce is registered only by the epoch-bound APT-138 supervisor.
func (w *LifecyclePlanWorker) RunOnce(ctx context.Context) (bool, error) {
	return w.runOnce(ctx, nil)
}

// runOnce accepts a deterministic as-of only for package integration tests.
// Production always passes nil and therefore uses transaction_timestamp().
func (w *LifecyclePlanWorker) runOnce(ctx context.Context, testAsOf *time.Time) (bool, error) {
	if w == nil || w.service == nil || w.repository == nil || w.workerID == "" {
		return false, ErrTokenCommandInvalid
	}
	if w.service.commandGovernanceActivation != nil {
		activation, err := w.service.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx)
		if err != nil || activation.State != "ACTIVE" || activation.Epoch < 1 {
			return false, ErrTokenRouteActivationChanged
		}
		if selected, ok := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection); ok && selected.TokenCommandGovernanceActivation != activation {
			return false, ErrTokenRouteActivationChanged
		}
		ctx = context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: activation, transactional: true})
	}
	expired, err := w.repository.ExpirePendingLifecyclePlan(ctx, w.workerID)
	if err != nil || expired {
		return expired, err
	}
	plan, claimed, err := w.repository.claimDueLifecyclePlan(ctx, w.workerID, testAsOf)
	if err != nil || !claimed {
		return claimed, err
	}
	var critical, financial json.RawMessage
	if plan.TargetLifecycle == "PUBLISHED" {
		commandRaw, _ := json.Marshal(modelPublishCommand{ModelID: plan.ModelID, PublishMode: "SCHEDULED", LifecyclePlanID: &plan.ID, LifecyclePlanVersion: &plan.PlanVersion, EffectiveAt: &plan.EffectiveAt})
		projection, precheckErr := w.service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: plan.ModelID.String(), Command: commandRaw})
		if precheckErr != nil {
			if deterministicLifecyclePrecheckError(precheckErr) {
				return true, w.repository.FailLifecyclePlan(ctx, plan.ID, w.workerID, "INVALIDATED", "DEPENDENCY_SNAPSHOT_CHANGED")
			}
			return true, precheckErr
		}
		critical, financial = projection.ExpectedVersions, json.RawMessage(`{}`)
		if len(projection.Blockers) > 0 {
			return true, w.repository.FailLifecyclePlan(ctx, plan.ID, w.workerID, "INVALIDATED", "DEPENDENCY_SNAPSHOT_CHANGED")
		}
	} else {
		commandRaw, _ := json.Marshal(lifecyclePlanCommandFromPlan(plan))
		projection, precheckErr := w.service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: plan.ModelID.String(), NormalizedCommand: commandRaw})
		if precheckErr != nil {
			return true, precheckErr
		}
		critical, financial = projection.CriticalResourceVersions, projection.FinancialSnapshot
	}
	criticalMatches := jsonSemanticEqual(critical, plan.CriticalResourceVersions)
	if plan.TargetLifecycle == "PUBLISHED" {
		// Governed expected_versions adds the two command proof fingerprints to
		// the thirteen authoritative domain facts returned by precheck. Compare
		// through the shared matcher so those envelope-only proofs do not make an
		// otherwise unchanged due plan look stale.
		criticalMatches = tokenCommandExpectedVersionsMatch(plan.CriticalResourceVersions, critical)
	}
	financialMatches := jsonSemanticEqual(financial, plan.FinancialSnapshot)
	if plan.TargetLifecycle != "PUBLISHED" {
		financialMatches = approvalpolicy.SameTokenModelFinancialFacts(financial, plan.FinancialSnapshot)
	}
	if !criticalMatches || !financialMatches {
		return true, w.repository.FailLifecyclePlan(ctx, plan.ID, w.workerID, "INVALIDATED", "DEPENDENCY_SNAPSHOT_CHANGED")
	}
	if plan.TargetLifecycle == "PUBLISHED" && w.service.upstream == nil {
		return true, errors.New("gateway protocol capabilities unavailable")
	}
	var capabilities []GatewayProtocolCapability
	if w.service.upstream != nil {
		capabilities = w.service.upstream.Protocols()
	}
	err = w.repository.ApplyClaimedLifecyclePlan(ctx, plan.ID, w.workerID, capabilities)
	if errors.Is(err, ErrCatalogVersionConflict) {
		return true, w.repository.FailLifecyclePlan(ctx, plan.ID, w.workerID, "INVALIDATED", "DEPENDENCY_SNAPSHOT_CHANGED")
	}
	return true, err
}

func deterministicLifecyclePrecheckError(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrCatalogVersionConflict) || errors.Is(err, ErrProtocolFormulaPrecheck) || errors.Is(err, ErrTokenCommandInvalid)
}

func lifecyclePlanCommandFromPlan(plan LifecyclePlan) lifecyclePlanCommand {
	return lifecyclePlanCommand{ModelID: plan.ModelID, LifecyclePlanID: plan.ID, LifecyclePlanVersion: plan.PlanVersion, TargetLifecycle: plan.TargetLifecycle, EffectiveAt: plan.EffectiveAt, ExistingSessionCutoffAt: plan.ExistingSessionCutoffAt, ReplacementModelID: plan.ReplacementModelID}
}

// ExpirePendingLifecyclePlan records one immutable expiry event per call so
// the transition is observable and can be retried safely after a worker crash.
func (r *PostgresRepository) ExpirePendingLifecyclePlan(ctx context.Context, workerID string) (bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(workerID) > 160 {
		return false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var planID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM model_lifecycle_plans WHERE status='PENDING_APPROVAL' AND created_at+make_interval(secs=>$1)<=transaction_timestamp() ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, int64(lifecyclePlanApprovalTTL/time.Second)).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	plan, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1`, planID))
	if err != nil {
		return false, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return false, err
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='EXPIRED',completed_at=$2,updated_at=$2,stable_error_code='PLAN_APPROVAL_EXPIRED' WHERE id=$1 AND status='PENDING_APPROVAL'`, planID, now)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return false, err
		}
		return false, ErrCatalogVersionConflict
	}
	if err = appendLifecyclePlanEvent(ctx, tx, plan.ID, plan.PlanVersion, "EXPIRED", "SERVICE", workerID, plan.Reason, "PLAN_APPROVAL_EXPIRED", map[string]any{"approval_ttl_seconds": int64(lifecyclePlanApprovalTTL / time.Second)}, now); err != nil {
		return false, err
	}
	return true, r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) ClaimDueLifecyclePlan(ctx context.Context, workerID string) (LifecyclePlan, bool, error) {
	return r.claimDueLifecyclePlan(ctx, workerID, nil)
}

func (r *PostgresRepository) claimDueLifecyclePlan(ctx context.Context, workerID string, testAsOf *time.Time) (LifecyclePlan, bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(workerID) > 160 {
		return LifecyclePlan{}, false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	defer tx.Rollback(ctx)
	var asOf time.Time
	if testAsOf == nil {
		if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&asOf); err != nil {
			return LifecyclePlan{}, false, err
		}
	} else {
		asOf = testAsOf.UTC()
	}
	var planID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM model_lifecycle_plans WHERE (status='SCHEDULED' AND effective_at<=$1) OR (status='EXECUTING' AND execution_lease_until<=$1) ORDER BY effective_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, asOf).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LifecyclePlan{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	now := asOf
	leaseUntil := now.Add(lifecyclePlanLeaseTTL)
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return LifecyclePlan{}, false, err
	}
	result, err := tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='EXECUTING',execution_lease_owner=$2,execution_lease_until=$3,execution_attempt=execution_attempt+1,updated_at=$4 WHERE id=$1 AND ((status='SCHEDULED' AND effective_at<=$4) OR (status='EXECUTING' AND execution_lease_until<=$4))`, planID, workerID, leaseUntil, now)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return LifecyclePlan{}, false, err
		}
		return LifecyclePlan{}, false, ErrCatalogVersionConflict
	}
	plan, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1`, planID))
	if err != nil {
		return LifecyclePlan{}, false, err
	}
	selection, _ := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
	if err = appendLifecyclePlanEvent(ctx, tx, plan.ID, plan.PlanVersion, "LEASED", "SERVICE", workerID, plan.Reason, "", map[string]any{"execution_attempt": plan.ExecutionAttempt, "lease_until": leaseUntil, "activation_epoch": selection.Epoch}, now); err != nil {
		return LifecyclePlan{}, false, err
	}
	return plan, true, r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) FailLifecyclePlan(ctx context.Context, planID uuid.UUID, workerID, status, stableCode string) error {
	status = strings.ToUpper(strings.TrimSpace(status))
	stableCode = strings.ToUpper(strings.TrimSpace(stableCode))
	workerID = strings.TrimSpace(workerID)
	if planID == uuid.Nil || workerID == "" || (status != "INVALIDATED" && status != "FAILED" && status != "UNKNOWN") || !validStableTokenErrorCode(stableCode) {
		return ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	plan, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1 FOR UPDATE`, planID))
	if err != nil {
		return err
	}
	if plan.Status == status {
		return tx.Commit(ctx)
	}
	if plan.Status != "EXECUTING" {
		return ErrCatalogVersionConflict
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return err
	}
	if err = r.checkLifecycleLeaseEpoch(ctx, tx, plan, workerID); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status=$2,stable_error_code=$3,execution_lease_owner=NULL,execution_lease_until=NULL,completed_at=CASE WHEN $2='UNKNOWN' THEN NULL ELSE $4 END,updated_at=$4 WHERE id=$1 AND status='EXECUTING' AND execution_lease_owner=$5 AND execution_lease_until>clock_timestamp()`, planID, status, stableCode, now, workerID)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return err
		}
		return ErrCatalogVersionConflict
	}
	if err = appendLifecyclePlanEvent(ctx, tx, plan.ID, plan.PlanVersion, status, "SERVICE", workerID, plan.Reason, stableCode, map[string]any{"execution_attempt": plan.ExecutionAttempt}, now); err != nil {
		return err
	}
	return r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) ApplyClaimedLifecyclePlan(ctx context.Context, planID uuid.UUID, workerID string, capabilities []GatewayProtocolCapability) error {
	workerID = strings.TrimSpace(workerID)
	if planID == uuid.Nil || workerID == "" {
		return ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return err
	}
	if err = lockCatalogPublication(ctx, tx); err != nil {
		return err
	}
	plan, err := scanLifecyclePlan(tx.QueryRow(ctx, lifecyclePlanSelect+` WHERE id=$1 FOR UPDATE`, planID))
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if plan.Status != "EXECUTING" || plan.ExecutionLeaseUntil == nil || !plan.ExecutionLeaseUntil.After(now) {
		return ErrCatalogVersionConflict
	}
	var leaseOwner string
	if err = tx.QueryRow(ctx, `SELECT execution_lease_owner FROM model_lifecycle_plans WHERE id=$1`, planID).Scan(&leaseOwner); err != nil || leaseOwner != workerID {
		return ErrCatalogVersionConflict
	}
	if err = r.checkLifecycleLeaseEpoch(ctx, tx, plan, workerID); err != nil {
		return err
	}
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current != plan.ExpectedCatalogVersion {
		return ErrCatalogVersionConflict
	}
	if plan.TargetLifecycle == "PUBLISHED" {
		if plan.GovernedCommandReference == nil || plan.ApprovalRequestID != nil {
			return ErrCatalogVersionConflict
		}
		expected, parseErr := parseModelPublishExpected(plan.CriticalResourceVersions, plan.ModelID)
		if parseErr != nil || expected.ExpectedCatalogVersion != current {
			return ErrCatalogVersionConflict
		}
		commandRaw, _ := json.Marshal(modelPublishCommand{ModelID: plan.ModelID, PublishMode: "SCHEDULED", LifecyclePlanID: &plan.ID, LifecyclePlanVersion: &plan.PlanVersion, EffectiveAt: &plan.EffectiveAt})
		local, localErr := loadCommandPrecheckSnapshot(ctx, tx, "TOKEN_MODEL_PUBLISH", plan.ModelID.String(), commandRaw)
		if localErr != nil || len(local.Blockers) > 0 {
			return ErrCatalogVersionConflict
		}
		var frozen map[string]any
		if decodeExactCommandJSON(plan.CriticalResourceVersions, &frozen) != nil {
			return ErrCatalogVersionConflict
		}
		for key, value := range local.ExpectedVersions {
			candidate, ok := frozen[key]
			left, _ := json.Marshal(value)
			right, _ := json.Marshal(candidate)
			if !ok || !jsonSemanticEqual(left, right) {
				return ErrCatalogVersionConflict
			}
		}
		published, publishErr := publishCatalogDraftEffect(ctx, tx, plan.ModelID, expected.ExpectedCatalogVersion, expected.ExpectedDraftVersion, plan.Reason, plan.CreatedBy, capabilities, now)
		if publishErr != nil {
			return publishErr
		}
		if err = completeAppliedLifecyclePlan(ctx, tx, plan, workerID, published.CatalogVersion, now); err != nil {
			return err
		}
		return r.commitGovernedMutation(ctx, tx)
	}
	item, err := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, current, plan.ModelID))
	if err != nil {
		return err
	}
	if item.LifecycleStatus != plan.CurrentLifecycle {
		return ErrCatalogVersionConflict
	}
	var expected lifecyclePlanCritical
	if json.Unmarshal(plan.CriticalResourceVersions, &expected) != nil || expected.ModelID != plan.ModelID || expected.LifecyclePlanVersion != plan.PlanVersion || expected.CatalogVersion != current {
		return ErrCatalogVersionConflict
	}
	evidence, err := modelImpactInTx(ctx, tx, plan.ModelID, plan.ReplacementModelID)
	if err != nil || evidence.CurrentVersion.CatalogVersion == nil || *evidence.CurrentVersion.CatalogVersion != expected.CatalogVersion || evidence.CurrentStatus != expected.CurrentLifecycle || evidence.RouteSetFingerprint != expected.RouteSetFingerprint || evidence.ProvisioningSetFingerprint != expected.ProvisioningSetFingerprint || evidence.InflightSetFingerprint != expected.InflightRequestSetFingerprint {
		return ErrCatalogVersionConflict
	}
	if plan.ReplacementModelID != nil {
		ready, readyErr := replacementModelReadyInTx(ctx, tx, current, *plan.ReplacementModelID)
		if readyErr != nil || !ready {
			return ErrCatalogVersionConflict
		}
	}
	version, err := createCatalogVersion(ctx, tx, nil, plan.Reason, plan.CreatedBy, now)
	if err != nil {
		return err
	}
	if err = copyCatalogExcept(ctx, tx, current, version, []uuid.UUID{plan.ModelID}); err != nil {
		return err
	}
	var newCutoff, continuation *time.Time
	if plan.TargetLifecycle == "DEPRECATED" {
		newCutoff, continuation = &plan.EffectiveAt, plan.ExistingSessionCutoffAt
	} else if plan.TargetLifecycle == "REMOVED" {
		newCutoff, continuation = plan.ExistingSessionCutoffAt, plan.ExistingSessionCutoffAt
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight) SELECT $1,model_id,display_name,normalized_name,description,aliases,$2,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,$3,$4,$5,$6,$7 FROM model_catalog_entries WHERE catalog_version=$8 AND model_id=$9`, version, plan.TargetLifecycle, plan.EffectiveAt, plan.ReplacementModelID, newCutoff, continuation, plan.ForceCancelInflight, current, plan.ModelID)
	if err != nil {
		return err
	}
	if err = appendCatalogAudit(ctx, tx, plan.CreatedBy, "MODEL_LIFECYCLE_PLAN_APPLIED", &plan.ModelID, &item.LifecycleStatus, &plan.TargetLifecycle, version, plan.Reason, map[string]any{"lifecycle_plan_id": plan.ID, "lifecycle_plan_version": plan.PlanVersion, "approval_request_id": plan.ApprovalRequestID}); err != nil {
		return err
	}
	if plan.TargetLifecycle == "REMOVED" && plan.ForceCancelInflight {
		if _, err = tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(catalog_version,model_id) DO NOTHING`, uuid.Must(uuid.NewV7()), version, plan.ModelID, plan.EffectiveAt, plan.CreatedBy, plan.Reason, now); err != nil {
			return err
		}
	}
	if err = completeAppliedLifecyclePlan(ctx, tx, plan, workerID, version, now); err != nil {
		return err
	}
	return r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) checkLifecycleLeaseEpoch(ctx context.Context, tx pgx.Tx, plan LifecyclePlan, workerID string) error {
	if r.routeActivationGuard == nil {
		return nil
	} // historical isolated domain tests only
	selected, ok := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
	if !ok || !selected.transactional || selected.State != "ACTIVE" || selected.Epoch < 1 {
		return ErrTokenRouteActivationChanged
	}
	var owner, epoch, attempt string
	err := tx.QueryRow(ctx, `SELECT actor_id,evidence->>'activation_epoch',evidence->>'execution_attempt' FROM model_lifecycle_plan_events WHERE plan_id=$1 AND event_type='LEASED' ORDER BY occurred_at DESC,id DESC LIMIT 1`, plan.ID).Scan(&owner, &epoch, &attempt)
	if err != nil || owner != workerID || epoch != strconv.FormatInt(selected.Epoch, 10) || attempt != strconv.Itoa(plan.ExecutionAttempt) {
		return ErrTokenRouteActivationChanged
	}
	return nil
}

func completeAppliedLifecyclePlan(ctx context.Context, tx pgx.Tx, plan LifecyclePlan, workerID string, version int64, now time.Time) error {
	result, err := tx.Exec(ctx, `UPDATE model_lifecycle_plans SET status='APPLIED',result_catalog_version=$2,execution_lease_owner=NULL,execution_lease_until=NULL,completed_at=$3,updated_at=$3,stable_error_code=NULL WHERE id=$1 AND status='EXECUTING' AND execution_lease_owner=$4 AND execution_attempt=$5 AND execution_lease_until>clock_timestamp()`, plan.ID, version, now, workerID, plan.ExecutionAttempt)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return err
		}
		return ErrCatalogVersionConflict
	}
	if err = appendLifecyclePlanEvent(ctx, tx, plan.ID, plan.PlanVersion, "APPLIED", "SERVICE", workerID, plan.Reason, "", map[string]any{"result_catalog_version": version}, now); err != nil {
		return err
	}
	return nil
}
