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

// reconciliationRepairCrossDomainFacts names the seeded provider bill row that
// never matched an internal order, together with its provider-side refund
// evidence.
type reconciliationRepairCrossDomainFacts struct {
	CaseID               uuid.UUID
	RunID                uuid.UUID
	EvidenceID           uuid.UUID
	MerchantOrderNo      string
	ChannelTransactionID string
	AmountFen            int64
	Currency             string
	BusinessDate         string
}

// newReconciliationRepairCrossDomainBillingFixture provisions a
// production-shaped Billing database for the cross-domain reconciliation
// closure gate. Activation stays exactly as deployed: the command remains the
// original INACTIVE v1/s1 row.
func newReconciliationRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, reconciliationRepairCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_reconciliation_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	facts := seedReconciliationRepairCrossDomainCase(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedReconciliationRepairCrossDomainCase writes the production-shaped
// difference the closed plan accepts: a completed reconciliation run with one
// OPEN PROVIDER_PAID_INTERNAL_MISSING case whose merchant order has no internal
// order, plus the provider bill refund row that disposed of the amount.
func seedReconciliationRepairCrossDomainCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) reconciliationRepairCrossDomainFacts {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	facts := reconciliationRepairCrossDomainFacts{
		CaseID: uuid.New(), RunID: uuid.New(), EvidenceID: uuid.New(),
		MerchantOrderNo:      "unmatched-" + uuid.NewString(),
		ChannelTransactionID: "channel-tx-" + uuid.NewString(),
		AmountFen:            1500, Currency: "CNY",
		BusinessDate: now.Format("2006-01-02"),
	}
	snapshot, err := json.Marshal(map[string]any{
		"entry_type": "PAYMENT", "merchant_order_no": facts.MerchantOrderNo,
		"channel_transaction_id": facts.ChannelTransactionID, "channel": "ALIPAY",
		"amount_fen": facts.AmountFen, "currency": facts.Currency, "status": "SUCCESS",
		"paid_at": now.Format(time.RFC3339Nano),
	})
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
		{`INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at,
provider_id,provider_version,source_mode)
VALUES($1,'ALIPAY',$2::date,'COMPLETED',1,1,$3,$3,'test-provider','v1','MANUAL_API')`,
			[]any{facts.RunID, facts.BusinessDate, now}},
		{`INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,
severity,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,created_at,updated_at)
VALUES($1,$2,NULL,$3,'PROVIDER_PAID_INTERNAL_MISSING','OPEN','P1',$4,NULL,NULL,NULL,$5,$5)`,
			[]any{facts.CaseID, facts.RunID, facts.MerchantOrderNo, snapshot, now}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,
evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,
provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,NULL,NULL,'BILL','test-provider','v1',$2,$3,$4,$5,$6,'CNY','REFUND_SUCCESS',$7,$7,$8,'{}','REVIEW_REQUIRED')`,
			[]any{facts.EvidenceID, "bill|REFUND|" + facts.MerchantOrderNo + "|" + now.Format(time.RFC3339Nano),
				facts.MerchantOrderNo, facts.ChannelTransactionID, "refund-" + uuid.NewString(),
				facts.AmountFen, now, strings.Repeat("ef", 32)}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("reconciliation repair fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return facts
}
