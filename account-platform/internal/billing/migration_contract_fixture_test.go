package billing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// A DownOne contract needs its actual installed head. Stage original SQL bytes
// before creating a database, never edit installed SQL or delete ledger rows.
// keep selects only explicitly named post-038 migrations for a historical
// contract; ordinary current fixtures elsewhere still install the full carrier.
func stageBillingMigrationContract(t *testing.T, through int64, keep ...int64) (string, string) {
	t.Helper()
	source, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	staged, work := filepath.Join(root, "migrations", "billing"), filepath.Join(root, "internal", "billing")
	if err = os.MkdirAll(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load(source)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	selected := map[int64]string{}
	for _, migration := range loaded {
		if migration.Version > through {
			continue
		}
		if len(keep) > 0 && migration.Version > 900038 {
			included := false
			for _, version := range keep {
				included = included || version == migration.Version
			}
			if !included {
				continue
			}
		}
		found = found || migration.Version == through
		selected[migration.Version] = migration.Name
	}
	if !found {
		t.Fatal("exact migration contract head missing", through)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		prefix, rest, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			continue
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		name, ok := selected[version]
		if !ok || (rest != name+".up.sql" && rest != name+".down.sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(staged, entry.Name()), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)
	return source, staged
}

func installBillingFixturePrefix(t *testing.T, ctx context.Context, r *PostgresRepository, through int64) {
	t.Helper()
	before := providerHealthFinancialFacts(t, ctx, r)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var prefix []migrations.Migration
	for _, migration := range loaded {
		if migration.Version <= through {
			prefix = append(prefix, migration)
		}
	}
	if len(prefix) == 0 || prefix[len(prefix)-1].Version != through {
		t.Fatal("exact fixture prefix missing", through)
	}
	config := r.pool.Config().ConnConfig.Copy()
	config.User, config.Password = "billing_migrator", ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	if err = billingschema.Up(ctx, connection, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err = billingschema.VerifyApplied(ctx, connection, prefix); err != nil {
		t.Fatal(err)
	}
	if providerHealthFinancialFacts(t, ctx, r) != before {
		t.Fatal("historical prefix upgrade changed original paid financial facts")
	}
}

// After the exact historical/Down contract, exercise the complete current
// carrier on the same real facts, including lower versions delivered after45.
// New migration objects are legitimate; every pre-existing ledger row and
// original financial/observation field must remain identical. The exact two
// nullable 900046 additions have their own no-backfill/schema/ACL oracle.
func completeBillingMigrationContract(t *testing.T, ctx context.Context, r *PostgresRepository, source, staged, product string) {
	t.Helper()
	loaded, err := migrations.Load(source)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	rows, err := r.pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var version int64
		if err = rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	ledger := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version=ANY($1::bigint[])`, versions).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	facts := func(phase string) string {
		logPackageExpiryPhase(t, ctx, phase+".begin")
		value := providerHealthTableFacts(t, ctx, r, "wallets", "payment_transactions", "credit_lots", "entitlement_packages", "entitlement_components", "journal_entries", "ledger_postings", "payment_provider_attempts", "payment_provider_health_observations", "payment_stop_loss_evaluations", "payment_stop_loss_evaluation_members", "payment_provider_controls", "outbox_events")
		logPackageExpiryPhase(t, ctx, phase+".end")
		return value
	}
	queryUpgrade := freezeHistoricalQueryUpgrade(t, ctx, r)
	beforeLedger, beforeFacts := ledger(), facts("carrier.facts.pre")
	// Frozen900003 intentionally DROP/CREATEs billing_no_truncate for each
	// pending migration. Bind original triggers by original tableOID and name,
	// preserving enabled state plus full function definition and authority.
	// A new trigger OID alone is not a changed security contract.
	var classIDs []int64
	var triggerKeys []string
	if err = r.pool.QueryRow(ctx, `SELECT
  ARRAY(SELECT oid::bigint FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','p','S','v','m','i','I') ORDER BY oid),
  ARRAY(SELECT tgrelid::text||':'||tgname FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN(SELECT oid FROM pg_class WHERE relnamespace='public'::regnamespace) ORDER BY tgrelid,tgname)`).Scan(&classIDs, &triggerKeys); err != nil {
		t.Fatal(err)
	}
	authority := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
   (SELECT jsonb_agg(jsonb_build_array(c.oid,c.relname,c.relkind,pg_get_userbyid(c.relowner),c.relacl::text,CASE WHEN c.relkind IN('i','I') THEN pg_get_indexdef(c.oid) ELSE NULL END) ORDER BY c.oid) FROM pg_class c WHERE c.oid::bigint=ANY($1::bigint[])),
   (SELECT jsonb_agg(jsonb_build_array(g.tgrelid,g.tgname,g.tgenabled,pg_get_triggerdef(g.oid),pg_get_functiondef(p.oid),pg_get_userbyid(p.proowner),p.proacl::text,p.prosecdef,p.proconfig) ORDER BY g.tgrelid,g.tgname) FROM pg_trigger g JOIN pg_proc p ON p.oid=g.tgfoid WHERE (g.tgrelid::text||':'||g.tgname)=ANY($2::text[])),
   (SELECT jsonb_build_array(pg_get_userbyid(nspowner),nspacl::text) FROM pg_namespace WHERE oid='public'::regnamespace),
   pg_has_role('billing_service','billing_schema_owner','MEMBER'),has_schema_privilege('billing_service','public','CREATE'))::text`, classIDs, triggerKeys).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	beforeAuthority := authority()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(staged, entry.Name()), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var completeLedgerBeforeReplay string
	if product != "" {
		logPackageExpiryPhase(t, ctx, "carrier.cli.up.begin")
		output, err := providerHealthCLI(ctx, product, r, "billing_migrator", "up")
		logPackageExpiryPhase(t, ctx, "carrier.cli.up.returned")
		var evidence struct {
			Event          string `json:"event"`
			Domain         string `json:"domain"`
			AppliedCount   int    `json:"applied_count"`
			LatestVersion  int64  `json:"latest_version"`
			LatestChecksum string `json:"latest_checksum"`
		}
		latest := loaded[len(loaded)-1]
		if err != nil || json.Unmarshal(output, &evidence) != nil || evidence.Event != "migration.post_verification" || evidence.Domain != "billing" || evidence.AppliedCount != len(loaded) || evidence.LatestVersion != latest.Version || evidence.LatestChecksum != latest.Checksum {
			t.Fatalf("complete carrier actual CLI verification: %v\n%s", err, output)
		}
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&completeLedgerBeforeReplay); err != nil {
			t.Fatal(err)
		}
	}
	config := r.pool.Config().ConnConfig.Copy()
	config.User, config.Password = "billing_migrator", ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	logPackageExpiryPhase(t, ctx, "carrier.api.up.begin")
	if err = billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	logPackageExpiryPhase(t, ctx, "carrier.api.up.end")
	if _, err = billingschema.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	if completeLedgerBeforeReplay != "" {
		var current string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&current); err != nil || current != completeLedgerBeforeReplay {
			t.Fatal("complete replay changed an original or newly installed ledger row", err)
		}
	}
	queryUpgrade.assertAfter(t, ctx, r)
	afterLedger, afterFacts, afterAuthority := ledger(), facts("carrier.facts.post"), authority()
	if afterAuthority != beforeAuthority {
		var beforeParts, afterParts []json.RawMessage
		if json.Unmarshal([]byte(beforeAuthority), &beforeParts) != nil || json.Unmarshal([]byte(afterAuthority), &afterParts) != nil {
			t.Fatal("invalid authority fixture observation")
		}
		for section := range beforeParts {
			if string(beforeParts[section]) == string(afterParts[section]) {
				continue
			}
			if section > 1 {
				t.Logf("AUTHORITY_CHANGED section=%d before=%s after=%s", section, beforeParts[section], afterParts[section])
				continue
			}
			var oldRows, newRows [][]json.RawMessage
			if json.Unmarshal(beforeParts[section], &oldRows) != nil || json.Unmarshal(afterParts[section], &newRows) != nil {
				t.Fatal("invalid authority object observation")
			}
			current := map[string]string{}
			for _, row := range newRows {
				encoded, _ := json.Marshal(row)
				current[string(row[0])+"|"+string(row[1])] = string(encoded)
			}
			reported := 0
			for _, row := range oldRows {
				encoded, _ := json.Marshal(row)
				key := string(row[0]) + "|" + string(row[1])
				if string(encoded) != current[key] {
					if reported < 8 {
						t.Logf("AUTHORITY_OBJECT_CHANGED section=%d oid=%s before=%s after=%s", section, key, encoded, current[key])
					}
					reported++
				}
			}
			t.Logf("AUTHORITY_OBJECT_CHANGES section=%d count=%d", section, reported)
		}
	}
	if afterLedger != beforeLedger || afterFacts != beforeFacts || afterAuthority != beforeAuthority {
		t.Logf("FULL_CARRIER_CHANGED ledger=%t facts=%t authority=%t", afterLedger != beforeLedger, afterFacts != beforeFacts, afterAuthority != beforeAuthority)
		t.Fatal("complete carrier rewrote existing ledger, financial/observation facts or original object authority")
	}
	providerHealthIndex(t, ctx, r)
	t.Logf("COMPLETE_BILLING_CARRIER_VERIFIED latest=%d migrations=%d original_ledger_rows=%d", loaded[len(loaded)-1].Version, len(loaded), len(versions))
}
