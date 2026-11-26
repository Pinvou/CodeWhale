package billingschema

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
)

const statisticsBaseline = "4bbf359d8cf3547af401d7cdb561f7bc0477ebd6"

// The published main package ending at generation 900011 is the exact
// predecessor of the new statistics index.
const statisticsTokenBaseline = "5d8b45efc31e07299c4024cb5de2cbb58786feb3"

func TestOutboxStatisticsMigrationIdentity(t *testing.T) {
	for _, domain := range []struct {
		name    string
		version int64
		ordered func([]migrations.Migration) ([]migrations.Migration, error)
	}{{"account", 900021, accountschema.Ordered}, {"billing", 900046, Ordered}, {"token", 900012, tokenschema.Ordered}} {
		t.Run(domain.name, func(t *testing.T) {
			loaded, err := migrations.Load("../../migrations/" + domain.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = domain.ordered(loaded); err != nil {
				t.Fatal(err)
			}
			index := -1
			for i, m := range loaded {
				if m.Version == domain.version {
					index = i
				}
			}
			if index < 0 {
				t.Fatal("new pinned migration missing")
			}
			for _, mutation := range []string{"name", "up", "down", "alias", "duplicate"} {
				t.Run(mutation, func(t *testing.T) {
					changed := append([]migrations.Migration(nil), loaded...)
					switch mutation {
					case "name":
						changed[index].Name = "other"
					case "up":
						changed[index].UpSQL += "\nSELECT 1;"
					case "down":
						changed[index].DownSQL += "\nSELECT 1;"
					case "alias":
						changed[index].Version += 1000000
					case "duplicate":
						changed = append(changed, changed[index])
					}
					if _, err := domain.ordered(changed); err == nil {
						t.Fatal("changed migration identity accepted")
					}
				})
			}
		})
	}
}

// Archive an exact public implementation into the test-owned temporary tree.
func statisticsArchiveModule(t *testing.T, ctx context.Context, revision string) string {
	t.Helper()
	module, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := exec.CommandContext(ctx, "git", "-C", filepath.Dir(module), "archive", revision, "account-platform").Output()
	if err != nil {
		t.Fatal("frozen public migration baseline must be available; CI fetches its exact commit")
	}
	directory := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(directory, filepath.FromSlash(header.Name))
		rel, err := filepath.Rel(directory, target)
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatal("baseline archive escaped private fixture")
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			if header.PAXRecords["comment"] != revision {
				t.Fatal("baseline archive commit identity differs")
			}
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				t.Fatal("baseline fixture write failed")
			}
		default:
			t.Fatal("baseline contains unsupported link or special file")
		}
	}
	return filepath.Join(directory, "account-platform")
}

// Build real complete public CLIs, without synthesizing a historical schema or
// weakening the current Token package identity checks to accept a partial set.
func statisticsMigrationCLIs(t *testing.T, ctx context.Context) (string, string, string, string, string) {
	t.Helper()
	module, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	baselineModule := statisticsArchiveModule(t, ctx, statisticsBaseline)
	tokenModule := statisticsArchiveModule(t, ctx, statisticsTokenBaseline)
	historical, err := migrations.Load(filepath.Join(tokenModule, "migrations", "token"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := migrations.Load(filepath.Join(module, "migrations", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tokenschema.Ordered(current); err != nil {
		t.Fatal(err)
	}
	if len(historical) != 44 || historical[len(historical)-1].Version != 900011 || len(current) != 45 || current[len(current)-1].Version != 900012 {
		t.Fatal("frozen Token fixture must be the complete published package through 900011 before statistics 900012")
	}
	for i, migration := range historical {
		candidate := current[i]
		if migration.Version != candidate.Version || migration.Name != candidate.Name || migration.UpSQL != candidate.UpSQL || migration.DownSQL != candidate.DownSQL {
			t.Fatal("historical Token SQL identity differs from current package", migration.Version)
		}
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	build := func(source, name string) string {
		binary := filepath.Join(t.TempDir(), name+extension)
		command := exec.CommandContext(ctx, goBinary, "build", "-tags=nomsgpack", "-mod=readonly", "-o", binary, "./cmd/migrate")
		command.Dir = source
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("public migration CLI build: %v %s", err, output)
		}
		return binary
	}
	return build(baselineModule, "baseline-migrate"), build(module, "candidate-migrate"), baselineModule, build(tokenModule, "token-baseline-migrate"), filepath.Join(tokenModule, "migrations", "token")
}

// Keep real SQL bytes and filenames; never manufacture an older installed head
// by deleting later ledger entries.
func statisticsMigrationPackage(t *testing.T, directory string, through, omit int64) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	result := t.TempDir()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil {
			t.Fatal("invalid original migration filename", entry.Name())
		}
		if (through != 0 && version > through) || version == omit {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(result, entry.Name()), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func statisticsAccountUpgradeFacts(t *testing.T, ctx context.Context, operator *pgx.Conn, omitOwn bool) string {
	t.Helper()
	var identities string
	if err := operator.QueryRow(ctx, `SELECT jsonb_build_object(
'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE NOT $1 OR version<>900021),
'indexes',(SELECT jsonb_agg(jsonb_build_array(indexname,indexdef) ORDER BY indexname) FROM pg_indexes WHERE schemaname='public' AND tablename='outbox_events' AND (NOT $1 OR indexname<>'outbox_events_dead_statistics_idx')),
'triggers',(SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgname) FROM pg_trigger WHERE tgrelid='public.outbox_events'::regclass AND NOT tgisinternal)
)::text`, omitOwn).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	return statisticsFacts(t, ctx, operator) + statisticsAccessFacts(t, ctx, operator) + identities
}

func statisticsLedgerFacts(t *testing.T, ctx context.Context, operator *pgx.Conn) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := operator.QueryRow(ctx, `SELECT coalesce(jsonb_object_agg(version::text,to_jsonb(m)),'{}'::jsonb) FROM schema_migrations m`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func requireStatisticsLedgerPreserved(t *testing.T, ctx context.Context, operator *pgx.Conn, before map[string]json.RawMessage, omit int64) {
	t.Helper()
	after := statisticsLedgerFacts(t, ctx, operator)
	for version, raw := range before {
		if version != strconv.FormatInt(omit, 10) && !bytes.Equal(raw, after[version]) {
			t.Fatal("migration changed an original ledger identity, timestamp or checksum", version)
		}
	}
}

func statisticsOriginalIndexFacts(t *testing.T, ctx context.Context, operator *pgx.Conn, replacedIndexes ...string) string {
	t.Helper()
	var raw string
	excluded := append([]string{"outbox_events_dead_statistics_idx"}, replacedIndexes...)
	if err := operator.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_array(c.oid,c.relname,c.relowner,pg_get_indexdef(c.oid)) ORDER BY c.relname),'[]'::jsonb)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='i' AND c.relname <> ALL($1::text[])`, excluded).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireStatisticsOriginalIndexesPreserved(t *testing.T, ctx context.Context, operator *pgx.Conn, before string) {
	t.Helper()
	var oldIndexes, newIndexes []json.RawMessage
	if err := json.Unmarshal([]byte(before), &oldIndexes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(statisticsOriginalIndexFacts(t, ctx, operator)), &newIndexes); err != nil {
		t.Fatal(err)
	}
	current := make(map[string]bool, len(newIndexes))
	for _, index := range newIndexes {
		current[string(index)] = true
	}
	for _, index := range oldIndexes {
		if !current[string(index)] {
			t.Fatal("upgrade changed an original index OID, owner or definition")
		}
	}
}

const statisticsRegistrationMobileIndex = "registration_operations_active_mobile_idx"

func requireStatisticsRegistrationMobileIndex(t *testing.T, ctx context.Context, operator *pgx.Conn) {
	t.Helper()
	const predicate = "((committed_user_id IS NULL) AND (status = ANY (ARRAY[" +
		"'SMS_PENDING'::text, 'REGISTRATION_IN_PROGRESS'::text, 'IDENTITY_CREATING'::text, " +
		"'IDENTITY_CREATED'::text, 'ACCOUNT_COMMITTING'::text, 'REPAIR_REQUIRED'::text, 'COMPENSATION_PENDING'::text])))"
	var valid bool
	if err := operator.QueryRow(ctx, `
SELECT i.indisunique AND i.indisvalid AND i.indisready
   AND i.indrelid = 'public.registration_operations'::regclass
   AND idx.relowner = registered.relowner
   AND i.indnkeyatts = 1 AND pg_get_indexdef(i.indexrelid, 1, false) = 'mobile_e164'
   AND pg_get_expr(i.indpred, i.indrelid) = $2
FROM pg_index i
JOIN pg_class idx ON idx.oid = i.indexrelid
JOIN pg_class registered ON registered.oid = i.indrelid
WHERE i.indexrelid = to_regclass('public.' || $1)
`, statisticsRegistrationMobileIndex, predicate).Scan(&valid); err != nil || !valid {
		t.Fatal("900027 must install the unique uncommitted registration mobile reservation index", err)
	}
}

func TestRetainedOutboxStatisticsPublicPostgres(t *testing.T) {
	if os.Getenv("PINVOU_OUTBOX_STATISTICS_TEST") != "1" {
		t.Skip("explicit owned public-domain statistics fixture required")
	}
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Fatal("owned schema service required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	baselineCLI, candidateCLI, baselineModule, tokenBaselineCLI, tokenBaselineDirectory := statisticsMigrationCLIs(t, ctx)
	f := newStageFixture(t, ctx, raw)
	tokenRoles := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator"}
	var existing int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, tokenRoles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("Token fixture actors must be absent")
	}
	for _, role := range tokenRoles {
		login := "LOGIN"
		if role == "token_schema_owner" || role == "token_cap_policy_deployer" {
			login = "NOLOGIN"
		}
		if _, err := f.admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		f.roles = append(f.roles, role)
	}
	if _, err := f.admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := f.admin.Exec(clean, `REVOKE pg_read_all_stats FROM token_migrator`); err != nil {
			t.Error("owned bootstrap membership cleanup failed")
		}
	})
	query, err := os.ReadFile("../platform/events/queries/outbox_stats.sql")
	if err != nil {
		t.Fatal(err)
	}
	oldSource, err := os.ReadFile(filepath.Join(baselineModule, "internal/platform/events/postgres.go"))
	if err != nil {
		t.Fatal(err)
	}
	var oldQuery string
	for _, literal := range regexp.MustCompile("`[^`]+`").FindAllString(string(oldSource), -1) {
		if strings.HasPrefix(literal, "`SELECT count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL)") && strings.Contains(literal, "WHERE NOT(event_type=ANY($1::text[]))") {
			if oldQuery != "" {
				t.Fatal("ambiguous frozen query")
			}
			oldQuery = strings.Trim(literal, "`")
		}
	}
	if oldQuery == "" {
		t.Fatal("frozen full-history statistics query not found")
	}
	for _, domain := range []string{"account", "billing", "token"} {
		t.Run(domain, func(t *testing.T) {
			indexVersion := map[string]int64{"account": 900021, "billing": 900046, "token": 900012}[domain]
			owner := domain + "_service"
			if domain == "billing" {
				owner = "billing_schema_owner"
			}
			if domain == "token" {
				owner = f.config.User
			}
			setup := f.database(t, ctx, domain, owner)
			cfg := f.config.Copy()
			cfg.Database = setup.Config().Database
			operator, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer operator.Close(context.Background())
			if domain == "token" {
				bootstrap, e := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = operator.Exec(ctx, string(bootstrap)); e != nil {
					t.Fatal(e)
				}
			}
			actor := domain + "_service"
			if domain == "token" {
				actor = "token_gateway"
			}
			databaseURL := f.serviceURL(t, setup, domain+"_migrator")
			run := func(url, binary, directory, direction string, accepted bool) {
				t.Helper()
				environment := []string{}
				for _, item := range os.Environ() {
					key := strings.SplitN(item, "=", 2)[0]
					if key != "DATABASE_URL" && key != "MIGRATION_ROLE" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" {
						environment = append(environment, item)
					}
				}
				command := exec.CommandContext(ctx, binary, "--dir", directory, "--direction", direction)
				command.Env = append(environment, "DATABASE_URL="+url, "MIGRATION_ROLE="+actor, "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
				output, e := command.CombinedOutput()
				if (e == nil) != accepted {
					t.Fatalf("actual %s migration %s accepted=%t: %v %s", domain, direction, accepted, e, output)
				}
				if !accepted && !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
					t.Fatal("refusal did not reach actual migration execution", string(output))
				}
			}
			oldDirectory := filepath.Join(baselineModule, "migrations", domain)
			newDirectory, e := filepath.Abs("../../migrations/" + domain)
			if e != nil {
				t.Fatal(e)
			}
			fullDirectory := newDirectory
			downDirectory := fullDirectory
			if domain == "account" {
				newDirectory = statisticsMigrationPackage(t, fullDirectory, 900021, 0)
				downDirectory = newDirectory
			} else if domain == "billing" {
				newDirectory = statisticsMigrationPackage(t, fullDirectory, statisticsHeadVersion, 0)
				downDirectory = newDirectory
			}
			run(databaseURL, baselineCLI, oldDirectory, "up", true)
			var absent bool
			if e = operator.QueryRow(ctx, `SELECT to_regclass('public.outbox_events_dead_statistics_idx') IS NULL`).Scan(&absent); e != nil || !absent {
				t.Fatal("baseline already contains the new index")
			}
			if _, e = operator.Exec(ctx, statisticsSeed); e != nil {
				t.Fatal(e)
			}
			if domain == "billing" {
				if _, e = operator.Exec(ctx, `INSERT INTO payment_provider_attempts(id,provider_attempt_id,request_fingerprint,provider_id,provider_version,capability,channel,route,trusted_probe,created_at) VALUES(gen_random_uuid(),'statistics-attempt',repeat('a',64),'fixture','v1','PAY','fixture','fixture',true,now()); INSERT INTO payment_provider_health_observations(id,provider_attempt_id,provider_id,provider_version,capability,channel,route,trusted_probe,outcome,classification_rule_version,evidence_digest,received_at) VALUES(gen_random_uuid(),'statistics-attempt','fixture','v1','PAY','fixture','fixture',true,'SUCCESS','fixture/v1',repeat('b',64),now())`); e != nil {
					t.Fatal(e)
				}
			}
			if domain == "token" {
				// Exercise the published Token carrier before 900012. Preserve
				// original outbox facts while 900011 initializes the new column.
				oldFacts := statisticsFacts(t, ctx, operator)
				oldAccess := statisticsAccessFacts(t, ctx, operator)
				oldIndexes := statisticsOriginalIndexFacts(t, ctx, operator)
				oldLedger := statisticsLedgerFacts(t, ctx, operator)
				run(databaseURL, tokenBaselineCLI, tokenBaselineDirectory, "up", true)
				requireStatisticsLedgerPreserved(t, ctx, operator, oldLedger, 0)
				requireStatisticsOriginalIndexesPreserved(t, ctx, operator, oldIndexes)
				var projectedFacts string
				if e = operator.QueryRow(ctx, `SELECT string_agg((to_jsonb(e)-'publication_generation')::text,'' ORDER BY event_id) FROM public.outbox_events e`).Scan(&projectedFacts); e != nil {
					t.Fatal(e)
				}
				sum := sha256.Sum256([]byte(projectedFacts))
				var nonzero, rows int
				if e = operator.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE publication_generation IS DISTINCT FROM 0) FROM public.outbox_events`).Scan(&rows, &nonzero); e != nil || rows != 25005 || nonzero != 0 || hex.EncodeToString(sum[:]) != oldFacts {
					t.Fatal("publication upgrade changed retained delivery facts or failed to initialize every row", rows, nonzero, e)
				}
				if statisticsAccessFacts(t, ctx, operator) != oldAccess {
					t.Fatal("publication upgrade changed persistent outbox authority")
				}
			}
			if _, e = operator.Exec(ctx, `VACUUM (ANALYZE) public.outbox_events`); e != nil {
				t.Fatal(e)
			}
			facts := statisticsFacts(t, ctx, operator)
			policy := statisticsAccessFacts(t, ctx, operator)
			oldPlan := statisticsPlan(t, ctx, operator, oldQuery)
			oldLedger := statisticsLedgerFacts(t, ctx, operator)
			oldIndexes := statisticsOriginalIndexFacts(t, ctx, operator)
			run(databaseURL, candidateCLI, newDirectory, "up", true)
			requireStatisticsLedgerPreserved(t, ctx, operator, oldLedger, 0)
			requireStatisticsOriginalIndexesPreserved(t, ctx, operator, oldIndexes)
			installedLedger := statisticsLedgerFacts(t, ctx, operator)
			originalIndexes := statisticsOriginalIndexFacts(t, ctx, operator)
			if statisticsFacts(t, ctx, operator) != facts {
				t.Fatal("upgrade rewrote retained delivery facts")
			}
			if statisticsAccessFacts(t, ctx, operator) != policy {
				t.Fatal("upgrade changed persistent ownership or schema/table grants")
			}
			if _, e = operator.Exec(ctx, `VACUUM (ANALYZE) public.outbox_events`); e != nil {
				t.Fatal(e)
			}
			newPlan := statisticsPlan(t, ctx, operator, string(query))
			if !oldPlan.sequential || oldPlan.visited < 25000 || !newPlan.deadIndex || newPlan.sequential || newPlan.visited > 20 {
				t.Fatalf("unexpected actual old/new query plans: old=%s new=%s", oldPlan.raw, newPlan.raw)
			}
			t.Logf("actual plan fixture: published=25000, old rows visited=%g, new rows visited=%g; old=%s new=%s", oldPlan.visited, newPlan.visited, oldPlan.raw, newPlan.raw)
			runtimeURL := f.serviceURL(t, setup, actor)
			store, e := events.NewPostgresStore(ctx, runtimeURL)
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			partitioned, e := events.NewPartitionedPostgresStore(ctx, runtimeURL, []string{"history.ignored.v1"})
			if e != nil {
				t.Fatal(e)
			}
			defer partitioned.Close()
			check := func() {
				t.Helper()
				all, e := store.Stats(ctx)
				if e != nil || all.Pending != 3 || all.DeadLettered != 2 || all.OldestPendingAge < 9*time.Minute || all.OldestPendingAge > 11*time.Minute || all.RetainedStorageBytes <= 0 {
					t.Fatalf("unpartitioned actual statistics: %+v %v", all, e)
				}
				scoped, e := partitioned.Stats(ctx)
				if e != nil || scoped.Pending != 2 || scoped.DeadLettered != 1 || scoped.OldestPendingAge < 5*time.Minute || scoped.OldestPendingAge > 7*time.Minute || scoped.RetainedStorageBytes != all.RetainedStorageBytes {
					t.Fatalf("partitioned actual statistics: %+v %v", scoped, e)
				}
			}
			check()
			tx, e := operator.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer func() {
				clean, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				_ = tx.Rollback(clean)
			}()
			if _, e = tx.Exec(ctx, `LOCK TABLE public.outbox_events IN ACCESS EXCLUSIVE MODE`); e != nil {
				t.Fatal(e)
			}
			started := time.Now()
			_, blocked := partitioned.Stats(context.Background())
			if blocked == nil || time.Since(started) > 8*time.Second {
				t.Fatal("statistics own budget did not bound an unbounded caller")
			}
			if e = tx.Rollback(ctx); e != nil {
				t.Fatal(e)
			}
			check()
			if domain == "billing" {
				verifyHealthStorageRecovery(t, ctx, operator, runtimeURL)
			}
			var installedHead int64
			if e = operator.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&installedHead); e != nil || installedHead != indexVersion {
				t.Fatal("rollback fixture must actually have the statistics index as its head", installedHead, e)
			}
			// A wrong named index must not be removed or its ledger row consumed.
			if _, e = operator.Exec(ctx, `DROP INDEX public.outbox_events_dead_statistics_idx; CREATE INDEX outbox_events_dead_statistics_idx ON public.outbox_events USING hash(event_type) WHERE dead_lettered_at IS NOT NULL`); e != nil {
				t.Fatal(e)
			}
			run(databaseURL, candidateCLI, downDirectory, "down", false)
			requireStatisticsLedgerPreserved(t, ctx, operator, installedLedger, 0)
			if statisticsOriginalIndexFacts(t, ctx, operator) != originalIndexes {
				t.Fatal("refused rollback changed an unrelated original index")
			}
			var method string
			var recorded bool
			if e = operator.QueryRow(ctx, `SELECT a.amname,EXISTS(SELECT FROM public.schema_migrations WHERE name='outbox_dead_statistics_index') FROM pg_class c JOIN pg_am a ON a.oid=c.relam WHERE c.oid='public.outbox_events_dead_statistics_idx'::regclass`).Scan(&method, &recorded); e != nil || method != "hash" || !recorded {
				t.Fatal("refused rollback changed the foreign index or consumed migration history")
			}
			if statisticsFacts(t, ctx, operator) != facts || statisticsAccessFacts(t, ctx, operator) != policy {
				t.Fatal("refused rollback changed retained facts or authority")
			}
			if _, e = operator.Exec(ctx, `DROP INDEX public.outbox_events_dead_statistics_idx; CREATE INDEX outbox_events_dead_statistics_idx ON public.outbox_events(event_type) WHERE dead_lettered_at IS NOT NULL`); e != nil {
				t.Fatal(e)
			}
			run(databaseURL, candidateCLI, downDirectory, "down", true)
			requireStatisticsLedgerPreserved(t, ctx, operator, installedLedger, indexVersion)
			if statisticsOriginalIndexFacts(t, ctx, operator) != originalIndexes {
				t.Fatal("rollback changed an unrelated original index")
			}
			if e = operator.QueryRow(ctx, `SELECT to_regclass('public.outbox_events_dead_statistics_idx') IS NULL AND NOT EXISTS(SELECT FROM public.schema_migrations WHERE name='outbox_dead_statistics_index')`).Scan(&absent); e != nil || !absent {
				t.Fatal("public rollback did not remove only the new index identity")
			}
			if statisticsFacts(t, ctx, operator) != facts {
				t.Fatal("rollback rewrote delivery history")
			}
			if statisticsAccessFacts(t, ctx, operator) != policy {
				t.Fatal("rollback changed ownership or grants")
			}
			if domain == "token" {
				verifyStatisticsTokenGenerationRefusal(t, ctx, operator, databaseURL, fullDirectory, tokenBaselineDirectory, func() {
					run(databaseURL, tokenBaselineCLI, tokenBaselineDirectory, "up", true)
				})
			}
			run(databaseURL, candidateCLI, newDirectory, "up", true)
			requireStatisticsLedgerPreserved(t, ctx, operator, installedLedger, indexVersion)
			if statisticsOriginalIndexFacts(t, ctx, operator) != originalIndexes {
				t.Fatal("index replay changed an unrelated original index")
			}
			check()
			if domain != "account" {
				beforeRoleLedger := statisticsLedgerFacts(t, ctx, operator)
				verifyStatisticsRollbackRole(t, ctx, domain, databaseURL, actor, downDirectory)
				requireStatisticsLedgerPreserved(t, ctx, operator, beforeRoleLedger, indexVersion)
			} else {
				// The actual historical 900022 head retains its exact refusal.
				// Later current migrations are installed and verified separately.
				forwardDirectory := statisticsMigrationPackage(t, fullDirectory, 900022, 0)
				run(databaseURL, candidateCLI, forwardDirectory, "up", true)
				var head int64
				if e = operator.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&head); e != nil || head != 900022 {
					t.Fatal("forward-only fixture must really stop at 900022", head, e)
				}
				current, err := pgx.Connect(ctx, databaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer current.Close(context.Background())
				if _, err = current.Exec(ctx, "SET ROLE account_service"); err != nil {
					t.Fatal(err)
				}
				loaded, err := migrations.Load(forwardDirectory)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = accountschema.VerifyApplied(ctx, current, loaded); err != nil {
					t.Fatal(err)
				}
				before := statisticsAccountUpgradeFacts(t, ctx, operator, false)
				err = accountschema.DownOne(ctx, current, loaded)
				if unlockErr := migrations.Unlock(ctx, current); unlockErr != nil {
					t.Fatal(unlockErr)
				}
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "55000" || !strings.Contains(failure.Message, "900022 inventory is forward-only") {
					t.Fatal("current head did not preserve exact forward-only refusal", err)
				}
				if statisticsAccountUpgradeFacts(t, ctx, operator, false) != before {
					t.Fatal("current-head refusal changed old ledger, events, indexes or authority")
				}
				run(databaseURL, candidateCLI, forwardDirectory, "up", true)
				if statisticsAccountUpgradeFacts(t, ctx, operator, false) != before {
					t.Fatal("forward-only replay changed retained facts")
				}
				beforeCurrentLedger := statisticsLedgerFacts(t, ctx, operator)
				// The full Account upgrade includes 900027's intentional replacement.
				beforeCurrentIndexes := statisticsOriginalIndexFacts(t, ctx, operator, statisticsRegistrationMobileIndex)
				run(databaseURL, candidateCLI, fullDirectory, "up", true)
				requireStatisticsLedgerPreserved(t, ctx, operator, beforeCurrentLedger, 0)
				requireStatisticsOriginalIndexesPreserved(t, ctx, operator, beforeCurrentIndexes)
				requireStatisticsRegistrationMobileIndex(t, ctx, operator)
				if statisticsFacts(t, ctx, operator) != facts || statisticsAccessFacts(t, ctx, operator) != policy {
					t.Fatal("full current upgrade changed retained facts or authority")
				}
				fullLoaded, err := migrations.Load(fullDirectory)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = accountschema.VerifyApplied(ctx, current, fullLoaded); err != nil {
					t.Fatal(err)
				}
				fullFacts := statisticsAccountUpgradeFacts(t, ctx, operator, false)
				run(databaseURL, candidateCLI, fullDirectory, "up", true)
				if statisticsAccountUpgradeFacts(t, ctx, operator, false) != fullFacts {
					t.Fatal("full current replay changed retained facts")
				}
				check()

				// An independent nonempty current database may already have22
				// while missing lower21. Public installation must fill only21.
				gapSetup := f.database(t, ctx, domain, owner)
				gapConfig := f.config.Copy()
				gapConfig.Database = gapSetup.Config().Database
				gapOperator, err := pgx.ConnectConfig(ctx, gapConfig)
				if err != nil {
					t.Fatal(err)
				}
				defer gapOperator.Close(context.Background())
				gapURL := f.serviceURL(t, gapSetup, domain+"_migrator")
				gapPackage := statisticsMigrationPackage(t, fullDirectory, 0, 900021)
				run(gapURL, candidateCLI, gapPackage, "up", true)
				var has22, has21 bool
				if err = gapOperator.QueryRow(ctx, `SELECT EXISTS(SELECT FROM schema_migrations WHERE version=900022),EXISTS(SELECT FROM schema_migrations WHERE version=900021) OR to_regclass('public.outbox_events_dead_statistics_idx') IS NOT NULL`).Scan(&has22, &has21); err != nil || !has22 || has21 {
					t.Fatal("current lower-gap precondition", has22, has21, err)
				}
				if _, err = gapOperator.Exec(ctx, statisticsSeed); err != nil {
					t.Fatal(err)
				}
				gapFacts := statisticsAccountUpgradeFacts(t, ctx, gapOperator, true)
				run(gapURL, candidateCLI, fullDirectory, "up", true)
				if err = gapOperator.QueryRow(ctx, `SELECT to_regclass('public.outbox_events_dead_statistics_idx') IS NOT NULL AND EXISTS(SELECT FROM schema_migrations WHERE version=900021)`).Scan(&has21); err != nil || !has21 || statisticsAccountUpgradeFacts(t, ctx, gapOperator, true) != gapFacts {
					t.Fatal("public lower-gap upgrade changed original facts or missed its own index", err)
				}
				run(gapURL, candidateCLI, fullDirectory, "up", true)
				if statisticsAccountUpgradeFacts(t, ctx, gapOperator, true) != gapFacts {
					t.Fatal("full current replay changed retained history")
				}
			}
			if domain == "billing" {
				beforeCurrentLedger := statisticsLedgerFacts(t, ctx, operator)
				beforeCurrentIndexes := statisticsOriginalIndexFacts(t, ctx, operator)
				for replay := 0; replay < 2; replay++ {
					run(databaseURL, candidateCLI, fullDirectory, "up", true)
					requireStatisticsLedgerPreserved(t, ctx, operator, beforeCurrentLedger, 0)
					requireStatisticsOriginalIndexesPreserved(t, ctx, operator, beforeCurrentIndexes)
					if statisticsFacts(t, ctx, operator) != facts || statisticsAccessFacts(t, ctx, operator) != policy {
						t.Fatal("full billing upgrade changed retained facts or authority")
					}
					beforeCurrentLedger = statisticsLedgerFacts(t, ctx, operator)
					check()
				}
			}
			freshSetup := f.database(t, ctx, domain, owner)
			freshConfig := f.config.Copy()
			freshConfig.Database = freshSetup.Config().Database
			freshOperator, freshErr := pgx.ConnectConfig(ctx, freshConfig)
			if freshErr != nil {
				t.Fatal(freshErr)
			}
			defer freshOperator.Close(context.Background())
			if domain == "token" {
				bootstrap, readErr := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
				if readErr != nil {
					t.Fatal(readErr)
				}
				if _, freshErr = freshOperator.Exec(ctx, string(bootstrap)); freshErr != nil {
					t.Fatal(freshErr)
				}
			}
			freshURL := f.serviceURL(t, freshSetup, domain+"_migrator")
			run(freshURL, candidateCLI, fullDirectory, "up", true)
			run(freshURL, candidateCLI, fullDirectory, "up", true)
			if statisticsAccessFacts(t, ctx, freshOperator) != policy {
				t.Fatal("fresh and upgraded persistent authority differ")
			}
			var installed bool
			if freshErr = freshOperator.QueryRow(ctx, `SELECT to_regclass('public.outbox_events_dead_statistics_idx') IS NOT NULL`).Scan(&installed); freshErr != nil || !installed {
				t.Fatal("candidate fresh install omitted statistics index")
			}
			freshStore, freshErr := events.NewPostgresStore(ctx, f.serviceURL(t, freshSetup, actor))
			if freshErr != nil {
				t.Fatal(freshErr)
			}
			defer freshStore.Close()
			empty, freshErr := freshStore.Stats(ctx)
			if freshErr != nil || empty.Pending != 0 || empty.DeadLettered != 0 || empty.OldestPendingAge != 0 || empty.RetainedStorageBytes <= 0 {
				t.Fatalf("fresh runtime statistics: %+v %v", empty, freshErr)
			}
			if domain == "billing" {
				var retained int
				if e = operator.QueryRow(ctx, `SELECT count(*) FROM payment_provider_health_observations WHERE provider_attempt_id='statistics-attempt' AND outcome='SUCCESS' AND evidence_digest=repeat('b',64)`).Scan(&retained); e != nil || retained != 1 {
					t.Fatal("measurement or index migration changed retained health evidence")
				}
			}
		})
	}
}

func statisticsFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	var facts string
	if e := c.QueryRow(ctx, `SELECT string_agg(to_jsonb(e)::text,'' ORDER BY event_id) FROM public.outbox_events e`).Scan(&facts); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(facts))
	return hex.EncodeToString(sum[:])
}
func statisticsAccessFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	var facts string
	if e := c.QueryRow(ctx, `SELECT jsonb_build_object('schema_owner',n.nspowner,'schema_acl',n.nspacl,'table_owner',c.relowner,'table_acl',c.relacl,'ledger_owner',l.relowner,'ledger_acl',l.relacl)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_class l WHERE c.oid='public.outbox_events'::regclass AND l.oid='public.schema_migrations'::regclass`).Scan(&facts); e != nil {
		t.Fatal(e)
	}
	return facts
}

type statisticsQueryPlan struct {
	raw                   []byte
	sequential, deadIndex bool
	visited               float64
}

func statisticsPlan(t *testing.T, ctx context.Context, c *pgx.Conn, sql string) statisticsQueryPlan {
	t.Helper()
	var plan []byte
	if e := c.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+sql, []string{"history.ignored.v1"}).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	var parsed []map[string]any
	if e := json.Unmarshal(plan, &parsed); e != nil {
		t.Fatal(e)
	}
	result := statisticsQueryPlan{raw: plan}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "outbox_events" {
			if node["Node Type"] == "Seq Scan" {
				result.sequential = true
			}
			rows, _ := node["Actual Rows"].(float64)
			removed, _ := node["Rows Removed by Filter"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			result.visited += (rows + removed) * loops
		}
		if node["Index Name"] == "outbox_events_dead_statistics_idx" {
			result.deadIndex = true
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if child, ok := child.(map[string]any); ok {
				walk(child)
			}
		}
	}
	if len(parsed) != 1 {
		t.Fatal("expected one actual query plan")
	}
	node, ok := parsed[0]["Plan"].(map[string]any)
	if !ok {
		t.Fatal("missing query plan")
	}
	walk(node)
	return result
}

func verifyHealthStorageRecovery(t *testing.T, ctx context.Context, operator *pgx.Conn, url string) {
	t.Helper()
	repository, e := billing.NewPostgresRepository(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer repository.Close()
	registry := prometheus.NewRegistry()
	registry.MustRegister(repository.HealthRetentionCollectors()...)
	check := func(available bool) {
		t.Helper()
		families, e := registry.Gather()
		if e != nil {
			t.Fatal(e)
		}
		values := map[string]float64{}
		for _, family := range families {
			if len(family.Metric) != 1 {
				t.Fatal("unexpected metric cardinality")
			}
			values[family.GetName()] = family.Metric[0].Gauge.GetValue()
		}
		if available {
			if values["pinvou_payment_health_storage_available"] != 1 || values["pinvou_payment_health_retained_storage_bytes"] <= 0 {
				t.Fatal("health storage unavailable after recovery")
			}
		} else {
			if values["pinvou_payment_health_storage_available"] != 0 {
				t.Fatal("health storage failure hidden")
			}
			if _, present := values["pinvou_payment_health_retained_storage_bytes"]; present {
				t.Fatal("failed measurement reported false size")
			}
		}
	}
	check(true)
	if _, e = operator.Exec(ctx, `ALTER TABLE public.payment_provider_health_observations RENAME TO history_fixture_observations_hidden`); e != nil {
		t.Fatal(e)
	}
	check(false)
	if _, e = operator.Exec(ctx, `ALTER TABLE public.history_fixture_observations_hidden RENAME TO payment_provider_health_observations`); e != nil {
		t.Fatal(e)
	}
	check(true)
}

const statisticsSeed = `INSERT INTO public.outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,published_at,attempts)
 SELECT gen_random_uuid(),'history.complete.v1',gen_random_uuid(),1,jsonb_build_object('fixture','retained','n',n),'history-statistics',now()-interval '1 day',now(),3 FROM generate_series(1,25000) n;
 INSERT INTO public.outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,dead_lettered_at,attempts) VALUES
 (gen_random_uuid(),'history.live.v1',gen_random_uuid(),1,'{"fixture":"retained"}','history-statistics',now()-interval '5 minutes',NULL,0),
 (gen_random_uuid(),'history.live.v1',gen_random_uuid(),1,'{"fixture":"retained"}','history-statistics',now()-interval '1 minute',NULL,0),
 (gen_random_uuid(),'history.ignored.v1',gen_random_uuid(),1,'{"fixture":"retained"}','history-statistics',now()-interval '9 minutes',NULL,0),
 (gen_random_uuid(),'history.dead.v1',gen_random_uuid(),1,'{"fixture":"retained"}','history-statistics',now()-interval '6 minutes',now(),11),
 (gen_random_uuid(),'history.ignored.v1',gen_random_uuid(),1,'{"fixture":"retained"}','history-statistics',now()-interval '8 minutes',now(),11);`

// Reuse the genuine migration connection after public wrapper return. A CLI
// process exit cannot demonstrate that this public library restores its role.
func verifyStatisticsRollbackRole(t *testing.T, ctx context.Context, domain, url, actor, directory string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, "SET ROLE "+pgx.Identifier{actor}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	verifyStatisticsLedgerDenied(t, ctx, conn)
	loaded, err := migrations.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	if domain == "billing" {
		err = DownOne(ctx, conn, loaded)
	} else {
		err = tokenschema.DownOne(ctx, conn, loaded)
	}
	if err != nil {
		t.Fatal(err)
	}
	var returned string
	if err = conn.QueryRow(ctx, `SELECT current_user`).Scan(&returned); err != nil || returned != actor {
		t.Fatalf("public rollback returned role %q; want original %q (%v)", returned, actor, err)
	}
	verifyStatisticsLedgerDenied(t, ctx, conn)
	if err := migrations.Unlock(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if domain == "billing" {
		err = Up(ctx, conn, loaded)
	} else {
		err = tokenschema.Up(ctx, conn, loaded)
	}
	if err != nil {
		t.Fatal("next public migration after rollback", err)
	}
	if err = conn.QueryRow(ctx, `SELECT current_user`).Scan(&returned); err != nil || returned != actor {
		t.Fatal("next call changed the caller identity")
	}
	verifyStatisticsLedgerDenied(t, ctx, conn)
	if err := migrations.Unlock(ctx, conn); err != nil {
		t.Fatal(err)
	}
	probe, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(context.Background())
	var available bool
	if err = probe.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&available); err != nil || !available {
		t.Fatal("returned migration connection retained its lock", err)
	}
	if err = migrations.Unlock(ctx, probe); err != nil {
		t.Fatal(err)
	}
}

func verifyStatisticsLedgerDenied(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	_, err := conn.Exec(ctx, `DELETE FROM public.schema_migrations WHERE false`)
	failure, ok := err.(*pgconn.PgError)
	if !ok || failure.Code != "42501" {
		t.Fatalf("runtime ledger DELETE must remain forbidden: %v", err)
	}
}

// After the real statistics rollback, generation must be the head and refuse
// rollback without changing data. Replay uses the exact pre-statistics package.
func verifyStatisticsTokenGenerationRefusal(t *testing.T, ctx context.Context, operator *pgx.Conn, url, directory, previousDirectory string, replay func()) {
	t.Helper()
	current, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close(context.Background())
	if _, err = current.Exec(ctx, "SET ROLE token_gateway"); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := migrations.Load(previousDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, current, previous); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err = operator.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&head); err != nil || head != 900011 {
		t.Fatal("current Token fixture did not install generation head", head, err)
	}
	beforeLedger := statisticsLedgerFacts(t, ctx, operator)
	before := statisticsFacts(t, ctx, operator) + statisticsAccessFacts(t, ctx, operator) + statisticsOriginalIndexFacts(t, ctx, operator)
	err = tokenschema.DownOne(ctx, current, loaded)
	if unlockErr := migrations.Unlock(ctx, current); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "55000" || !strings.Contains(failure.Message, "Token outbox publication generation is forward-only") {
		t.Fatal("generation head did not preserve its exact refusal", err)
	}
	requireStatisticsLedgerPreserved(t, ctx, operator, beforeLedger, 0)
	if len(statisticsLedgerFacts(t, ctx, operator)) != len(beforeLedger) || statisticsFacts(t, ctx, operator)+statisticsAccessFacts(t, ctx, operator)+statisticsOriginalIndexFacts(t, ctx, operator) != before {
		t.Fatal("generation refusal changed retained facts or authority")
	}
	replay()
	if _, err = migrations.VerifyApplied(ctx, current, previous); err != nil {
		t.Fatal("verify replay after generation refusal", err)
	}
	requireStatisticsLedgerPreserved(t, ctx, operator, beforeLedger, 0)
	if len(statisticsLedgerFacts(t, ctx, operator)) != len(beforeLedger) || statisticsFacts(t, ctx, operator)+statisticsAccessFacts(t, ctx, operator)+statisticsOriginalIndexFacts(t, ctx, operator) != before {
		t.Fatal("generation replay changed retained facts or authority")
	}
	verifyStatisticsLedgerDenied(t, ctx, current)
}
