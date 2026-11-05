package tokenschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/f17fixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestProductionTokenReplayAuditHistoryUpgrade(t *testing.T) {
	legacyRoot := os.Getenv("PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY")
	dsn := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if legacyRoot == "" || dsn == "" {
		t.Skip("owned PostgreSQL and exact published migration package required")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !f17fixture.OwnedConfigurationAllowed(config, os.Getenv("F17_TEST_OWNED_PORT"), os.Getenv("F17_TEST_OWNED_DATA_DIRECTORY")) {
		t.Fatal("explicit owned temporary PostgreSQL required")
	}
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var actualDirectory string
	var outside int
	if err := admin.QueryRow(ctx, `SELECT current_setting('data_directory'),
 (SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1'))+
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user)`).Scan(&actualDirectory, &outside); err != nil || outside != 0 || !f17fixture.DirectoryMatches(actualDirectory, os.Getenv("F17_TEST_OWNED_DATA_DIRECTORY")) {
		t.Fatal("nonempty or unowned PostgreSQL refused", err)
	}
	roles := []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"}
	created := []string{}
	database := "token_history_upgrade_" + uuid.NewString()[:8]
	var fixtureDatabase *cifixture.Database
	databaseCreated := false
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if databaseCreated {
			if _, err := cifixture.DropDatabase(cleanup, admin, fixtureDatabase, admin.Config().User); err != nil {
				t.Error("owned test database cleanup failed; roles retained", err)
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(cleanup, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Error("owned role cleanup failed", err)
			}
		}
	}()
	for _, role := range roles {
		login := "LOGIN"
		if role == "token_schema_owner" || role == "token_cap_policy_deployer" {
			login = "NOLOGIN"
		}
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		created = append(created, role)
	}
	if _, err := admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, admin, database, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal(err)
	}
	databaseCreated = true
	config.Database = database
	operator, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	bootstrap, err := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operator.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	config.User = "token_migrator"
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	legacy, err := migrations.Load(filepath.Join(legacyRoot, "token"))
	if err != nil || len(legacy) != 34 || legacy[17].Checksum != originalEventReplayChecksum {
		t.Fatal("exact published Token 1..34 package required", err)
	}
	if err := migrations.UpWithHooks(ctx, c, legacy, migrations.UpHooks{BeforeApply: prepareTokenMigration, AfterApply: finishTokenMigration, LedgerRole: "token_schema_owner"}); err != nil {
		t.Fatal("install actual published Token package", err)
	}
	migrations.Unlock(ctx, c)
	_, err = c.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
 VALUES('11111111-1111-4111-8111-111111111111','token.fixture','11111111-1111-4111-8111-111111111112',1,'{"preserved":true}','published-upgrade',now());
 INSERT INTO event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at)
 VALUES('11111111-1111-4111-8111-111111111113','11111111-1111-4111-8111-111111111111',repeat('a',64),'published-replay-key',repeat('b',64),'SERVICE','fixture','EVENT_REPLAY','original-run','original audit reason must survive upgrade',now())`)
	if err != nil {
		t.Fatal(err)
	}
	var history, audit string
	if err := c.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s),(SELECT to_jsonb(a)::text FROM event_replay_requests a LIMIT 1)`).Scan(&history, &audit); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, sql string }{
		{"original-function-config", `ALTER FUNCTION reject_event_replay_audit_mutation() RESET search_path`},
		{"canonical-function-config", `ALTER FUNCTION reject_event_replay_audit_mutation() SET search_path TO pg_catalog`},
		{"production-restored-function-config", `ALTER FUNCTION reject_event_replay_audit_mutation() SET search_path TO public, public, pg_catalog`},
	} {
		t.Run(config.name, func(t *testing.T) {
			if _, err := operator.Exec(ctx, config.sql); err != nil {
				t.Fatal(err)
			}
			if _, err := ExpectedHistory(ctx, c, loaded); err != nil {
				t.Fatal("known search path changed the constant immutable audit guard", err)
			}
		})
	}
	for _, corruption := range []struct{ name, apply, restore string }{
		{"unknown-checksum", `UPDATE schema_migrations SET checksum=repeat('0',64) WHERE version=18`, `UPDATE schema_migrations SET checksum='` + originalEventReplayChecksum + `' WHERE version=18`},
		{"unknown-name", `UPDATE schema_migrations SET name='unknown_history' WHERE version=18`, `UPDATE schema_migrations SET name='event_replay_audit' WHERE version=18`},
		{"disabled-audit-guard", `ALTER TABLE event_replay_requests DISABLE TRIGGER event_replay_requests_immutable`, `ALTER TABLE event_replay_requests ENABLE TRIGGER event_replay_requests_immutable`},
		{"unknown-function-setting", `ALTER FUNCTION reject_event_replay_audit_mutation() SET client_min_messages TO debug1`, `ALTER FUNCTION reject_event_replay_audit_mutation() RESET client_min_messages`},
		{"conditional-audit-guard", `DROP TRIGGER event_replay_requests_immutable ON event_replay_requests;
 CREATE TRIGGER event_replay_requests_immutable BEFORE UPDATE OR DELETE ON event_replay_requests FOR EACH ROW WHEN (false) EXECUTE FUNCTION reject_event_replay_audit_mutation()`,
			`DROP TRIGGER event_replay_requests_immutable ON event_replay_requests;
 CREATE TRIGGER event_replay_requests_immutable BEFORE UPDATE OR DELETE ON event_replay_requests FOR EACH ROW EXECUTE FUNCTION reject_event_replay_audit_mutation()`},
	} {
		t.Run(corruption.name, func(t *testing.T) {
			if _, err := operator.Exec(ctx, corruption.apply); err != nil {
				t.Fatal(err)
			}
			if err := Up(ctx, c, loaded); err == nil {
				t.Fatal("corrupted retained history accepted")
			}
			var count int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 34 {
				t.Fatal("rejected history must not apply any migration", count, err)
			}
			if _, err := operator.Exec(ctx, corruption.restore); err != nil {
				t.Fatal(err)
			}
		})
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("upgrade original replay audit history", err)
		}
		migrations.Unlock(ctx, c)
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("migration/runtime verification of retained history", err)
		}
		var afterHistory, afterAudit string
		if err := c.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s WHERE version<=34),
 (SELECT to_jsonb(a)::text FROM event_replay_requests a LIMIT 1)`).Scan(&afterHistory, &afterAudit); err != nil || afterHistory != history || afterAudit != audit {
			t.Fatal("upgrade changed original ledger or replay audit facts", err)
		}
	}
	if _, err := migrations.VerifyApplied(ctx, c, loaded); err == nil {
		t.Fatal("generic verifier must reject the extra history")
	}
	for _, sql := range []string{`UPDATE event_replay_requests SET reason='changed'`, `DELETE FROM event_replay_requests`, `TRUNCATE event_replay_requests`} {
		if _, err := c.Exec(ctx, sql); err == nil {
			t.Fatal("original audit history became mutable", sql)
		}
	}
}
