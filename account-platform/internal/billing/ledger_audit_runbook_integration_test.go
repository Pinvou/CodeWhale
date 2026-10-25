package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ledgerAuditTables = []string{"ledger_postings", "ledger_accounts", "wallets", "usage_holds", "refunds", "payment_orders", "receivables", "receivable_allocations", "chargeback_cases", "payment_transactions", "entitlement_packages", "entitlement_components", "credit_lots", "usage_hold_allocations", "usage_hold_lease_events", "member_quota_grants", "member_quota_hold_allocations"}

func ledgerAuditFingerprint(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var result strings.Builder
	for _, table := range ledgerAuditTables {
		var snapshot string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM public.`+pgx.Identifier{table}.Sanitize()+` t`).Scan(&snapshot); err != nil {
			t.Fatal("owned audit table fingerprint failed", table, err)
		}
		result.WriteString(table + snapshot)
	}
	return result.String()
}

// Execute the actual checked-in operator script through psql, not a substitute
// query builder. Credentials are process environment only and never printed.
func ledgerAuditPSQL(t *testing.T, r *PostgresRepository, ctx context.Context, role, script string) ([]byte, error) {
	t.Helper()
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Fatal("psql is required for the actual audit-tool contract")
	}
	c := r.pool.Config().ConnConfig
	command := exec.CommandContext(ctx, psql, "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--no-align", "--tuples-only", "--field-separator=|", "--file", script)
	// Remove ambient libpq connection/service options before setting the owned
	// fixture identity. Never allow a developer's service file to select a site.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(strings.SplitN(value, "=", 2)[0]), "PG") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "PGHOST="+c.Host, fmt.Sprintf("PGPORT=%d", c.Port), "PGDATABASE="+c.Database, "PGUSER="+role, "PGPASSWORD=", "PGSSLMODE=disable", "PGCONNECT_TIMEOUT=5")
	return command.CombinedOutput()
}

func ledgerAuditCounts(t *testing.T, output []byte) map[string]int64 {
	t.Helper()
	counts := map[string]int64{}
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n < 0 || counts[parts[0]] != 0 {
			t.Fatal("invalid or duplicate audit count")
		}
		if _, exists := counts[parts[0]]; exists {
			t.Fatal("duplicate audit check")
		}
		counts[parts[0]] = n
	}
	if len(counts) != 18 {
		t.Fatal("audit did not complete all18 checks", len(counts))
	}
	return counts
}

func TestBillingLedgerAuditReadOnlyPublicPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	// Actual domain issuance/hold/overage/grant transitions on the complete
	// public migration chain; upstream payment facts are controlled.
	_, paid, _, _ := retainedFinanceOrder(t, r, ctx, 10000, true)
	active, err := r.Reserve(ctx, uuid.New(), paid.WalletID, 100, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	review, err := r.Reserve(ctx, uuid.New(), paid.WalletID, 200, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET status='REVIEW' WHERE id=$1`, review.ID); err != nil {
		t.Fatal(err)
	}
	overage, err := r.Reserve(ctx, uuid.New(), paid.WalletID, 50, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Commit(ctx, overage.UsageRequestID, overage.ID, 70, map[string]any{"controlled": true}); err != nil {
		t.Fatal(err)
	}
	_, orgOrder, _, orgPackage := retainedOrganizationFinanceOrder(t, r, ctx, 10000, true)
	member := uuid.New()
	grant := reviewExpiryGrant(t, r, ctx, orgOrder, orgPackage, member, 400)
	memberHold, err := r.ReserveMemberQuota(ctx, uuid.New(), orgOrder.WalletID, *orgOrder.BeneficiaryOrganizationID, member, 1, 100, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyOrganizationMemberLifecycle(ctx, *orgOrder.BeneficiaryOrganizationID, member, "SUSPENDED", 2); err != nil {
		t.Fatal(err)
	}
	// Keep an ACTIVE grant alongside a suspended grant, and a normally released
	// terminal hold alongside ACTIVE/REVIEW/COMMITTED reservations.
	reviewExpiryGrant(t, r, ctx, orgOrder, orgPackage, uuid.New(), 200)
	_, terminalOrder, _, _ := retainedFinanceOrder(t, r, ctx, 10000, true)
	released, err := r.Reserve(ctx, uuid.New(), terminalOrder.WalletID, 30, nil, r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Release(ctx, released.UsageRequestID, released.ID, "controlled audit release"); err != nil {
		t.Fatal(err)
	}
	coordinator, refundOrder, transaction, pack := retainedFinanceOrder(t, r, ctx, 10000, true)
	coordinator.AllowNormalRefundCreation(true)
	refund, _, err := coordinator.CreateRefund(ctx, refundOrder.ID, refundOrder.RequesterID, transaction, pack, uuid.NewString(), "controlled read-only audit source")
	if err != nil {
		t.Fatal(err)
	}
	_, _, chargeTransaction, _ := retainedFinanceOrder(t, r, ctx, 10000, true)
	chargeback, err := r.RecordConfirmedChargebackForReview(ctx, chargeTransaction, "controlled-audit-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := r.pool.Config().ConnConfig.Copy()
	adminConfig.User, adminConfig.Password = "postgres", ""
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal("owned audit administration connection failed")
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	role := "audit_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	grants := []string{}
	for _, table := range ledgerAuditTables {
		grants = append(grants, "public."+pgx.Identifier{table}.Sanitize())
	}
	tableList := strings.Join(grants, ",")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "REVOKE ALL ON "+tableList+" FROM "+quoted+"; REVOKE ALL ON SCHEMA public FROM "+quoted+"; DROP ROLE "+quoted); err != nil {
			t.Error("owned read-only role cleanup failed", err)
		}
	})
	if _, err = retainedFinanceOwnerExec(t, r, ctx, "GRANT USAGE ON SCHEMA public TO "+quoted+"; GRANT SELECT ON "+tableList+" TO "+quoted); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "deploy", "runbooks", "sql", "billing-ledger-audit.sql")
	run := func() map[string]int64 {
		t.Helper()
		before := ledgerAuditFingerprint(t, r, ctx)
		output, err := ledgerAuditPSQL(t, r, ctx, role, script)
		if err != nil {
			t.Fatal("actual read-only audit script failed", string(output))
		}
		if after := ledgerAuditFingerprint(t, r, ctx); before != after {
			t.Fatal("audit tool changed persisted financial rows")
		}
		return ledgerAuditCounts(t, output)
	}
	t.Run("complete-schema-real-select-only-reader-active-review-member-suspended-and-targeted-refund", func(t *testing.T) {
		for check, count := range run() {
			if count != 0 {
				t.Fatal("consistent controlled domain state was reported inconsistent", check, count)
			}
		}
	})
	t.Run("reader-cannot-write", func(t *testing.T) {
		var elevated bool
		if err := admin.QueryRow(ctx, `SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR r.rolreplication OR r.rolinherit OR has_schema_privilege(r.rolname,'public','CREATE') OR EXISTS(SELECT 1 FROM unnest($2::text[]) tab WHERE has_table_privilege(r.rolname,tab,'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER,REFERENCES')) FROM pg_roles r WHERE r.rolname=$1`, role, ledgerAuditTables).Scan(&elevated); err != nil || elevated {
			t.Fatal("reader has unexpected privilege", elevated, err)
		}
		probe := filepath.Join(t.TempDir(), "deny-write.sql")
		if err := os.WriteFile(probe, []byte("\\set ON_ERROR_STOP on\nUPDATE public.wallets SET version=version+1;\n"), 0600); err != nil {
			t.Fatal(err)
		}
		before := ledgerAuditFingerprint(t, r, ctx)
		if _, err := ledgerAuditPSQL(t, r, ctx, role, probe); err == nil {
			t.Fatal("read-only role/script accepted a write")
		}
		if before != ledgerAuditFingerprint(t, r, ctx) {
			t.Fatal("failed write probe changed rows")
		}
	})
	t.Run("same-session-temp-wallet-cannot-mask-public-ledger-difference", func(t *testing.T) {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `UPDATE public.wallets SET posted_microcredits=posted_microcredits+1 WHERE id='`+paid.WalletID.String()+`'`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `UPDATE public.wallets SET posted_microcredits=posted_microcredits-1 WHERE id='`+paid.WalletID.String()+`'`); err != nil {
				t.Error(err)
			}
		}()
		if count := run()["wallet_posted_snapshot_mismatch"]; count != 1 {
			t.Fatal("owned public wallet difference was not established", count)
		}
		absolute, err := filepath.Abs(script)
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(t.TempDir(), "same-session.sql")
		// PostgreSQL grants TEMP by default independently of table SELECT. Use
		// the actual reader and one psql backend: its own healthy TEMP copy must
		// not hide the damaged public row that the visibility guard validates.
		body := "\\set ON_ERROR_STOP on\nCREATE TEMP TABLE wallets AS SELECT * FROM public.wallets;\nUPDATE pg_temp.wallets SET posted_microcredits=posted_microcredits-1 WHERE id='" + paid.WalletID.String() + "';\n\\i '" + filepath.ToSlash(absolute) + "'\n"
		if err := os.WriteFile(wrapper, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		before := ledgerAuditFingerprint(t, r, ctx)
		output, err := ledgerAuditPSQL(t, r, ctx, role, wrapper)
		if err != nil {
			t.Fatal("actual same-session audit failed", string(output))
		}
		if before != ledgerAuditFingerprint(t, r, ctx) {
			t.Fatal("same-session audit changed persisted rows")
		}
		if count := ledgerAuditCounts(t, output)["wallet_posted_snapshot_mismatch"]; count != 1 {
			t.Fatal("healthy TEMP copy masked the real public difference", count)
		}
	})
	t.Run("controlled-legacy-refund-reservation-requested-and-review", func(t *testing.T) {
		// This models the existing legacy reservation marker on a current
		// UUID fixture. It is not claimed as a pre016 upgrade/migration test.
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `UPDATE refunds SET entitlement_snapshot=entitlement_snapshot||'{"migration_source":"LEGACY_ORDER_REFUND"}'::jsonb WHERE id='`+refund.ID.String()+`'; UPDATE wallets SET reserved_microcredits=reserved_microcredits+10000 WHERE id='`+refundOrder.WalletID.String()+`'`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `UPDATE refunds SET entitlement_snapshot=entitlement_snapshot-'migration_source',status='REQUESTED' WHERE id='`+refund.ID.String()+`'; UPDATE wallets SET reserved_microcredits=reserved_microcredits-10000 WHERE id='`+refundOrder.WalletID.String()+`'`); err != nil {
				t.Error(err)
			}
		}()
		for _, status := range []string{"REQUESTED", "MANUAL_REVIEW"} {
			if _, err := r.pool.Exec(ctx, `UPDATE refunds SET status=$2 WHERE id=$1`, refund.ID, status); err != nil {
				t.Fatal(err)
			}
			for check, count := range run() {
				if count != 0 {
					t.Fatal("consistent legacy reservation was omitted/double counted", status, check, count)
				}
			}
		}
	})
	t.Run("missing-select-fails-closed", func(t *testing.T) {
		before := ledgerAuditFingerprint(t, r, ctx)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, "REVOKE SELECT ON public.receivables FROM "+quoted); err != nil {
			t.Fatal(err)
		}
		if _, err := ledgerAuditPSQL(t, r, ctx, role, script); err == nil {
			t.Fatal("partial visibility passed as healthy")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, "GRANT SELECT ON public.receivables TO "+quoted); err != nil {
			t.Fatal(err)
		}
		if before != ledgerAuditFingerprint(t, r, ctx) {
			t.Fatal("failed permission check changed rows")
		}
	})
	t.Run("rls-filter-fails-closed", func(t *testing.T) {
		before := ledgerAuditFingerprint(t, r, ctx)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE wallets ENABLE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
		if _, err := ledgerAuditPSQL(t, r, ctx, role, script); err == nil {
			t.Fatal("RLS-hidden rows passed as healthy")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE wallets DISABLE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
		if before != ledgerAuditFingerprint(t, r, ctx) {
			t.Fatal("failed visibility check changed rows")
		}
	})
	// From here on only, emulate damaged restored rows in this UUID database.
	// Disable immutable/balance/renewal triggers, never production code or permissions.
	// Every fault has its inverse and must be observed by the actual reader SQL.
	faultTriggerTables := []string{"ledger_postings", "usage_hold_lease_events", "entitlement_components", "usage_holds"}
	for _, table := range faultTriggerTables {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+" DISABLE TRIGGER USER"); err != nil {
			t.Fatal(err)
		}
	}
	// A damaged restored snapshot can violate a CHECK even though ordinary
	// writers cannot. Remove only two identified conservation constraints in
	// this owned DB, then restore their exact definitions after all inverses.
	constraintRestore := []string{}
	for _, spec := range []struct{ table, bucket string }{{"entitlement_components", "available_microcredits"}, {"receivables", "settled_microcredits"}} {
		var name, definition string
		if err := r.pool.QueryRow(ctx, `SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid=$1::regclass AND contype='c' AND position('original_microcredits' IN pg_get_constraintdef(oid))>0 AND position($2 IN pg_get_constraintdef(oid))>0`, "public."+spec.table, spec.bucket).Scan(&name, &definition); err != nil {
			t.Fatal("owned conservation constraint identity not unique", err)
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, "ALTER TABLE "+pgx.Identifier{spec.table}.Sanitize()+" DROP CONSTRAINT "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		constraintRestore = append(constraintRestore, "ALTER TABLE "+pgx.Identifier{spec.table}.Sanitize()+" ADD CONSTRAINT "+pgx.Identifier{name}.Sanitize()+" "+definition)
	}
	var component, posting, receivable, lease uuid.UUID
	for _, query := range []struct {
		sql string
		id  *uuid.UUID
	}{
		{`SELECT id FROM entitlement_components WHERE package_id='` + orgPackage.String() + `' AND component_type='BASE'`, &component},
		{`SELECT id FROM ledger_postings ORDER BY id LIMIT 1`, &posting},
		{`SELECT id FROM receivables LIMIT 1`, &receivable},
		{`SELECT id FROM usage_hold_lease_events WHERE hold_id='` + active.ID.String() + `' ORDER BY created_at DESC,id DESC LIMIT 1`, &lease},
	} {
		if err = r.pool.QueryRow(ctx, query.sql).Scan(query.id); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ name, apply, restore, check string }{
		{"journal", `UPDATE ledger_postings SET delta_microcredits=delta_microcredits+1 WHERE id='` + posting.String() + `'`, `UPDATE ledger_postings SET delta_microcredits=delta_microcredits-1 WHERE id='` + posting.String() + `'`, "unbalanced_journal_entries"},
		{"wallet-posted", `UPDATE wallets SET posted_microcredits=posted_microcredits+1 WHERE id='` + paid.WalletID.String() + `'`, `UPDATE wallets SET posted_microcredits=posted_microcredits-1 WHERE id='` + paid.WalletID.String() + `'`, "wallet_posted_snapshot_mismatch"},
		{"wallet-reserved-review-included", `UPDATE wallets SET reserved_microcredits=reserved_microcredits+1 WHERE id='` + paid.WalletID.String() + `'`, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-1 WHERE id='` + paid.WalletID.String() + `'`, "wallet_reserved_snapshot_mismatch"},
		{"receivable", `UPDATE wallets SET receivable_microcredits=receivable_microcredits+1 WHERE id='` + paid.WalletID.String() + `'`, `UPDATE wallets SET receivable_microcredits=receivable_microcredits-1 WHERE id='` + paid.WalletID.String() + `'`, "wallet_receivable_snapshot_mismatch"},
		{"receivable-allocation", `UPDATE receivables SET settled_microcredits=settled_microcredits+1,outstanding_microcredits=outstanding_microcredits-1 WHERE id='` + receivable.String() + `'`, `UPDATE receivables SET settled_microcredits=settled_microcredits-1,outstanding_microcredits=outstanding_microcredits+1 WHERE id='` + receivable.String() + `'`, "receivable_allocation_mismatch"},
		{"receivable-conservation", `UPDATE receivables SET original_microcredits=original_microcredits+1 WHERE id='` + receivable.String() + `'`, `UPDATE receivables SET original_microcredits=original_microcredits-1 WHERE id='` + receivable.String() + `'`, "receivable_amount_mismatch"},
		{"chargeback-owner", `UPDATE chargeback_cases SET wallet_id='` + paid.WalletID.String() + `' WHERE id='` + chargeback.ID.String() + `'`, `UPDATE chargeback_cases SET wallet_id=(SELECT p.wallet_id FROM entitlement_packages p WHERE p.id=entitlement_package_id) WHERE id='` + chargeback.ID.String() + `'`, "chargeback_package_mismatch"},
		{"component-conservation", `UPDATE entitlement_components SET original_microcredits=original_microcredits+1 WHERE id='` + component.String() + `'`, `UPDATE entitlement_components SET original_microcredits=original_microcredits-1 WHERE id='` + component.String() + `'`, "entitlement_component_conservation_mismatch"},
		{"component-lot-package", `UPDATE entitlement_components SET available_microcredits=available_microcredits-1,expired_microcredits=expired_microcredits+1 WHERE id='` + component.String() + `'`, `UPDATE entitlement_components SET available_microcredits=available_microcredits+1,expired_microcredits=expired_microcredits-1 WHERE id='` + component.String() + `'`, "entitlement_component_lot_mismatch"},
		{"package-projection", `UPDATE entitlement_packages SET points_remaining=points_remaining-1,points_used=points_used+1 WHERE id='` + orgPackage.String() + `'`, `UPDATE entitlement_packages SET points_remaining=points_remaining+1,points_used=points_used-1 WHERE id='` + orgPackage.String() + `'`, "entitlement_package_projection_mismatch"},
		{"component-hold-allocation", `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits+1,available_microcredits=available_microcredits-1 WHERE id='` + component.String() + `'`, `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-1,available_microcredits=available_microcredits+1 WHERE id='` + component.String() + `'`, "entitlement_hold_allocation_mismatch"},
		{"hold-allocation", `UPDATE usage_holds SET max_microcredits=max_microcredits+1 WHERE id='` + active.ID.String() + `'`, `UPDATE usage_holds SET max_microcredits=max_microcredits-1 WHERE id='` + active.ID.String() + `'`, "usage_hold_allocation_mismatch"},
		{"expired-active-attention", `UPDATE usage_holds SET expires_at=expires_at-interval '2 hours' WHERE id='` + active.ID.String() + `'`, `UPDATE usage_holds SET expires_at=expires_at+interval '2 hours' WHERE id='` + active.ID.String() + `'`, "expired_active_holds"},
		{"latest-lease", `UPDATE usage_hold_lease_events SET expires_at=expires_at+interval '1 second' WHERE id='` + lease.String() + `'`, `UPDATE usage_hold_lease_events SET expires_at=expires_at-interval '1 second' WHERE id='` + lease.String() + `'`, "usage_hold_latest_lease_mismatch"},
		{"member-projection", `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits+1,available_microcredits=available_microcredits-1 WHERE id='` + component.String() + `'`, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits-1,available_microcredits=available_microcredits+1 WHERE id='` + component.String() + `'`, "member_grant_component_projection_mismatch"},
		{"member-reserved", `UPDATE member_quota_grants SET reserved_microcredits=reserved_microcredits+1,available_microcredits=available_microcredits-1 WHERE id='` + grant.ID.String() + `'`, `UPDATE member_quota_grants SET reserved_microcredits=reserved_microcredits-1,available_microcredits=available_microcredits+1 WHERE id='` + grant.ID.String() + `'`, "member_grant_reserved_projection_mismatch"},
		{"member-owner", `UPDATE usage_holds SET member_user_id='` + uuid.NewString() + `' WHERE id='` + memberHold.ID.String() + `'`, `UPDATE usage_holds SET member_user_id='` + member.String() + `' WHERE id='` + memberHold.ID.String() + `'`, "member_hold_source_identity_mismatch"},
		{"paid-review-attention", `UPDATE payment_orders SET fulfillment_status='MANUAL_REVIEW' WHERE id='` + paid.ID.String() + `'`, `UPDATE payment_orders SET fulfillment_status='CREDITED' WHERE id='` + paid.ID.String() + `'`, "paid_orders_not_credited"},
	}
	for _, item := range cases {
		t.Run("owned-corrupt-"+item.name, func(t *testing.T) {
			if _, err := retainedFinanceOwnerExec(t, r, ctx, item.apply); err != nil {
				t.Fatal("owned negative fixture mutation failed", err)
			}
			defer func() {
				if _, err := retainedFinanceOwnerExec(t, r, ctx, item.restore); err != nil {
					t.Error("owned negative fixture inverse failed", err)
				}
			}()
			if count := run()[item.check]; count == 0 {
				t.Fatal("actual audit failed to detect owned inconsistent fact", item.check)
			}
		})
	}
	for _, statement := range constraintRestore {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
			t.Fatal("owned exact constraint restoration failed", err)
		}
	}
	for _, table := range faultTriggerTables {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+" ENABLE TRIGGER USER"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("restored-all-row-faults-zero", func(t *testing.T) {
		for check, count := range run() {
			if count != 0 {
				t.Fatal("inverse did not restore controlled facts", check, count)
			}
		}
	})
	t.Run("nonzero-package-with-all-components-missing-is-compared", func(t *testing.T) {
		// A fresh normally issued package has no hold/grant/refund references.
		// Simulate a damaged restored component set only in this owned UUID DB.
		_, newOrder, _, newPackage := retainedFinanceOrder(t, r, ctx, 10000, true)
		var components string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id)::text FROM public.entitlement_components c WHERE c.package_id=$1`, newPackage).Scan(&components); err != nil {
			t.Fatal(err)
		}
		otherFacts := func() string {
			t.Helper()
			var result strings.Builder
			for _, table := range ledgerAuditTables {
				if table == "entitlement_components" {
					continue
				}
				var snapshot string
				if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM public.`+pgx.Identifier{table}.Sanitize()+` t`).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				result.WriteString(table + snapshot)
			}
			return result.String()
		}
		before := otherFacts()
		beforeAll := ledgerAuditFingerprint(t, r, ctx)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE public.entitlement_components DISABLE TRIGGER USER; DELETE FROM public.entitlement_components WHERE package_id='`+newPackage.String()+`'; ALTER TABLE public.entitlement_components ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			literal := "'" + strings.ReplaceAll(components, "'", "''") + "'::jsonb"
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `ALTER TABLE public.entitlement_components DISABLE TRIGGER USER; INSERT INTO public.entitlement_components SELECT (jsonb_populate_record(NULL::public.entitlement_components,value)).* FROM jsonb_array_elements(`+literal+`) item(value); ALTER TABLE public.entitlement_components ENABLE TRIGGER USER`); err != nil {
				t.Error("owned exact component restoration failed", err)
			}
			if beforeAll != ledgerAuditFingerprint(t, r, ctx) {
				t.Error("restored component rows differ from the original facts")
			}
		}()
		if before != otherFacts() {
			t.Fatal("component fault changed other financial facts")
		}
		var established bool
		if err := r.pool.QueryRow(ctx, `SELECT p.points_total=10000 AND p.points_remaining=10000 AND w.posted_microcredits=10000 AND NOT EXISTS(SELECT 1 FROM public.entitlement_components c WHERE c.package_id=p.id) FROM public.entitlement_packages p JOIN public.wallets w ON w.id=p.wallet_id WHERE p.id=$1 AND w.id=$2`, newPackage, newOrder.WalletID).Scan(&established); err != nil || !established {
			t.Fatal("nonzero package/missing component fault not established", err)
		}
		if count := run()["entitlement_package_projection_mismatch"]; count != 1 {
			t.Fatal("package without components was silently excluded", count)
		}
	})
	// The actual operator file must contain no data-changing statement. This
	// secondary check supplements, rather than substitutes for, the real role.
	body, err := os.ReadFile(script)
	if err != nil || !bytes.Contains(body, []byte("REPEATABLE READ READ ONLY")) || !bytes.HasSuffix(bytes.TrimSpace(body), []byte("ROLLBACK;")) {
		t.Fatal("operator transaction boundary lost")
	}
}

func TestBillingFinancialEventOutboxContractsPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 10000, true)
	_, pending, _, _ := retainedFinanceOrder(t, r, ctx, 10000, false)
	late, err := r.EscalateLatePaymentReview(ctx, pending.ID, "CLOSED")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err = r.ClaimLatePaymentReview(ctx, late.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = r.EscalateProviderBlind(ctx, late.ID, actor, "渠道异常", "outbox-blind-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RecordAndFreezeForcedRefund(ctx, transaction, "contract-forced-"+uuid.NewString(), 100, json.RawMessage(`{"controlled":true}`)); err != nil {
		t.Fatal(err)
	}
	// Candidate discovery deliberately uses the database clock. Set the owned
	// fixture's real deadline instead of pretending an application clock makes
	// a future row eligible; the actual worker still owns event emission.
	if _, err = r.pool.Exec(ctx, `UPDATE manual_review_cases SET resolution_due_at=clock_timestamp()-interval '1 hour',mitigation_due_at=clock_timestamp()+interval '1 hour' WHERE status='OPEN'`); err != nil {
		t.Fatal(err)
	}
	if count, err := r.EscalateOverdueManualReviews(ctx, 100); err != nil || count < 1 {
		t.Fatal("actual overdue domain writer did not produce evidence", count, err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node with installed account-platform contract dependencies is required")
	}
	types := []string{"billing.recharge.succeeded.v1", "billing.late-payment.review-required.v1", "billing.provider-blind.detected.v1", "billing.payment-anomaly.review-required.v1", "billing.manual-review.sla-escalated.v1"}
	for _, eventType := range types {
		t.Run(eventType, func(t *testing.T) {
			rows, err := r.pool.Query(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE event_type=$1 ORDER BY occurred_at,event_id`, eventType)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			before := ledgerAuditFingerprint(t, r, ctx)
			for rows.Next() {
				var event events.Event
				if err := rows.Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
					t.Fatal(err)
				}
				body, err := event.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				validate := func(body []byte) error {
					command := exec.CommandContext(ctx, node, filepath.Join("..", "..", "test", "fixtures", "financial-event-validator.mjs"))
					command.Stdin = bytes.NewReader(body)
					return command.Run()
				}
				if err := validate(body); err != nil {
					t.Fatal("actual persisted outbox -> production envelope -> exact schema rejected", eventType, err)
				}
				var changed map[string]any
				if err := json.Unmarshal(body, &changed); err != nil {
					t.Fatal(err)
				}
				changed["payload"].(map[string]any)["unknown_financial_fact"] = true
				invalid, err := json.Marshal(changed)
				if err != nil || validate(invalid) == nil {
					t.Fatal("unknown payload fact did not fail closed")
				}
				count++
			}
			if rows.Err() != nil || count == 0 {
				t.Fatal("missing actual emitted rows", eventType, rows.Err())
			}
			if before != ledgerAuditFingerprint(t, r, ctx) {
				t.Fatal("schema validation changed persisted domain facts")
			}
		})
	}
}
