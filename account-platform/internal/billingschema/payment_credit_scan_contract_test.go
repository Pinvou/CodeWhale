package billingschema

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Read the actual SQL passed by the production method, not a test copy of it.
func paymentCreditScanSQL(t *testing.T, file, method, prefix string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var matches []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != method {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(text, prefix) {
				matches = append(matches, text)
			}
			return true
		})
	}
	if len(matches) != 1 {
		t.Fatal("production scan SQL not unique", method, len(matches))
	}
	return matches[0]
}

// Reuse the real role/database lifecycle and original 180-second root budget.
// Plans here prove index availability, not a measured history-scale speedup.
func assertPaymentCreditScanContract(t *testing.T, ctx context.Context, ac, bc *pgx.Conn) {
	t.Helper()
	// Ordinary owned fixture inserts, with the original registration capability.
	// One closure per subject is the published UNIQUE(user_id) contract.
	if _, err := ac.Exec(ctx, `WITH capability AS MATERIALIZED (
 SELECT set_config('pinvou.registration_bonus_capable','1',true)
), inserted AS (
 INSERT INTO users(id,status,user_code,username,created_at,updated_at)
 SELECT gen_random_uuid(),'ACTIVE',generate_unique_user_code(),
 'credit_scan_'||n,clock_timestamp(),clock_timestamp()
 FROM generate_series(1,1024) n CROSS JOIN capability RETURNING id
)
INSERT INTO account_closure_requests(id,user_id,idempotency_key_digest,request_fingerprint,status,stage,reauthenticated_at,sla_expires_at,created_at,updated_at)
SELECT gen_random_uuid(),id,repeat('a',64),repeat('b',64),'IN_PROGRESS',
'FINANCIAL_CLEARANCE',clock_timestamp(),clock_timestamp()+interval '1 day',clock_timestamp(),clock_timestamp()
FROM inserted`); err != nil {
		t.Fatal("seed owned closure plan history", err)
	}
	if _, err := ac.Exec(ctx, `ANALYZE account_closure_requests`); err != nil {
		t.Fatal(err)
	}
	var uniqueUserIndex bool
	if err := ac.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_index i
 WHERE i.indrelid='public.account_closure_requests'::regclass AND i.indisunique
 AND i.indisvalid AND i.indnkeyatts=1 AND i.indpred IS NULL
 AND pg_catalog.pg_get_indexdef(i.indexrelid,1,true)='user_id')`).Scan(&uniqueUserIndex); err != nil || !uniqueUserIndex {
		t.Fatal("published closure user uniqueness index missing", uniqueUserIndex, err)
	}
	for _, c := range []*pgx.Conn{ac, bc} {
		var before, exact, after, actuallyDue bool
		var previousStatement time.Time
		// Cross the future boundary inside this one SQL statement. A moving
		// clock predicate would now include the third row; a statement cutoff
		// must retain it for the next scan. No application clock is involved.
		if err := c.QueryRow(ctx, `WITH boundary AS MATERIALIZED (
 SELECT statement_timestamp() AS at
), delayed AS MATERIALIZED (
 SELECT at,pg_sleep(0.05) FROM boundary
)
SELECT at-interval '1 microsecond'<=statement_timestamp(),
 at<=statement_timestamp(),
 at+interval '10 milliseconds'<=statement_timestamp(),
 at+interval '10 milliseconds'<clock_timestamp(),at FROM delayed`).Scan(&before, &exact, &after, &actuallyDue, &previousStatement); err != nil || !before || !exact || after || !actuallyDue {
			t.Fatal("statement cutoff before/equal/after boundary", before, exact, after, actuallyDue, err)
		}
		var fresh bool
		if err := c.QueryRow(ctx, `SELECT statement_timestamp()>$1`, previousStatement).Scan(&fresh); err != nil || !fresh {
			t.Fatal("later statement reused an earlier cutoff", fresh, err)
		}
	}
	plan := func(c *pgx.Conn, forceIndexAvailability bool, sql string, args ...any) string {
		t.Helper()
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Small fresh tables may correctly prefer sequential scans. Disable
		// that preference only in this rollback-only plan-availability check.
		if forceIndexAvailability {
			if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
				t.Fatal(err)
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		var conditions []string
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if text, ok := x["Index Cond"].(string); ok {
					conditions = append(conditions, text)
				}
				for _, child := range x {
					walk(child)
				}
			case []any:
				for _, child := range x {
					walk(child)
				}
			}
		}
		walk(parsed)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		return strings.Join(conditions, "\n")
	}
	admission := paymentCreditScanSQL(t, "../account/payment_credit_admissions.go", "AdmitPaymentCredit", "SELECT EXISTS(")
	// Do not force the planner for the allegedly unsupported historical lookup.
	if !strings.Contains(plan(ac, false, admission, uuid.New()), "user_id") {
		t.Fatal("existing closure user unique index unavailable to actual admission SQL")
	}
	accountPurge := paymentCreditScanSQL(t, "../account/payment_credit_admissions.go", "PurgeExpiredPaymentCreditAdmissions", "WITH candidates AS MATERIALIZED(")
	if strings.Count(accountPurge, "<=statement_timestamp()") != 2 || !strings.Contains(plan(ac, true, accountPurge, uuid.New()), "retained_until <= statement_timestamp()") {
		t.Fatal("Account records/proof cutoff not indexable in actual SQL")
	}
	billingRead := paymentCreditScanSQL(t, "../billing/payment_credit_retention.go", "PurgeExpiredPaymentCreditAttempts", "SELECT id,payer_user_id,")
	conditions := plan(bc, true, billingRead)
	if strings.Count(billingRead, "<=statement_timestamp()") != 2 || !strings.Contains(conditions, "next_retention_check_at <= statement_timestamp()") {
		t.Fatal("Billing candidate cutoff not indexable in actual SQL")
	}
	billingPurge := paymentCreditScanSQL(t, "../billing/payment_credit_retention.go", "PurgeExpiredPaymentCreditAttempts", "WITH candidates AS MATERIALIZED(")
	if strings.Count(billingPurge, "<=statement_timestamp()") != 3 || !strings.Contains(plan(bc, true, billingPurge, uuid.New(), []uuid.UUID{uuid.New()}), "retained_until <= statement_timestamp()") {
		t.Fatal("Billing post-RPC records/proof statement cutoff missing")
	}
}
