package token

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Resolve only literal/identifier/+ SQL expressions from the actual three
// production sources. An unfamiliar expression fails rather than becoming a
// test-maintained copy of the claim or finalization query.
func emergencyPlanSQL(t *testing.T, method string) string {
	t.Helper()
	files := map[string]*ast.File{}
	values := map[string]ast.Expr{}
	for _, name := range []string{"emergency_cancellation.go", "hold_expiry_unused_dispatch.go", "cancellation.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = file
		for _, decl := range file.Decls {
			if g, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range g.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok && len(v.Names) == 1 && len(v.Values) == 1 {
						values[v.Names[0].Name] = v.Values[0]
					}
				}
			}
		}
	}
	var eval func(ast.Expr, int) (string, bool)
	eval = func(e ast.Expr, depth int) (string, bool) {
		if depth > 16 {
			return "", false
		}
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				return s, err == nil
			}
		case *ast.Ident:
			if x, ok := values[v.Name]; ok {
				return eval(x, depth+1)
			}
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && len(v.Args) == 1 && sel.Sel.Name == "EncodeToString" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "hex" {
					if cast, ok := v.Args[0].(*ast.CallExpr); ok && len(cast.Args) == 1 {
						if a, ok := cast.Fun.(*ast.ArrayType); ok && a.Len == nil {
							if elem, ok := a.Elt.(*ast.Ident); ok && elem.Name == "byte" {
								if text, ok := eval(cast.Args[0], depth+1); ok {
									return hex.EncodeToString([]byte(text)), true
								}
							}
						}
					}
				}
			}

		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				a, ok := eval(v.X, depth+1)
				b, ok2 := eval(v.Y, depth+1)
				return a + b, ok && ok2
			}
		}
		return "", false
	}
	var found []string
	for _, decl := range files["emergency_cancellation.go"].Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == method {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && len(c.Args) >= 2 {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" {
						if s, ok := eval(c.Args[1], 0); ok {
							found = append(found, s)
						}
					}
				}
				return true
			})
		}
	}
	if len(found) != 1 {
		t.Fatal("production plan query not unique", method, len(found))
	}
	return found[0]
}

func assertEmergencyCancellationPlans(t *testing.T, f modelExpiryObservationFixture, sourceUsage, sourceTask uuid.UUID) {
	t.Helper()
	cfg := f.r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_migrator"
	cfg.Password = ""
	c, err := pgx.ConnectConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	tx, err := c.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Every benchmark row, changed claim and DDL operation rolls back. These are
	// constraint-valid physical history inputs, not a claim that 10,000 actual
	// provider cancellations or Billing operations were performed.
	if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	if seeded, err := tx.Exec(f.ctx, `WITH generated AS MATERIALIZED(SELECT n,gen_random_uuid() AS id FROM generate_series(1,10000) n),
 inserted AS (
 INSERT INTO usage_requests
 SELECT (jsonb_populate_record(NULL::usage_requests,to_jsonb(original)||jsonb_build_object(
 'id',g.id,'idempotency_key_digest',encode(digest(g.id::text,'sha256'),'hex')))).*
 FROM generated g CROSS JOIN usage_requests original WHERE original.id=$1 RETURNING id
 )
 INSERT INTO model_emergency_cancellations(id,policy_id,usage_request_id,status,attempts,retry_attempts,next_attempt_at,requested_at,updated_at)
 SELECT gen_random_uuid(),original.policy_id,u.id,
 CASE WHEN row_number() OVER(ORDER BY u.id)<=4000 THEN 'MANUAL_REVIEW'
      WHEN row_number() OVER(ORDER BY u.id)<=8000 THEN 'PENDING' ELSE 'CONVERGING' END,
 8,CASE WHEN row_number() OVER(ORDER BY u.id)<=4000 THEN 8 ELSE 0 END,
 CASE WHEN row_number() OVER(ORDER BY u.id) BETWEEN 4001 AND 6000 THEN $3::timestamptz+interval '1 hour' ELSE NULL END,
 CASE WHEN row_number() OVER(ORDER BY u.id) BETWEEN 4001 AND 6000 THEN original.requested_at-interval '1 day' ELSE original.requested_at END,original.updated_at
 FROM inserted u CROSS JOIN model_emergency_cancellations original WHERE original.id=$2`, sourceUsage, sourceTask, f.r.now()); err != nil || seeded.RowsAffected() != 10000 {
		t.Fatal("seed controlled physical cancellation history", err)
	}
	up, err := os.ReadFile("../../migrations/token/900010_emergency_cancellation_retry.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(up), "CREATE INDEX model_emergency_cancellations_claim_order_idx")
	if start < 0 {
		t.Fatal("candidate work index carrier missing")
	}
	ddl := string(up)[start:]
	if strings.Count(ddl, "CREATE INDEX") != 2 || strings.Count(ddl, ";") != 2 {
		t.Fatal("unexpected migration work-index tail")
	}

	if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner;
 DROP INDEX model_emergency_cancellations_claim_order_idx;
 DROP INDEX model_emergency_cancellations_converging_order_idx;
 ANALYZE model_emergency_cancellations; ANALYZE usage_requests; SET LOCAL ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	type metrics struct {
		Blocks           float64
		Rows             float64
		UsedOrderedIndex bool
	}
	explain := func(label, query string, args ...any) metrics {
		t.Helper()
		if _, err := tx.Exec(f.ctx, `SAVEPOINT benchmark_statement`); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := tx.QueryRow(f.ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+query, args...).Scan(&raw); err != nil {
			t.Fatal(label, err)
		}
		if _, err := tx.Exec(f.ctx, `ROLLBACK TO SAVEPOINT benchmark_statement; RELEASE SAVEPOINT benchmark_statement`); err != nil {
			t.Fatal(err)
		}
		var p []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || len(p) != 1 {
			t.Fatal(label, err)
		}
		number := func(key string) float64 {
			if x, ok := p[0].Plan[key].(float64); ok {
				return x
			}
			return 0
		}
		if !strings.HasPrefix(label, "WARM") {
			t.Logf("EMERGENCY_PLAN_%s %s", label, raw)
		}
		return metrics{number("Shared Hit Blocks") + number("Shared Read Blocks"), number("Actual Rows"), strings.Contains(string(raw), "model_emergency_cancellations_claim_order_idx") || strings.Contains(string(raw), "model_emergency_cancellations_converging_order_idx")}
	}
	claim := emergencyPlanSQL(t, "claimEmergencyCancellations")
	finalize := emergencyPlanSQL(t, "finalizeEmergencyCancellations")
	if _, err = tx.Exec(f.ctx, `PREPARE benchmark_claim AS `+claim); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `PREPARE benchmark_finalize AS `+finalize); err != nil {
		t.Fatal(err)
	}
	claimExecution := fmt.Sprintf("EXECUTE benchmark_claim('%s'::timestamptz,1,%d)", f.r.now().Format(time.RFC3339Nano), emergencyCancellationRetryBudget)
	for _, mode := range []string{"auto", "force_generic_plan"} {
		warmups := 5
		if mode == "force_generic_plan" {
			warmups = 1
		}
		if _, err = tx.Exec(f.ctx, `SELECT set_config('plan_cache_mode',$1,true)`, mode); err != nil {
			t.Fatal(err)
		}
		if mode != "auto" {
			if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner; DROP INDEX model_emergency_cancellations_claim_order_idx; DROP INDEX model_emergency_cancellations_converging_order_idx; SET LOCAL ROLE token_gateway`); err != nil {
				t.Fatal(err)
			}
		}
		for range warmups {
			explain("WARM_CLAIM_BEFORE", claimExecution)
			explain("WARM_FINALIZE_BEFORE", "EXECUTE benchmark_finalize(100)")
		}
		beforeClaim := explain(mode+"_CLAIM_BEFORE", claimExecution)
		beforeFinal := explain(mode+"_FINALIZE_BEFORE", "EXECUTE benchmark_finalize(100)")
		if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner;`+ddl+`SET LOCAL ROLE token_gateway`); err != nil {
			t.Fatal(err)
		}
		for range warmups {
			explain("WARM_CLAIM_AFTER", claimExecution)
			explain("WARM_FINALIZE_AFTER", "EXECUTE benchmark_finalize(100)")
		}
		afterClaim := explain(mode+"_CLAIM_AFTER", claimExecution)
		afterFinal := explain(mode+"_FINALIZE_AFTER", "EXECUTE benchmark_finalize(100)")
		if beforeClaim.Rows != afterClaim.Rows || afterClaim.Rows != 1 || beforeFinal.Rows != afterFinal.Rows || afterFinal.Rows != 100 || !afterClaim.UsedOrderedIndex || !afterFinal.UsedOrderedIndex || afterClaim.Blocks >= beforeClaim.Blocks || afterFinal.Blocks >= beforeFinal.Blocks {
			t.Fatal("ordered index hypothesis not established at controlled cardinality", mode, beforeClaim, afterClaim, beforeFinal, afterFinal)
		}
		var counters string
		if err = tx.QueryRow(f.ctx, `SELECT jsonb_agg(jsonb_build_object('name',name,'custom',custom_plans,'generic',generic_plans,'parameter_types',parameter_types::text) ORDER BY name)::text FROM pg_prepared_statements WHERE name IN ('benchmark_claim','benchmark_finalize')`).Scan(&counters); err != nil {
			t.Fatal(err)
		}
		t.Log("EMERGENCY_PLAN_CACHE", mode, counters)
	}
	var genericObserved bool
	if err = tx.QueryRow(f.ctx, `SELECT count(*)=2 AND bool_and(generic_plans>0) FROM pg_prepared_statements WHERE name IN ('benchmark_claim','benchmark_finalize')`).Scan(&genericObserved); err != nil || !genericObserved {
		t.Fatal("generic execution was not actually observed", err)
	}
	var counts string
	if err = tx.QueryRow(f.ctx, `SELECT jsonb_object_agg(status,n)::text FROM(SELECT status,count(*) n FROM model_emergency_cancellations GROUP BY status) x`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	t.Log("EMERGENCY_PLAN_CARDINALITY", counts)
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
}
