package token

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Public migrations and runtime identity are real. The usage-state matrix is
// controlled query data, not an execution of financial state transitions.
func TestGovernanceUsageResourcePrechecksPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	target, _ := f.preparedExpiry(t, "hold")
	background, _ := f.preparedExpiry(t, "hold")
	for _, usage := range []UsageRequest{target, background} {
		if _, err := f.r.pool.Exec(f.ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, usage.CatalogVersion, usage.ModelID, usage.ModelID.String(), usage.CapabilityVersionID, usage.ProtocolDefinitionID); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []uuid.UUID{target.TokenAccountID, background.TokenAccountID} {
		if _, err := f.r.pool.Exec(f.ctx, `INSERT INTO routing_policies(id,token_account_id,model,model_id,protocol_code,protocol_version_id,route_generation,reference_status,priority,default_scope,health_status,health_observed_at,health_expires_at,updated_at) VALUES($1,$2,$3,$4,'CHAT',$5,1,'RESOLVED',100,'NONE','HEALTHY',now(),now()+interval '3 minutes',now())`, uuid.New(), account, target.Model, target.ModelID, target.ProtocolDefinitionID); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 3; index++ {
		if _, err := f.r.pool.Exec(f.ctx, `INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at)
VALUES($1,'PERSONAL',$2,$3,$4,$5,'CHAT',$6,1,1,$7,'READY','READY',1,'{}',3,now()+interval '1 hour',CASE WHEN $8=0 THEN now()+interval '1 hour' ELSE NULL END,$9,$10,$11,now(),now())`, uuid.New(), uuid.New(), target.WalletID, target.ModelID, target.Model, target.ProtocolDefinitionID, strings.Repeat("a", 64), index, target.TokenAccountID, target.CredentialID, target.UserID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.r.pool.Exec(f.ctx, `INSERT INTO usage_requests
SELECT copied.* FROM generate_series(1,114) n(x)
JOIN usage_requests original ON original.id=$1
CROSS JOIN LATERAL jsonb_populate_record(NULL::usage_requests,to_jsonb(original)||jsonb_build_object(
 'id',gen_random_uuid(),'session_id',CASE WHEN x<=14 THEN 'shared-active-session' ELSE 'historical-session' END,
 'idempotency_key_digest',encode(digest('governance-index-'||x::text,'sha256'),'hex'),
 'status',CASE WHEN x<=14 THEN 'RESERVED' ELSE 'COMMITTED' END,
 'execution_status',CASE WHEN x<=14 THEN (ARRAY['RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'])[1+(x-1)%7] ELSE 'SUCCEEDED' END
)) copied`, target.ID); err != nil {
		t.Fatal(err)
	}
	assertSnapshot := func(t *testing.T, requests, sessions int64) ImpactLocalEvidence {
		t.Helper()
		var before, after string
		const snapshot = `SELECT md5(jsonb_agg(to_jsonb(request) ORDER BY id)::text) FROM usage_requests request`
		if err := f.r.pool.QueryRow(f.ctx, snapshot).Scan(&before); err != nil {
			t.Fatal(err)
		}
		evidence, err := f.r.CatalogRollbackImpact(f.ctx, *target.CatalogVersion)
		if err != nil {
			t.Fatal("registered impact repository path failed", err)
		}
		old := governanceHistoricalJoinCounts(t, f.ctx, f.r, evidence.ModelIDs)
		current := evidence.Dependencies
		current.ReadyProvisionings = 0 // Calculated by the unchanged serving-coverage query.
		if old != current || current.Routes != 2 || current.HealthyRoutes != 2 || current.Provisionings != 3 || current.ActiveLeases != 1 || current.InflightRequests != requests || current.ActiveSessions != sessions {
			t.Fatal("complete dependency facts changed", old, current)
		}
		tx, err := f.r.pool.BeginTx(f.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(f.ctx)
		fingerprint, err := impactRowsFingerprint(f.ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE model_id=ANY($1::uuid[]) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, evidence.ModelIDs)
		if err != nil || fingerprint != evidence.InflightSetFingerprint {
			t.Fatal("complete inflight fingerprint changed", err)
		}
		if err := f.r.pool.QueryRow(f.ctx, snapshot).Scan(&after); err != nil || before != after {
			t.Fatal("impact read changed persisted usage facts", err)
		}
		return evidence
	}
	var active ImpactLocalEvidence
	t.Run("all-active-states-preserve-full-facts", func(t *testing.T) {
		active = assertSnapshot(t, 16, 2)
	})
	t.Run("repeated-read-stable-and-read-only", func(t *testing.T) {
		replayed := assertSnapshot(t, 16, 2)
		if replayed.CurrentVersion.EvidenceETag != active.CurrentVersion.EvidenceETag || replayed.CurrentVersion.EvidenceSetSHA256 != active.CurrentVersion.EvidenceSetSHA256 {
			t.Fatal("same facts changed governance fingerprints")
		}
	})
	t.Run("historical-only-preserves-other-dependencies", func(t *testing.T) {
		if _, err := f.r.pool.Exec(f.ctx, `UPDATE usage_requests SET execution_status='SUCCEEDED'`); err != nil {
			t.Fatal(err)
		}
		historical := assertSnapshot(t, 0, 0)
		if historical.InflightSetFingerprint == active.InflightSetFingerprint {
			t.Fatal("changed active set did not change its full fingerprint")
		}
	})
}

// The predecessor's full historical join is an independent equivalence control.
func governanceHistoricalJoinCounts(t *testing.T, ctx context.Context, repository *PostgresRepository, ids []uuid.UUID) ImpactDependencySummary {
	t.Helper()
	var counts ImpactDependencySummary
	err := repository.pool.QueryRow(ctx, `SELECT
 count(DISTINCT route.id),count(DISTINCT route.id) FILTER (WHERE route.reference_status='RESOLVED' AND route.health_status='HEALTHY' AND route.health_expires_at>transaction_timestamp()),
 count(DISTINCT provision.id),0,count(DISTINCT provision.id) FILTER (WHERE provision.lease_until>transaction_timestamp()),
 count(DISTINCT request.id) FILTER (WHERE request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
 count(DISTINCT request.session_id) FILTER (WHERE request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'))
 FROM model_identities identity
 LEFT JOIN routing_policies route ON route.model_id=identity.id OR route.model IN (identity.id::text,identity.upstream_model_ref)
 LEFT JOIN route_provisionings provision ON provision.model_id=identity.id
 LEFT JOIN usage_requests request ON request.model_id=identity.id
 WHERE identity.id=ANY($1::uuid[])`, ids).Scan(&counts.Routes, &counts.HealthyRoutes, &counts.Provisionings, &counts.ReadyProvisionings, &counts.ActiveLeases, &counts.InflightRequests, &counts.ActiveSessions)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}
