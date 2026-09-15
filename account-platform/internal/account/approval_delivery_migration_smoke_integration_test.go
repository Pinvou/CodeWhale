package account

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This early migration smoke owns a fresh database on the explicitly opted-in
// isolated cluster. It does not substitute for Begin/CAS/concurrency tests.
func newApprovalDeliveryIntegrationDatabase(t *testing.T) (*approvalAuthorizationIntegrationDatabase, *PostgresRepository, *ApprovalRelayStore, []migrations.Migration) {
	t.Helper()
	db, relay := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = approvalDeliveryCurrentMigrations(loaded)
	if err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			t.Logf("consumer migration SQLSTATE=%s position=%d internal_position=%d context=%s", postgresError.Code, postgresError.Position, postgresError.InternalPosition, postgresError.Where)
		}
		t.Fatalf("apply consumer migration: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	db.runtimePool.Close()
	db.runtimePool = newApprovalDeliveryRuntimeLogin(t, ctx, db)
	repository := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool,
		now: func() time.Time { return time.Now().UTC() }}).
		WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
	return db, repository, relay, loaded
}

func approvalDeliveryBaselineMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 53 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("consumer integration fixture requires Account migration 53")
}

func approvalDeliveryHardeningMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 54 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("consumer integration fixture requires Account migration 54")
}

func approvalDeliveryCurrentMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 56 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("consumer integration fixture requires Account migration 56")
}

func TestApprovalDeliveryHistoricalMigrationBoundary(t *testing.T) {
	for _, versions := range [][]int64{{52, 53}, {52, 53, 54, 55}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "original up", DownSQL: "original down", Checksum: "original checksum"}
		}
		baseline, err := approvalDeliveryBaselineMigrations(loaded)
		if err != nil || len(baseline) != 2 || baseline[1] != loaded[1] || baseline[1].Version != 53 {
			t.Fatal("consumer historical boundary changed")
		}
		if len(loaded) != len(versions) || loaded[len(loaded)-1].Version != versions[len(versions)-1] {
			t.Fatal("historical selection changed full chain")
		}
	}
	if _, err := approvalDeliveryBaselineMigrations([]migrations.Migration{{Version: 52}, {Version: 54}}); err == nil {
		t.Fatal("missing consumer baseline accepted")
	}
	for _, versions := range [][]int64{{53, 54}, {53, 54, 55, 56}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "original up", DownSQL: "original down", Checksum: "original checksum"}
		}
		hardening, err := approvalDeliveryHardeningMigrations(loaded)
		if err != nil || len(hardening) != 2 || hardening[1] != loaded[1] || hardening[1].Version != 54 {
			t.Fatal("consumer hardening boundary changed")
		}
	}
	if _, err := approvalDeliveryHardeningMigrations([]migrations.Migration{{Version: 53}, {Version: 55}}); err == nil {
		t.Fatal("missing consumer hardening accepted")
	}
	for _, versions := range [][]int64{{55, 56}, {55, 56, 57, 58}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "original up", DownSQL: "original down", Checksum: "original checksum"}
		}
		current, err := approvalDeliveryCurrentMigrations(loaded)
		if err != nil || len(current) != 2 || current[1] != loaded[1] || current[1].Version != 56 {
			t.Fatal("consumer current boundary changed")
		}
	}
	if _, err := approvalDeliveryCurrentMigrations([]migrations.Migration{{Version: 55}, {Version: 57}}); err == nil {
		t.Fatal("missing consumer delivery reader accepted")
	}
}

func newApprovalDeliveryRuntimeLogin(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase) *pgxpool.Pool {
	t.Helper()
	configuration, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil || (configuration.Scheme != "postgresql" && configuration.Scheme != "postgres") {
		t.Fatal("invalid isolated database URL")
	}
	configuration.Path = "/" + db.databaseName
	var originalVerifier *string
	var login bool
	if err := db.adminPool.QueryRow(ctx, `SELECT rolpassword,rolcanlogin FROM pg_catalog.pg_authid WHERE rolname='account_service'`).Scan(&originalVerifier, &login); err != nil || !login {
		t.Fatal("isolated runtime LOGIN precondition")
	}
	password := uuid.NewString()
	// Isolated-cluster tests are serialized. Restore the previous verifier in
	// memory; neither it nor the temporary credential is printed or persisted.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		literal := "NULL"
		if originalVerifier != nil {
			literal = "'" + strings.ReplaceAll(*originalVerifier, "'", "''") + "'"
		}
		if _, err := db.adminPool.Exec(cleanup, "ALTER ROLE account_service PASSWORD "+literal); err != nil {
			t.Error("restore isolated runtime verifier")
		}
	})
	if _, err := db.adminPool.Exec(ctx, "ALTER ROLE account_service PASSWORD '"+password+"'"); err != nil {
		t.Fatal("configure isolated runtime LOGIN")
	}
	configuration.User = url.UserPassword("account_service", password)
	pool, err := pgxpool.New(ctx, configuration.String())
	if err != nil {
		t.Fatal("create isolated runtime pool")
	}
	t.Cleanup(pool.Close)
	var actual bool
	if err := pool.QueryRow(ctx, `SELECT session_user='account_service' AND current_user='account_service'`).Scan(&actual); err != nil || !actual {
		t.Fatal("real runtime identity assertion")
	}
	return pool
}

func TestApprovalDeliveryMigrationSmokeIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var version, rows int64
	if err := db.adminPool.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&version); err != nil || version != 56 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM account_approval_authorization.approval_delivery_states`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("initial delivery rows=%d err=%v", rows, err)
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT * FROM account_approval_authorization.approval_delivery_states`); err == nil {
		t.Fatal("runtime acquired direct delivery table access")
	} else {
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "42501" {
			t.Fatalf("expected permission rejection: %v", err)
		}
	}
}
