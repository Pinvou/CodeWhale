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
// production-volume gate for APT-136. CI constrains PostgreSQL 17 to 2 vCPU / 4
// GiB; the deterministic seed freezes the reviewed 1m/100k fact distribution.
func TestTokenOperationsOverviewPerformancePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_DATABASE_URL is not configured")
	}
	phase := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE"))
	if phase != "seed" && phase != "measure" {
		t.Fatal("PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE must be seed or measure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
		measureTokenOverviewPlans(t, ctx, connection)
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
	assertTokenOverviewPerformanceSeed(t, ctx, connection)
	// VACUUM cannot share the implicit transaction created by a multi-statement
	// simple query, so keep each frozen cache/statistics preparation step alone.
	for _, relation := range []string{"usage_requests", "manual_review_cases"} {
		if _, err := connection.Exec(ctx, `VACUUM (ANALYZE) `+pgx.Identifier{relation}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(ctx, `CHECKPOINT`); err != nil {
		t.Fatal(err)
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
		loaded, err := migrations.Load("../../migrations/token")
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
