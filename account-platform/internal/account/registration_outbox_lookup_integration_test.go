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

func registrationOutboxCLI(ctx context.Context, product string, r *PostgresRepository, actor, direction string) ([]byte, error) {
	configuration := r.pool.Config().ConnConfig.Copy()
	directory, err := filepath.Abs("../../migrations/account")
	if err != nil {
		return nil, err
	}
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
			r := f.repositoryWithInitialStage(t, initial)
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
			// The deliberate historical reference must veto compensation even
			// though no current user/wallet/identity row carries that candidate.
			before = registrationOutboxFacts(t, ctx, r)
			var exists bool
			if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1)`, registered).Scan(&exists); err != nil || !exists {
				t.Fatal("actual registration facts disappeared", exists, err)
			}
			if output, err := registrationOutboxCLI(ctx, product, r, "account_service", "down"); err == nil {
				t.Fatal("runtime login removed the owner-only index", string(output))
			}
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("refused runtime Down changed retained facts")
			}
			registrationOutboxIndex(t, ctx, r, true)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "down"); err != nil {
				t.Fatalf("actual owner-role CLI Down: %v\n%s", err, output)
			}
			registrationOutboxIndex(t, ctx, r, false)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("index Down changed original evidence/ledger/ACL")
			}
			if output, err := registrationOutboxCLI(ctx, product, r, "account_service", "up"); err == nil {
				t.Fatal("runtime login installed owner-only index", string(output))
			}
			registrationOutboxIndex(t, ctx, r, false)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("failed runtime Up left schema authority or changed retained facts")
			}
			registrationOutboxAnalyze(t, ctx, r)
			registrationOutboxMeasurePhase(t, ctx, r, "baseline", registered, approval, absent, referenced)
			if output, err := registrationOutboxCLI(ctx, product, r, "account_migrator", "up"); err != nil {
				t.Fatalf("actual owner-role CLI Up: %v\n%s", err, output)
			}
			registrationOutboxIndex(t, ctx, r, true)
			registrationOutboxAnalyze(t, ctx, r)
			registrationOutboxMeasurePhase(t, ctx, r, "indexed", registered, approval, absent, referenced)
			if registrationOutboxFacts(t, ctx, r) != before {
				t.Fatal("index restore changed retained facts")
			}
			t.Logf("OUTBOX_AGGREGATE_VERIFIED scenario=%s runtime=account_service installer=account_migrator raw_event_and_old_ledger_unchanged=true", scenario)
		})
	}
}
