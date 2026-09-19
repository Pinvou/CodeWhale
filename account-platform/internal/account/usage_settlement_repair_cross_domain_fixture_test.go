package account

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// usageRepairCrossDomainFacts names the seeded unsettled usage loss.
type usageRepairCrossDomainFacts struct {
	HoldID         uuid.UUID
	UsageRequestID uuid.UUID
	LossReviewID   uuid.UUID
	WalletID       uuid.UUID
	PackageID      uuid.UUID
	ComponentID    uuid.UUID
	Claimed        int64
}

// newUsageRepairCrossDomainBillingFixture provisions a production-shaped Billing
// database for the cross-domain usage settlement repair gate. Activation stays
// exactly as deployed: the command remains the original INACTIVE v1/s1 row.
func newUsageRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, usageRepairCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_usage_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER billing_schema_owner TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, `DROP DATABASE IF EXISTS `+identifier)
		_ = admin.Close(context.Background())
	})
	migrationConfig := base.Copy()
	migrationConfig.Database = database
	connection, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	if _, err := connection.Exec(ctx, `ALTER SCHEMA public OWNER TO billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION billing_migrator; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("Billing fixture migrations: %v", err)
	}
	migrations.Unlock(ctx, connection)

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 20
	configuration.ConnConfig.Database = database
	configuration.AfterConnect = func(connectContext context.Context, connection *pgx.Conn) error {
		_, connectErr := connection.Exec(connectContext, `SET SESSION AUTHORIZATION billing_service; SET search_path TO public,pg_temp`)
		return connectErr
	}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := billingdomain.NewPostgresRepositoryWithPool(pool)
	facts := seedUsageRepairCrossDomainCase(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedUsageRepairCrossDomainCase writes the production-shaped unsettled loss: a
// released hold whose reservation was returned to the component, the matching
// allocation row and the open loss review that recorded the claimed usage.
func seedUsageRepairCrossDomainCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) usageRepairCrossDomainFacts {
	t.Helper()
	now := time.Now().UTC()
	facts := usageRepairCrossDomainFacts{
		HoldID: uuid.New(), UsageRequestID: uuid.New(), LossReviewID: uuid.New(), WalletID: uuid.New(),
		PackageID: uuid.New(), ComponentID: uuid.New(), Claimed: 100,
	}
	ownerID, lotID, orderID := uuid.New(), uuid.New(), uuid.New()
	product, err := json.Marshal(billingdomain.Product{ProductID: "USAGE_REPAIR", AmountFen: 100,
		CreditMicrocredits: facts.Claimed, BasePoints: facts.Claimed, Currency: "CNY", PlanName: "UsageRepair"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',$3,1,$4,$4)`, []any{facts.WalletID, ownerID, facts.Claimed, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), facts.WalletID, "wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'USAGE_REPAIR',$5,'ALIPAY',100,'CNY',$6,'PAID','CREDITED',$7,$7,$8,$9,$10,$10,'sku-repair','test-provider','v1')`,
			[]any{orderID, "usage-repair-" + orderID.String(), ownerID, facts.WalletID, product, facts.Claimed,
				now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,$4,TRUE,$5,$6)`,
			[]any{lotID, facts.WalletID, "recharge:" + lotID.String(), facts.Claimed, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,
credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference,credit_lot_id)
VALUES($1,$2,$3,'USAGE_REPAIR','UsageRepair','sku-repair',100,$4,$4,$5,$5,$6,'ACTIVE',$5,$5,'SYSTEM',$7,$8)`,
			[]any{facts.PackageID, ownerID, facts.WalletID, facts.Claimed, now, now.Add(24 * time.Hour),
				"usage-repair:" + facts.PackageID.String(), lotID}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,$4,$4,$5,$6,'ACTIVE',$5,$5)`,
			[]any{facts.ComponentID, facts.PackageID, lotID, facts.Claimed, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO usage_holds(id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,usage_snapshot,
expires_at,terminal_at,created_at,updated_at)
VALUES($1,$2,$3,'RELEASED',$4,NULL,jsonb_build_object('release_reason','ENTITLEMENT_EXPIRED_UNCERTAIN'),$5,$5,$5,$5)`,
			[]any{facts.HoldID, facts.UsageRequestID, facts.WalletID, facts.Claimed, now}},
		{`INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,settled_microcredits,
released_microcredits,created_at,updated_at)
VALUES($1,$2,$3,$4,0,$4,$5,$5)`,
			[]any{facts.HoldID, facts.PackageID, facts.ComponentID, facts.Claimed, now}},
		{`INSERT INTO usage_loss_reviews(id,usage_request_id,hold_id,wallet_id,reason_code,claimed_actual_microcredits,
usage_snapshot,status,created_at,updated_at)
VALUES($1,$2,$3,$4,'ENTITLEMENT_EXPIRED_UNCERTAIN',$5,'{}','REVIEW_REQUIRED',$6,$6)`,
			[]any{facts.LossReviewID, facts.UsageRequestID, facts.HoldID, facts.WalletID, facts.Claimed, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("usage repair fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return facts
}
