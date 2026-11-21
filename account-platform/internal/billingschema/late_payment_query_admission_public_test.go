package billingschema

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type legacyArchiveQueryProvider struct {
	payment.Provider
	observation billing.ProviderOrder
}

func (p *legacyArchiveQueryProvider) Capabilities() billing.PaymentCapabilities {
	return billing.PaymentCapabilities{ProviderID: "controlled-legacy", Version: "v1", Channels: []string{"ALIPAY"}, OrderQuerySupported: true, AuthoritativeUnpaidTerminal: true, MinimumQueryIntervalSeconds: 1, TestOnly: true}
}

func (p *legacyArchiveQueryProvider) Get(context.Context, string) (billing.ProviderOrder, error) {
	return p.observation, nil
}

func TestLatePaymentArchiveQueryAdmissionPublicMigration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	binary := buildRetainedBillingMigrationCLI(t, ctx)
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var previous []migrations.Migration
	for _, migration := range loaded {
		if migration.Version != latePaymentQueryAdmissionVersion && migration.Version != latePaymentQueryGenerationsVersion {
			previous = append(previous, migration)
		}
	}
	if len(previous)+2 != len(loaded) {
		t.Fatal("exact admission and dependent binding migrations must both be present")
	}
	directory, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	upCLI := func(t *testing.T, c *pgx.Conn) {
		t.Helper()
		child := exec.CommandContext(ctx, binary, "--direction", "up", "--dir", directory)
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if key != "DATABASE_URL" && key != "MIGRATIONS_DIR" && key != "MIGRATION_ROLE" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" {
				child.Env = append(child.Env, value)
			}
		}
		child.Env = append(child.Env, "DATABASE_URL="+f.serviceURL(t, c, "billing_migrator"), "MIGRATION_ROLE=billing_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
		output, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(output), `"event":"migration.post_verification"`) || !strings.Contains(string(output), `"domain":"billing"`) {
			t.Fatal("actual public Billing CLI Up/Verify failed", err, string(output))
		}
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("full public applied-ledger verification", err)
		}
		migrations.Unlock(ctx, c)
	}
	shape := func(t *testing.T, c *pgx.Conn) {
		t.Helper()
		if stageValue(t, ctx, c, `SELECT (count(*)=2 AND bool_and(NOT attnotnull AND NOT atthasdef AND attmissingval IS NULL) AND bool_and((attname='archive_query_generation' AND atttypid='uuid'::regtype) OR (attname='archive_query_authoritative_unpaid' AND atttypid='bool'::regtype)))::text FROM pg_attribute WHERE attrelid='public.payment_orders'::regclass AND attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT attisdropped`) != "true" {
			t.Fatal("current query facts are not genuinely nullable without defaults/backfill")
		}
		if stageValue(t, ctx, c, `SELECT (current_user='billing_service' AND session_user='billing_migrator' AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND NOT has_schema_privilege('billing_service','public','CREATE') AND pg_get_userbyid(relowner)='billing_schema_owner' AND has_table_privilege('billing_service',oid,'SELECT,INSERT,UPDATE,DELETE'))::text FROM pg_class WHERE oid='public.payment_orders'::regclass`) != "true" {
			t.Fatal("migration failed to restore exact runtime/schema-owner boundary")
		}
		if stageValue(t, ctx, c, `SELECT (count(*)=1 AND bool_and(name='late_payment_query_admission' AND checksum='`+latePaymentQueryAdmissionUp+`'))::text FROM schema_migrations WHERE version=900047`) != "true" {
			t.Fatal("public migration identity ledger differs")
		}
	}
	t.Run("fresh-public-cli", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		upCLI(t, c)
		shape(t, c)
		runtime, err := pgx.Connect(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(ctx)
		for _, forbidden := range []string{`ALTER TABLE payment_orders ADD COLUMN unauthorized_query_fact TEXT`, `SET ROLE billing_schema_owner`} {
			if _, err := runtime.Exec(ctx, forbidden); err == nil {
				t.Fatal("runtime gained DDL/owner authority")
			}
		}
	})
	// Fixed version set from the authorized 2026-11-19 schema-only observation.
	// The source package has new 081 instead of retained original053; a numeric
	// ceiling alone therefore cannot reconstruct this historical deployment.
	productionVersions := []int64{
		1, 2, 3, 4, 5, 6, 7, 9,
		10, 11, 12, 13, 14, 15, 16, 17,
		18, 19, 20, 21, 22, 23, 24, 25,
		26, 27, 28, 29, 30, 31, 32, 33,
		34, 35, 36, 37, 38, 39, 40, 41,
		42, 43, 44, 45, 46, 47, 48, 49,
		50, 51, 52, 53, 54, 55, 56, 57,
		58, 59, 60, 61, 62, 63, 64, 65,
		66, 67, 68, 69, 70, 71, 72, 73,
		74, 75, 76, 77, 78, 79, 80, 900001,
		900002, 900003, 900004, 900005, 900006, 900007, 900008, 900009,
		900010, 900011, 900012, 900013, 900014, 900015, 900016, 900017,
		900018, 900019, 900020, 900021, 900022, 900023, 900024, 900025,
		900026, 900027, 900028, 900029, 900030, 900031, 900032, 900033,
		900037, 900038,
	}
	productionSet := make(map[int64]bool, len(productionVersions))
	for _, version := range productionVersions {
		productionSet[version] = true
	}
	var productionPrefix []migrations.Migration
	for _, migration := range loaded {
		if productionSet[migration.Version] {
			productionPrefix = append(productionPrefix, migration)
		}
	}
	if len(productionPrefix) != 113 {
		t.Fatal("production-set fixture requires published original053 plus exactly113 current source entries")
	}

	seedOrder := func(t *testing.T, c *pgx.Conn) (uuid.UUID, uuid.UUID) {
		t.Helper()
		wallet, owner, order := uuid.New(), uuid.New(), uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, wallet, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,last_successful_query_at,last_authoritative_unpaid_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'QUERY_ADMISSION_LEGACY','{}','ALIPAY',100,'CNY',100,'PENDING','PENDING',now()-interval '9 days',now()-interval '9 days',now()-interval '7 days',now()-interval '1 hour',now(),now(),now()-interval '10 days',now(),'controlled-legacy','v1')`, order, "query-admission-"+order.String(), owner, wallet); err != nil {
			t.Fatal(err)
		}
		return owner, order
	}
	t.Run("installed-binding-head-fills-statistics-gap", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		var beforeStatistics []migrations.Migration
		for _, migration := range loaded {
			if migration.Version != statisticsHeadVersion {
				beforeStatistics = append(beforeStatistics, migration)
			}
		}
		if err := Up(ctx, c, beforeStatistics); err != nil {
			t.Fatal("install genuine query package without statistics", err)
		}
		if err := migrations.Unlock(ctx, c); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(ctx, c, beforeStatistics); err != nil {
			t.Fatal("verify query predecessor", err)
		}
		if stageValue(t, ctx, c, `SELECT ((SELECT max(version) FROM schema_migrations)=900048 AND NOT EXISTS(SELECT FROM schema_migrations WHERE version=900046) AND to_regclass('public.outbox_events_dead_statistics_idx') IS NULL)::text`) != "true" {
			t.Fatal("fixture must have 900048 installed and no 900046")
		}
		_, order := seedOrder(t, c)
		pool, err := pgxpool.New(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		repository := billing.NewPostgresRepositoryWithPool(pool)
		item, err := repository.EscalateLatePaymentReview(ctx, order, "CLOSED")
		if err != nil {
			t.Fatal(err)
		}
		actor, key := uuid.New(), "retained-query-"+uuid.NewString()
		if _, err := repository.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
			t.Fatal(err)
		}
		input := billing.LatePaymentQueryInput{CommandProtocol: 1, PaymentOrderID: order, ProviderID: item.ProviderID, ProviderVersion: item.ProviderVersion, MerchantOrderNo: item.MerchantOrderNo}
		if receipt, err := repository.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input); err != nil || receipt.State != "UNKNOWN" {
			t.Fatal("record real committed UNKNOWN with generation binding", err)
		}
		const bindingFacts = `SELECT jsonb_agg(jsonb_build_array(to_jsonb(q),to_jsonb(g),to_jsonb(p)) ORDER BY q.idempotency_key)::text FROM late_payment_query_commands q JOIN late_payment_query_generations g USING(idempotency_key) JOIN payment_orders p ON p.id=q.payment_order_id WHERE p.archive_query_generation=g.generation AND p.archive_query_authoritative_unpaid IS NULL`
		oldBinding := stageValue(t, ctx, c, bindingFacts)
		oldLedger := stageValue(t, ctx, c, retainedStatisticsLedger)
		oldOutbox := stageValue(t, ctx, c, statisticsOutboxFacts)
		oldShape := canonicalStageShape(t, stageShape(t, ctx, c))
		upCLI(t, c)
		assertStatisticsHeadIndex(t, ctx, c)
		if shapeWithoutStatisticsIndex(t, stageShape(t, ctx, c)) != oldShape || stageValue(t, ctx, c, bindingFacts) != oldBinding || stageValue(t, ctx, c, retainedStatisticsLedger) != oldLedger || stageValue(t, ctx, c, statisticsOutboxFacts) != oldOutbox {
			t.Fatal("lower statistics migration changed prior schema, authority, binding, ledger or outbox")
		}
		before := retainedDataRollbackSnapshot(t, ctx, c)
		upCLI(t, c)
		if receipt, err := repository.ReadLatePaymentQuery(ctx, item.ID, actor, key, input); err != nil || receipt.Reference != key || receipt.State != "UNKNOWN" {
			t.Fatal("retained receipt diverged after replay", err)
		}
		if _, err := repository.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, billing.ErrConflict) {
			t.Fatal("restored unknown no longer blocks archive", err)
		}
		err = DownOne(ctx, c, loaded)
		unlockErr := migrations.Unlock(ctx, c)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "P0001" || rejected.Message != "late payment query generation bindings are forward-only" || unlockErr != nil || retainedDataRollbackSnapshot(t, ctx, c) != before {
			t.Fatal("replay or refused 900048 Down changed the combined schema or facts", err, unlockErr)
		}
	})
	upgradeFrom := func(t *testing.T, predecessor []migrations.Migration, legacy53 bool) {
		t.Helper()
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		legacyLedgerBefore := ""
		if legacy53 {
			// The original053 deployment envelope owns this historical actor.
			// Never adopt a preexisting role or grant it to billing_service.
			var exists bool
			if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_wallet_receipt_deployer')`).Scan(&exists); err != nil || exists {
				t.Fatal("legacy actor must be absent", err)
			}
			if _, err := f.admin.Exec(ctx, `CREATE ROLE billing_wallet_receipt_deployer NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; GRANT billing_wallet_receipt_deployer TO billing_migrator`); err != nil {
				t.Fatal(err)
			}
			f.roles = append(f.roles, "billing_wallet_receipt_deployer")
			// Reuse the published 053 fixture, not a fabricated ledger INSERT.
			// This models the observed production version set, not unqueried
			// production 053 checksum/owner or production business rows.
			var before53 []migrations.Migration
			for _, migration := range predecessor {
				if migration.Version <= 52 || (migration.Version >= 900001 && migration.Version <= 900012 && migration.Version != 900003) {
					before53 = append(before53, migration)
				}
			}
			if err := Up(ctx, c, before53); err != nil {
				t.Fatal("published original053 predecessor", err)
			}
			migrations.Unlock(ctx, c)
			legacy := migrations.Migration{Version: 53, Name: "token_account_wallet_provisioning_receipts", UpSQL: originalWalletReceiptSQL, Checksum: originalWalletReceiptChecksum}
			if err := migrations.Up(ctx, c, []migrations.Migration{legacy}); err != nil {
				t.Fatal("published original053 installation", err)
			}
			migrations.Unlock(ctx, c)
		}
		if err := Up(ctx, c, predecessor); err != nil {
			t.Fatal("genuine full predecessor installation", err)
		}
		migrations.Unlock(ctx, c)
		if legacy53 {
			var observed []int64
			if err := c.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migrations`).Scan(&observed); err != nil {
				t.Fatal("observed production-version-set fixture", err)
			}
			if len(observed) != len(productionVersions) || len(observed) != 114 {
				t.Fatal("historical fixture invented or omitted a production version")
			}
			for index, version := range productionVersions {
				if observed[index] != version {
					t.Fatal("historical fixture differs from observed production version set")
				}
			}
			legacyLedgerBefore = stageValue(t, ctx, c, `SELECT to_jsonb(m)::text FROM schema_migrations m WHERE version=53`)
		}
		owner, order := seedOrder(t, c)
		old := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id)::text FROM payment_orders p`)
		injected := errors.New("owned current-query migration post-DDL rollback")
		ordered, err := Ordered(loaded)
		if err != nil {
			t.Fatal(err)
		}
		err = migrations.UpWithHooks(ctx, c, ordered, migrations.UpHooks{BeforeApply: prepareBillingMigration, ApplySQL: applyBillingMigration, LedgerRole: "billing_schema_owner", AfterApply: func(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
			if migration.Version == latePaymentQueryAdmissionVersion {
				return injected
			}
			return finishBillingMigration(ctx, tx, migration)
		}})
		migrations.Unlock(ctx, c)
		if err == nil || !strings.Contains(err.Error(), injected.Error()) {
			t.Fatal("actual new DDL did not reach the rollback injection", err)
		}
		if stageValue(t, ctx, c, `SELECT (NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.payment_orders'::regclass AND attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT attisdropped) AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900047) AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND NOT has_schema_privilege('billing_service','public','CREATE'))::text`) != "true" || old != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id)::text FROM payment_orders p`) {
			t.Fatal("failed new migration changed rows/ledger/schema or leaked DDL authority")
		}
		upCLI(t, c)
		shape(t, c)
		if legacy53 && legacyLedgerBefore != stageValue(t, ctx, c, `SELECT to_jsonb(m)::text FROM schema_migrations m WHERE version=53`) {
			t.Fatal("complete upgrade rewrote original053 ledger identity")
		}

		if old != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p)-ARRAY['archive_query_generation','archive_query_authoritative_unpaid'] ORDER BY p.id)::text FROM payment_orders p`) || stageValue(t, ctx, c, `SELECT bool_and(archive_query_generation IS NULL AND archive_query_authoritative_unpaid IS NULL)::text FROM payment_orders`) != "true" {
			t.Fatal("historical authority timestamps were rewritten or current authority was backfilled")
		}
		pool, err := pgxpool.New(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		repository := billing.NewPostgresRepositoryWithPool(pool)
		item, err := repository.EscalateLatePaymentReview(ctx, order, "CLOSED")
		if err != nil {
			t.Fatal("existing row could not enter its real review producer", err)
		}
		actor := uuid.New()
		if _, err := repository.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
			t.Fatal(err)
		}
		beforeUnknown := retainedDataRollbackSnapshot(t, ctx, c)
		if _, err := repository.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, billing.ErrConflict) || beforeUnknown != retainedDataRollbackSnapshot(t, ctx, c) {
			t.Fatal("historical timestamps granted authority without a real new query", err)
		}
		provider := &legacyArchiveQueryProvider{observation: billing.ProviderOrder{MerchantOrderNo: "query-admission-" + order.String(), Status: "CLOSED", ProviderStatus: "PAY_CLOSED", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", ExternalCustomerKey: owner.String()}}
		coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.SyncReviewedOrder(ctx, order); err != nil {
			t.Fatal("old row could not produce a real current query fact", err)
		}
		archived, err := repository.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString())
		if err != nil || archived.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" || stageValue(t, ctx, c, `SELECT bool_and(provider_capability_snapshot='{}'::jsonb)::text FROM payment_orders`) != "true" {
			t.Fatal("real new query did not restore lawful old-row admission or rewrote its missing frozen capability", err)
		}
		before := retainedDataRollbackSnapshot(t, ctx, c)
		err = DownOne(ctx, c, loaded)
		migrations.Unlock(ctx, c)
		if err == nil || !strings.Contains(err.Error(), "late payment query generation bindings are forward-only") || before != retainedDataRollbackSnapshot(t, ctx, c) {
			t.Fatal("latest public Down did not preserve exact current facts and ledger", err)
		}
		// 48 is now the latest package entry. Keep independent proof that the
		// original published 47 Down bytes still refuse without changing data.
		for _, migration := range loaded {
			if migration.Version == latePaymentQueryAdmissionVersion {
				_, err := c.Exec(ctx, migration.DownSQL)
				var rejected *pgconn.PgError
				if !errors.As(err, &rejected) || rejected.Code != "P0001" || rejected.Message != "late payment query admission facts are forward-only" || before != retainedDataRollbackSnapshot(t, ctx, c) {
					t.Fatal("original published 47 rollback refusal changed", err)
				}
			}
		}
		output, err := runRetainedBillingDownCLI(t, ctx, binary, directory, f.serviceURL(t, c, "billing_migrator"))
		if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") || before != retainedDataRollbackSnapshot(t, ctx, c) {
			t.Fatal("actual CLI Down did not preserve exact current facts and ledger", err)
		}
	}
	t.Run("previous-full-upgrade-and-forward-only", func(t *testing.T) {
		upgradeFrom(t, previous, false)
	})
	t.Run("production-900038-version-set-upgrade-and-forward-only", func(t *testing.T) {
		upgradeFrom(t, productionPrefix, true)
	})
}
