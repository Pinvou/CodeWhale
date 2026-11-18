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
		if migration.Version != latePaymentQueryAdmissionVersion {
			previous = append(previous, migration)
		}
	}
	if len(previous)+1 != len(loaded) {
		t.Fatal("exact current-query migration must be present")
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
		if stageValue(t, ctx, c, `SELECT (count(*)=1 AND bool_and(name='late_payment_query_admission' AND checksum='`+latePaymentQueryAdmissionUp+`'))::text FROM schema_migrations WHERE version=900046`) != "true" {
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
	t.Run("previous-full-upgrade-and-forward-only", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, c, previous); err != nil {
			t.Fatal("genuine full predecessor installation", err)
		}
		migrations.Unlock(ctx, c)
		wallet, owner, order := uuid.New(), uuid.New(), uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, wallet, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,last_successful_query_at,last_authoritative_unpaid_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'QUERY_ADMISSION_LEGACY','{}','ALIPAY',100,'CNY',100,'PENDING','PENDING',now()-interval '9 days',now()-interval '9 days',now()-interval '7 days',now()-interval '1 hour',now(),now(),now()-interval '10 days',now(),'controlled-legacy','v1')`, order, "query-admission-"+order.String(), owner, wallet); err != nil {
			t.Fatal(err)
		}
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
		if stageValue(t, ctx, c, `SELECT (NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.payment_orders'::regclass AND attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT attisdropped) AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900046) AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND NOT has_schema_privilege('billing_service','public','CREATE'))::text`) != "true" || old != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id)::text FROM payment_orders p`) {
			t.Fatal("failed new migration changed rows/ledger/schema or leaked DDL authority")
		}
		upCLI(t, c)
		shape(t, c)
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
		if err == nil || !strings.Contains(err.Error(), "late payment query admission facts are forward-only") || before != retainedDataRollbackSnapshot(t, ctx, c) {
			t.Fatal("public Down did not preserve exact current facts and ledger", err)
		}
		output, err := runRetainedBillingDownCLI(t, ctx, binary, directory, f.serviceURL(t, c, "billing_migrator"))
		if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") || before != retainedDataRollbackSnapshot(t, ctx, c) {
			t.Fatal("actual CLI Down did not preserve exact current facts and ledger", err)
		}
	})
}
