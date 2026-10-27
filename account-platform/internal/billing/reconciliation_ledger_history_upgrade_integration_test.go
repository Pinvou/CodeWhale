package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Install the exact public 900031 prefix, create real retained financial facts,
// and verify that only the new case type becomes admissible at 900032.
func TestLedgerRepairProofHistoricalUpgradePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t, 900031)
	original, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	source := original.provider.(*retainedFinancialProvider).result
	if source.ChannelTransactionID == nil || source.PaidAt == nil {
		t.Fatal("fixture lacks paid provider facts")
	}
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: *source.ChannelTransactionID, ChannelTransactionID: *source.ChannelTransactionID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen + 1, Currency: order.Currency, Status: "SUCCESS", PaidAt: *source.PaidAt}
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: retainedFinancialProvider{result: source}, rows: []ProviderBillEntry{row}}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	run, err := coordinator.RunAdminReconciliation(ctx, uuid.New(), order.Channel, reconciliationFixtureDateForPaidAt(*source.PaidAt))
	if err != nil {
		t.Fatal("create real pre-upgrade reconciliation", err)
	}
	cases, err := r.AdminReconciliationCases(ctx, run.ID, 10)
	if err != nil || len(cases) != 1 || cases[0].CaseType != "AMOUNT_MISMATCH" {
		t.Fatal("real pre-upgrade case missing", len(cases), err)
	}
	caseID := cases[0].ID
	snapshot := func() string {
		t.Helper()
		var value string
		err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
          'wallets',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),'[]'),
          'lots',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),'[]'),
          'orders',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),'[]'),
          'transactions',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),'[]'),
          'packages',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),'[]'),
          'components',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),'[]'),
          'journals',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),'[]'),
          'postings',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),'[]'),
          'cases',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_cases x),'[]'),
          'runs',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_runs x),'[]'),
          'activation',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY command_type,policy_version) FROM billing_approval_command_activations x),'[]'),
          'history',coalesce((SELECT jsonb_agg(to_jsonb(x) ORDER BY command_type,policy_version,version) FROM billing_approval_command_activation_history x),'[]')
        )::text`).Scan(&value)
		if err != nil {
			t.Fatal("snapshot historical financial facts", err)
		}
		return value
	}
	acl := func() string {
		t.Helper()
		var value string
		err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
          'cases_owner',(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.reconciliation_cases'::regclass),
          'cases_select',has_table_privilege('billing_service','public.reconciliation_cases','SELECT'),
          'cases_insert',has_table_privilege('billing_service','public.reconciliation_cases','INSERT'),
          'cases_update',has_table_privilege('billing_service','public.reconciliation_cases','UPDATE'),
          'cases_delete',has_table_privilege('billing_service','public.reconciliation_cases','DELETE'),
          'cases_truncate',has_table_privilege('billing_service','public.reconciliation_cases','TRUNCATE'),
          'history_read',has_table_privilege('billing_service','public.billing_approval_command_activation_history','SELECT'),
          'history_write',has_table_privilege('billing_service','public.billing_approval_command_activation_history','INSERT,UPDATE,DELETE,TRUNCATE'),
          'owner_member',pg_has_role('billing_service','billing_schema_owner','MEMBER')
        )::text`).Scan(&value)
		if err != nil {
			t.Fatal("read runtime privilege boundary", err)
		}
		return value
	}
	factBefore, aclBefore := snapshot(), acl()
	var highest int64
	if err := r.pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&highest); err != nil || highest != 900031 {
		t.Fatal("not exact historical prefix", highest, err)
	}
	tryCaseType := func(want bool) {
		t.Helper()
		tx, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		tag, e := tx.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,created_at,updated_at) SELECT $2,reconciliation_run_id,payment_order_id,merchant_order_no,'LEDGER_REPAIR_PROOF_UNAVAILABLE','OPEN',now(),now() FROM reconciliation_cases WHERE id=$1`, caseID, uuid.New())
		if want && (e != nil || tag.RowsAffected() != 1) {
			t.Fatal("new type rejected or no row inserted", e)
		}
		if !want {
			var pg *pgconn.PgError
			if !errors.As(e, &pg) || pg.Code != "23514" {
				t.Fatal("new type unexpectedly accepted under old constraint", e)
			}
		}
	}
	tryCaseType(false)
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User, cfg.Password = "billing_migrator", ""
	operator, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	if _, err = operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var prefix []migrations.Migration
	for _, m := range all {
		if m.Version <= 900031 {
			prefix = append(prefix, m)
		}
	}
	if _, err = billingschema.VerifyApplied(ctx, operator, prefix); err != nil {
		t.Fatal("verify original migration ledger", err)
	}
	if err = billingschema.Up(ctx, operator, all); err != nil {
		t.Fatal("official upgrade", err)
	}
	if _, err = billingschema.VerifyApplied(ctx, operator, all); err != nil {
		t.Fatal("verify upgraded ledger", err)
	}
	if snapshot() != factBefore || acl() != aclBefore {
		t.Fatal("upgrade changed historical data or runtime permissions")
	}
	tryCaseType(true)
	if snapshot() != factBefore || acl() != aclBefore {
		t.Fatal("rolled-back type probe changed historical data or ACL")
	}
	var ledger string
	if err = r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err = billingschema.Up(ctx, operator, all); err != nil {
		t.Fatal("official replay", err)
	}
	if _, err = billingschema.VerifyApplied(ctx, operator, all); err != nil {
		t.Fatal("verify replay", err)
	}
	var after string
	if err = r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&after); err != nil || after != ledger || snapshot() != factBefore || acl() != aclBefore {
		t.Fatal("replay changed ledger or financial facts", err)
	}
	err = billingschema.DownOne(ctx, operator, all)
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != "55000" || rejected.Message != "ledger repair history proof is forward-only" {
		t.Fatal("unexpected down result", err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&after); err != nil || after != ledger || snapshot() != factBefore || acl() != aclBefore {
		t.Fatal("refused down changed data or privileges", err)
	}
	if _, err = billingschema.VerifyApplied(ctx, operator, all); err != nil {
		t.Fatal("verify after refused down", err)
	}
}
