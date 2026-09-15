package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalConsumerDeliveryClosedMappingAndPrivilegesIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	allowed := []struct {
		durable, workload, command string
		policy                     int64
	}{
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", 1},
		{"billing-payment-provider-resume-executor-p2-v1", "billing-command-executor", "PAYMENT_PROVIDER_RESUME", 2},
		{"billing-payment-product-resume-executor-p1-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", 1},
		{"token-model-lifecycle-executor-v1", "token-command-executor", "TOKEN_MODEL_LIFECYCLE_CHANGE", 2},
		{"token-catalog-rollback-executor-v1", "token-command-executor", "TOKEN_MODEL_CATALOG_ROLLBACK", 2},
		{"token-credential-revoke-executor-v1", "token-command-executor", "TOKEN_CREDENTIAL_REVOKE", 1},
		{"billing-command-executor-v1", "billing-command-executor", "WALLET_ADJUSTMENT", 1},
		{"billing-command-executor-v1", "billing-command-executor", "PAYMENT_PROVIDER_RESUME", 2},
		{"billing-command-executor-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", 1},
	}
	for _, item := range allowed {
		var valid bool
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4,$2)`,
			item.durable, item.workload, item.command, item.policy).Scan(&valid); err != nil || !valid {
			t.Fatalf("allowed mapping %s/%s/%s/%d valid=%t err=%v", item.durable, item.workload, item.command, item.policy, valid, err)
		}
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4+1,$2)`,
			item.durable, item.workload, item.command, item.policy).Scan(&valid); err != nil || valid {
			t.Fatalf("cross-version mapping %s/%s/%s/%d valid=%t err=%v", item.durable, item.workload, item.command, item.policy, valid, err)
		}
	}
	invalid := []struct {
		durable  any
		workload any
		command  any
		policy   any
		executor any
	}{
		{"billing-wallet-adjustment-executor-p1-v1", "token-command-executor", "WALLET_ADJUSTMENT", int64(1), "token-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", int64(1), "billing-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "token-command-executor"},
		{" billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor"},
		{nil, "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", nil, "WALLET_ADJUSTMENT", int64(1), "billing-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", nil, int64(1), "billing-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", nil, "billing-command-executor"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), nil},
	}
	for index, item := range invalid {
		var valid bool
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4,$5)`,
			item.durable, item.workload, item.command, item.policy, item.executor).Scan(&valid); err != nil || valid {
			t.Fatalf("invalid mapping %d valid=%t err=%v", index, valid, err)
		}
	}

	if _, err := db.writerPool.Exec(ctx, `SELECT public.resolve_approval_authorization_binding($1,$2,$3)`,
		uuid.New(), "billing-wallet-adjustment-executor-p1-v1", "billing-command-executor"); postgresSQLState(err) != "42501" {
		t.Fatalf("writer called consumer binding function: state=%s", postgresSQLState(err))
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT count(*) FROM account_approval_authorization.approval_delivery_states`); postgresSQLState(err) != "42501" {
		t.Fatalf("runtime read protected state directly: state=%s", postgresSQLState(err))
	}
	var functions, leaked int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE
  pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
    OR pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
    OR pg_catalog.has_function_privilege('account_approval_writer',p.oid,'EXECUTE')
    OR pg_catalog.has_function_privilege('account_approval_backfill',p.oid,'EXECUTE')
    OR pg_catalog.has_function_privilege('account_approval_relay',p.oid,'EXECUTE'))
FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname='account_approval_authorization'
  AND p.proname IN ('authorization_consumer_mapping_allows','protect_approval_delivery_state',
	    'reject_approval_delivery_state_removal','resolve_authorization_binding','validate_legacy_authorization_begin')`).Scan(&functions, &leaked); err != nil || functions != 5 || leaked != 0 {
		t.Fatalf("protected functions=%d leaked=%d err=%v", functions, leaked, err)
	}
}

func TestApprovalConsumerDeliveryDownReentryIntegration(t *testing.T) {
	db, repository, _, loaded := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	var version int64
	var clean bool
	if err := db.adminPool.QueryRow(ctx, `SELECT max(version),
  to_regclass('account_approval_authorization.approval_delivery_states') IS NULL
  AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid='public.approval_executions'::regclass
    AND attname IN ('authorization_event_id','authorization_durable','authorization_approved_request_version',
      'authorization_event_digest','authorization_digest_kind','authorization_serialization_version',
      'authorization_provenance','authorization_approval_epoch') AND NOT attisdropped)
  AND to_regprocedure('public.resolve_approval_authorization_binding(uuid,text,text)') IS NULL
FROM public.schema_migrations`).Scan(&version, &clean); err != nil || version != 52 || !clean {
		t.Fatalf("down version=%d clean=%t err=%v", version, clean, err)
	}
	if err := repository.ApprovalWriterReady(ctx); err != nil {
		t.Fatal("down broke approval writer readiness")
	}
	migrations.Unlock(ctx, connection.Conn())
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	if err := repository.ApprovalWriterReady(ctx); err != nil {
		t.Fatal("reentry broke approval writer readiness")
	}
}

func TestApprovalConsumerDeliveryDownPreservesHistoryIntegration(t *testing.T) {
	db, repository, relay, loaded := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	observeApprovalDelivery(t, ctx, repository, binding, 81)
	var before string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM account_approval_authorization.approval_delivery_states d
WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&before); err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); postgresSQLState(err) != "55000" {
		t.Fatalf("history down state=%s err=%v", postgresSQLState(err), err)
	}
	var after string
	var version int64
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text,(SELECT max(version) FROM public.schema_migrations)
FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`,
		binding.EventID, binding.Durable).Scan(&after, &version); err != nil || after != before || version != 53 {
		t.Fatalf("refused down changed history=%t version=%d err=%v", after != before, version, err)
	}
}

func postgresSQLState(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code
	}
	return ""
}
