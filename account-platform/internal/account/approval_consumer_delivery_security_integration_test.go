package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalConsumerDeliveryClosedMappingAndPrivilegesIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	allowed := []struct {
		durable, workload, command, serialization string
		policy                                    int64
	}{
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", "APPROVAL_RFC8785_DECIMAL_V1", 1},
		{"billing-payment-provider-resume-executor-p2-v1", "billing-command-executor", "PAYMENT_PROVIDER_RESUME", "APPROVAL_RFC8785_DECIMAL_V1", 2},
		{"billing-payment-product-resume-executor-p1-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", "APPROVAL_RFC8785_DECIMAL_V1", 1},
		{"token-model-lifecycle-executor-v1", "token-command-executor", "TOKEN_MODEL_LIFECYCLE_CHANGE", "LEGACY_TYPED_V1", 2},
		{"token-model-lifecycle-executor-v1", "token-command-executor", "TOKEN_MODEL_LIFECYCLE_CHANGE", "APPROVAL_RFC8785_DECIMAL_V1", 2},
		{"token-catalog-rollback-executor-v1", "token-command-executor", "TOKEN_MODEL_CATALOG_ROLLBACK", "LEGACY_TYPED_V1", 2},
		{"token-catalog-rollback-executor-v1", "token-command-executor", "TOKEN_MODEL_CATALOG_ROLLBACK", "APPROVAL_RFC8785_DECIMAL_V1", 2},
		{"token-credential-revoke-executor-v1", "token-command-executor", "TOKEN_CREDENTIAL_REVOKE", "LEGACY_TYPED_V1", 1},
		{"token-credential-revoke-executor-v1", "token-command-executor", "TOKEN_CREDENTIAL_REVOKE", "APPROVAL_RFC8785_DECIMAL_V1", 1},
		{"billing-command-executor-v1", "billing-command-executor", "WALLET_ADJUSTMENT", "LEGACY_TYPED_V1", 1},
		{"billing-command-executor-v1", "billing-command-executor", "PAYMENT_PROVIDER_RESUME", "LEGACY_TYPED_V1", 2},
		{"billing-command-executor-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", "LEGACY_TYPED_V1", 1},
	}
	for _, item := range allowed {
		var valid bool
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4,$2,$5)`,
			item.durable, item.workload, item.command, item.policy, item.serialization).Scan(&valid); err != nil || !valid {
			t.Fatalf("allowed mapping %s/%s/%s/%d/%s valid=%t err=%v", item.durable, item.workload, item.command, item.policy, item.serialization, valid, err)
		}
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4+1,$2,$5)`,
			item.durable, item.workload, item.command, item.policy, item.serialization).Scan(&valid); err != nil || valid {
			t.Fatalf("cross-version mapping %s/%s/%s/%d valid=%t err=%v", item.durable, item.workload, item.command, item.policy, valid, err)
		}
	}
	invalid := []struct {
		durable       any
		workload      any
		command       any
		policy        any
		executor      any
		serialization any
	}{
		{"billing-wallet-adjustment-executor-p1-v1", "token-command-executor", "WALLET_ADJUSTMENT", int64(1), "token-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "PAYMENT_PRODUCT_RESUME", int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "token-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{" billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{nil, "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", nil, "WALLET_ADJUSTMENT", int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", nil, int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", nil, "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), nil, "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-command-executor-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor", "APPROVAL_RFC8785_DECIMAL_V1"},
		{"billing-wallet-adjustment-executor-p1-v1", "billing-command-executor", "WALLET_ADJUSTMENT", int64(1), "billing-command-executor", "LEGACY_TYPED_V1"},
		{"token-model-lifecycle-executor-v1", "token-command-executor", "TOKEN_MODEL_LIFECYCLE_CHANGE", int64(2), "token-command-executor", "UNKNOWN"},
	}
	for index, item := range invalid {
		var valid bool
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_mapping_allows($1,$2,$3,$4,$5,$6)`,
			item.durable, item.workload, item.command, item.policy, item.executor, item.serialization).Scan(&valid); err != nil || valid {
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
	var exactOwnerACL bool
	if err := db.adminPool.QueryRow(ctx, `SELECT
  NOT pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_requests','SELECT')
  AND NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','id','UPDATE')
  AND pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','id','SELECT')
  AND pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','version','UPDATE')
  AND NOT pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_command_policies','SELECT')
  AND pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','command_type','SELECT')
  AND pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','policy_version','SELECT')
  AND pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','executor_identity','SELECT')
  AND NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','requester_permission','SELECT')`).Scan(&exactOwnerACL); err != nil || !exactOwnerACL {
		t.Fatalf("authorization owner ACL is not closed: valid=%t err=%v", exactOwnerACL, err)
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
		  AND p.proname IN ('authorization_consumer_mapping_allows','authorization_begin_action_allows','protect_approval_delivery_state',
	    'reject_approval_delivery_state_removal','resolve_authorization_binding','authorization_consumer_route_durable','resolve_authorization_route','validate_legacy_authorization_begin')`).Scan(&functions, &leaked); err != nil || functions != 9 || leaked != 0 {
		t.Fatalf("protected functions=%d leaked=%d err=%v", functions, leaked, err)
	}
}

func TestApprovalConsumerDeliveryDownForbiddenWithoutHistoryIntegration(t *testing.T) {
	db, _, _, loaded := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	before := approvalConsumerHardeningSnapshot(t, ctx, db)
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	err := migrations.DownOne(ctx, connection.Conn(), loaded)
	if postgresSQLState(err) != "55000" || postgresSQLMessage(err) != "000060 approval exclusion reason binding is forward-only; database down is forbidden" {
		t.Fatalf("down state=%s message=%q err=%v", postgresSQLState(err), postgresSQLMessage(err), err)
	}
	after := approvalConsumerHardeningSnapshot(t, ctx, db)
	if after != before {
		t.Fatal("forbidden down changed migration, schema, ACL, or data state")
	}
}

func TestApprovalConsumerDeliveryDownPreservesHistoryIntegration(t *testing.T) {
	db, repository, relay, loaded := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	observeApprovalDelivery(t, ctx, repository, binding, 81)
	var before string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM account_approval_authorization.approval_delivery_states d
WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&before); err != nil {
		t.Fatal(err)
	}
	beforeHardening := approvalConsumerHardeningSnapshot(t, ctx, db)
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	err := migrations.DownOne(ctx, connection.Conn(), loaded)
	if postgresSQLState(err) != "55000" || postgresSQLMessage(err) != "000060 approval exclusion reason binding is forward-only; database down is forbidden" {
		t.Fatalf("history down state=%s message=%q err=%v", postgresSQLState(err), postgresSQLMessage(err), err)
	}
	var after string
	var version int64
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text,(SELECT max(version) FROM public.schema_migrations)
FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`,
		binding.EventID, binding.Durable).Scan(&after, &version); err != nil || after != before || version != 60 {
		t.Fatalf("refused down changed history=%t version=%d err=%v", after != before, version, err)
	}
	if afterHardening := approvalConsumerHardeningSnapshot(t, ctx, db); afterHardening != beforeHardening {
		t.Fatal("forbidden down changed migration, schema, ACL, or data state with history")
	}
}

func approvalConsumerHardeningSnapshot(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase) string {
	t.Helper()
	var snapshot string
	if err := db.adminPool.QueryRow(ctx, `SELECT pg_catalog.jsonb_build_object(
  'version',(SELECT max(version) FROM public.schema_migrations),
  'rows',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(d) ORDER BY d.original_event_id,d.durable),'[]'::jsonb)
    FROM account_approval_authorization.approval_delivery_states d),
  'stream_type',(SELECT pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a
    WHERE a.attrelid='account_approval_authorization.approval_delivery_states'::regclass AND a.attname='stream_sequence' AND NOT a.attisdropped),
  'dlq_type',(SELECT pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a
    WHERE a.attrelid='account_approval_authorization.approval_delivery_states'::regclass AND a.attname='dlq_stream_sequence' AND NOT a.attisdropped),
  'constraints',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(c.conname,pg_catalog.pg_get_constraintdef(c.oid)) ORDER BY c.conname),'[]'::jsonb)
    FROM pg_catalog.pg_constraint c WHERE c.conrelid='account_approval_authorization.approval_delivery_states'::regclass),
  'numeric_observe',to_regprocedure('public.observe_approval_authorization_delivery(uuid,text,text,text,text,text,text,bigint,bigint,text,numeric,text,bigint,text)')::text,
  'numeric_dlq',to_regprocedure('public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)')::text,
  'action_gate',to_regprocedure('account_approval_authorization.authorization_begin_action_allows(text,text,smallint,boolean,bigint,bigint,text,text,text,boolean)')::text,
  'action_gate_definition',(SELECT pg_catalog.pg_get_functiondef(to_regprocedure('account_approval_authorization.authorization_begin_action_allows(text,text,smallint,boolean,bigint,bigint,text,text,text,boolean)'))),
  'bound_gate_definition',(SELECT pg_catalog.pg_get_functiondef(to_regprocedure('public.validate_approval_authorization_begin(uuid,text,text,uuid,bigint,bigint,text,text,text,text,text,bigint,text)'))),
  'legacy_gate_definition',(SELECT pg_catalog.pg_get_functiondef(to_regprocedure('account_approval_authorization.validate_legacy_authorization_begin(uuid,text)'))),
  'runtime_observe_execute',pg_catalog.has_function_privilege('account_service','public.observe_approval_authorization_delivery(uuid,text,text,text,text,text,text,bigint,bigint,text,numeric,text,bigint,text)','EXECUTE'),
  'runtime_dlq_execute',pg_catalog.has_function_privilege('account_service','public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)','EXECUTE'),
  'public_observe_execute',pg_catalog.has_function_privilege('public','public.observe_approval_authorization_delivery(uuid,text,text,text,text,text,text,bigint,bigint,text,numeric,text,bigint,text)','EXECUTE'),
  'public_dlq_execute',pg_catalog.has_function_privilege('public','public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)','EXECUTE'),
  'request_table_select',pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_requests','SELECT'),
  'request_id_update',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','id','UPDATE'),
  'request_id_select',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','id','SELECT'),
  'request_version_update',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests','version','UPDATE'),
  'policy_table_select',pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_command_policies','SELECT'),
  'policy_command_select',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','command_type','SELECT'),
  'policy_version_select',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','policy_version','SELECT'),
  'policy_executor_select',pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies','executor_identity','SELECT')
)::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func postgresSQLState(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code
	}
	return ""
}

func postgresSQLMessage(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Message
	}
	return ""
}
