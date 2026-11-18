package billing

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const packageExpiryIndexName = "entitlement_packages_expiry_scan_idx"

// Read the actual candidate statement without changing the production worker
// for test instrumentation. The expected ordering/result is checked separately.
func packageExpiryCandidateStatement(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "packages.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "expireOnePackage" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil && strings.Contains(value, "FROM entitlement_packages package") {
				found = append(found, value)
			}
			return true
		})
	}
	if len(found) != 1 {
		t.Fatal("one actual expiry candidate statement required", len(found))
	}
	return found[0]
}

func packageExpiryIndex(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	var valid bool
	err := r.pool.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready AND NOT i.indisunique
 AND i.indnkeyatts=3 AND i.indnatts=4 AND i.indexprs IS NULL AND am.amname='btree'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey::smallint[]) WITH ORDINALITY k(n,ord)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.n ORDER BY k.ord)=ARRAY['valid_until','credited_at','id','wallet_id']
 AND pg_get_expr(i.indpred,i.indrelid)='(status = ''ACTIVE''::text)'
 AND pg_get_userbyid(c.relowner)='billing_schema_owner'
 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
 WHERE i.indexrelid=to_regclass('public.entitlement_packages_expiry_scan_idx')
 AND i.indrelid='public.entitlement_packages'::regclass`).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("expiry index shape/owner", valid, err)
	}
}

func packageExpiryFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	return packageExpiryFactsWithOrder(t, ctx, r, false)
}

// Keep the original JSON-derived ordering as an independent query reference.
// Both paths hash every column of every row and retain all metadata checks.
func packageExpiryFactsWithOrder(t *testing.T, ctx context.Context, r *PostgresRepository, originalJSONOrder bool) string {
	t.Helper()
	var facts []string
	for _, table := range []string{"wallets", "credit_lots", "entitlement_packages", "entitlement_components", "journal_entries", "ledger_postings", "outbox_events", "payment_orders", "payment_transactions"} {
		var value string
		// The digest is a test fingerprint, not a security or production attestation.
		// These UUID primary keys are non-null and unique. Their text order is
		// the original JSON text order, without serializing each wide row twice
		// more just to obtain its sort key. Outbox uses event_id instead of id.
		key := "id"
		if table == "outbox_events" {
			key = "event_id"
		}
		query := `SELECT COALESCE(md5(string_agg(md5(to_jsonb(row_value)::text),'' ORDER BY row_value.` + pgx.Identifier{key}.Sanitize() + `::text)),'empty') FROM ` + pgx.Identifier{table}.Sanitize() + ` row_value`
		if originalJSONOrder {
			query = `SELECT COALESCE(md5(string_agg(md5(to_jsonb(row_value)::text),'' ORDER BY to_jsonb(row_value)->>'id',to_jsonb(row_value)->>'event_id')),'empty') FROM ` + pgx.Identifier{table}.Sanitize() + ` row_value`
		}
		if err := r.pool.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatal(table, err)
		}
		facts = append(facts, value)
	}
	var metadata string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<>900043),
 (SELECT jsonb_agg(jsonb_build_array(relname,pg_get_userbyid(relowner),relacl::text) ORDER BY relname) FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind='r'),
 (SELECT jsonb_agg(pg_get_indexdef(indexrelid) ORDER BY indexrelid::regclass::text) FROM pg_index WHERE indexrelid<>COALESCE(to_regclass('public.entitlement_packages_expiry_scan_idx'),0)),
 (SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgrelid,tgname) FROM pg_trigger WHERE NOT tgisinternal),
 (SELECT nspacl::text FROM pg_namespace WHERE oid='public'::regnamespace),
 pg_has_role('billing_service','billing_schema_owner','MEMBER'),has_schema_privilege('billing_service','public','CREATE'))::text`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	return strings.Join(facts, "|") + metadata
}

// Only scale history is synthesized. The four packages used by the real worker
// below come from the original payment merge and forced-refund freeze writers.
func packageExpiryHistory(t *testing.T, ctx context.Context, owner *pgx.Conn, base time.Time) {
	t.Helper()
	_, err := owner.Exec(ctx, `WITH seed AS MATERIALIZED (
 SELECT gen_random_uuid() id,g,g%10=0 active FROM generate_series(1,100000) g),
 wallets_created AS (
 INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at)
 SELECT id,'PERSONAL',id,'ACTIVE',CASE WHEN active THEN 1 ELSE 0 END,$1,$1 FROM seed RETURNING id),
 lots_created AS (
 INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
 SELECT s.id,s.id,'PROMOTION','expiry-index-history:'||s.id,1,CASE WHEN active THEN 1 ELSE 0 END,false,
 $1::timestamptz+CASE WHEN active THEN interval '60 days' ELSE interval '1 day' END,$1
 FROM seed s JOIN wallets_created w ON w.id=s.id RETURNING id),
 packages_created AS (
 INSERT INTO entitlement_packages(id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,points_expired,credited_at,valid_from,valid_until,status,source_type,source_reference,created_at,updated_at)
 SELECT s.id,s.id,s.id,'SYSTEM','controlled expiry index history','',0,1,CASE WHEN active THEN 1 ELSE 0 END,
 CASE WHEN active THEN 0 ELSE 1 END,$1,$1,$1::timestamptz+CASE WHEN active THEN interval '60 days' ELSE interval '1 day' END,
 CASE WHEN active THEN 'ACTIVE' ELSE 'EXPIRED' END,'SYSTEM',s.id::text,$1,$1
 FROM seed s JOIN lots_created l ON l.id=s.id RETURNING id)
 INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,expired_microcredits,credited_at,expires_at,status,created_at,updated_at)
 SELECT s.id,s.id,'BASE',s.id,1,CASE WHEN active THEN 1 ELSE 0 END,CASE WHEN active THEN 0 ELSE 1 END,
 $1,$1::timestamptz+CASE WHEN active THEN interval '60 days' ELSE interval '1 day' END,
 CASE WHEN active THEN 'ACTIVE' ELSE 'EXPIRED' END,$1,$1
 FROM seed s JOIN packages_created p ON p.id=s.id`, base)
	if err != nil {
		t.Fatal("controlled cross-wallet history", err)
	}
}

func packageExpiryMeasure(t *testing.T, ctx context.Context, owner *pgx.Conn, statement string, now time.Time, indexed bool, want uuid.UUID) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if !indexed {
		if _, err = tx.Exec(ctx, `DROP INDEX public.entitlement_packages_expiry_scan_idx`); err != nil {
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
		var got, wallet uuid.UUID
		if err = tx.QueryRow(ctx, statement, now).Scan(&got, &wallet); err != nil || got != want {
			t.Fatal("actual complete candidate ordering", phase, got, want, err)
		}
		var plan json.RawMessage
		if err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+statement, now).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(plan), packageExpiryIndexName) != indexed {
			t.Fatal("natural complete query plan", phase, string(plan))
		}
		t.Logf("PACKAGE_EXPIRY_PLAN phase=%s repeat=%d %s", phase, repeat, plan)
	}
	// The baseline index removal is rolled back, never applied as production Down.
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPackageExpiryCrossWalletIndexPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("actual owned PostgreSQL required")
	}
	product := packageExpiryMigrationTool(t)
	statement := packageExpiryCandidateStatement(t)
	for _, scenario := range []string{"fresh-install", "retained-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			var source, staged string
			if scenario == "retained-upgrade" {
				// Keep the historical 038 carrier plus only the target index.
				// The final full-carrier check still restores every source SQL file.
				source, staged = stageBillingMigrationContract(t, 900043, 900043)
			} else {
				source, staged = stageBillingMigrationContract(t, 900043)
			}
			var r *PostgresRepository
			var ctx context.Context
			if scenario == "retained-upgrade" {
				r, ctx = retainedFinanceRepositoryWithBudgets(t, 3*time.Minute, 45*time.Second, 900032)
			} else {
				r, ctx = retainedFinanceRepositoryWithBudgets(t, 3*time.Minute, 45*time.Second)
			}
			base := time.Now().UTC().Truncate(time.Second)
			r.now = func() time.Time { return base }
			var packs []uuid.UUID
			wallets := map[uuid.UUID]uuid.UUID{}
			var frozen uuid.UUID
			for n := 0; n < 4; n++ {
				_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
				wallets[pack] = order.WalletID
				if n == 3 {
					if _, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "expiry-index-retained-freeze", 100, nil); err != nil {
						t.Fatal(err)
					}
					frozen = pack
				} else {
					packs = append(packs, pack)
				}
			}
			if scenario == "retained-upgrade" {
				// Preserve the real historical paid and frozen facts through 038
				// before testing the target-only index migration.
				installBillingFixturePrefix(t, ctx, r, 900038)
			}
			sort.Slice(packs, func(i, j int) bool { return packs[i].String() < packs[j].String() })
			config := r.pool.Config().ConnConfig.Copy()
			config.User, config.Password = "billing_migrator", ""
			owner, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			if _, err = owner.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
				t.Fatal(err)
			}
			packageExpiryHistory(t, ctx, owner, base)
			before := packageExpiryFacts(t, ctx, r)
			// Compare the full seeded carrier with the original query once per
			// scenario; all later financial/authority checkpoints remain intact.
			if reference := packageExpiryFactsWithOrder(t, ctx, r, true); reference != before {
				t.Fatal("full-row facts differ from original JSON ordering")
			}
			if scenario == "retained-upgrade" {
				if output, err := packageExpiryCLI(ctx, product, r, "billing_service", "up"); err == nil {
					t.Fatal("runtime installed index", string(output))
				} else if !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
					t.Fatal("runtime refusal did not reach the actual migration boundary", err, string(output))
				}
				if packageExpiryFacts(t, ctx, r) != before {
					t.Fatal("refused runtime Up changed facts or authority")
				}
				if output, err := packageExpiryCLI(ctx, product, r, "billing_migrator", "up"); err != nil {
					t.Fatalf("actual retained CLI Up: %v\n%s", err, output)
				}
			}
			packageExpiryIndex(t, ctx, r)
			if packageExpiryFacts(t, ctx, r) != before {
				t.Fatal("index upgrade changed financial/old ledger/ACL facts")
			}
			if output, err := packageExpiryCLI(ctx, product, r, "billing_migrator", "up"); err != nil {
				t.Fatalf("CLI replay: %v\n%s", err, output)
			}
			if packageExpiryFacts(t, ctx, r) != before {
				t.Fatal("index replay changed retained facts")
			}
			for _, actor := range []string{"billing_service", "billing_migrator"} {
				if output, err := packageExpiryCLI(ctx, product, r, actor, "down"); err == nil {
					t.Fatal("forward-only index Down succeeded", actor, string(output))
				} else if !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
					t.Fatal("Down refusal did not reach the actual migration boundary", actor, err, string(output))
				}
				packageExpiryIndex(t, ctx, r)
				if packageExpiryFacts(t, ctx, r) != before {
					t.Fatal("refused Down changed facts", actor)
				}
			}
			// The CLI deliberately redacts database details. Check the precise
			// public API error separately without weakening that log boundary.
			loaded, err := migrations.Load("../../migrations/billing")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = owner.Exec(ctx, `SET ROLE billing_service`); err != nil {
				t.Fatal(err)
			}
			refused := billingschema.DownOne(ctx, owner, loaded)
			if _, err = owner.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
				t.Fatal(err)
			}
			var pgError *pgconn.PgError
			if !errors.As(refused, &pgError) || pgError.Code != "55000" || pgError.Message != "package expiry scan index is forward-only" {
				t.Fatal("public Down did not preserve its exact forward-only refusal", refused)
			}
			if packageExpiryFacts(t, ctx, r) != before {
				t.Fatal("typed Down refusal changed retained facts")
			}
			if _, err = owner.Exec(ctx, `ANALYZE entitlement_packages; ANALYZE entitlement_components; ANALYZE usage_holds; ANALYZE usage_hold_allocations`); err != nil {
				t.Fatal(err)
			}
			now := base.Add(31 * 24 * time.Hour)
			r.now = func() time.Time { return now }
			packageExpiryMeasure(t, ctx, owner, statement, now, false, packs[0])
			packageExpiryIndex(t, ctx, r)
			packageExpiryMeasure(t, ctx, owner, statement, now, true, packs[0])
			if packageExpiryFacts(t, ctx, r) != before {
				t.Fatal("plan probes changed financial or migration facts")
			}
			if count, err := r.ExpirePackages(ctx, 1); err != nil || count != 1 {
				t.Fatal("actual bounded first worker pass", count, err)
			}
			var first string
			if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, packs[0]).Scan(&first); err != nil || first != "EXPIRED" {
				t.Fatal("worker ordering", first, err)
			}
			if count, err := r.ExpirePackages(ctx, 10); err != nil || count != 2 {
				t.Fatal("actual next worker pass", count, err)
			}
			for _, pack := range packs {
				wallet, err := r.GetWallet(ctx, wallets[pack])
				if err != nil || wallet.PostedMicrocredits != 0 {
					t.Fatal("expiry financial conservation", wallet.PostedMicrocredits, err)
				}
			}
			var frozenStatus string
			if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, frozen).Scan(&frozenStatus); err != nil || frozenStatus != "FROZEN" {
				t.Fatal("frozen package was processed", frozenStatus, err)
			}
			wallet, err := r.GetWallet(ctx, wallets[frozen])
			if err != nil || wallet.PostedMicrocredits != 1000000 {
				t.Fatal("frozen wallet changed", wallet.PostedMicrocredits, err)
			}
			var journals int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='EXPIRY'`).Scan(&journals); err != nil || journals != 3 {
				t.Fatal("exact expiry journals", journals, err)
			}
			after := packageExpiryFacts(t, ctx, r)
			if count, err := r.ExpirePackages(ctx, 10); err != nil || count != 0 || packageExpiryFacts(t, ctx, r) != after {
				t.Fatal("next tick repeated or revived effects", count, err)
			}
			// Typed Down keeps a session lock even when its SQL refuses.
			// Retire only this owned actor before the independent full-carrier CLI.
			if err := owner.Close(ctx); err != nil {
				t.Fatal(err)
			}
			completeBillingMigrationContract(t, ctx, r, source, staged, product)
			packageExpiryIndex(t, ctx, r)
			t.Logf("PACKAGE_EXPIRY_VERIFIED scenario=%s synthetic_history=100000 cross_wallets=100000 future_active=10000 original_paid_packages=4 expired=3 frozen_preserved=1", scenario)
		})
	}
}
