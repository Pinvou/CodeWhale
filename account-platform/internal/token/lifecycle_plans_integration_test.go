package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

type lifecyclePricingStub struct{ observedAt time.Time }

func (stub lifecyclePricingStub) ImpactPriceCoverage(_ context.Context, dimensions []ImpactPriceQuery) (ImpactPriceCoverageResult, error) {
	items := make([]ImpactPriceCoverage, 0, len(dimensions))
	version := int64(3)
	effective := stub.observedAt.Add(-time.Hour)
	for _, dimension := range dimensions {
		formulaID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("formula:"+dimension.ProtocolCode))
		priceID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("price:"+dimension.ModelID.String()+":"+dimension.ProtocolCode))
		items = append(items, ImpactPriceCoverage{ModelID: dimension.ModelID, ProtocolCode: dimension.ProtocolCode, FormulaVersionID: &formulaID, CreditUnit: dimension.CreditUnit, Covered: true, PriceVersionID: &priceID, PriceVersion: &version, EffectiveFrom: &effective})
	}
	return ImpactPriceCoverageResult{Items: items, AsOf: stub.observedAt, Source: "BILLING", FinancialFactRevision: 9, PriceSetFingerprint: strings.Repeat("9", 64)}, nil
}

// TestModelLifecyclePlansPostgresIntegration is the APT-134 PostgreSQL gate.
// CI supplies PINVOU_TEST_TOKEN_DATABASE_URL and rejects a skipped result.
func TestModelLifecyclePlansPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	actor, otherActor, modelID, replacementID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	catalogVersion, draftModelID := seedLifecycleModels(t, ctx, repository, actor, modelID, replacementID, now)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,99,$3,'DRAFT','DEPRECATED',$4,false,'PENDING_APPROVAL','reject illegal lifecycle transition',$5,$6,$7,$8,$8)`, uuid.New(), draftModelID, catalogVersion, now.Add(time.Hour), strings.Repeat("a", 64), strings.Repeat("b", 64), actor, now); err == nil || !strings.Contains(err.Error(), "model_lifecycle_plans_transition_ck") {
		t.Fatalf("illegal transition constraint error=%v", err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,98,$3,'DRAFT','PUBLISHED',$4,false,'SCHEDULED','reject publication without frozen evidence',$5,$6,$7,$8,$8)`, uuid.New(), draftModelID, catalogVersion, now.Add(time.Hour), strings.Repeat("a", 64), strings.Repeat("b", 64), actor, now); err == nil || !strings.Contains(err.Error(), "exact frozen governed snapshot") {
		t.Fatalf("unfrozen publication insert error=%v", err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, NewHTTPUpstreamClient(), 0).
		WithImpactPrechecks(true, repository, lifecyclePricingStub{observedAt: now})
	governedHandler := newGovernedPublishIntegrationHandler(t, service, actor)
	lifecycleHandler := newLifecyclePlanIntegrationHandler(t, repository, actor)
	revokedHandler := newLifecyclePlanPermissionIntegrationHandler(t, repository, actor, []string{"platform.models.read"}, "lifecycle-plan-revoked")
	assertLifecyclePlanHTTP(t, lifecycleHandler, governedHandler, revokedHandler, repository, replacementID, catalogVersion, now)
	initialImpact, err := repository.ModelLifecycleImpact(ctx, modelID, &replacementID)
	if err != nil || initialImpact.ReplacementReady == nil || !*initialImpact.ReplacementReady {
		t.Fatalf("initial replacement impact=%+v err=%v", initialImpact, err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='FAILED',stage='FAILED',updated_at=transaction_timestamp() WHERE model_id=$1`, replacementID); err != nil {
		t.Fatal(err)
	}
	unreadyImpact, err := repository.ModelLifecycleImpact(ctx, modelID, &replacementID)
	if err != nil || unreadyImpact.ReplacementReady == nil || *unreadyImpact.ReplacementReady || unreadyImpact.ProvisioningSetFingerprint == initialImpact.ProvisioningSetFingerprint {
		t.Fatalf("unready replacement impact=%+v err=%v", unreadyImpact, err)
	}
	removedUnavailableCutoff := now.Add(12 * time.Minute)
	removedUnavailable := LifecyclePlanInput{ExpectedCatalogVersion: catalogVersion, TargetLifecycle: "REMOVED", EffectiveAt: removedUnavailableCutoff, ExistingSessionCutoffAt: &removedUnavailableCutoff, ReplacementModelID: &replacementID, Reason: "remove only after replacement is serviceable"}
	if _, _, err := repository.CreateLifecyclePlan(ctx, modelID, removedUnavailable, actor, "lifecycle-plan-unready-replacement"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("unready replacement create error=%v", err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='READY',stage='READY',updated_at=transaction_timestamp() WHERE model_id=$1`, replacementID); err != nil {
		t.Fatal(err)
	}
	restoredImpact, err := repository.ModelLifecycleImpact(ctx, modelID, &replacementID)
	if err != nil || restoredImpact.ReplacementReady == nil || !*restoredImpact.ReplacementReady {
		t.Fatalf("restored replacement impact=%+v err=%v", restoredImpact, err)
	}
	var accountID, oldCredentialID uuid.UUID
	if err = repository.pool.QueryRow(ctx, `SELECT active_token_account_id,active_credential_id FROM route_provisionings WHERE model_id=$1`, replacementID).Scan(&accountID, &oldCredentialID); err != nil {
		t.Fatal(err)
	}
	newCredentialID := uuid.New()
	if _, err = repository.pool.Exec(ctx, `UPDATE provider_credentials SET status='RETIRED' WHERE id=$1; INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) SELECT $2,token_account_id,provider,base_url,ciphertext,key_id,$3,version+1,'ACTIVE',allowed_models,validated_at,created_by,transaction_timestamp(),validation_status FROM provider_credentials WHERE id=$1; UPDATE route_provisionings SET active_credential_id=$2,updated_at=transaction_timestamp() WHERE model_id=$4`, oldCredentialID, newCredentialID, strings.Repeat("e", 64), replacementID); err != nil {
		t.Fatal(err)
	}
	replacedImpact, err := repository.ModelLifecycleImpact(ctx, modelID, &replacementID)
	if err != nil || replacedImpact.ReplacementReady == nil || !*replacedImpact.ReplacementReady || replacedImpact.RouteSetFingerprint == restoredImpact.RouteSetFingerprint || replacedImpact.ProvisioningSetFingerprint == restoredImpact.ProvisioningSetFingerprint || replacedImpact.CurrentVersion.EvidenceSetSHA256 == restoredImpact.CurrentVersion.EvidenceSetSHA256 {
		t.Fatalf("equal-count replacement drift was not frozen: before=%+v after=%+v err=%v", restoredImpact, replacedImpact, err)
	}

	base := LifecyclePlanInput{ExpectedCatalogVersion: catalogVersion, ExpectedPlanVersion: 0, TargetLifecycle: "DEPRECATED", EffectiveAt: now.Add(10 * time.Minute), Reason: "deprecate after reviewed dependency window"}
	if _, _, err := repository.CreateLifecyclePlan(ctx, modelID, LifecyclePlanInput{ExpectedCatalogVersion: catalogVersion, TargetLifecycle: "DEPRECATED", EffectiveAt: now.Add(time.Minute), Reason: base.Reason}, actor, "lifecycle-plan-too-early"); !errors.Is(err, errLifecyclePlanWindowInvalid) {
		t.Fatalf("minimum window error=%v", err)
	}
	forced := base
	forced.ForceCancelInflight = true
	if _, _, err := repository.CreateLifecyclePlan(ctx, modelID, forced, actor, "lifecycle-plan-force-denied"); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("force path error=%v", err)
	}
	removed := base
	removed.TargetLifecycle = "REMOVED"
	removed.ExistingSessionCutoffAt = &removed.EffectiveAt
	if _, _, err := repository.CreateLifecyclePlan(ctx, modelID, removed, actor, "lifecycle-plan-no-replacement"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("missing replacement error=%v", err)
	}

	plan, created, err := repository.CreateLifecyclePlan(ctx, modelID, base, actor, "lifecycle-plan-create-0001")
	if err != nil || !created || plan.PlanVersion != 1 || plan.Status != "PENDING_APPROVAL" {
		t.Fatalf("create plan=%+v created=%v err=%v", plan, created, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET effective_at=effective_at+interval '1 minute' WHERE id=$1`, plan.ID); err == nil {
		t.Fatal("immutable lifecycle plan definition was mutated")
	}
	replayed, created, err := repository.CreateLifecyclePlan(ctx, modelID, base, actor, "lifecycle-plan-create-0001")
	if err != nil || created || replayed.ID != plan.ID {
		t.Fatalf("replay plan=%+v created=%v err=%v", replayed, created, err)
	}
	if _, _, err := repository.CreateLifecyclePlan(ctx, modelID, base, otherActor, "lifecycle-plan-create-0001"); !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("cross actor active plan error=%v", err)
	}

	reschedule := base
	reschedule.ExpectedPlanVersion = 1
	reschedule.EffectiveAt = now.Add(20 * time.Minute)
	rescheduled, created, err := repository.CreateLifecyclePlan(ctx, modelID, reschedule, actor, "lifecycle-plan-create-0002")
	if err != nil || !created || rescheduled.PlanVersion != 2 || rescheduled.Status != "PENDING_APPROVAL" {
		t.Fatalf("reschedule plan=%+v created=%v err=%v", rescheduled, created, err)
	}
	old, err := repository.LifecyclePlan(ctx, modelID, plan.ID)
	if err != nil || old.Status != "CANCELLED" || old.StableErrorCode == nil || *old.StableErrorCode != "PLAN_SUPERSEDED" {
		t.Fatalf("superseded plan=%+v err=%v", old, err)
	}
	cancelled, err := repository.CancelLifecyclePlan(ctx, modelID, rescheduled.ID, rescheduled.PlanVersion, actor, "cancel after dependency review", "lifecycle-plan-cancel-0001")
	if err != nil || cancelled.Status != "CANCELLED" {
		t.Fatalf("cancel plan=%+v err=%v", cancelled, err)
	}
	replayedCancel, err := repository.CancelLifecyclePlan(ctx, modelID, rescheduled.ID, rescheduled.PlanVersion, actor, "cancel after dependency review", "lifecycle-plan-cancel-0001")
	if err != nil || replayedCancel.Status != "CANCELLED" {
		t.Fatalf("cancel replay=%+v err=%v", replayedCancel, err)
	}
	if _, err := repository.CancelLifecyclePlan(ctx, modelID, rescheduled.ID, rescheduled.PlanVersion, otherActor, "cancel after dependency review", "lifecycle-plan-cancel-0001"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross actor cancel error=%v", err)
	}
	afterCancel := reschedule
	afterCancel.ExpectedPlanVersion = 0
	recreated, created, err := repository.CreateLifecyclePlan(ctx, modelID, afterCancel, actor, "lifecycle-plan-create-0003")
	if err != nil || !created || recreated.ID == rescheduled.ID || recreated.PlanVersion != 3 {
		t.Fatalf("create after cancel=%+v created=%v err=%v", recreated, created, err)
	}
	if _, err = repository.CancelLifecyclePlan(ctx, modelID, recreated.ID, recreated.PlanVersion, actor, "cancel recreated lifecycle plan", "lifecycle-plan-cancel-0002"); err != nil {
		t.Fatal(err)
	}

	duePlan := insertDueLifecyclePlan(t, ctx, repository, actor, modelID, catalogVersion, now)
	projection, err := service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), NormalizedCommand: mustMarshalJSON(t, lifecyclePlanCommandFromPlan(duePlan))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot=$5,updated_at=transaction_timestamp() WHERE id=$1`, duePlan.ID, uuid.New(), strings.Repeat("a", 64), projection.CriticalResourceVersions, projection.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET critical_resource_versions=critical_resource_versions||'{"unexpected":true}'::jsonb WHERE id=$1`, duePlan.ID); err == nil || !strings.Contains(err.Error(), "immutable after one freeze") {
		t.Fatalf("lifecycle snapshot replacement error=%v", err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET approval_command_fingerprint=$2 WHERE id=$1`, duePlan.ID, strings.Repeat("b", 64)); err == nil || !strings.Contains(err.Error(), "immutable after one freeze") {
		t.Fatalf("lifecycle approval binding replacement error=%v", err)
	}
	worker := NewLifecyclePlanWorker(service, repository, "lifecycle-worker-a")
	worked, err := worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("worker worked=%v err=%v", worked, err)
	}
	applied, err := repository.LifecyclePlan(ctx, modelID, duePlan.ID)
	if err != nil || applied.Status != "APPLIED" || applied.ResultCatalogVersion == nil || applied.ExecutionAttempt != 1 {
		t.Fatalf("applied plan=%+v err=%v", applied, err)
	}
	var lifecycle string
	if err = repository.pool.QueryRow(ctx, `SELECT lifecycle_status FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2`, *applied.ResultCatalogVersion, modelID).Scan(&lifecycle); err != nil || lifecycle != "DEPRECATED" {
		t.Fatalf("catalog lifecycle=%q err=%v", lifecycle, err)
	}

	assertLifecycleExpiryAndLeaseRecovery(t, ctx, repository, actor, replacementID, *applied.ResultCatalogVersion, now)
	publishDraft, err := repository.SaveCatalogDraft(ctx, draftModelID, CatalogDraftInput{ExpectedCatalogVersion: *applied.ResultCatalogVersion, ExpectedDraftVersion: 0, DisplayName: "lifecycle-model-c", Description: "scheduled publish", ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 4096, MaximumOutputTokens: 1024, SupportedModalities: []string{"TEXT"}, TokenizerVersion: "v1"}, actor)
	if err != nil {
		t.Fatalf("save publish draft=%+v err=%v", publishDraft, err)
	}
	cancelledPublish, _, _ := scheduleModelPublishForTest(t, ctx, service, governedHandler, repository, draftModelID, 1, now.Add(10*time.Minute), "publish reviewed draft then cancel schedule", "scheduled-model-publish-cancel-0001")
	assertLifecycleCancelPermission(t, lifecycleHandler, repository, cancelledPublish, http.StatusForbidden, "scheduled-publish-lifecycle-cancel")
	assertLifecycleCancelPermission(t, governedHandler, repository, cancelledPublish, http.StatusOK, "scheduled-publish-manage-cancel")
	driftedPlan, publishResult, replayScheduledPublish := scheduleModelPublishForTest(t, ctx, service, governedHandler, repository, draftModelID, 2, now.Add(10*time.Minute), "publish reviewed draft on schedule", "scheduled-model-publish-0001")
	if publishResult.Status != "SUCCEEDED" || driftedPlan.Status != "SCHEDULED" || driftedPlan.GovernedCommandReference == nil || driftedPlan.ApprovalRequestID != nil {
		t.Fatalf("scheduled publish plan=%+v result=%+v", driftedPlan, publishResult)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET critical_resource_versions=jsonb_set(critical_resource_versions,'{request_fingerprint}',to_jsonb($2::text)) WHERE id=$1`, driftedPlan.ID, strings.Repeat("f", 64)); err == nil || !strings.Contains(err.Error(), "immutable after one freeze") {
		t.Fatalf("scheduled publication snapshot replacement error=%v", err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET financial_snapshot='{"unexpected":true}'::jsonb WHERE id=$1`, driftedPlan.ID); err == nil || !strings.Contains(err.Error(), "exact frozen governed snapshot") {
		t.Fatalf("scheduled publication financial replacement error=%v", err)
	}
	if _, err = repository.SaveCatalogDraft(ctx, draftModelID, CatalogDraftInput{ExpectedCatalogVersion: *applied.ResultCatalogVersion, ExpectedDraftVersion: publishDraft.DraftVersion, DisplayName: "lifecycle-model-c", Description: "drift after schedule", ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 4096, MaximumOutputTokens: 1024, SupportedModalities: []string{"TEXT"}, TokenizerVersion: "v1"}, actor); err != nil {
		t.Fatal(err)
	}
	replayedAfterDrift, replayStatus := replayScheduledPublish()
	if replayStatus != http.StatusOK || replayedAfterDrift.CommandReference != publishResult.CommandReference || replayedAfterDrift.Status != "SUCCEEDED" {
		t.Fatalf("scheduled publish replay after drift=%+v status=%d", replayedAfterDrift, replayStatus)
	}
	worked, err = worker.runOnce(ctx, func() *time.Time { value := driftedPlan.EffectiveAt.Add(time.Second); return &value }())
	if err != nil || !worked {
		t.Fatalf("drift worker worked=%v err=%v", worked, err)
	}
	driftedPlan, err = repository.LifecyclePlan(ctx, draftModelID, driftedPlan.ID)
	if err != nil || driftedPlan.Status != "INVALIDATED" {
		t.Fatalf("drifted scheduled publish=%+v err=%v", driftedPlan, err)
	}
	var catalogAfterDrift, draftVersionAfterDrift int64
	if err = repository.pool.QueryRow(ctx, `SELECT max(version) FROM model_catalog_versions`).Scan(&catalogAfterDrift); err != nil || catalogAfterDrift != *applied.ResultCatalogVersion {
		t.Fatalf("drifted schedule wrote catalog version=%d want=%d err=%v", catalogAfterDrift, *applied.ResultCatalogVersion, err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT draft_version FROM model_catalog_drafts WHERE model_id=$1`, draftModelID).Scan(&draftVersionAfterDrift); err != nil || draftVersionAfterDrift != publishDraft.DraftVersion+1 {
		t.Fatalf("drifted schedule changed draft version=%d want=%d err=%v", draftVersionAfterDrift, publishDraft.DraftVersion+1, err)
	}
	missingDraftPlan, _, _ := scheduleModelPublishForTest(t, ctx, service, governedHandler, repository, draftModelID, 3, now.Add(15*time.Minute), "invalidate schedule when reviewed draft disappears", "scheduled-model-publish-missing-draft")
	if _, err = repository.pool.Exec(ctx, `DELETE FROM model_catalog_drafts WHERE model_id=$1`, draftModelID); err != nil {
		t.Fatal(err)
	}
	worked, err = worker.runOnce(ctx, func() *time.Time { value := missingDraftPlan.EffectiveAt.Add(time.Second); return &value }())
	if err != nil || !worked {
		t.Fatalf("missing draft worker worked=%v err=%v", worked, err)
	}
	missingDraftPlan, err = repository.LifecyclePlan(ctx, draftModelID, missingDraftPlan.ID)
	if err != nil || missingDraftPlan.Status != "INVALIDATED" || missingDraftPlan.StableErrorCode == nil || *missingDraftPlan.StableErrorCode != "DEPENDENCY_SNAPSHOT_CHANGED" {
		t.Fatalf("missing draft scheduled publish=%+v err=%v", missingDraftPlan, err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT max(version) FROM model_catalog_versions`).Scan(&catalogAfterDrift); err != nil || catalogAfterDrift != *applied.ResultCatalogVersion {
		t.Fatalf("missing draft schedule wrote catalog version=%d want=%d err=%v", catalogAfterDrift, *applied.ResultCatalogVersion, err)
	}
	worked, err = worker.runOnce(ctx, func() *time.Time { value := missingDraftPlan.EffectiveAt.Add(2 * time.Second); return &value }())
	if err != nil || worked {
		t.Fatalf("invalidated missing draft was claimed again worked=%v err=%v", worked, err)
	}
	publishDraft, err = repository.SaveCatalogDraft(ctx, draftModelID, CatalogDraftInput{ExpectedCatalogVersion: *applied.ResultCatalogVersion, ExpectedDraftVersion: 0, DisplayName: "lifecycle-model-c", Description: "scheduled publish after deterministic invalidation", ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 4096, MaximumOutputTokens: 1024, SupportedModalities: []string{"TEXT"}, TokenizerVersion: "v1"}, actor)
	if err != nil || publishDraft.DraftVersion < 1 {
		t.Fatalf("recreate publish draft=%+v err=%v", publishDraft, err)
	}
	publishDue, publishResult, _ := scheduleModelPublishForTest(t, ctx, service, governedHandler, repository, draftModelID, 4, now.Add(20*time.Minute), "publish updated reviewed draft on schedule", "scheduled-model-publish-0002")
	if publishResult.Status != "SUCCEEDED" || publishDue.Status != "SCHEDULED" {
		t.Fatalf("rescheduled publish plan=%+v result=%+v", publishDue, publishResult)
	}
	worked, err = worker.runOnce(ctx, func() *time.Time { value := publishDue.EffectiveAt.Add(time.Second); return &value }())
	if err != nil || !worked {
		t.Fatalf("publish worker worked=%v err=%v", worked, err)
	}
	publishedPlan, err := repository.LifecyclePlan(ctx, draftModelID, publishDue.ID)
	if err != nil || publishedPlan.Status != "APPLIED" || publishedPlan.ResultCatalogVersion == nil {
		t.Fatalf("published plan=%+v err=%v", publishedPlan, err)
	}
	var publishedLifecycle string
	var newCutoff, continuation *time.Time
	if err = repository.pool.QueryRow(ctx, `SELECT lifecycle_status,new_session_cutoff_at,continuation_cutoff_at FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2`, *publishedPlan.ResultCatalogVersion, draftModelID).Scan(&publishedLifecycle, &newCutoff, &continuation); err != nil || publishedLifecycle != "PUBLISHED" || newCutoff != nil || continuation != nil {
		t.Fatalf("published catalog lifecycle=%q new=%v continuation=%v err=%v", publishedLifecycle, newCutoff, continuation, err)
	}
	var remainingDrafts int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM model_catalog_drafts WHERE model_id=$1`, draftModelID).Scan(&remainingDrafts); err != nil || remainingDrafts != 0 {
		t.Fatalf("published draft remained count=%d err=%v", remainingDrafts, err)
	}
	removalDue := insertDueRemovalLifecyclePlan(t, ctx, repository, actor, modelID, replacementID, *publishedPlan.ResultCatalogVersion, now)
	removalProjection, err := service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), NormalizedCommand: mustMarshalJSON(t, lifecyclePlanCommandFromPlan(removalDue))})
	if err != nil {
		t.Fatalf("removal precheck=%+v err=%v", removalProjection, err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot=$5,updated_at=transaction_timestamp() WHERE id=$1`, removalDue.ID, uuid.New(), strings.Repeat("8", 64), removalProjection.CriticalResourceVersions, removalProjection.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	worked, err = worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("removal worker worked=%v err=%v", worked, err)
	}
	removedPlan, err := repository.LifecyclePlan(ctx, modelID, removalDue.ID)
	if err != nil || removedPlan.Status != "APPLIED" || removedPlan.ResultCatalogVersion == nil {
		t.Fatalf("removed plan=%+v err=%v", removedPlan, err)
	}
	var removedLifecycle string
	if err = repository.pool.QueryRow(ctx, `SELECT lifecycle_status,new_session_cutoff_at,continuation_cutoff_at FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2`, *removedPlan.ResultCatalogVersion, modelID).Scan(&removedLifecycle, &newCutoff, &continuation); err != nil || removedLifecycle != "REMOVED" || !sameOptionalTime(newCutoff, removalDue.ExistingSessionCutoffAt) || !sameOptionalTime(continuation, removalDue.ExistingSessionCutoffAt) {
		t.Fatalf("removed catalog lifecycle=%q new=%v continuation=%v frozen=%v err=%v", removedLifecycle, newCutoff, continuation, removalDue.ExistingSessionCutoffAt, err)
	}
	assertLifecycleMigrationDownFailsClosed(t, ctx, repository)
}

func TestLifecycleMigrationDownRejectsPendingCreatedHistory(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	actor, modelID, replacementID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	catalogVersion, _ := seedLifecycleModels(t, ctx, repository, actor, modelID, replacementID, now)
	input := LifecyclePlanInput{ExpectedCatalogVersion: catalogVersion, TargetLifecycle: "DEPRECATED", EffectiveAt: now.Add(10 * time.Minute), Reason: "retain pending immutable lifecycle history"}
	plan, created, err := repository.CreateLifecyclePlan(ctx, modelID, input, actor, "lifecycle-down-pending-history")
	if err != nil || !created || plan.Status != "PENDING_APPROVAL" {
		t.Fatalf("pending plan=%+v created=%v err=%v", plan, created, err)
	}
	var events int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM model_lifecycle_plan_events WHERE plan_id=$1 AND event_type='CREATED'`, plan.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("created events=%d err=%v", events, err)
	}
	assertLifecycleMigrationDownFailsClosed(t, ctx, repository)
}

func newLifecyclePlanIntegrationHandler(t *testing.T, repository *PostgresRepository, actor uuid.UUID) http.Handler {
	return newLifecyclePlanPermissionIntegrationHandler(t, repository, actor, []string{"platform.models.read", "platform.models.lifecycle.request"}, "lifecycle-plan-integration")
}

func newLifecyclePlanPermissionIntegrationHandler(t *testing.T, repository *PostgresRepository, actor uuid.UUID, permissions []string, session string) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor, "granted_permissions": permissions})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "lifecycle-plan-integration")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}})
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actor.String(), SessionID: session, AuthenticatedAt: time.Now().UTC()}, nil
	}
	return requestdeadline.TokenAdminMiddleware(service.Handler("", verify), time.Now)
}

func newGovernedPublishIntegrationHandler(t *testing.T, service *Service, actor uuid.UUID) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor, "granted_permissions": []string{"platform.models.manage", "platform.models.read"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "scheduled-publish-integration")
	if err != nil {
		t.Fatal(err)
	}
	service.WithAuthorizer(authorizer).WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}})
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actor.String(), SessionID: "scheduled-publish-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return requestdeadline.TokenAdminMiddleware(service.Handler("", verify), time.Now)
}

func assertLifecyclePlanHTTP(t *testing.T, handler, manageOnlyHandler, revokedHandler http.Handler, repository *PostgresRepository, modelID uuid.UUID, catalogVersion int64, now time.Time) {
	t.Helper()
	input := LifecyclePlanInput{ExpectedCatalogVersion: catalogVersion, ExpectedPlanVersion: 0, TargetLifecycle: "DEPRECATED", EffectiveAt: now.Add(15 * time.Minute), Reason: "create lifecycle plan through admin handler"}
	body, _ := json.Marshal(input)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lifecycle-handler-create-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create handler=%d %s", response.Code, response.Body.String())
	}
	var created LifecyclePlan
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || created.ModelID != modelID || created.PlanVersion != 1 {
		t.Fatalf("create handler projection=%s", response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans?limit=25&status=PENDING_APPROVAL", nil)
	request.Header.Set("Authorization", "Bearer integration")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), created.ID.String()) || strings.Contains(response.Body.String(), "critical_resource_versions") {
		t.Fatalf("list handler=%d %s", response.Code, response.Body.String())
	}
	cancelBody := []byte(`{"expected_plan_version":1,"reason":"cancel lifecycle plan through handler"}`)
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans/"+created.ID.String()+"/cancel", bytes.NewReader(cancelBody))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lifecycle-handler-revoked-permission")
	response = httptest.NewRecorder()
	revokedHandler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked lifecycle cancel=%d %s", response.Code, response.Body.String())
	}
	unchanged, err := repository.LifecyclePlan(context.Background(), modelID, created.ID)
	if err != nil || unchanged.Status != "PENDING_APPROVAL" {
		t.Fatalf("revoked cancel wrote plan=%+v err=%v", unchanged, err)
	}
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans/"+created.ID.String()+"/cancel", bytes.NewReader(cancelBody))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lifecycle-handler-wrong-permission")
	response = httptest.NewRecorder()
	manageOnlyHandler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("manage-only lifecycle cancel=%d %s", response.Code, response.Body.String())
	}
	wrongFamilyBody := response.Body.String()
	randomRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans/"+uuid.New().String()+"/cancel", bytes.NewReader(cancelBody))
	randomRequest.Header.Set("Authorization", "Bearer integration")
	randomRequest.Header.Set("Content-Type", "application/json")
	randomRequest.Header.Set("Idempotency-Key", "lifecycle-handler-random-plan")
	randomResponse := httptest.NewRecorder()
	manageOnlyHandler.ServeHTTP(randomResponse, randomRequest)
	if randomResponse.Code != response.Code || randomResponse.Body.String() != wrongFamilyBody {
		t.Fatalf("cancel existence oracle existing=%d/%s random=%d/%s", response.Code, wrongFamilyBody, randomResponse.Code, randomResponse.Body.String())
	}
	unchanged, err = repository.LifecyclePlan(context.Background(), modelID, created.ID)
	if err != nil || unchanged.Status != "PENDING_APPROVAL" {
		t.Fatalf("forbidden cancel wrote plan=%+v err=%v", unchanged, err)
	}
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans/"+created.ID.String()+"/cancel", bytes.NewReader(cancelBody))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lifecycle-handler-cancel-0001")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"CANCELLED"`) {
		t.Fatalf("cancel handler=%d %s", response.Code, response.Body.String())
	}
	invalid := append(body[:len(body)-1], []byte(`,"ignore_blocker":true}`)...)
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+modelID.String()+"/lifecycle-plans", bytes.NewReader(invalid))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lifecycle-handler-invalid-0001")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid handler=%d %s", response.Code, response.Body.String())
	}
}

func assertLifecycleCancelPermission(t *testing.T, handler http.Handler, repository *PostgresRepository, plan LifecyclePlan, want int, key string) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"expected_plan_version":%d,"reason":"cancel scheduled publication after review"}`, plan.PlanVersion))
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+plan.ModelID.String()+"/lifecycle-plans/"+plan.ID.String()+"/cancel", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("cancel target=%s status=%d want=%d body=%s", plan.TargetLifecycle, response.Code, want, response.Body.String())
	}
	if want == http.StatusForbidden {
		randomRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/models/"+plan.ModelID.String()+"/lifecycle-plans/"+uuid.New().String()+"/cancel", bytes.NewReader(body))
		randomRequest.Header.Set("Authorization", "Bearer integration")
		randomRequest.Header.Set("Content-Type", "application/json")
		randomRequest.Header.Set("Idempotency-Key", key+"-random")
		randomResponse := httptest.NewRecorder()
		handler.ServeHTTP(randomResponse, randomRequest)
		if randomResponse.Code != response.Code || randomResponse.Body.String() != response.Body.String() {
			t.Fatalf("cancel existence oracle target=%s existing=%d/%s random=%d/%s", plan.TargetLifecycle, response.Code, response.Body.String(), randomResponse.Code, randomResponse.Body.String())
		}
	}
	stored, err := repository.LifecyclePlan(context.Background(), plan.ModelID, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want == http.StatusOK && stored.Status != "CANCELLED" || want != http.StatusOK && stored.Status != plan.Status {
		t.Fatalf("cancel target=%s result=%s wantHTTP=%d", plan.TargetLifecycle, stored.Status, want)
	}
}

func scheduleModelPublishForTest(t *testing.T, ctx context.Context, service *Service, handler http.Handler, repository *PostgresRepository, modelID uuid.UUID, planVersion int64, effectiveAt time.Time, reason, key string) (LifecyclePlan, TokenCommandResult, func() (TokenCommandResult, int)) {
	t.Helper()
	effectiveAt = effectiveAt.UTC().Truncate(time.Second)
	planID := uuid.Must(uuid.NewV7())
	command := modelPublishCommand{ModelID: modelID, PublishMode: "SCHEDULED", LifecyclePlanID: &planID, LifecyclePlanVersion: &planVersion, EffectiveAt: &effectiveAt}
	commandRaw := mustMarshalJSON(t, command)
	precheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: commandRaw})
	if err != nil || len(precheck.Blockers) != 0 {
		t.Fatalf("scheduled publish precheck=%+v err=%v", precheck, err)
	}
	var governedExpected map[string]any
	if err := json.Unmarshal(precheck.ExpectedVersions, &governedExpected); err != nil {
		t.Fatal(err)
	}
	envelope := bindModelPublishRequestFingerprint(t, TokenCommandEnvelope{CommandType: "TOKEN_MODEL_PUBLISH", Reason: reason, TargetResourceType: "MODEL", TargetResourceID: modelID.String(), PrecheckFingerprint: precheck.PrecheckFingerprint, Command: commandRaw}, governedExpected)
	body, _ := json.Marshal(envelope)
	invoke := func() (TokenCommandResult, int) {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer integration")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var result TokenCommandResult
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("schedule response=%d %s", response.Code, response.Body.String())
		}
		return result, response.Code
	}
	result, status := invoke()
	if status != http.StatusCreated || result.Status != "SUCCEEDED" {
		t.Fatalf("schedule result=%+v status=%d", result, status)
	}
	replayed, status := invoke()
	if status != http.StatusOK || replayed.CommandReference != result.CommandReference {
		t.Fatalf("schedule replay=%+v status=%d", replayed, status)
	}
	plan, err := repository.LifecyclePlan(ctx, modelID, planID)
	if err != nil {
		t.Fatal(err)
	}
	return plan, result, invoke
}

func seedLifecycleModels(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, modelID, replacementID uuid.UUID, now time.Time) (int64, uuid.UUID) {
	t.Helper()
	var protocolID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'lifecycle plan integration',$2,$3,$3) RETURNING version`, uuid.New(), actor, now.Add(-time.Hour)).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	draftModelID := uuid.New()
	for index, fixture := range []struct {
		id     uuid.UUID
		status string
	}{{modelID, "PUBLISHED"}, {replacementID, "PUBLISHED"}, {draftModelID, "DRAFT"}} {
		model := fixture.id
		capability := uuid.New()
		accountID, ownerID, walletID, credentialID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		upstream := "lifecycle-model-" + string(rune('a'+index))
		fingerprint := strings.Repeat(string(rune('a'+index)), 64)
		if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'integration','v1',$2,$3,$4,$5); INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($6,$1,1,$7,4096,1024,'["TEXT"]','v1',$4,$5); INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($8,$1,$2,$2,'','{}',$13,$6,$7,true); INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($9,'USER',$10,$11,$2,'ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($12,$9,'integration','https://provider.invalid','x'::bytea,'key',$3,1,'ACTIVE',ARRAY[$2],$5,$4,$5,'VALID'); INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,health_observed_at,health_expires_at,next_probe_at) VALUES(gen_random_uuid(),$9,$2,100,'NONE','HEALTHY',$5,1,'CHAT',$1,$7,'RESOLVED',1,1,'PROBE_SUCCEEDED',$5,$5+interval '4 minutes',$5+interval '3 minutes'); INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES(gen_random_uuid(),'PERSONAL',$10,$11,$1,$2,'CHAT',$7,1,1,$3,'READY','READY',1,'{}',3,$5+interval '1 hour','worker',$5+interval '1 minute',$9,$12,$4,$5,$5)`, model, upstream, fingerprint, actor, now, capability, protocolID, catalogVersion, accountID, ownerID, walletID, credentialID, fixture.status); err != nil {
			t.Fatal(err)
		}
	}
	return catalogVersion, draftModelID
}

func insertDueLifecyclePlan(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, modelID uuid.UUID, catalogVersion int64, now time.Time) LifecyclePlan {
	return insertDueLifecyclePlanWithTransition(t, ctx, repository, actor, modelID, catalogVersion, "PUBLISHED", "DEPRECATED", 4, now)
}

func insertDueLifecyclePlanWithTransition(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, modelID uuid.UUID, catalogVersion int64, current, target string, planVersion int64, now time.Time) LifecyclePlan {
	t.Helper()
	planID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,false,'PENDING_APPROVAL','apply reviewed lifecycle plan',$8,$9,$10,$11,$11)`, planID, modelID, planVersion, catalogVersion, current, target, now.Add(-time.Minute), strings.Repeat("b", 64), strings.Repeat("c", 64), actor, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan, err := repository.LifecyclePlan(ctx, modelID, planID)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func insertDueRemovalLifecyclePlan(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, modelID, replacementID uuid.UUID, catalogVersion int64, now time.Time) LifecyclePlan {
	t.Helper()
	planID := uuid.New()
	effectiveAt := now.Add(-time.Minute)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,5,$3,'DEPRECATED','REMOVED',$4,$4,$5,false,'PENDING_APPROVAL','apply reviewed removal lifecycle plan',$6,$7,$8,$9,$9)`, planID, modelID, catalogVersion, effectiveAt, replacementID, strings.Repeat("5", 64), strings.Repeat("6", 64), actor, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan, err := repository.LifecyclePlan(ctx, modelID, planID)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func assertLifecycleExpiryAndLeaseRecovery(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, modelID uuid.UUID, catalogVersion int64, now time.Time) {
	t.Helper()
	expiredID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,1,$3,'PUBLISHED','DEPRECATED',$4,false,'PENDING_APPROVAL','expire an abandoned lifecycle plan',$5,$6,$7,$8,$8)`, expiredID, modelID, catalogVersion, now.Add(time.Hour), strings.Repeat("d", 64), strings.Repeat("e", 64), actor, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if expired, err := repository.ExpirePendingLifecyclePlan(ctx, "expiry-worker"); err != nil || !expired {
		t.Fatalf("expired=%v err=%v", expired, err)
	}
	item, err := repository.LifecyclePlan(ctx, modelID, expiredID)
	if err != nil || item.Status != "EXPIRED" {
		t.Fatalf("expired plan=%+v err=%v", item, err)
	}

	leaseID := uuid.New()
	approvalID := uuid.New()
	critical := mustMarshalJSON(t, lifecyclePlanCritical{ModelID: modelID, CatalogVersion: catalogVersion, CurrentLifecycle: "PUBLISHED", TargetLifecycle: "DEPRECATED", LifecyclePlanVersion: 2, EffectiveAt: now.Add(-time.Minute), BillingPriceVersion: 9, RouteSetFingerprint: strings.Repeat("1", 64), ProvisioningSetFingerprint: strings.Repeat("2", 64), InflightRequestSetFingerprint: strings.Repeat("3", 64), PolicyVersion: 2})
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,force_cancel_inflight,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at) VALUES($1,$2,2,$3,'PUBLISHED','DEPRECATED',$4,false,'PENDING_APPROVAL','recover expired execution lease',$5,$6,$7,$8,$8)`, leaseID, modelID, catalogVersion, now.Add(-time.Minute), strings.Repeat("f", 64), strings.Repeat("0", 64), actor, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot='{}'::jsonb,updated_at=$5 WHERE id=$1`, leaseID, approvalID, strings.Repeat("4", 64), critical, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='EXECUTING',execution_lease_owner='dead-worker',execution_lease_until=$2,execution_attempt=1 WHERE id=$1`, leaseID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := repository.ClaimDueLifecyclePlan(ctx, "recovery-worker")
	if err != nil || !ok || claimed.ID != leaseID || claimed.ExecutionAttempt != 2 {
		t.Fatalf("reclaimed=%+v ok=%v err=%v", claimed, ok, err)
	}
}

func assertLifecycleMigrationDownFailsClosed(t *testing.T, ctx context.Context, repository *PostgresRepository) {
	t.Helper()
	var schema string
	if err := repository.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	down, err := loadTokenIntegrationMigrations("../../migrations/token", schema)
	if err != nil {
		t.Fatal(err)
	}
	var migrationSQL string
	for _, migration := range down {
		if migration.Version == 27 {
			migrationSQL = migration.DownSQL
		}
	}
	if migrationSQL == "" {
		t.Fatal("migration 27 down not loaded")
	}
	if _, err := repository.pool.Exec(ctx, migrationSQL); err == nil {
		t.Fatal("migration 27 down removed immutable lifecycle history")
	}
}
