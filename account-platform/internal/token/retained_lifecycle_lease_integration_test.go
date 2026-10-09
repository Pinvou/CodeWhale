package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

// Actual public migration/repository fixture. Approval freezing is controlled
// owner-domain persistence, not a real Account approval delivery. Only the
// stored lease is shortened to make the existing expiry invariant observable.
func TestRetainedLifecycleLeaseWait(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor, model, capability := uuid.New(), uuid.New(), uuid.New()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	ref := "lease-" + model.String()
	sum := sha256.Sum256([]byte(ref))
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1',$2,$3,$4,now())`, model, ref, hex.EncodeToString(sum[:]), actor); e != nil {
		t.Fatal(e)
	}
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, model, protocol, actor); e != nil {
		t.Fatal(e)
	}
	var version int64
	if e := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled lifecycle lease',$2,now(),now()) RETURNING version`, uuid.New(), actor).Scan(&version); e != nil {
		t.Fatal(e)
	}
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, version, model, ref, capability, protocol); e != nil {
		t.Fatal(e)
	}
	effective := time.Now().UTC().Add(6 * time.Minute).Truncate(time.Microsecond)
	plan, _, e := r.CreateLifecyclePlan(ctx, model, LifecyclePlanInput{ExpectedCatalogVersion: version, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, Reason: "controlled reviewed lifecycle lease"}, actor, uuid.NewString())
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.ModelLifecycleImpact(ctx, model, nil)
	if e != nil {
		t.Fatal(e)
	}
	critical, _ := json.Marshal(lifecyclePlanCritical{ModelID: model, CatalogVersion: version, CurrentLifecycle: "PUBLISHED", TargetLifecycle: "DEPRECATED", LifecyclePlanVersion: plan.PlanVersion, EffectiveAt: effective, PolicyVersion: 2, RouteSetFingerprint: evidence.RouteSetFingerprint, ProvisioningSetFingerprint: evidence.ProvisioningSetFingerprint, InflightRequestSetFingerprint: evidence.InflightSetFingerprint})
	if _, e = r.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot='{}' WHERE id=$1`, plan.ID, uuid.New(), hex.EncodeToString(sum[:]), critical); e != nil {
		t.Fatal(e)
	}
	claimed, ok, e := r.claimDueLifecyclePlan(ctx, "controlled-worker", &effective)
	if e != nil || !ok || claimed.ID != plan.ID {
		t.Fatal("actual claim failed", e)
	}
	var deadline time.Time
	if e = r.pool.QueryRow(ctx, `UPDATE model_lifecycle_plans SET execution_lease_until=clock_timestamp()+interval '600 milliseconds' WHERE id=$1 RETURNING execution_lease_until`, plan.ID).Scan(&deadline); e != nil {
		t.Fatal(e)
	}
	blocker, e := r.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(context.Background())
	if e = lockCatalogPublication(ctx, blocker); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- r.ApplyClaimedLifecyclePlan(ctx, plan.ID, "controlled-worker", nil) }()
	waitDeadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		var txstart *time.Time
		e = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%model-catalog-publication%'),(SELECT min(xact_start) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%model-catalog-publication%')`).Scan(&waiting, &txstart)
		if e != nil {
			t.Fatal(e)
		}
		if waiting {
			if txstart == nil || !txstart.Before(deadline) {
				t.Fatal("fixture transaction did not start before expiry")
			}
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("actual advisory wait not observed")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		var now time.Time
		if e = r.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); e != nil {
			t.Fatal(e)
		}
		if now.After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("actual apply did not return")
	}
	var status string
	var current int64
	if q := r.pool.QueryRow(ctx, `SELECT status,(SELECT max(version) FROM model_catalog_versions) FROM model_lifecycle_plans WHERE id=$1`, plan.ID).Scan(&status, &current); q != nil {
		t.Fatal(q)
	}
	if !errors.Is(e, ErrCatalogVersionConflict) || status != "EXECUTING" || current != version {
		t.Fatalf("EXPIRED_LEASE_APPLY_ACCEPTED rejected=%v status=%s catalog_changed=%v", e != nil, status, current != version)
	}
	_ = pgx.ErrNoRows
}

func TestRetainedLifecycleLiveLeaseApplies(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor, model, capability := uuid.New(), uuid.New(), uuid.New()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	ref := "lease-" + model.String()
	sum := sha256.Sum256([]byte(ref))
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1',$2,$3,$4,now())`, model, ref, hex.EncodeToString(sum[:]), actor); e != nil {
		t.Fatal(e)
	}
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, model, protocol, actor); e != nil {
		t.Fatal(e)
	}
	var version int64
	if e := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled lifecycle lease',$2,now(),now()) RETURNING version`, uuid.New(), actor).Scan(&version); e != nil {
		t.Fatal(e)
	}
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, version, model, ref, capability, protocol); e != nil {
		t.Fatal(e)
	}
	effective := time.Now().UTC().Add(6 * time.Minute).Truncate(time.Microsecond)
	plan, _, e := r.CreateLifecyclePlan(ctx, model, LifecyclePlanInput{ExpectedCatalogVersion: version, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, Reason: "controlled reviewed lifecycle lease"}, actor, uuid.NewString())
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.ModelLifecycleImpact(ctx, model, nil)
	if e != nil {
		t.Fatal(e)
	}
	critical, _ := json.Marshal(lifecyclePlanCritical{ModelID: model, CatalogVersion: version, CurrentLifecycle: "PUBLISHED", TargetLifecycle: "DEPRECATED", LifecyclePlanVersion: plan.PlanVersion, EffectiveAt: effective, PolicyVersion: 2, RouteSetFingerprint: evidence.RouteSetFingerprint, ProvisioningSetFingerprint: evidence.ProvisioningSetFingerprint, InflightRequestSetFingerprint: evidence.InflightSetFingerprint})
	if _, e = r.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot='{}' WHERE id=$1`, plan.ID, uuid.New(), hex.EncodeToString(sum[:]), critical); e != nil {
		t.Fatal(e)
	}
	claimed, ok, e := r.claimDueLifecyclePlan(ctx, "controlled-worker", &effective)
	if e != nil || !ok || claimed.ID != plan.ID {
		t.Fatal("actual claim failed", e)
	}
	if e = r.ApplyClaimedLifecyclePlan(ctx, plan.ID, "controlled-worker", nil); e != nil {
		t.Fatal(e)
	}
	var status string
	var current int64
	if e = r.pool.QueryRow(ctx, `SELECT status,(SELECT max(version) FROM model_catalog_versions) FROM model_lifecycle_plans WHERE id=$1`, plan.ID).Scan(&status, &current); e != nil {
		t.Fatal(e)
	}
	if status != "APPLIED" || current <= version {
		t.Fatal("live lease did not apply actual catalog transition")
	}
}
