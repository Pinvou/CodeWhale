package account

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The historical fixture installs the complete 1..51 chain. This fixture then
// applies 52 through the same real, non-superuser migrator connection and uses
// a separate LOGIN credential for the production Relay constructor. Tests on
// this explicitly isolated cluster must be serialized (roles are cluster-wide).
func newApprovalRelayIntegrationDatabase(t *testing.T) (*approvalAuthorizationIntegrationDatabase, *ApprovalRelayStore) {
	t.Helper()
	return newApprovalRelayDatabase(t, false)
}

func newApprovalRelayDatabase(t *testing.T, coldConstructor bool) (*approvalAuthorizationIntegrationDatabase, *ApprovalRelayStore) {
	t.Helper()
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	if _, err := db.adminPool.Exec(ctx, `DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_relay') THEN
  CREATE ROLE account_approval_relay LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
END $$`); err != nil {
		t.Fatal(err)
	}
	configuration, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil || (configuration.Scheme != "postgresql" && configuration.Scheme != "postgres") {
		t.Fatal("invalid isolated database configuration")
	}
	configuration.Path = "/" + db.databaseName
	password := uuid.NewString()
	configuration.User = url.UserPassword("account_approval_relay", password)
	// The password is generated inside the fixture, never printed or persisted.
	if _, err := db.adminPool.Exec(ctx, "ALTER ROLE account_approval_relay PASSWORD '"+password+"'"); err != nil {
		t.Fatal("configure isolated Relay login")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := db.adminPool.Exec(cleanupCtx, "ALTER ROLE account_approval_relay PASSWORD NULL"); err != nil {
			t.Error("clear isolated Relay login")
		}
	})
	loaded, err := loadApprovalRelayBaselineMigrations()
	if err != nil {
		t.Fatal("Relay integration requires complete Account migration 52 chain")
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			t.Logf("isolated Relay migration SQLSTATE=%s position=%d context=%s", postgresError.Code, postgresError.Position, postgresError.Where)
		}
		t.Fatalf("apply authorization Relay migration: %v", err)
	}
	constructorStarted := time.Now()
	var store *ApprovalRelayStore
	if coldConstructor {
		store, err = NewApprovalRelayStore(ctx, configuration.String())
	} else {
		// Cold process/connection creation is fixture preparation, not the
		// state-machine assertion. Keep its one-shot budget separate from the
		// unchanged five-second public readiness gate, with no retry or skip.
		var poolErr error
		pool, poolErr := newApprovalAuthorizationPool(ctx, configuration.String(), "account_approval_relay")
		if poolErr != nil {
			t.Fatal("prepare real Relay LOGIN pool")
		}
		fixtureConfiguration := pool.Config()
		pool.Close()
		// Only fixture preparation may spend the fixture's total minute on a
		// cold Windows backend. Runtime connect/readiness limits stay unchanged.
		fixtureConfiguration.ConnConfig.ConnectTimeout = time.Minute
		pool, poolErr = pgxpool.NewWithConfig(ctx, fixtureConfiguration)
		if poolErr != nil {
			t.Fatal("prepare isolated Relay pool configuration")
		}
		connection, acquireErr := pool.Acquire(ctx)
		if acquireErr != nil {
			pool.Close()
			t.Fatal("prepare real Relay LOGIN connection")
		}
		connection.Release()
		store = &ApprovalRelayStore{pool: pool}
		err = store.Ready(ctx)
	}
	if err != nil {
		if store != nil {
			store.Close()
		}
		var firstPostgresError *pgconn.PgError
		firstCode := "none"
		if errors.As(err, &firstPostgresError) {
			firstCode = firstPostgresError.Code
		}
		stage := "constructor"
		var readinessFailure *approvalRelayReadinessError
		if errors.As(err, &readinessFailure) {
			stage = readinessFailure.stage
		}
		t.Logf("Relay initialization elapsed=%s cold_constructor=%t public_error_redacted=%t parent_deadline=%t first_deadline=%t first_SQLSTATE=%s stage=%s", time.Since(constructorStarted), coldConstructor, errors.Is(err, ErrApprovalRelayUnavailable), errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), firstCode, stage)
		diagnosticPool, diagnosticErr := newApprovalAuthorizationPool(ctx, configuration.String(), "account_approval_relay")
		if diagnosticErr == nil {
			defer diagnosticPool.Close()
			diagnosticErr = (&ApprovalRelayStore{pool: diagnosticPool}).approvalRelayReadiness(ctx)
		}
		var postgresError *pgconn.PgError
		code := "none"
		if errors.As(diagnosticErr, &postgresError) {
			code = postgresError.Code
		}
		t.Logf("Relay diagnostic retry_ok=%t deadline=%t canceled=%t SQLSTATE=%s", diagnosticErr == nil, errors.Is(diagnosticErr, context.DeadlineExceeded), errors.Is(diagnosticErr, context.Canceled), code)
		t.Fatal("production Relay constructor rejected isolated migrated database")
	}
	t.Cleanup(store.Close)
	return db, store
}

func loadApprovalRelayBaselineMigrations() ([]migrations.Migration, error) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		return nil, err
	}
	return approvalRelayBaselineMigrations(loaded)
}

func approvalRelayBaselineMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 52 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("Relay integration fixture requires Account migration 52")
}

func TestApprovalRelayHistoricalMigrationBoundary(t *testing.T) {
	for _, versions := range [][]int64{{50, 51, 52}, {50, 51, 52, 53, 54}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "original up", DownSQL: "original down", Checksum: "original checksum"}
		}
		baseline, err := approvalRelayBaselineMigrations(loaded)
		if err != nil || len(baseline) != 3 || baseline[2] != loaded[2] || baseline[2].Version != 52 {
			t.Fatalf("Relay historical boundary changed migration contents: %v", err)
		}
		if len(loaded) != len(versions) || loaded[len(loaded)-1].Version != versions[len(versions)-1] {
			t.Fatal("Relay historical selection changed the full-chain input")
		}
	}
	if _, err := approvalRelayBaselineMigrations([]migrations.Migration{{Version: 51}, {Version: 53}}); err == nil {
		t.Fatal("missing Relay historical baseline was accepted")
	}
}

func TestApprovalRelayMigrationReadinessIntegration(t *testing.T) {
	db, store := newApprovalRelayDatabase(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version int64
	if err := db.adminPool.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&version); err != nil || version != 52 {
		t.Fatalf("migration version=%d error=%v", version, err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal("dedicated Relay readiness failed")
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats != (ApprovalRelayStats{}) {
		t.Fatalf("empty authorization partition: %+v error=%v", stats, err)
	}
	if err := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool}).ApprovalWriterReady(ctx); err != nil {
		t.Fatal("Relay migration broke existing writer readiness")
	}
	if err := ApprovalBackfillReady(ctx, db.backfillPool); err != nil {
		t.Fatal("Relay migration broke existing backfill readiness")
	}
}
