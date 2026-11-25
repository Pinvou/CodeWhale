package billing

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const providerHealthIndexName = "payment_provider_health_recent_idx"

func providerHealthWindowStatement(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "provider_health.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "observationCandidates" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			value, ok := node.(*ast.BasicLit)
			if !ok || value.Kind != token.STRING {
				return true
			}
			statement, err := strconv.Unquote(value.Value)
			if err == nil && strings.HasPrefix(statement, "SELECT DISTINCT provider_id,provider_version,capability,channel,route FROM payment_provider_health_observations ") {
				found = append(found, statement)
			}
			return true
		})
	}
	if len(found) != 1 {
		t.Fatal("one actual global discovery statement required", len(found))
	}
	return found[0]
}

func providerHealthIndex(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	var valid bool
	err := r.pool.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready AND NOT i.indisunique
 AND i.indnkeyatts=1 AND i.indnatts=6 AND i.indpred IS NULL AND i.indexprs IS NULL AND am.amname='btree'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey::smallint[]) WITH ORDINALITY k(n,ord)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.n ORDER BY k.ord)=ARRAY['received_at','provider_id','provider_version','capability','channel','route']
 AND pg_get_userbyid(c.relowner)='billing_schema_owner'
 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
 WHERE i.indexrelid=to_regclass('public.payment_provider_health_recent_idx')
 AND i.indrelid='public.payment_provider_health_observations'::regclass`).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("global health index shape/owner", valid, err)
	}
}

func providerHealthTableFacts(t *testing.T, ctx context.Context, r *PostgresRepository, tables ...string) string {
	t.Helper()
	var facts []string
	for _, table := range tables {
		var value string
		order := `COALESCE(to_jsonb(row_value)->>'id',to_jsonb(row_value)->>'event_id',to_jsonb(row_value)::text)`
		rowHash := `md5(to_jsonb(row_value)::text)`
		switch table {
		case "wallets", "credit_lots", "entitlement_packages", "entitlement_components":
			// These published tables have a non-null UUID primary key. Its
			// canonical text equals the first JSON key above, without another
			// full-row conversion for each large retained-history sort key.
			order = `row_value.id::text`
			// These fixed-schema carrier tables can hold 100000 rows. Retain
			// every row/column (including null/type/length information) in the
			// same-database fingerprint without rebuilding JSON at each check.
			rowHash = `md5(record_send(row_value))`
		}
		statement := `SELECT COALESCE(md5(string_agg(` + rowHash + `,'' ORDER BY ` + order + `)),'empty') FROM ` + pgx.Identifier{table}.Sanitize() + ` row_value`
		if err := r.pool.QueryRow(ctx, statement).Scan(&value); err != nil {
			t.Fatal(table, err)
		}
		facts = append(facts, value)
	}
	return strings.Join(facts, "|")
}

func providerHealthFinancialFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	return providerHealthTableFacts(t, ctx, r, "wallets", "payment_orders", "payment_transactions", "credit_lots", "entitlement_packages", "entitlement_components", "journal_entries", "ledger_postings")
}

func providerHealthFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	facts := providerHealthTableFacts(t, ctx, r, "payment_provider_attempts", "payment_provider_health_observations", "payment_stop_loss_evaluations", "payment_stop_loss_evaluation_members", "payment_provider_controls", "outbox_events")
	var metadata string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<>900045),
 (SELECT jsonb_agg(jsonb_build_array(relname,pg_get_userbyid(relowner),relacl::text) ORDER BY relname) FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','p','S','v','m')),
 (SELECT jsonb_agg(pg_get_indexdef(indexrelid) ORDER BY indexrelid::regclass::text) FROM pg_index WHERE indexrelid<>COALESCE(to_regclass('public.payment_provider_health_recent_idx'),0)),
 (SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgrelid,tgname) FROM pg_trigger WHERE NOT tgisinternal),
 (SELECT nspacl::text FROM pg_namespace WHERE oid='public'::regnamespace),
 pg_has_role('billing_service','billing_schema_owner','MEMBER'),has_schema_privilege('billing_service','public','CREATE'))::text`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	return facts + metadata + providerHealthFinancialFacts(t, ctx, r)
}

func providerHealthRecord(t *testing.T, ctx context.Context, r *PostgresRepository, order PaymentOrder, capability, route, outcome string) uuid.UUID {
	t.Helper()
	attempt, err := r.beginProviderAttempt(ctx, order.ProviderID, order.ProviderVersion, capability, order.Channel, route, providerCallScope(order))
	if err != nil {
		t.Fatal(err)
	}
	var providerErr error
	if outcome == "INFRASTRUCTURE_FAILURE" {
		providerErr = errors.New("controlled observation failure")
	}
	if err = r.finishProviderAttempt(ctx, attempt, outcome, providerErr); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT id FROM payment_provider_health_observations WHERE provider_attempt_id=$1`, attempt.ProviderAttemptID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func providerHealthHistory(t *testing.T, ctx context.Context, owner *pgx.Conn, base time.Time) {
	t.Helper()
	_, err := owner.Exec(ctx, `WITH seed AS MATERIALIZED (
 SELECT gen_random_uuid() id,g FROM generate_series(1,100000) g),
 attempts AS (
 INSERT INTO payment_provider_attempts(id,provider_attempt_id,request_fingerprint,provider_id,provider_version,capability,channel,route,status,created_at,result_recorded_at)
 SELECT id,id::text,repeat('a',64),'history-provider-'||(g%1000),'v1',CASE WHEN g%2=0 THEN 'ORDER_QUERY' ELSE 'REFUND' END,
 'ALIPAY','history-route','RESULT_RECORDED',$1::timestamptz-interval '90 days',$1::timestamptz-interval '90 days'
 FROM seed RETURNING id,provider_attempt_id,provider_id,provider_version,capability,channel,route,created_at)
 INSERT INTO payment_provider_health_observations(id,provider_attempt_id,provider_id,provider_version,capability,channel,route,outcome,classification_rule_version,evidence_digest,received_at)
 SELECT id,provider_attempt_id,provider_id,provider_version,capability,channel,route,'SUCCESS','payment-adapter-outcome/v1',repeat('b',64),created_at FROM attempts`, base)
	if err != nil {
		t.Fatal("controlled archived observation history", err)
	}
}

type providerHealthQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func providerHealthPartitions(t *testing.T, ctx context.Context, q providerHealthQuery, statement string, now time.Time) map[[5]string]bool {
	t.Helper()
	rows, err := q.Query(ctx, statement, now)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	values := map[[5]string]bool{}
	for rows.Next() {
		var row [5]string
		if err = rows.Scan(&row[0], &row[1], &row[2], &row[3], &row[4]); err != nil {
			t.Fatal(err)
		}
		values[row] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func providerHealthMeasure(t *testing.T, ctx context.Context, owner *pgx.Conn, statement string, now time.Time, indexed bool, want map[[5]string]bool) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if !indexed {
		if _, err = tx.Exec(ctx, `DROP INDEX public.payment_provider_health_recent_idx`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	phase := "baseline"
	if indexed {
		phase = "indexed"
	}
	for repeat := 1; repeat <= 3; repeat++ {
		got := providerHealthPartitions(t, ctx, tx, statement, now)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("complete partition set/time boundary/capability", phase, got, want)
		}
		var raw json.RawMessage
		if err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+statement, now).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), providerHealthIndexName) != indexed {
			t.Fatal("natural global observation plan", phase, string(raw))
		}
		t.Logf("PROVIDER_HEALTH_GLOBAL_PLAN phase=%s repeat=%d %s", phase, repeat, raw)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProviderHealthGlobalWindowIndexPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("actual owned PostgreSQL required")
	}
	product := providerHealthMigrationTool(t)
	statement := providerHealthWindowStatement(t)
	for _, scenario := range []string{"fresh-install", "retained-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			// Isolate main's exact45 Down and unchanged038 upgrade first.
			// The complete branch carrier is installed on these same facts below.
			var source, staged string
			if scenario == "retained-upgrade" {
				source, staged = stageBillingMigrationContract(t, 900045, 900045)
			} else {
				source, staged = stageBillingMigrationContract(t, 900045)
			}
			var r *PostgresRepository
			var ctx context.Context
			if scenario == "retained-upgrade" {
				r, ctx = retainedFinanceRepositoryWithBudgets(t, 3*time.Minute, 45*time.Second, 900032)
			} else {
				r, ctx = retainedFinanceRepositoryWithBudgets(t, 3*time.Minute, 45*time.Second)
			}
			base := time.Now().UTC().Truncate(time.Second)
			old := base.Add(-24 * time.Hour)
			r.now = func() time.Time { return old }
			var orders []PaymentOrder
			if scenario == "fresh-install" {
				before := providerHealthFinancialFacts(t, ctx, r)
				if !errors.Is(requireProviderHealth038Schema(ctx, r.pool), errProviderHealth038Fixture) || providerHealthFinancialFacts(t, ctx, r) != before {
					t.Fatal("historical038 producer must not admit or alter current-schema funds")
				}
			}
			for n := 0; n < 5; n++ {
				_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
				orders = append(orders, order)
			}
			if scenario == "retained-upgrade" {
				// The frozen pre-admission writer remains restricted to31/32.
				// Create real paid facts there, then install the real038 prefix
				// before taking the original health-upgrade snapshot.
				installBillingFixturePrefix(t, ctx, r, 900038)
			}
			originalFinancial := providerHealthFinancialFacts(t, ctx, r)
			for n := 0; n < 5; n++ {
				providerHealthRecord(t, ctx, r, orders[n%3], "ORDER_QUERY", "old-route", "INFRASTRUCTURE_FAILURE")
			}
			if count, err := r.EvaluateProviderStopLosses(ctx, 10); err != nil || count != 1 {
				t.Fatal("original historical stop-loss producer", count, err)
			}
			r.now = func() time.Time { return base }
			wantedIDs := map[uuid.UUID]bool{}
			for n := 0; n < 5; n++ {
				wantedIDs[providerHealthRecord(t, ctx, r, orders[n%3], "ORDER_QUERY", "current-route", "INFRASTRUCTURE_FAILURE")] = true
			}
			for n := 0; n < 5; n++ {
				providerHealthRecord(t, ctx, r, orders[n%3], "ORDER_QUERY", "business-route", "BUSINESS_REJECTION")
				providerHealthRecord(t, ctx, r, orders[n%3], "REFUND", "refund-route", "INFRASTRUCTURE_FAILURE")
			}
			rateIDs := map[uuid.UUID]bool{}
			for n := 0; n < 20; n++ {
				outcome := "SUCCESS"
				if n%2 == 0 {
					outcome = "INFRASTRUCTURE_FAILURE"
				}
				rateIDs[providerHealthRecord(t, ctx, r, orders[n%5], "ORDER_QUERY", "rate-route", outcome)] = true
			}
			// The 21st raw rate sample must not dilute the 20-sample 50% denominator.
			providerHealthRecord(t, ctx, r, orders[0], "ORDER_QUERY", "rate-route", "BUSINESS_REJECTION")
			boundary := base.Add(-5 * time.Minute)
			r.now = func() time.Time { return boundary }
			providerHealthRecord(t, ctx, r, orders[0], "ORDER_QUERY", "exact-window-edge", "INFRASTRUCTURE_FAILURE")
			r.now = func() time.Time { return base }
			configuration := r.pool.Config().ConnConfig.Copy()
			configuration.User, configuration.Password = "billing_migrator", ""
			owner, err := pgx.ConnectConfig(ctx, configuration)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			if _, err = owner.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
				t.Fatal(err)
			}
			providerHealthHistory(t, ctx, owner, base)
			before := providerHealthFacts(t, ctx, r)
			if scenario == "retained-upgrade" {
				if output, err := providerHealthCLI(ctx, product, r, "billing_service", "up"); err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
					t.Fatal("runtime Up refusal boundary", err, string(output))
				}
				if providerHealthFacts(t, ctx, r) != before {
					t.Fatal("refused runtime Up changed evidence/authority")
				}
				if output, err := providerHealthCLI(ctx, product, r, "billing_migrator", "up"); err != nil {
					t.Fatalf("actual retained CLI Up: %v\n%s", err, output)
				}
			}
			providerHealthIndex(t, ctx, r)
			if providerHealthFacts(t, ctx, r) != before {
				t.Fatal("installation changed financial/observation/member/ledger/ACL facts")
			}
			if output, err := providerHealthCLI(ctx, product, r, "billing_migrator", "up"); err != nil {
				t.Fatalf("actual CLI replay: %v\n%s", err, output)
			}
			for _, actor := range []string{"billing_service", "billing_migrator"} {
				if output, err := providerHealthCLI(ctx, product, r, actor, "down"); err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
					t.Fatal("forward-only Down boundary", actor, err, string(output))
				}
				providerHealthIndex(t, ctx, r)
			}
			if providerHealthFacts(t, ctx, r) != before {
				t.Fatal("replay/refused Down changed retained evidence")
			}
			if _, err = owner.Exec(ctx, `ANALYZE payment_provider_attempts; ANALYZE payment_provider_health_observations`); err != nil {
				t.Fatal(err)
			}
			want := map[[5]string]bool{{"controlled", "v1", "ORDER_QUERY", "ALIPAY", "current-route"}: true, {"controlled", "v1", "ORDER_QUERY", "ALIPAY", "business-route"}: true, {"controlled", "v1", "ORDER_QUERY", "ALIPAY", "rate-route"}: true}
			providerHealthMeasure(t, ctx, owner, statement, base, false, want)
			providerHealthIndex(t, ctx, r)
			providerHealthMeasure(t, ctx, owner, statement, base, true, want)
			if got := providerHealthPartitions(t, ctx, r.pool, statement, base); !reflect.DeepEqual(got, want) {
				t.Fatal("runtime full discovery set", got, want)
			}
			if providerHealthFacts(t, ctx, r) != before {
				t.Fatal("plan probe changed retained facts")
			}
			candidates, err := r.observationCandidates(ctx, base)
			if err != nil || len(candidates) != 2 {
				t.Fatal("original per-partition derivation", candidates, err)
			}
			seen := map[string]bool{}
			for _, candidate := range candidates {
				if candidate.ProviderID != "controlled" || candidate.ProviderVersion != "v1" || candidate.Capability != "ORDER_QUERY" || candidate.Channel != "ALIPAY" {
					t.Fatal("partition identity changed", candidate)
				}
				actualIDs := map[uuid.UUID]bool{}
				for _, id := range candidate.ObservationIDs {
					actualIDs[id] = true
				}
				switch candidate.Route {
				case "current-route":
					if candidate.TriggerType != "CONSECUTIVE_FAILURES" || candidate.DistinctOrders != 3 || candidate.FailureCount != 5 || candidate.SampleCount != 5 || !reflect.DeepEqual(actualIDs, wantedIDs) {
						t.Fatal("original consecutive threshold/members", candidate, actualIDs, wantedIDs)
					}
				case "rate-route":
					if candidate.TriggerType != "FAILURE_RATE" || candidate.DistinctOrders != 5 || candidate.FailureCount != 10 || candidate.SampleCount != 20 || !reflect.DeepEqual(actualIDs, rateIDs) {
						t.Fatal("original rate threshold/members", candidate, actualIDs, rateIDs)
					}
				default:
					t.Fatal("unqualified partition produced stop-loss", candidate)
				}
				seen[candidate.Route] = true
			}
			if len(seen) != 2 {
				t.Fatal("qualified partition omitted", seen)
			}
			if count, err := r.EvaluateProviderStopLosses(ctx, 10); err != nil || count != 0 {
				t.Fatal("already stopped provider was reopened/reapplied", count, err)
			}
			var enabled bool
			var version int64
			var evaluations, members int
			if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version,(SELECT count(*) FROM payment_stop_loss_evaluations),(SELECT count(*) FROM payment_stop_loss_evaluation_members) FROM payment_provider_controls WHERE provider_id='controlled' AND provider_version='v1'`).Scan(&enabled, &version, &evaluations, &members); err != nil || enabled || version != 1 || evaluations != 3 || members != 30 {
				t.Fatal("original historical/current proof and stop-loss fence", enabled, version, evaluations, members, err)
			}
			after := providerHealthFacts(t, ctx, r)
			if count, err := r.EvaluateProviderStopLosses(ctx, 10); err != nil || count != 0 || providerHealthFacts(t, ctx, r) != after {
				t.Fatal("next evaluation duplicated effects/evidence", count, err)
			}
			if providerHealthFinancialFacts(t, ctx, r) != originalFinancial {
				t.Fatal("observation pipeline changed actual payment financial facts")
			}
			completeBillingMigrationContract(t, ctx, r, source, staged, product)
			t.Logf("PROVIDER_HEALTH_GLOBAL_VERIFIED scenario=%s synthetic_archived_observations=100000 source_observations=42 global_partitions=3 current_candidates=2 retained_evaluation_members=30", scenario)
		})
	}
}
