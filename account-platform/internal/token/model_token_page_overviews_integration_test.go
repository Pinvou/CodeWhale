package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestModelTokenPageOverviewsPostgresIntegration is a no-SKIP CI release gate.
// It runs the production queries and persistent 60/user/endpoint plus shared
// 120/source limiter against the complete Token migration chain.
func TestModelTokenPageOverviewsPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	setupContext, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()
	repository := newTokenQueryIntegrationRepository(t, setupContext, databaseURL)
	now := time.Now().UTC()
	modelID, capabilityID, catalogID := uuid.New(), uuid.New(), uuid.New()
	accountID, credentialID, routeID := uuid.New(), uuid.New(), uuid.New()
	ownerID, walletID, actorID := uuid.New(), uuid.New(), uuid.New()
	usageID, reviewID, provisioningID := uuid.New(), uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	var catalogVersion int64
	if err := repository.pool.QueryRow(setupContext, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'page-overview',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("7", 64)
	if _, err := repository.pool.Exec(setupContext, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'provider','adapter-v1','page-overview-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,128000,4096,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Page Overview Model','page overview model','PUBLISHED',$5,$6,true);
INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at) VALUES($1,$7,1,'Page Overview Draft','CHAT',1,128000,4096,'["TEXT"]','v1',$3,$4);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($8,'USER',$9,$10,'Page Overview Account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($11,$8,'provider','https://provider.invalid','x'::bytea,'key','page-overview-fingerprint',1,'ACTIVE',ARRAY['page-overview-model'],$4,$3,$4,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,model_id,protocol_version_id,protocol_code,priority,default_scope,reference_status,health_status,health_observed_at,health_expires_at,route_version,route_generation,updated_at) VALUES($12,$8,'page-overview-model',$1,$6,'CHAT',100,'NONE','RESOLVED','HEALTHY',$4,$4::timestamptz+interval '4 minutes',1,1,$4);
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($13,'PERSONAL',$9,$10,$1,'page-overview-model','CHAT',$6,1,$2,'REPAIR_REQUIRED','VERIFY',1,'{}',3,$4,$8,$11,$3,$4,$4);
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint,reconciliation_stopped_at) VALUES($14,$9,'page-overview-session',$8,$11,$10,'page-overview-model','REVIEW',100,'{}',$4::timestamptz-interval '1 hour',$4,$2,$2,NULL);
INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($15,'TOKEN','USAGE_RECONCILIATION_REVIEW','USAGE_REQUEST',$14,'P2','OPEN',$4::timestamptz-interval '30 minutes',$4,$4,$4,$4,$4)`,
		pgx.QueryExecModeSimpleProtocol, modelID, digest, actorID, now, capabilityID, protocolID, catalogVersion, accountID, ownerID, walletID, credentialID, routeID, provisioningID, usageID, reviewID); err != nil {
		t.Fatal(err)
	}

	queryContext, queryCancel := context.WithTimeout(setupContext, 3*time.Second)
	models, err := repository.AdminModelCatalogOverview(queryContext)
	queryCancel()
	if err != nil || models.PublishedModels != 1 || models.DeprecatedModels != 0 || models.ModelDrafts != 1 || models.AsOf.IsZero() {
		t.Fatalf("model overview=%+v err=%v", models, err)
	}
	queryContext, queryCancel = context.WithTimeout(setupContext, 3*time.Second)
	routes, err := repository.AdminTokenResourcesOverview(queryContext)
	queryCancel()
	if err != nil || routes.StableRoutes != 1 || routes.HealthyRoutes != 1 || routes.RouteAttention != 0 || routes.AsOf.IsZero() {
		t.Fatalf("route overview=%+v err=%v", routes, err)
	}
	for _, item := range []struct {
		name         string
		token, usage bool
	}{
		{"token-only", true, false},
		{"usage-only", false, true},
		{"both", true, true},
	} {
		queryContext, queryCancel = context.WithTimeout(setupContext, 3*time.Second)
		window := time.Duration(0)
		if item.usage {
			window = 24 * time.Hour
		}
		values, queryErr := repository.AdminModelOperationsOverview(queryContext, window, item.token, item.usage)
		queryCancel()
		if queryErr != nil || values.AsOf.IsZero() || item.token && values.ProvisioningFailedOrRepair != 1 || item.usage && (values.UsageRequests != 1 || values.ReconciliationPending != 1 || len(values.TrendPoints) != 24) {
			t.Fatalf("%s overview=%+v err=%v", item.name, values, queryErr)
		}
		if !item.token && (values.ProvisioningPending != 0 || values.ProvisioningFailedOrRepair != 0) || !item.usage && (!values.WindowFrom.IsZero() || len(values.TrendPoints) != 0) {
			t.Fatalf("%s executed or exposed an unauthorized group: %+v", item.name, values)
		}
	}
	if _, err := repository.AdminModelCatalogOverview(context.Background()); err == nil {
		t.Fatal("production overview query accepted a context without the trusted deadline chain")
	}
	waitForOverviewRateWindowSafety(t, setupContext, repository)

	firstSubject, firstSource := uuid.New(), "198.51.100.11"
	for attempt := 0; attempt < 60; attempt++ {
		if err := repository.reserveAdminOverviewRate(setupContext, firstSubject, modelCatalogOverviewKind, firstSource); err != nil {
			t.Fatalf("subject attempt %d: %v", attempt+1, err)
		}
	}
	if err := repository.reserveAdminOverviewRate(setupContext, firstSubject, modelCatalogOverviewKind, firstSource); !errors.Is(err, ErrAdminQueryRateLimited) {
		t.Fatalf("61st endpoint request was not limited: %v", err)
	}
	sharedSource := "198.51.100.12"
	for attempt := 0; attempt < 120; attempt++ {
		endpoint := modelCatalogOverviewKind
		if attempt%3 == 1 {
			endpoint = tokenResourcesOverviewKind
		} else if attempt%3 == 2 {
			endpoint = modelOperationsOverviewKind
		}
		if err := repository.reserveAdminOverviewRate(setupContext, uuid.New(), endpoint, sharedSource); err != nil {
			t.Fatalf("shared source attempt %d: %v", attempt+1, err)
		}
	}
	if err := repository.reserveAdminOverviewRate(setupContext, uuid.New(), modelCatalogOverviewKind, sharedSource); !errors.Is(err, ErrAdminQueryRateLimited) {
		t.Fatalf("121st shared-source request was not limited: %v", err)
	}

	replica := newPageOverviewReplica(t, setupContext, repository)
	t.Run("barrier-concurrent-subject-endpoint", func(t *testing.T) {
		subject, source := uuid.New(), "198.51.100.31"
		allowed, limited, unexpected := runConcurrentOverviewReservations(setupContext, []*PostgresRepository{repository, replica}, 80, func(int) (uuid.UUID, string, string) {
			return subject, modelCatalogOverviewKind, source
		})
		if allowed != 60 || limited != 20 || unexpected != 0 {
			t.Fatalf("allowed=%d limited=%d unexpected=%d", allowed, limited, unexpected)
		}
		assertOverviewRateCounter(t, setupContext, repository, "overview:subject:"+subject.String()+":"+modelCatalogOverviewKind, 80)
		assertOverviewRateCounter(t, setupContext, repository, overviewSourceRateKey(source), 80)
	})
	t.Run("barrier-concurrent-source-across-three-endpoints", func(t *testing.T) {
		source := "198.51.100.32"
		endpoints := []string{modelCatalogOverviewKind, tokenResourcesOverviewKind, modelOperationsOverviewKind}
		allowed, limited, unexpected := runConcurrentOverviewReservations(setupContext, []*PostgresRepository{repository, replica}, 150, func(index int) (uuid.UUID, string, string) {
			return uuid.New(), endpoints[index%len(endpoints)], source
		})
		if allowed != 120 || limited != 30 || unexpected != 0 {
			t.Fatalf("allowed=%d limited=%d unexpected=%d", allowed, limited, unexpected)
		}
		assertOverviewRateCounter(t, setupContext, repository, overviewSourceRateKey(source), 150)
	})
	t.Run("database-minute-boundary", func(t *testing.T) {
		subject, source := uuid.New(), "198.51.100.33"
		subjectKey := "overview:subject:" + subject.String() + ":" + tokenResourcesOverviewKind
		sourceKey := overviewSourceRateKey(source)
		var previousWindow time.Time
		if err := repository.pool.QueryRow(setupContext, `SELECT date_trunc('minute',clock_timestamp())-interval '1 minute'`).Scan(&previousWindow); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.pool.Exec(setupContext, `INSERT INTO token_admin_pagination_creation_windows(rate_key,window_start,creation_count)
VALUES($1,$3,60),($2,$3,120)`, subjectKey, sourceKey, previousWindow); err != nil {
			t.Fatal(err)
		}
		if err := replica.reserveAdminOverviewRate(setupContext, subject, tokenResourcesOverviewKind, source); err != nil {
			t.Fatalf("previous minute leaked into current allowance: %v", err)
		}
		var previousSubject, laterSubject, rows int
		if err := repository.pool.QueryRow(setupContext, `SELECT
COALESCE(sum(creation_count) FILTER (WHERE window_start=$2),0)::int,
COALESCE(sum(creation_count) FILTER (WHERE window_start<>$2),0)::int,
count(*)::int
FROM token_admin_pagination_creation_windows WHERE rate_key=$1`, subjectKey, previousWindow).Scan(&previousSubject, &laterSubject, &rows); err != nil {
			t.Fatal(err)
		}
		if previousSubject != 60 || laterSubject != 1 || rows != 2 {
			t.Fatalf("minute counters previous/later/rows=%d/%d/%d", previousSubject, laterSubject, rows)
		}
	})
	t.Run("pool-wait-is-deducted-from-statement-timeout", func(t *testing.T) {
		assertPageOverviewPoolWaitDeadline(t, setupContext, repository)
	})

	var exists bool
	if err := repository.pool.QueryRow(setupContext, `SELECT to_regclass('routing_policies_overview_health_idx') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("overview route index missing: exists=%v err=%v", exists, err)
	}
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "token", "000033_model_token_page_overview_indexes.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "token", "000033_model_token_page_overview_indexes.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(setupContext, string(down)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(setupContext, string(up)); err != nil {
		t.Fatal(err)
	}
}

func waitForOverviewRateWindowSafety(t *testing.T, ctx context.Context, repository *PostgresRepository) {
	t.Helper()
	var seconds float64
	if err := repository.pool.QueryRow(ctx, `SELECT extract(epoch FROM clock_timestamp()-date_trunc('minute',clock_timestamp()))`).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	if seconds < 50 {
		return
	}
	delay := time.Duration((60-seconds)*float64(time.Second)) + 100*time.Millisecond
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func newPageOverviewReplica(t *testing.T, ctx context.Context, repository *PostgresRepository) *PostgresRepository {
	t.Helper()
	configuration := repository.pool.Config().Copy()
	configuration.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &PostgresRepository{pool: pool}
}

func runConcurrentOverviewReservations(ctx context.Context, repositories []*PostgresRepository, attempts int, input func(int) (uuid.UUID, string, string)) (int64, int64, int64) {
	start := make(chan struct{})
	results := make(chan error, attempts)
	var ready sync.WaitGroup
	ready.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			ready.Done()
			<-start
			subject, endpoint, source := input(index)
			results <- repositories[index%len(repositories)].reserveAdminOverviewRate(ctx, subject, endpoint, source)
		}(index)
	}
	ready.Wait()
	close(start)
	var allowed, limited, unexpected atomic.Int64
	for range attempts {
		err := <-results
		switch {
		case err == nil:
			allowed.Add(1)
		case errors.Is(err, ErrAdminQueryRateLimited):
			limited.Add(1)
		default:
			unexpected.Add(1)
		}
	}
	return allowed.Load(), limited.Load(), unexpected.Load()
}

func overviewSourceRateKey(source string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(source)))
	return "overview:source:" + hex.EncodeToString(digest[:])
}

func assertOverviewRateCounter(t *testing.T, ctx context.Context, repository *PostgresRepository, key string, expected int) {
	t.Helper()
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT COALESCE(sum(creation_count),0)::int FROM token_admin_pagination_creation_windows WHERE rate_key=$1`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != expected {
		t.Fatalf("rate counter %s=%d want=%d", key, count, expected)
	}
}

func assertPageOverviewPoolWaitDeadline(t *testing.T, setupContext context.Context, repository *PostgresRepository) {
	t.Helper()
	configuration := repository.pool.Config().Copy()
	configuration.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(setupContext, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(setupContext)
	if err != nil {
		t.Fatal(err)
	}
	repositoryWithBlockedPool := &PostgresRepository{pool: pool}
	ctx, cancel := context.WithTimeout(setupContext, 600*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	var callbackCalls atomic.Int64
	var installedTimeout atomic.Int64
	go func() {
		result <- repositoryWithBlockedPool.withModelTokenPageOverviewTx(ctx, func(tx pgx.Tx) error {
			callbackCalls.Add(1)
			var raw string
			if err := tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&raw); err != nil {
				return err
			}
			duration, err := time.ParseDuration(raw)
			if err != nil {
				return err
			}
			installedTimeout.Store(duration.Milliseconds())
			_, err = tx.Exec(ctx, `SELECT pg_sleep(1)`)
			return err
		})
	}()
	time.Sleep(250 * time.Millisecond)
	held.Release()
	err = <-result
	if err == nil || callbackCalls.Load() != 1 || installedTimeout.Load() <= 0 || installedTimeout.Load() > 400 {
		t.Fatalf("waited transaction err=%v callback_calls=%d statement_timeout_ms=%d", err, callbackCalls.Load(), installedTimeout.Load())
	}

	held, err = pool.Acquire(setupContext)
	if err != nil {
		t.Fatal(err)
	}
	expiredCtx, expiredCancel := context.WithTimeout(setupContext, 100*time.Millisecond)
	defer expiredCancel()
	callbackCalls.Store(0)
	err = repositoryWithBlockedPool.withModelTokenPageOverviewTx(expiredCtx, func(pgx.Tx) error {
		callbackCalls.Add(1)
		return nil
	})
	held.Release()
	if !errors.Is(err, context.DeadlineExceeded) || callbackCalls.Load() != 0 {
		t.Fatalf("expired pool wait err=%v callback_calls=%d", err, callbackCalls.Load())
	}
}
