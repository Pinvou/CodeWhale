package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminApprovalControlPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	connection, pool, loaded := newApprovalControlIntegrationDatabase(t, ctx)
	var database string
	if err := connection.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{database}.Sanitize()+` TO account_migrator`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	var session, role string
	var superuser bool
	if err := connection.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&session, &role, &superuser); err != nil || session != "account_migrator" || role != "account_service" || superuser {
		t.Fatal("control migrations must execute under the dedicated non-superuser identity")
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	initial := readControlTuple(t, ctx, pool)
	if initial.ID != "ADMIN_APPROVAL" || initial.Schema != 1 || initial.Mode != "LEGACY" || initial.Wire != 1 || initial.Proof || initial.Relay != "RUNNING" || initial.ApprovalEpoch != 1 || initial.RelayEpoch != 1 || initial.Version != 1 || initial.UpdatedAt.IsZero() {
		t.Fatalf("unsafe initial control tuple: %+v", initial)
	}
	var canCreate bool
	if err := connection.QueryRow(ctx, `SELECT has_database_privilege('account_migrator',current_database(),'CREATE')`).Scan(&canCreate); err != nil || canCreate {
		t.Fatal("temporary migration database CREATE was not revoked")
	}
	runtime := newControlActor(t, ctx, pool, "account_service")
	owner := newControlActor(t, ctx, pool, "account_migrator")
	var functionDefinition string
	if err := connection.QueryRow(ctx, `SELECT pg_get_functiondef('account_approval_control.lock_admin_approval_control_shared()'::regprocedure)`).Scan(&functionDefinition); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE account_approval_control.admin_approval_control SET version=version+1`,
		`DELETE FROM account_approval_control.admin_approval_control`,
		`TRUNCATE account_approval_control.admin_approval_control`,
		`ALTER TABLE account_approval_control.admin_approval_control DISABLE TRIGGER USER`,
		`DROP TABLE account_approval_control.admin_approval_control`,
		`DROP SCHEMA account_approval_control CASCADE`,
		`SET ROLE account_approval_control_owner`,
		`SET ROLE account_migrator`,
		functionDefinition,
	} {
		_, err := runtime.Exec(ctx, statement)
		requireControlSQLState(t, err, "42501")
	}
	// A malicious same-name public object and a runtime-controlled search_path
	// must not redirect either product reader or its SECURITY DEFINER lock.
	if _, err := runtime.Exec(ctx, `CREATE TABLE public.admin_approval_control(fake TEXT); CREATE FUNCTION public.lock_admin_approval_control_shared() RETURNS TEXT LANGUAGE SQL AS 'SELECT ''HIJACKED'''; SET search_path TO public,pg_catalog`); err != nil {
		t.Fatal(err)
	}
	tx, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil || !reflect.DeepEqual(read, shared) {
		t.Fatalf("qualified read/shared lock was hijacked: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockAdminApprovalControlExclusive(ctx, tx); err == nil {
		t.Fatal("runtime obtained the owner-only exclusive lock")
	}
	_ = tx.Rollback(ctx)

	t.Run("shared before exclusive", func(t *testing.T) {
		sharedTx, err := runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sharedTx.Rollback(ctx)
		if _, err := LockAdminApprovalControlShared(ctx, sharedTx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			ownerTx, err := owner.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			defer ownerTx.Rollback(ctx)
			if _, err = ownerTx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err == nil {
				_, err = LockAdminApprovalControlExclusive(ctx, ownerTx)
			}
			if err == nil {
				_, err = ownerTx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1`)
			}
			if err == nil {
				err = ownerTx.Commit(ctx)
			}
			done <- err
		}()
		waitControlBlocked(t, ctx, pool, owner.PgConn().PID(), runtime.PgConn().PID())
		if err := sharedTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	for _, finish := range []string{"commit", "rollback", "connection-close"} {
		t.Run("exclusive before shared/"+finish, func(t *testing.T) {
			before := readControlTuple(t, ctx, pool)
			ownerTx, err := owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer ownerTx.Rollback(ctx)
			if _, err := ownerTx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
				t.Fatal(err)
			}
			if _, err := LockAdminApprovalControlExclusive(ctx, ownerTx); err != nil {
				t.Fatal(err)
			}
			if _, err := ownerTx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state=CASE authorization_relay_state WHEN 'FENCED' THEN 'RUNNING' ELSE 'FENCED' END,authorization_relay_epoch=authorization_relay_epoch+1,version=version+1`); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				sharedTx, err := runtime.Begin(ctx)
				if err != nil {
					done <- err
					return
				}
				defer sharedTx.Rollback(ctx)
				_, err = LockAdminApprovalControlShared(ctx, sharedTx)
				if err == nil {
					err = sharedTx.Commit(ctx)
				}
				done <- err
			}()
			waitControlBlocked(t, ctx, pool, runtime.PgConn().PID(), owner.PgConn().PID())
			switch finish {
			case "commit":
				err = ownerTx.Commit(ctx)
			case "rollback":
				err = ownerTx.Rollback(ctx)
			case "connection-close":
				err = owner.Close(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			after := readControlTuple(t, ctx, pool)
			if finish == "commit" {
				if after.ApprovalEpoch != before.ApprovalEpoch || after.RelayEpoch != before.RelayEpoch+1 || after.Version != before.Version+1 || after.Relay == before.Relay {
					t.Fatal("relay transition changed wrong epoch")
				}
			} else if !reflect.DeepEqual(before, after) {
				t.Fatal("aborted control transaction left partial state")
			}
		})
	}
	owner = newControlActor(t, ctx, pool, "account_migrator")
	if _, err := owner.Exec(ctx, `SET ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct{ statement, state string }{
		{`UPDATE account_approval_control.admin_approval_control SET mode=mode`, "23514"},
		{`UPDATE account_approval_control.admin_approval_control SET control_id='OTHER'`, "55000"},
		{`UPDATE account_approval_control.admin_approval_control SET schema_version=2`, "55000"},
		{`UPDATE account_approval_control.admin_approval_control SET version=version+1`, "23514"},
		{`UPDATE account_approval_control.admin_approval_control SET approval_epoch=approval_epoch-1`, "23514"},
		{`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',authorization_relay_state='FENCED',approval_epoch=approval_epoch+1,authorization_relay_epoch=authorization_relay_epoch+1,version=version+1`, "23514"},
		{`DELETE FROM account_approval_control.admin_approval_control`, "55000"},
		{`TRUNCATE account_approval_control.admin_approval_control`, "55000"},
	} {
		before := readControlTuple(t, ctx, pool)
		_, err := owner.Exec(ctx, attempt.statement)
		requireControlSQLState(t, err, attempt.state)
		if !reflect.DeepEqual(before, readControlTuple(t, ctx, pool)) {
			t.Fatal("rejected owner mutation changed control")
		}
	}
	modeSQL := `UPDATE account_approval_control.admin_approval_control SET mode=$1,approval_wire_version=CASE WHEN $1='PROOF_REQUIRED' THEN 2 ELSE 1 END,proof_required=($1 IN ('PROOF_REQUIRED','SAFE_ROLLBACK')),approval_epoch=approval_epoch+1,version=version+1,updated_at='2000-01-01' WHERE version=$2`
	relaySQL := `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state=$1,authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at='2000-01-01' WHERE version=$2`
	beforeStale := readControlTuple(t, ctx, pool)
	if tag, err := owner.Exec(ctx, modeSQL, "QUIESCING", beforeStale.Version-1); err != nil || tag.RowsAffected() != 0 {
		t.Fatal("stale expected_version CAS did not reject without changing state")
	}
	if !reflect.DeepEqual(beforeStale, readControlTuple(t, ctx, pool)) {
		t.Fatal("stale CAS changed control")
	}
	for _, step := range []struct {
		kind, target string
		allowed      bool
	}{
		{"mode", "PROOF_REQUIRED", false},
		{"mode", "SAFE_ROLLBACK", false},
		{"relay", "FENCED", true},
		{"mode", "QUIESCING", false},
		{"relay", "RUNNING", true},
		{"mode", "QUIESCING", true},
		{"mode", "LEGACY", true},
		{"mode", "QUIESCING", true},
		{"mode", "PROOF_REQUIRED", false},
		{"relay", "FENCED", true},
		{"mode", "LEGACY", false},
		{"mode", "PROOF_REQUIRED", true},
		{"mode", "LEGACY", false},
		{"mode", "QUIESCING", false},
		{"mode", "SAFE_ROLLBACK", true},
		{"relay", "RUNNING", false},
		{"mode", "PROOF_REQUIRED", true},
		{"relay", "RUNNING", true},
		{"mode", "SAFE_ROLLBACK", true},
		{"mode", "PROOF_REQUIRED", false},
		{"relay", "FENCED", true},
		{"mode", "PROOF_REQUIRED", true},
	} {
		before := readControlTuple(t, ctx, pool)
		statement := modeSQL
		if step.kind == "relay" {
			statement = relaySQL
		}
		tag, err := owner.Exec(ctx, statement, step.target, before.Version)
		if !step.allowed {
			requireControlSQLState(t, err, "23514")
			if !reflect.DeepEqual(before, readControlTuple(t, ctx, pool)) {
				t.Fatal("illegal control edge changed state")
			}
			continue
		}
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("legal %s transition to %s failed: %v", step.kind, step.target, err)
		}
		after := readControlTuple(t, ctx, pool)
		if after.Version != before.Version+1 || after.UpdatedAt.Before(time.Now().Add(-time.Minute)) {
			t.Fatal("control version or trusted database timestamp was not advanced")
		}
		if step.kind == "mode" {
			if after.ApprovalEpoch != before.ApprovalEpoch+1 || after.RelayEpoch != before.RelayEpoch || after.Relay != before.Relay || after.Mode != step.target {
				t.Fatal("mode transition changed the wrong epoch or relay state")
			}
		} else if after.RelayEpoch != before.RelayEpoch+1 || after.ApprovalEpoch != before.ApprovalEpoch || after.Mode != before.Mode || after.Relay != step.target {
			t.Fatal("relay transition invalidated the proof epoch")
		}
	}
	beforeRepeat := readControlTuple(t, ctx, pool)
	t.Run("lock cancellation", func(t *testing.T) {
		ownerTx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer ownerTx.Rollback(ctx)
		if _, err := LockAdminApprovalControlExclusive(ctx, ownerTx); err != nil {
			t.Fatal(err)
		}
		waiting := newControlActor(t, ctx, pool, "account_service")
		waitingTx, err := waiting.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer waitingTx.Rollback(ctx)
		lockCtx, cancelLock := context.WithCancel(ctx)
		defer cancelLock()
		done := make(chan error, 1)
		go func() { _, err := LockAdminApprovalControlShared(lockCtx, waitingTx); done <- err }()
		waitControlBlocked(t, ctx, pool, waiting.PgConn().PID(), owner.PgConn().PID())
		cancelLock()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled lock returned an authorization observation")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("control lock ignored caller context cancellation")
		}
	})
	t.Run("concurrent expected version CAS", func(t *testing.T) {
		before := readControlTuple(t, ctx, pool)
		first, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		if _, err := LockAdminApprovalControlExclusive(ctx, first); err != nil {
			t.Fatal(err)
		}
		if tag, err := first.Exec(ctx, relaySQL, "RUNNING", before.Version); err != nil || tag.RowsAffected() != 1 {
			t.Fatal("first CAS failed")
		}
		second := newControlActor(t, ctx, pool, "account_migrator")
		if _, err := second.Exec(ctx, `SET ROLE account_approval_control_owner`); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			rows int64
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			tag, err := second.Exec(ctx, relaySQL, "RUNNING", before.Version)
			done <- outcome{tag.RowsAffected(), err}
		}()
		waitControlBlocked(t, ctx, pool, second.PgConn().PID(), owner.PgConn().PID())
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		result := <-done
		if result.err != nil || result.rows != 0 {
			t.Fatalf("stale racing CAS advanced control: %+v", result)
		}
		after := readControlTuple(t, ctx, pool)
		if after.Version != before.Version+1 || after.RelayEpoch != before.RelayEpoch+1 || after.ApprovalEpoch != before.ApprovalEpoch {
			t.Fatal("racing CAS advanced more than once")
		}
	})
	beforeRepeat = readControlTuple(t, ctx, pool)
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if !reflect.DeepEqual(beforeRepeat, readControlTuple(t, ctx, pool)) {
		t.Fatal("migration reentry reset control epochs")
	}
	requireControlSQLState(t, migrations.DownOne(ctx, connection, loaded), "55000")
	migrations.Unlock(ctx, connection)
	if !reflect.DeepEqual(beforeRepeat, readControlTuple(t, ctx, pool)) {
		t.Fatal("rollback changed control")
	}
}

func TestAdminApprovalControlMigrationFailClosedPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	connection, pool, loaded := newApprovalControlIntegrationDatabase(t, ctx)
	var database string
	if err := connection.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{database}.Sanitize()+` TO account_migrator; SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded[:len(loaded)-1]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	for _, targetRole := range []string{"account_approval_control_owner", "account_migrator", "account_service"} {
		for _, attribute := range []string{"SUPERUSER", "CREATEDB", "CREATEROLE", "REPLICATION", "BYPASSRLS"} {
			t.Run(targetRole+"/"+attribute, func(t *testing.T) {
				tx, err := connection.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				// Role changes are transactional and deliberately never committed.
				// Other tests/databases cannot observe a privileged role fixture.
				if _, err := tx.Exec(ctx, `ALTER ROLE `+pgx.Identifier{targetRole}.Sanitize()+` `+attribute+`; SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(ctx, loaded[len(loaded)-1].UpSQL)
				requireControlSQLState(t, err, "P0001")
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
					t.Fatal(err)
				}
				var drift bool
				if err := pool.QueryRow(ctx, `SELECT bool_or(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls) FROM pg_roles WHERE rolname IN ('account_approval_control_owner','account_migrator','account_service')`).Scan(&drift); err != nil || drift {
					t.Fatal("role drift escaped its rolled-back test transaction")
				}
				var schemaExists bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='account_approval_control')`).Scan(&schemaExists); err != nil || schemaExists {
					t.Fatal("privileged-role migration created control objects")
				}
				assertControlMigrationUnapplied(t, ctx, pool)
			})
		}
	}
	for _, fixture := range []string{
		`CREATE SCHEMA account_approval_control AUTHORIZATION account_service`,
		`CREATE SCHEMA account_approval_control AUTHORIZATION account_approval_control_owner; SET ROLE account_approval_control_owner; CREATE FUNCTION account_approval_control.future_function() RETURNS TEXT LANGUAGE SQL AS 'SELECT ''future-schema'''; RESET ROLE`,
	} {
		if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE; `+fixture); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
			t.Fatal(err)
		}
		err := migrations.Up(ctx, connection, loaded)
		migrations.Unlock(ctx, connection)
		if err == nil {
			t.Fatal("migration accepted an occupied control schema")
		}
		assertControlMigrationUnapplied(t, ctx, pool)
		if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE; DROP SCHEMA account_approval_control CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	// Inject only a test-side final SQL failure. The original migration bytes
	// are separately executed by the successful database test above.
	failing := append([]migrations.Migration(nil), loaded...)
	last := len(failing) - 1
	failing[last].UpSQL += "\nSELECT 1/0;"
	digest := sha256.Sum256([]byte(strings.ReplaceAll(failing[last].UpSQL, "\r\n", "\n")))
	failing[last].Checksum = hex.EncodeToString(digest[:])
	err := migrations.Up(ctx, connection, failing)
	migrations.Unlock(ctx, connection)
	requireControlSQLState(t, err, "22012")
	assertControlMigrationUnapplied(t, ctx, pool)
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='account_approval_control')`).Scan(&present); err != nil || present {
		t.Fatal("failed migration left control schema or privileges partially committed")
	}
	var createStillGranted bool
	if err := pool.QueryRow(ctx, `SELECT has_database_privilege('account_migrator',current_database(),'CREATE')`).Scan(&createStillGranted); err != nil || !createStillGranted {
		t.Fatal("failed transaction did not restore externally provisioned CREATE prerequisite")
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := migrations.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	if got := readControlTuple(t, ctx, pool); got.Mode != "LEGACY" || got.Version != 1 {
		t.Fatal("retry after failed migration did not initialize exactly once")
	}
}

func assertControlMigrationUnapplied(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var latest int64
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != 49 {
		t.Fatalf("failed migration changed ledger: %d %v", latest, err)
	}
}

type controlTuple struct {
	ID            string
	Schema        int
	Mode          string
	Wire          int
	Proof         bool
	Relay         string
	ApprovalEpoch int64
	RelayEpoch    int64
	Version       int64
	UpdatedAt     time.Time
}

func readControlTuple(t *testing.T, ctx context.Context, pool *pgxpool.Pool) controlTuple {
	t.Helper()
	var item controlTuple
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_approval_control.admin_approval_control`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("control is not a singleton: %d %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT control_id,schema_version,mode,approval_wire_version,proof_required,authorization_relay_state,approval_epoch,authorization_relay_epoch,version,updated_at FROM account_approval_control.admin_approval_control`).Scan(&item.ID, &item.Schema, &item.Mode, &item.Wire, &item.Proof, &item.Relay, &item.ApprovalEpoch, &item.RelayEpoch, &item.Version, &item.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	return item
}

func newControlActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) *pgx.Conn {
	t.Helper()
	connection, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return connection
}

func requireControlSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != state {
		t.Fatalf("expected SQLSTATE %s, got %v", state, err)
	}
}

func waitControlBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter, holder uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT $2::integer=ANY(pg_blocking_pids($1::integer))`, int32(waiter), int32(holder)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("expected database lock wait was not observed")
		case <-ticker.C:
		}
	}
}

// Control SQL is deliberately schema-qualified in production. A separate
// database tests that exact SQL, including its owner and search-path boundary;
// do not make the product schema configurable to fit older schema-only tests.
func newApprovalControlIntegrationDatabase(t *testing.T, ctx context.Context) (*pgx.Conn, *pgxpool.Pool, []migrations.Migration) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	configuration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	name := "gsa04_control_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	// Preserve the existing deployment boundary: the runtime still owns the
	// database, but must not own or be able to mutate the isolated control schema.
	if _, err := bootstrap.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER account_service TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// The target is solely this test's UUID-named database, never the database
		// supplied by the developer. All child connections close before cleanup.
		if _, err := bootstrap.Exec(cleanupCtx, `DROP DATABASE `+identifier); err != nil {
			t.Errorf("remove isolated control test database: %v", err)
		}
	})
	configuration = configuration.Copy()
	configuration.Database = name
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 || loaded[len(loaded)-1].Version != 50 {
		t.Fatal("control integration fixture must explicitly isolate migration 50 from future migrations")
	}
	return connection, pool, loaded
}
