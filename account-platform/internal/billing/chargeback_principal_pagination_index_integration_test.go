package billing

import (
	"context"
	"encoding/json"
	"errors"
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

const principalPageIndexName = "chargeback_principal_evidence_case_page_idx"

// The production statement, including its projection and keyset predicate, is
// the plan input. A second hand-written SELECT would not detect caller drift.
func chargebackPrincipalPageStatement(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "chargeback_principal_http.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "ChargebackPrincipalEvidence" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Query" {
				return true
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatal("principal pagination query is no longer a literal")
			}
			statement, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			statements = append(statements, statement)
			return true
		})
	}
	if len(statements) != 1 || !strings.HasSuffix(strings.Join(strings.Fields(statements[0]), " "), "FROM chargeback_principal_evidence WHERE chargeback_case_id=$1 AND id>$2 ORDER BY id LIMIT $3") {
		t.Fatal("principal pagination statement changed its exact scope or ordering")
	}
	return statements[0]
}

func chargebackPrincipalPageIndex(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	var valid bool
	err := r.pool.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready AND NOT i.indisunique
 AND i.indnkeyatts=2 AND i.indnatts=2 AND i.indpred IS NULL AND am.amname='btree'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(n,ord)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.n ORDER BY k.ord)=ARRAY['chargeback_case_id','id']
 AND pg_get_userbyid(c.relowner)='billing_schema_owner'
 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
 WHERE i.indexrelid=to_regclass('public.chargeback_principal_evidence_case_page_idx')
 AND i.indrelid='public.chargeback_principal_evidence'::regclass`).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("principal pagination index shape/owner", valid, err)
	}
}

func chargebackPrincipalPageIndexProbe(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	chargebackPrincipalPageIndex(t, ctx, r)
	seed := func() ChargebackCase {
		_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
		var reference string
		if err := r.pool.QueryRow(ctx, `SELECT provider_transaction_id FROM payment_transactions WHERE id=$1`, transaction).Scan(&reference); err != nil {
			t.Fatal(err)
		}
		return principalSupplierHTTPFixture(t, ctx, r, transaction, "page-index-"+uuid.NewString(), "40", reference, "CNY")
	}
	selected, other := seed(), seed()
	before := principalMoneySnapshot(t, r, ctx)
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User, configuration.Password = "billing_migrator", ""
	owner, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owner.Close(cleanup); err != nil {
			t.Error("principal plan fixture connection did not close", err)
		}
	}()
	if _, err := owner.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	// Only the plan history is synthesized. Two real authenticated supplier
	// receipts provide the binding shape; these copied rows are not supplier
	// authority or evidence of 52,200 actual receipts/payments. Interleave cases
	// to model a global append-only history rather than precluster one case.
	if _, err := owner.Exec(ctx, `INSERT INTO public.chargeback_principal_evidence
 (id,chargeback_case_id,payment_transaction_id,provider_id,provider_version,provider_chargeback_id,
 provider_transaction_id,amount_fen,currency,supplier_evidence_reference,payload_digest,payload_snapshot,
 observation_digest,recorded_by_service,recorded_by_subject,received_at)
 SELECT md5(e.id::text||':plan:'||g::text)::uuid,e.chargeback_case_id,e.payment_transaction_id,e.provider_id,e.provider_version,
 e.provider_chargeback_id,e.provider_transaction_id,e.amount_fen,e.currency,'SYNTHETIC_INDEX_PLAN_FIXTURE',e.payload_digest,
 jsonb_build_object('fixture','synthetic-index-plan-only'),encode(sha256(convert_to('plan:'||g::text,'UTF8')),'hex'),
 e.recorded_by_service,e.recorded_by_subject,e.received_at
 FROM generate_series(1,52200) g JOIN public.chargeback_principal_evidence e
 ON e.chargeback_case_id=CASE WHEN g%261=0 THEN $1::uuid ELSE $2::uuid END`, selected.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `ANALYZE public.chargeback_principal_evidence`); err != nil {
		t.Fatal(err)
	}
	statement := chargebackPrincipalPageStatement(t)
	type planResult struct {
		Plan struct {
			Rows  int `json:"Actual Rows"`
			Hits  int `json:"Shared Hit Blocks"`
			Reads int `json:"Shared Read Blocks"`
		}
	}
	measure := func(query interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}, indexed bool) int {
		var raw json.RawMessage
		if err := query.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+statement, selected.ID, uuid.Nil, 18).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []planResult
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 || plans[0].Plan.Rows != 18 || strings.Contains(string(raw), principalPageIndexName) != indexed {
			t.Fatal("natural principal pagination plan/result", indexed, string(raw), err)
		}
		t.Logf("natural principal page indexed=%v plan=%s", indexed, raw)
		return plans[0].Plan.Hits + plans[0].Plan.Reads
	}
	probe, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := probe.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error("principal plan control did not roll back", err)
		}
	}()
	// This is a private uncommitted plan control, never a public Down or a
	// production recovery instruction. Rollback restores the exact index.
	if _, err := probe.Exec(ctx, `DROP INDEX public.chargeback_principal_evidence_case_page_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, `SET LOCAL ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	baseline := measure(probe, false)
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	indexed := measure(r.pool, true)
	if indexed >= baseline {
		t.Fatal("keyset index did not reduce controlled-history buffer work", baseline, indexed)
	}
	expected := map[uuid.UUID]bool{}
	rows, err := r.pool.Query(ctx, `SELECT id FROM public.chargeback_principal_evidence WHERE chargeback_case_id=$1`, selected.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		expected[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(expected) != 201 {
		t.Fatal("controlled selected-case inventory", len(expected), err)
	}
	after := uuid.Nil
	for len(expected) > 0 {
		page, err := r.ChargebackPrincipalEvidence(ctx, selected.ID, after, 17)
		if err != nil || len(page.Observations) == 0 || len(page.Observations) > 17 {
			t.Fatal("actual keyset page", err)
		}
		for _, raw := range page.Observations {
			var observation struct {
				ID          uuid.UUID `json:"id"`
				Transaction uuid.UUID `json:"payment_transaction_id"`
			}
			if json.Unmarshal(raw, &observation) != nil || !expected[observation.ID] || observation.Transaction != selected.PaymentTransactionID || strings.Compare(after.String(), observation.ID.String()) >= 0 {
				t.Fatal("page leaked another case, duplicated, skipped, or reordered evidence")
			}
			delete(expected, observation.ID)
			after = observation.ID
		}
		if (len(expected) == 0) != (page.NextAfter == nil) || (page.NextAfter != nil && *page.NextAfter != after) {
			t.Fatal("page cursor did not preserve the last returned identity")
		}
	}
	empty, err := r.ChargebackPrincipalEvidence(ctx, selected.ID, after, 17)
	if err != nil || len(empty.Observations) != 0 || empty.NextAfter != nil || before != principalMoneySnapshot(t, r, ctx) {
		t.Fatal("pagination end or plan probe changed financial facts", err)
	}
	chargebackPrincipalPageIndex(t, ctx, r)
}
