package token

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenOperationsOverviewPerformancePostgresIntegration is the no-SKIP CI
// production-volume gate for APT-136 and APT-139. CI constrains PostgreSQL 17
// to 2 vCPU / 4 GiB. Each measure process runs exactly one cold production SQL
// after a database restart; the workflow repeats the restart for every target.
func TestTokenOperationsOverviewPerformancePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_DATABASE_URL is not configured")
	}
	phase := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE"))
	if phase != "seed" && phase != "measure" {
		t.Fatal("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE must be seed or measure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	repository := newTokenOverviewPerformanceRepository(t, ctx, databaseURL, phase == "seed")
	connection, err := repository.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET work_mem='16MB'; SET max_parallel_workers_per_gather=0`); err != nil {
		t.Fatal(err)
	}
	if phase == "measure" {
		var serverVersion int
		var sharedBuffers, workMem string
		if err := connection.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,current_setting('shared_buffers'),current_setting('work_mem')`).Scan(&serverVersion, &sharedBuffers, &workMem); err != nil || serverVersion/10000 != 17 || sharedBuffers != "512MB" || workMem != "16MB" {
			t.Fatalf("performance environment drift: version=%d shared_buffers=%s work_mem=%s err=%v", serverVersion, sharedBuffers, workMem, err)
		}
		measureTokenOverviewTarget(t, ctx, connection, strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_TARGET")))
		return
	}
	if _, err := connection.Exec(ctx, `ALTER SYSTEM SET shared_buffers='512MB'`); err != nil {
		t.Fatal(err)
	}
	accountID, credentialID, ownerID, walletID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($1,'USER',$2,$3,'Overview performance','ACTIVE',$4,transaction_timestamp(),transaction_timestamp(),'PLATFORM')`, accountID, ownerID, walletID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($1,$2,'provider','https://provider.invalid','x'::bytea,'key','fingerprint',1,'ACTIVE',ARRAY['overview-model'],transaction_timestamp(),$3,transaction_timestamp(),'VALID')`, credentialID, accountID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO usage_requests(
 id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,
 request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint,reconciliation_stopped_at)
SELECT ('10000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,$1,'overview-'||i,$2,$3,$4,'overview-model',
 CASE WHEN i<=850000 THEN 'COMMITTED' WHEN i<=950000 THEN 'SETTLEMENT_PENDING' ELSE 'REVIEW' END,
 1,'{}',
 CASE WHEN i<=700000 THEN transaction_timestamp()-(i%2505600)*interval '1 second'
      ELSE transaction_timestamp()-interval '31 days'-(i%12873600)*interval '1 second' END,
 transaction_timestamp(),md5('idem-'||i)||md5('idem2-'||i),md5('req-'||i)||md5('req2-'||i),
 CASE WHEN i>950000 THEN transaction_timestamp() ELSE NULL END
FROM generate_series(1,1000000) AS series(i)`, ownerID, accountID, credentialID, walletID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO manual_review_cases(
 id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
SELECT ('20000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
 'TOKEN',
 CASE WHEN i>60000 AND i<=80000 THEN 'DECOY_REVIEW' ELSE 'USAGE_RECONCILIATION_REVIEW' END,
 CASE WHEN i>80000 THEN 'PAYMENT_ORDER' ELSE 'USAGE_REQUEST' END,
 ('10000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,'P2','OPEN',
 CASE WHEN i<=42000 OR i>60000 AND i<=88000 THEN transaction_timestamp()-(i%2505600)*interval '1 second'
      ELSE transaction_timestamp()-interval '31 days'-(i%12873600)*interval '1 second' END,
 transaction_timestamp(),transaction_timestamp(),transaction_timestamp(),transaction_timestamp(),transaction_timestamp()
FROM generate_series(1,100000) AS series(i)`); err != nil {
		t.Fatal(err)
	}
	seedModelTokenPageOverviewPerformance(t, ctx, connection, actorID)
	assertTokenOverviewPerformanceSeed(t, ctx, connection)
	// VACUUM cannot share the implicit transaction created by a multi-statement
	// simple query, so keep each frozen cache/statistics preparation step alone.
	for _, relation := range []string{"usage_requests", "manual_review_cases", "model_catalog_versions", "model_catalog_entries", "model_catalog_drafts", "route_provisionings", "routing_policies"} {
		if relation == "routing_policies" {
			// The production TTL is intentionally capped at five minutes. Refresh
			// the deterministic current/expired halves immediately before their
			// final VACUUM so the first post-restart route plan observes the
			// frozen boundary without weakening the domain constraint.
			for _, refresh := range []string{
				`UPDATE routing_policies SET health_observed_at=transaction_timestamp(),health_expires_at=transaction_timestamp()+interval '5 minutes' WHERE health_diagnostic_code='OVERVIEW_CURRENT'`,
				`UPDATE routing_policies SET health_observed_at=transaction_timestamp()-interval '5 minutes',health_expires_at=transaction_timestamp()-interval '1 minute' WHERE health_diagnostic_code='OVERVIEW_EXPIRED'`,
			} {
				if _, err := connection.Exec(ctx, refresh); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := connection.Exec(ctx, `VACUUM (ANALYZE) `+pgx.Identifier{relation}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(ctx, `CHECKPOINT`); err != nil {
		t.Fatal(err)
	}
}

// seedModelTokenPageOverviewPerformance adds the exact page-overview scale to
// the existing operations fixture. UUID prefixes keep every generated family
// disjoint while making the seed reproducible and cheap to audit.
func seedModelTokenPageOverviewPerformance(t *testing.T, ctx context.Context, connection *pgxpool.Conn, actorID uuid.UUID) {
	t.Helper()
	const protocolID = "00000000-0000-7000-8200-000000000000"
	statements := []string{
		`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
SELECT ('30000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       'overview-performance-provider','adapter-v1','overview-model-'||i,
       md5('overview-model-'||i)||md5('overview-model-second-'||i),$1,transaction_timestamp()
  FROM generate_series(1,50000) AS series(i) WHERE $2::uuid IS NOT NULL`,
		`INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
SELECT ('31000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       ('30000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,1,$2::uuid,
       128000,4096,'["TEXT"]'::jsonb,'v1',$1,transaction_timestamp()
  FROM generate_series(1,50000) AS series(i)`,
		`INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at)
SELECT ('32000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       'overview-performance-'||i,$1,transaction_timestamp(),transaction_timestamp()
  FROM generate_series(1,100) AS series(i) WHERE $2::uuid IS NOT NULL ORDER BY i`,
		`INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
SELECT version,
       ('30000000-0000-4000-8000-'||lpad(to_hex(model_number),12,'0'))::uuid,
       'Overview Model '||model_number,'overview model '||model_number,
       CASE WHEN version=(SELECT max(version) FROM model_catalog_versions)
             THEN CASE WHEN model_number<=7000 THEN 'PUBLISHED' WHEN model_number<=9000 THEN 'DEPRECATED' ELSE 'REMOVED' END
            ELSE 'PUBLISHED' END,
       ('31000000-0000-4000-8000-'||lpad(to_hex(model_number),12,'0'))::uuid,$2::uuid,true
  FROM model_catalog_versions
 CROSS JOIN generate_series(1,10000) AS models(model_number)`,
		`WITH head AS (SELECT max(version) AS version FROM model_catalog_versions)
INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at)
SELECT ('30000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       CASE WHEN i<=30000 THEN head.version WHEN i<=45000 THEN head.version-1 ELSE 1 END,
       CASE WHEN i<=30000 THEN 1 WHEN i<=45000 THEN 2 ELSE 3 END,
       'Overview Draft '||i,'CHAT',1,128000,4096,'["TEXT"]'::jsonb,'v1',$1,transaction_timestamp()
  FROM head CROSS JOIN generate_series(1,50000) AS series(i) WHERE $2::uuid IS NOT NULL`,
		`INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
SELECT ('33000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,'USER',
       ('33100000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       ('33200000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       'Overview route account '||i,'ACTIVE',$1,transaction_timestamp(),transaction_timestamp(),'PLATFORM'
  FROM generate_series(1,100) AS series(i) WHERE $2::uuid IS NOT NULL`,
		`INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,requested_by,created_at,updated_at)
SELECT ('35000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,'PERSONAL',
       ('35100000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       ('35200000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       ('30000000-0000-4000-8000-'||lpad(to_hex(((i-1)%8000)+1),12,'0'))::uuid,
       'overview-model-'||(((i-1)%8000)+1),'CHAT',$2::uuid,1,
       md5('overview-provisioning-'||i)||md5('overview-provisioning-second-'||i),
       CASE WHEN i<=50000 THEN 'PENDING' WHEN i<=75000 THEN 'FAILED' ELSE 'REPAIR_REQUIRED' END,
       'VERIFY',1,'{}'::jsonb,3,transaction_timestamp()+interval '1 hour',$1,transaction_timestamp(),transaction_timestamp()
  FROM generate_series(1,100000) AS series(i)`,
		`WITH clock AS MATERIALIZED (SELECT transaction_timestamp() AS as_of)
INSERT INTO routing_policies(id,token_account_id,model,model_id,protocol_version_id,protocol_code,priority,default_scope,reference_status,health_status,health_observed_at,health_expires_at,health_diagnostic_code,probe_lease_owner,probe_lease_until,route_version,route_generation,updated_at)
SELECT ('34000000-0000-4000-8000-'||lpad(to_hex(i),12,'0'))::uuid,
       ('33000000-0000-4000-8000-'||lpad(to_hex(CASE WHEN i<=800000 THEN ((i-1)/8000)+1 ELSE ((i-800001)/2000)+1 END),12,'0'))::uuid,
       'overview-route-'||i,
       CASE WHEN i<=800000 THEN ('30000000-0000-4000-8000-'||lpad(to_hex(((i-1)%8000)+1),12,'0'))::uuid
            WHEN i>950000 THEN ('30000000-0000-4000-8000-'||lpad(to_hex(((i-950001)%500)+1),12,'0'))::uuid END,
       CASE WHEN i<=800000 OR i>950000 THEN $2::uuid END,
       'CHAT',100,'NONE',
       CASE WHEN i<=800000 THEN 'RESOLVED' WHEN i<=950000 THEN 'LEGACY_UNRESOLVED' ELSE 'DELETED' END,
       CASE WHEN i<=480000 THEN 'HEALTHY' WHEN i<=560000 THEN 'UNKNOWN' WHEN i<=640000 THEN 'PROBING'
            WHEN i<=720000 THEN 'DEGRADED' WHEN i<=760000 THEN 'UNHEALTHY' ELSE 'STALE' END,
	       CASE WHEN i<=400000 THEN clock.as_of WHEN i<=480000 THEN clock.as_of-interval '5 minutes' ELSE NULL END,
	       CASE WHEN i<=400000 THEN clock.as_of+interval '5 minutes' WHEN i<=480000 THEN clock.as_of-interval '1 minute' ELSE NULL END,
	       CASE WHEN i<=400000 THEN 'OVERVIEW_CURRENT' WHEN i<=480000 THEN 'OVERVIEW_EXPIRED' END,
	       CASE WHEN i>560000 AND i<=640000 THEN 'overview-performance-worker' END,
       CASE WHEN i>560000 AND i<=640000 THEN clock.as_of+interval '1 minute' END,
       CASE WHEN i<=500000 THEN 1 WHEN i<=800000 THEN 2 ELSE 3 END,
       1,clock.as_of
  FROM clock CROSS JOIN generate_series(1,1000000) AS series(i)`,
	}
	for index, statement := range statements {
		if _, err := connection.Exec(ctx, statement, actorID, protocolID); err != nil {
			t.Fatalf("page-overview seed statement %d: %v", index+1, err)
		}
	}
}

// assertTokenOverviewPerformanceSeed freezes the reviewed scale and mutually
// exclusive legal decoy distribution before VACUUM/restart. It must never run
// in the measure process: the first post-restart fact-table access is the cold
// production EXPLAIN below.
func assertTokenOverviewPerformanceSeed(t *testing.T, ctx context.Context, connection *pgxpool.Conn) {
	t.Helper()
	var usageCount, usageWindow, usageHistorical, committed, pending, review int64
	var reviewCount, relevantReview, decoyReview, reviewWindow, otherDomain, otherCase, otherResource, overlappingDecoy int64
	if err := connection.QueryRow(ctx, `WITH clock AS (SELECT transaction_timestamp() AS now)
SELECT
 (SELECT count(*) FROM usage_requests),
 (SELECT count(*) FROM usage_requests,clock WHERE started_at>=clock.now-interval '30 days'),
 (SELECT count(*) FROM usage_requests,clock WHERE started_at<clock.now-interval '30 days'),
 (SELECT count(*) FROM usage_requests WHERE status='COMMITTED'),
 (SELECT count(*) FROM usage_requests WHERE status='SETTLEMENT_PENDING'),
 (SELECT count(*) FROM usage_requests WHERE status='REVIEW'),
 (SELECT count(*) FROM manual_review_cases),
 (SELECT count(*) FROM manual_review_cases WHERE domain='TOKEN' AND case_type='USAGE_RECONCILIATION_REVIEW' AND resource_type='USAGE_REQUEST'),
 (SELECT count(*) FROM manual_review_cases WHERE NOT(case_type='USAGE_RECONCILIATION_REVIEW' AND resource_type='USAGE_REQUEST')),
 (SELECT count(*) FROM manual_review_cases,clock WHERE domain='TOKEN' AND case_type='USAGE_RECONCILIATION_REVIEW' AND resource_type='USAGE_REQUEST' AND created_at>=clock.now-interval '30 days'),
 (SELECT count(*) FROM manual_review_cases WHERE domain<>'TOKEN'),
 (SELECT count(*) FROM manual_review_cases WHERE case_type<>'USAGE_RECONCILIATION_REVIEW' AND resource_type='USAGE_REQUEST'),
 (SELECT count(*) FROM manual_review_cases WHERE case_type='USAGE_RECONCILIATION_REVIEW' AND resource_type<>'USAGE_REQUEST'),
 (SELECT count(*) FROM manual_review_cases WHERE case_type<>'USAGE_RECONCILIATION_REVIEW' AND resource_type<>'USAGE_REQUEST')
FROM clock`).Scan(&usageCount, &usageWindow, &usageHistorical, &committed, &pending, &review, &reviewCount, &relevantReview, &decoyReview, &reviewWindow, &otherDomain, &otherCase, &otherResource, &overlappingDecoy); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1000000 || usageWindow != 700000 || usageHistorical != 300000 || committed != 850000 || pending != 100000 || review != 50000 || reviewCount != 100000 || relevantReview != 60000 || decoyReview != 40000 || reviewWindow != 42000 || otherDomain != 0 || otherCase != 20000 || otherResource != 20000 || overlappingDecoy != 0 {
		t.Fatalf("frozen performance distribution drift: usage=%d window=%d historical=%d status=%d/%d/%d review=%d relevant=%d decoy=%d window=%d dimensions=%d/%d/%d overlap=%d", usageCount, usageWindow, usageHistorical, committed, pending, review, reviewCount, relevantReview, decoyReview, reviewWindow, otherDomain, otherCase, otherResource, overlappingDecoy)
	}
	var catalogVersions, catalogHead, catalogEntries, headEntries, headPublished, headDeprecated, headRemoved int64
	if err := connection.QueryRow(ctx, `WITH head AS (SELECT max(version) version FROM model_catalog_versions)
SELECT (SELECT count(*) FROM model_catalog_versions),head.version,
       (SELECT count(*) FROM model_catalog_entries),
       (SELECT count(*) FROM model_catalog_entries WHERE catalog_version=head.version),
       (SELECT count(*) FROM model_catalog_entries WHERE catalog_version=head.version AND lifecycle_status='PUBLISHED'),
       (SELECT count(*) FROM model_catalog_entries WHERE catalog_version=head.version AND lifecycle_status='DEPRECATED'),
       (SELECT count(*) FROM model_catalog_entries WHERE catalog_version=head.version AND lifecycle_status='REMOVED')
  FROM head`).Scan(&catalogVersions, &catalogHead, &catalogEntries, &headEntries, &headPublished, &headDeprecated, &headRemoved); err != nil {
		t.Fatal(err)
	}
	if catalogVersions != 100 || catalogHead != 100 || catalogEntries != 1000000 || headEntries != 10000 || headPublished != 7000 || headDeprecated != 2000 || headRemoved != 1000 {
		t.Fatalf("frozen catalog distribution drift: versions=%d head=%d entries=%d head_entries=%d statuses=%d/%d/%d", catalogVersions, catalogHead, catalogEntries, headEntries, headPublished, headDeprecated, headRemoved)
	}
	var drafts, draftV1, draftV2, draftV3, baseHead, basePrevious, baseEarlier int64
	if err := connection.QueryRow(ctx, `WITH head AS (SELECT max(version) version FROM model_catalog_versions)
SELECT count(*),
       count(*) FILTER (WHERE draft_version=1),count(*) FILTER (WHERE draft_version=2),count(*) FILTER (WHERE draft_version=3),
       count(*) FILTER (WHERE base_catalog_version=head.version),
       count(*) FILTER (WHERE base_catalog_version=head.version-1),
       count(*) FILTER (WHERE base_catalog_version<head.version-1)
  FROM model_catalog_drafts CROSS JOIN head GROUP BY head.version`).Scan(&drafts, &draftV1, &draftV2, &draftV3, &baseHead, &basePrevious, &baseEarlier); err != nil {
		t.Fatal(err)
	}
	if drafts != 50000 || draftV1 != 30000 || draftV2 != 15000 || draftV3 != 5000 || baseHead != 30000 || basePrevious != 15000 || baseEarlier != 5000 {
		t.Fatalf("frozen draft distribution drift: total=%d versions=%d/%d/%d bases=%d/%d/%d", drafts, draftV1, draftV2, draftV3, baseHead, basePrevious, baseEarlier)
	}
	var routes, resolved, legacy, deleted, healthyCurrent, healthyExpired, unknown, probing, degraded, unhealthy, stale, routeV1, routeV2, routeV3 int64
	if err := connection.QueryRow(ctx, `WITH clock AS (SELECT transaction_timestamp() as_of)
SELECT count(*),
       count(*) FILTER (WHERE reference_status='RESOLVED'),count(*) FILTER (WHERE reference_status='LEGACY_UNRESOLVED'),count(*) FILTER (WHERE reference_status='DELETED'),
       count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at>clock.as_of),
       count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at<=clock.as_of),
       count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='UNKNOWN'),count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='PROBING'),
       count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='DEGRADED'),count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='UNHEALTHY'),
       count(*) FILTER (WHERE reference_status='RESOLVED' AND health_status='STALE'),
       count(*) FILTER (WHERE route_version=1),count(*) FILTER (WHERE route_version=2),count(*) FILTER (WHERE route_version=3)
  FROM routing_policies CROSS JOIN clock GROUP BY clock.as_of`).Scan(&routes, &resolved, &legacy, &deleted, &healthyCurrent, &healthyExpired, &unknown, &probing, &degraded, &unhealthy, &stale, &routeV1, &routeV2, &routeV3); err != nil {
		t.Fatal(err)
	}
	if routes != 1000000 || resolved != 800000 || legacy != 150000 || deleted != 50000 || healthyCurrent != 400000 || healthyExpired != 80000 || unknown != 80000 || probing != 80000 || degraded != 80000 || unhealthy != 40000 || stale != 40000 || routeV1 != 500000 || routeV2 != 300000 || routeV3 != 200000 {
		t.Fatalf("frozen route distribution drift: total=%d references=%d/%d/%d health=%d/%d/%d/%d/%d/%d/%d versions=%d/%d/%d", routes, resolved, legacy, deleted, healthyCurrent, healthyExpired, unknown, probing, degraded, unhealthy, stale, routeV1, routeV2, routeV3)
	}
	var provisionings, provisioningPending, provisioningFailed, provisioningRepair int64
	if err := connection.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='PENDING'),count(*) FILTER (WHERE status='FAILED'),count(*) FILTER (WHERE status='REPAIR_REQUIRED') FROM route_provisionings`).Scan(&provisionings, &provisioningPending, &provisioningFailed, &provisioningRepair); err != nil {
		t.Fatal(err)
	}
	if provisionings != 100000 || provisioningPending != 50000 || provisioningFailed != 25000 || provisioningRepair != 25000 {
		t.Fatalf("frozen provisioning distribution drift: total=%d statuses=%d/%d/%d", provisionings, provisioningPending, provisioningFailed, provisioningRepair)
	}
}

func measureTokenOverviewTarget(t *testing.T, ctx context.Context, connection *pgxpool.Conn, target string) {
	t.Helper()
	switch target {
	case "legacy":
		measureTokenOverviewPlans(t, ctx, connection)
	case "model-catalog":
		measureProductionOverviewPlans(t, ctx, connection, target, adminModelCatalogOverviewSQL, nil, func(plan map[string]any) {
			assertPlanScan(t, plan, "model_catalog_entries", "model_catalog_entries_status_idx", false)
			assertPlanScan(t, plan, "model_catalog_drafts", "", true)
			assertPlanScan(t, plan, "model_catalog_versions", "", false)
		})
	case "token-resources":
		measureProductionOverviewPlans(t, ctx, connection, target, adminTokenResourcesOverviewSQL, nil, func(plan map[string]any) {
			assertPlanScan(t, plan, "routing_policies", "routing_policies_overview_health_idx", false)
		})
	case "model-operations-token-only":
		measureProductionOverviewPlans(t, ctx, connection, target, adminModelOperationsTokenOnlySQL, nil, func(plan map[string]any) {
			assertPlanScan(t, plan, "route_provisionings", "", true)
			assertPlanRelationsAbsent(t, plan, "usage_requests", "manual_review_cases")
		})
	case "model-operations-usage-only":
		measureProductionOverviewPlans(t, ctx, connection, target, adminModelOperationsUsageOnlySQL, []any{int64((30 * 24 * time.Hour) / time.Second)}, func(plan map[string]any) {
			assertOperationsFactPlan(t, plan)
			assertPlanRelationsAbsent(t, plan, "route_provisionings")
		})
	case "model-operations-both":
		measureProductionOverviewPlans(t, ctx, connection, target, adminModelOperationsBothSQL, []any{int64((30 * 24 * time.Hour) / time.Second)}, func(plan map[string]any) {
			assertOperationsFactPlan(t, plan)
			assertPlanScan(t, plan, "route_provisionings", "", true)
		})
	default:
		t.Fatalf("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_TARGET must name one frozen production query, got %q", target)
	}
}

func measureProductionOverviewPlans(t *testing.T, ctx context.Context, connection *pgxpool.Conn, target, query string, arguments []any, check func(map[string]any)) {
	t.Helper()
	for run := 0; run < 4; run++ {
		var encoded []byte
		if err := connection.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, arguments...).Scan(&encoded); err != nil {
			t.Fatalf("%s run %d: %v", target, run+1, err)
		}
		var plans []map[string]any
		if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("%s run %d invalid EXPLAIN JSON: %v", target, run+1, err)
		}
		executionMS, ok := plans[0]["Execution Time"].(float64)
		if !ok || executionMS > 3000 {
			t.Fatalf("%s run %d exceeded 3s: %.3fms", target, run+1, executionMS)
		}
		plan, ok := plans[0]["Plan"].(map[string]any)
		if !ok {
			t.Fatalf("%s run %d has no plan", target, run+1)
		}
		check(plan)
	}
}

func assertPlanScan(t *testing.T, plan map[string]any, relation, requiredIndex string, allowSequential bool) {
	t.Helper()
	scans, indexes := 0, 0
	walkPlan(plan, func(node map[string]any) {
		if nodeRelation, _ := node["Relation Name"].(string); nodeRelation == relation {
			nodeType, _ := node["Node Type"].(string)
			if strings.Contains(nodeType, "Scan") {
				scans++
				if !allowSequential && nodeType == "Seq Scan" {
					t.Fatalf("page overview must not sequentially scan %s", relation)
				}
				if numericPlanValue(node["Actual Loops"]) != 1 {
					t.Fatalf("page overview must execute the %s scan exactly once, got loops=%v", relation, node["Actual Loops"])
				}
			}
		}
		if indexName, _ := node["Index Name"].(string); indexName == requiredIndex && requiredIndex != "" {
			indexes++
		}
	})
	if scans != 1 {
		t.Fatalf("page overview must scan %s exactly once, got %d plan scans", relation, scans)
	}
	if requiredIndex != "" && indexes != 1 {
		t.Fatalf("page overview must use %s exactly once, got %d", requiredIndex, indexes)
	}
}

func assertPlanRelationsAbsent(t *testing.T, plan map[string]any, relations ...string) {
	t.Helper()
	forbidden := make(map[string]struct{}, len(relations))
	for _, relation := range relations {
		forbidden[relation] = struct{}{}
	}
	walkPlan(plan, func(node map[string]any) {
		relation, _ := node["Relation Name"].(string)
		if _, denied := forbidden[relation]; denied {
			t.Fatalf("unauthorized relation %s is present in the production plan instead of zero execution/buffers", relation)
		}
	})
}

func assertOperationsFactPlan(t *testing.T, plan map[string]any) {
	t.Helper()
	usageWindow, usagePoint, reviews := 0, 0, 0
	walkPlan(plan, func(node map[string]any) {
		relation, _ := node["Relation Name"].(string)
		nodeType, _ := node["Node Type"].(string)
		indexName, _ := node["Index Name"].(string)
		if nodeType == "Seq Scan" && (relation == "usage_requests" || relation == "manual_review_cases") {
			t.Fatalf("overview must not sequentially scan %s", relation)
		}
		switch indexName {
		case "usage_requests_operations_overview_started_idx":
			usageWindow++
		case "usage_requests_pending_idx":
			usagePoint++
		case "manual_review_cases_token_usage_overview_created_idx":
			reviews++
		}
	})
	if usageWindow != 1 || usagePoint != 1 || reviews != 1 {
		t.Fatalf("operations overview must scan each logical fact set exactly once: window=%d point=%d review=%d", usageWindow, usagePoint, reviews)
	}
}

func measureTokenOverviewPlans(t *testing.T, ctx context.Context, connection *pgxpool.Conn) {
	t.Helper()
	for run := 0; run < 4; run++ {
		var encoded []byte
		if err := connection.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+tokenOperationsOverviewSQL, int64((30*24*time.Hour)/time.Second)).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var plans []map[string]any
		if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("invalid EXPLAIN JSON: %v", err)
		}
		executionMS, ok := plans[0]["Execution Time"].(float64)
		if !ok || executionMS > 3000 {
			t.Fatalf("overview run %d exceeded 3s: %.3fms", run+1, executionMS)
		}
		plan, ok := plans[0]["Plan"].(map[string]any)
		if !ok {
			t.Fatalf("overview run %d has no plan", run+1)
		}
		bufferEvidence := 0.0
		bufferFields := 0
		for _, field := range []string{"Shared Hit Blocks", "Shared Read Blocks", "Shared Dirtied Blocks", "Shared Written Blocks", "Local Hit Blocks", "Local Read Blocks", "Temp Read Blocks", "Temp Written Blocks"} {
			if value, present := plan[field].(float64); present {
				bufferFields++
				bufferEvidence += value
			}
		}
		if bufferFields == 0 || bufferEvidence <= 0 {
			t.Fatalf("overview run %d has no buffer activity evidence", run+1)
		}
		usageWindow, usagePoint, reviews := 0, 0, 0
		walkPlan(plan, func(node map[string]any) {
			relation, _ := node["Relation Name"].(string)
			nodeType, _ := node["Node Type"].(string)
			indexName, _ := node["Index Name"].(string)
			if nodeType == "Seq Scan" && (relation == "usage_requests" || relation == "manual_review_cases") {
				t.Fatalf("overview must not sequentially scan %s", relation)
			}
			if (relation == "usage_requests" || relation == "manual_review_cases") && numericPlanValue(node["Actual Loops"]) > 1 {
				t.Fatalf("overview must not repeatedly or correlatively scan %s", relation)
			}
			switch indexName {
			case "usage_requests_operations_overview_started_idx":
				usageWindow++
			case "usage_requests_pending_idx":
				usagePoint++
			case "manual_review_cases_token_usage_overview_created_idx":
				reviews++
			}
		})
		if usageWindow != 1 || usagePoint != 1 || reviews != 1 {
			t.Fatalf("run %d must scan each logical fact set exactly once: window=%d point=%d review=%d", run+1, usageWindow, usagePoint, reviews)
		}
	}
}

func numericPlanValue(value any) float64 {
	number, _ := value.(float64)
	return number
}

func newTokenOverviewPerformanceRepository(t *testing.T, ctx context.Context, databaseURL string, reset bool) *PostgresRepository {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	const schema = "token_operations_overview_performance"
	identifier := pgx.Identifier{schema}.Sanitize()
	if reset {
		if _, err := connection.Exec(ctx, `DROP SCHEMA IF EXISTS `+identifier+` CASCADE`); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	if reset {
		loaded, err := loadTokenIntegrationMigrations("../../migrations/token", schema)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrations.Up(ctx, connection, loaded); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 2
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = map[string]string{}
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, adminPaginationMetrics: newAdminPaginationMetrics()}
}

func walkPlan(value any, visit func(map[string]any)) {
	node, ok := value.(map[string]any)
	if !ok {
		return
	}
	visit(node)
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		walkPlan(child, visit)
	}
}
