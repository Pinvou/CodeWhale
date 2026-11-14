package account

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const registrationOutboxIndexVersion int64 = 900020

func registrationOutboxFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var facts string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'events',(SELECT md5(COALESCE(string_agg(to_jsonb(e)::text,E'\n' ORDER BY event_id),'')) FROM outbox_events e),
'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<>900020),
'outbox_owner',(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.outbox_events'::regclass),
'outbox_acl',(SELECT relacl::text FROM pg_class WHERE oid='public.outbox_events'::regclass),
'schema_acl',(SELECT nspacl::text FROM pg_namespace WHERE nspname='public'),
'original_indexes',(SELECT jsonb_agg(jsonb_build_array(indexname,indexdef) ORDER BY indexname) FROM pg_indexes WHERE schemaname='public' AND tablename='outbox_events' AND indexname<>'outbox_events_aggregate_idx'),
'triggers',(SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgname) FROM pg_trigger WHERE tgrelid='public.outbox_events'::regclass AND NOT tgisinternal)
)::text`).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func registrationOutboxMigrationTool(t *testing.T) string {
	t.Helper()
	name := "migrate"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	product := filepath.Join(t.TempDir(), name)
	goTool := os.Getenv("PINVOU_ACCOUNT_TEST_GO")
	if goTool == "" {
		var err error
		goTool, err = exec.LookPath("go")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, goTool, "build", "-tags=nomsgpack", "-mod=readonly", "-o", product, "./cmd/migrate")
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build actual migration command: %v\n%s", err, output)
	}
	return product
}

func registrationOutboxCLI(ctx context.Context, product string, r *PostgresRepository, actor, direction, directory string) ([]byte, error) {
	configuration := r.pool.Config().ConnConfig.Copy()
	command := exec.CommandContext(ctx, product, "--dir", directory, "--direction", direction)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "DATABASE_URL" && key != "MIGRATION_ROLE" && key != "MIGRATIONS_DIR" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" && !strings.HasPrefix(key, "MIGRATION_BACKUP_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "DATABASE_URL="+registrationOutboxDSN(configuration, actor), "MIGRATION_ROLE=account_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
	return command.CombinedOutput()
}

// Exercise a real versioned public package, preserving original filenames and
// bytes. Never erase a later applied ledger entry to manufacture an older head.
func registrationOutboxPackageThrough(t *testing.T, through int64) string {
	t.Helper()
	source := "../../migrations/account"
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil {
			t.Fatal("invalid source migration name", entry.Name())
		}
		if version > through {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func registrationOutboxStableFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var facts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(registrationOutboxFacts(t, ctx, r)), &facts); err != nil {
		t.Fatal(err)
	}
	// Permit only later ledger additions. Every earlier row (including its
	// original timestamps/checksum) and all outbox evidence/authority stay exact.
	var ledger []json.RawMessage
	if err := json.Unmarshal(facts["ledger"], &ledger); err != nil {
		t.Fatal(err)
	}
	retained := make([]json.RawMessage, 0, len(ledger))
	for _, entry := range ledger {
		var identity struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal(entry, &identity); err != nil {
			t.Fatal(err)
		}
		if identity.Version <= registrationOutboxIndexVersion {
			retained = append(retained, entry)
		}
	}
	var err error
	facts["ledger"], err = json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ConnConfig.ConnString retains the original parsed text, so changing its
// fields must not be used to choose a different fixture database or actor.
func registrationOutboxDSN(configuration *pgx.ConnConfig, actor string) string {
	endpoint := url.URL{Scheme: "postgres", User: url.User(actor), Host: net.JoinHostPort(configuration.Host, strconv.Itoa(int(configuration.Port))), Path: "/" + configuration.Database, RawQuery: "sslmode=disable"}
	return endpoint.String()
}

func registrationOutboxApproval(t *testing.T, ctx context.Context, r *PostgresRepository) uuid.UUID {
	t.Helper()
	r.approvalPresenterReady = approvalPolicyPresenterReady
	configuration := r.pool.Config().ConnConfig.Copy()
	if err := r.ConfigureApprovalWriter(ctx, registrationOutboxDSN(configuration, "account_approval_writer")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.approvalWriterPool.Close)
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
	owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "outbox-source-requester")
	approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "outbox-source-approver")
	submitted, draft := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(owner))
	approved, err := r.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), submitted.ID, submitted.Version, "APPROVED", "controlled original approval outbox source", draft.CriticalResourceVersions)
	if err != nil {
		t.Fatal("actual authorization writer", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, approved.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("real protected approval event missing", count, err)
	}
	return approved.ID
}

func registrationOutboxSeed(t *testing.T, ctx context.Context, r *PostgresRepository) (uuid.UUID, uuid.UUID) {
	t.Helper()
	p := identity.Principal{Issuer: "https://outbox-fixture.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "outboxhistory" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]}
	me, _, err := r.Bootstrap(ctx, p)
	if err != nil {
		t.Fatal("real account registration producer", err)
	}
	approval := registrationOutboxApproval(t, ctx, r)
	var eventType string
	var payload json.RawMessage
	if err := r.pool.QueryRow(ctx, `SELECT event_type,payload FROM outbox_events WHERE aggregate_id=$1 AND event_type<>'account.approval-command.authorized.v1' ORDER BY occurred_at,event_id LIMIT 1`, me.UserID).Scan(&eventType, &payload); err != nil {
		t.Fatal("real registration outbox sample", err)
	}
	// Historical load is synthetic, retaining a real registration event shape.
	// Protected approval rows above still come from the unchanged authority writer.
	// Published metadata belongs to the owner/relay boundary, not runtime INSERT.
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User = "account_migrator"
	configuration.Password = ""
	owner, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,published_at)
SELECT md5($1::text||':event:'||g)::uuid,$2,md5($1::text||':aggregate:'||g)::uuid,1,$3::jsonb,'controlled-outbox-history',clock_timestamp()-interval '30 days',clock_timestamp()-interval '1 day' FROM generate_series(1,100000) g`, uuid.NewString(), eventType, payload)
	if err != nil {
		t.Fatal("controlled representative history", err)
	}
	var total, published, authorizations int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE published_at IS NOT NULL),count(*) FILTER(WHERE event_type='account.approval-command.authorized.v1') FROM outbox_events`).Scan(&total, &published, &authorizations); err != nil || total < 100002 || published != 100000 || authorizations != 1 {
		t.Fatal("representative distribution missing", total, published, authorizations, err)
	}
	t.Logf("OUTBOX_AGGREGATE_DISTRIBUTION total=%d published_historical=%d actual_authorization=%d actual_registration_event_type=%s", total, published, authorizations, eventType)
	return me.UserID, approval
}

func registrationOutboxAnalyze(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User = "account_migrator"
	configuration.Password = ""
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(ctx, `SET ROLE account_approval_authorization_owner; ANALYZE public.outbox_events`); err != nil {
		t.Fatal(err)
	}
}

func registrationOutboxIndex(t *testing.T, ctx context.Context, r *PostgresRepository, present bool) {
	t.Helper()
	var definition, owner string
	err := r.pool.QueryRow(ctx, `SELECT pg_get_indexdef(i.oid),pg_get_userbyid(i.relowner) FROM pg_class i WHERE i.oid=to_regclass('public.outbox_events_aggregate_idx')`).Scan(&definition, &owner)
	if !present {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("index remains after exact Down", definition, err)
		}
		return
	}
	if err != nil || definition != "CREATE INDEX outbox_events_aggregate_idx ON public.outbox_events USING btree (aggregate_id)" || owner != "account_approval_authorization_owner" {
		t.Fatal("index definition/owner", definition, owner, err)
	}
	var schemaCreate bool
	if err := r.pool.QueryRow(ctx, `SELECT has_schema_privilege('account_approval_authorization_owner','public','CREATE')`).Scan(&schemaCreate); err != nil || schemaCreate {
		t.Fatal("temporary schema CREATE was not revoked", schemaCreate, err)
	}
}

func registrationOutboxPlan(t *testing.T, ctx context.Context, r *PostgresRepository, candidate uuid.UUID, phase string) {
	t.Helper()
	var raw json.RawMessage
	if err := r.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1)`, candidate).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	indexed := strings.Contains(string(raw), "outbox_events_aggregate_idx")
	if (phase == "indexed") != indexed {
		t.Fatal("actual unrestricted aggregate predicate did not use expected plan", phase, string(raw))
	}
	t.Logf("OUTBOX_AGGREGATE_PLAN %s %s", phase, raw)
}

func TestRegistrationOutboxAggregateLookupPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	product := registrationOutboxMigrationTool(t)
	indexPackage := registrationOutboxPackageThrough(t, registrationOutboxIndexVersion)
	unstartedPackage := registrationOutboxPackageThrough(t, 900022)
	fullPackage, err := filepath.Abs("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"fresh-install", "retained-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			f := retainedWalletFenceActors(t)
			ctx := f.ctx
			var before string
			var registered uuid.UUID
			var approval uuid.UUID
			var initial func(context.Context, *pgx.Conn, []migrations.Migration) error
			if scenario == "retained-upgrade" {
				initial = func(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) error {
					var old []migrations.Migration
					for _, m := range loaded {
						if m.Version != registrationOutboxIndexVersion {
							old = append(old, m)
						}
					}
					if err := accountschema.Up(ctx, connection, old); err != nil {
						return err
					}
					migrations.Unlock(ctx, connection)
					configuration := connection.Config().Copy()
					legacy, err := NewPostgresRepository(ctx, registrationOutboxDSN(configuration, "account_service"))
					if err != nil {
						return err
					}
					defer legacy.Close()
					registered, approval = registrationOutboxSeed(t, ctx, legacy)
					before = registrationOutboxFacts(t, ctx, legacy)
					return nil
				}
			}
			r := f.repositoryThroughVersion(t, registrationOutboxIndexVersion, initial)
			if scenario == "fresh-install" {
				registered, approval = registrationOutboxSeed(t, ctx, r)
				before = registrationOutboxFacts(t, ctx, r)
			}
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("index installation changed original event/ledger/ACL facts")
			}
			registrationOutboxIndex(t, ctx, r, true)
			registrationOutboxAnalyze(t, ctx, r)
			absent := registrationOutboxCandidate(t, ctx, r, "+8613800138010")
			referenced := registrationOutboxCandidate(t, ctx, r, "+8613800138011")
			var eventType string
			var raw json.RawMessage
			if err := r.pool.QueryRow(ctx, `SELECT event_type,payload FROM outbox_events WHERE aggregate_id=$1 ORDER BY occurred_at,event_id LIMIT 1`, registered).Scan(&eventType, &raw); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if json.Unmarshal(raw, &payload) != nil {
				t.Fatal("registration event shape")
			}
			payload["user_id"] = referenced.CandidateUserID
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := appendOutbox(ctx, tx, eventType, referenced.CandidateUserID, 1, payload); err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			configuration := r.pool.Config().ConnConfig.Copy()
			owner, err := pgx.Connect(ctx, registrationOutboxDSN(configuration, "account_migrator"))
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			if _, err := owner.Exec(ctx, "SET ROLE account_approval_authorization_owner"); err != nil {
				t.Fatal(err)
			}
			// Controlled owner publication of the same candidate's retained
			// reference: unrelated published load alone cannot prove this veto.
			published, err := owner.Exec(ctx, `UPDATE outbox_events SET published_at=clock_timestamp() WHERE aggregate_id=$1 AND event_type=$2`, referenced.CandidateUserID, eventType)
			if err != nil || published.RowsAffected() != 1 {
				t.Fatal("could not publish the candidate's controlled historical reference", err)
			}
			if _, err := owner.Exec(ctx, "SET ROLE account_service"); err != nil {
				t.Fatal(err)
			}
			var referencePublished bool
			if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1 AND published_at IS NOT NULL)`, referenced.CandidateUserID).Scan(&referencePublished); err != nil || !referencePublished {
				t.Fatal("same-candidate published reference is missing", err)
			}
			// The deliberate historical reference must veto compensation even
			// though no current user/wallet/identity row carries that candidate.
			before = registrationOutboxFacts(t, ctx, r)
			var exists bool
			if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1)`, registered).Scan(&exists); err != nil || !exists {
				t.Fatal("actual registration facts disappeared", exists, err)
			}
			if output, err := registrationOutboxCLI(ctx, product, r, "account_service", "down", indexPackage); err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
				t.Fatal("runtime login removed the owner-only index", string(output))
			}
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("refused runtime Down changed retained facts")
			}
			registrationOutboxIndex(t, ctx, r, true)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "down", indexPackage); err != nil {
				t.Fatalf("actual owner-role CLI Down: %v\n%s", err, output)
			}
			registrationOutboxIndex(t, ctx, r, false)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("index Down changed original evidence/ledger/ACL")
			}
			if output, err := registrationOutboxCLI(ctx, product, r, "account_service", "up", indexPackage); err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
				t.Fatal("runtime login installed owner-only index", string(output))
			}
			registrationOutboxIndex(t, ctx, r, false)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("failed runtime Up left schema authority or changed retained facts")
			}
			registrationOutboxAnalyze(t, ctx, r)
			registrationOutboxMeasurePhase(t, ctx, r, "baseline", registered, approval, absent, referenced)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "up", indexPackage); err != nil {
				t.Fatalf("actual owner-role CLI Up: %v\n%s", err, output)
			}
			registrationOutboxIndex(t, ctx, r, true)
			registrationOutboxAnalyze(t, ctx, r)
			registrationOutboxMeasurePhase(t, ctx, r, "indexed", registered, approval, absent, referenced)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("index restore changed retained facts")
			}
			stable := registrationOutboxStableFacts(t, ctx, r)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "up", unstartedPackage); err != nil {
				t.Fatalf("public upgrade to main unstarted-grant inventory: %v\n%s", err, output)
			}
			loaded, err := migrations.Load(unstartedPackage)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := accountschema.VerifyApplied(ctx, owner, loaded); err != nil {
				t.Fatal("main inventory and index delivery failed verification", err)
			}
			migrations.Unlock(ctx, owner)
			beforeCurrent := registrationOutboxFacts(t, ctx, r)
			err = accountschema.DownOne(ctx, owner, loaded)
			migrations.Unlock(ctx, owner)
			var refused *pgconn.PgError
			if !errors.As(err, &refused) || refused.Code != "55000" || !strings.Contains(refused.Message, "900022 inventory is forward-only") {
				t.Fatalf("later main guard was bypassed or refused for another reason: %v", err)
			}
			if registrationOutboxFacts(t, ctx, r) != beforeCurrent {
				t.Fatal("refused main Down changed index or original facts")
			}
			registrationOutboxIndex(t, ctx, r, true)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "up", fullPackage); err != nil {
				t.Fatalf("complete current public package upgrade: %v\n%s", err, output)
			}
			loaded, err = migrations.Load(fullPackage)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := accountschema.VerifyApplied(ctx, owner, loaded); err != nil {
				t.Fatal("complete current package failed public verification", err)
			}
			migrations.Unlock(ctx, owner)
			if registrationOutboxStableFacts(t, ctx, r) != stable {
				t.Fatal("later main upgrade changed original outbox evidence or authority")
			}
			if scenario == "retained-upgrade" {
				// A separate genuine current-main database already carries900022
				// but has never seen900020. The public installer must fill this
				// lower gap without modifying any older row or skipping main guards.
				var gapBefore string
				backfilled := f.repositoryWithInitialStage(t, func(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) error {
					var previous []migrations.Migration
					for _, migration := range loaded {
						if migration.Version != registrationOutboxIndexVersion {
							previous = append(previous, migration)
						}
					}
					if err := accountschema.Up(ctx, connection, previous); err != nil {
						return err
					}
					migrations.Unlock(ctx, connection)
					var later bool
					if err := connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900022) AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900020)`).Scan(&later); err != nil {
						return err
					}
					if !later {
						return errors.New("current-main lower-gap fixture was not established")
					}
					legacy, err := NewPostgresRepository(ctx, registrationOutboxDSN(connection.Config(), "account_service"))
					if err != nil {
						return err
					}
					defer legacy.Close()
					registrationOutboxSeed(t, ctx, legacy)
					registrationOutboxIndex(t, ctx, legacy, false)
					gapBefore = registrationOutboxFacts(t, ctx, legacy)
					return nil
				})
				registrationOutboxIndex(t, ctx, backfilled, true)
				if registrationOutboxFacts(t, ctx, backfilled) != gapBefore {
					t.Fatal("lower-gap installation changed current main evidence or ledger")
				}
				if output, err := registrationOutboxCLI(ctx, product, backfilled, "account_migrator", "up", fullPackage); err != nil {
					t.Fatalf("current lower-gap idempotent CLI install: %v\n%s", err, output)
				}
				if registrationOutboxFacts(t, ctx, backfilled) != gapBefore {
					t.Fatal("idempotent install changed lower-gap evidence or ledger")
				}
			}
			t.Logf("OUTBOX_AGGREGATE_VERIFIED scenario=%s runtime=account_service installer=account_migrator raw_event_and_old_ledger_unchanged=true", scenario)
		})
	}
}
