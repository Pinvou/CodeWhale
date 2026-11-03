package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	errOwnedBillingCleanupIdentity = errors.New("IDENTITY_MISMATCH")
	errOwnedBillingCleanupQuery    = errors.New("OBSERVATION_FAILED")
	errOwnedBillingCleanupSessions = errors.New("SESSIONS_NOT_CLOSED")
	ownedBillingFixtureName        = regexp.MustCompile(`^billing_entitlement_[0-9a-f]{32}$`)
)

// pgx Close closes the socket; it does not acknowledge removal of the server
// backend from pg_stat_activity. Keep every identity/session guard while
// observing server completion within the caller's existing cleanup budget.
// This helper never drops a database or terminates a connection.
func waitOwnedBillingFixtureSessionsClosed(ctx context.Context, admin *pgx.Conn, database string) error {
	if !ownedBillingFixtureName.MatchString(database) {
		return errOwnedBillingCleanupIdentity
	}
	err := cifixture.WaitDatabaseIdle(ctx, admin, database, "billing_schema_owner")
	if err == nil {
		return nil
	}
	if errors.Is(err, cifixture.ErrDatabaseOwner) || errors.Is(err, cifixture.ErrDatabaseIdentity) || errors.Is(err, cifixture.ErrDatabaseUnregistered) {
		return errOwnedBillingCleanupIdentity
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errOwnedBillingCleanupSessions
	}
	return errOwnedBillingCleanupQuery
}

func TestOwnedBillingCleanupGuardsPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("explicit owned Billing fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("invalid owned fixture configuration")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("owned fixture control connection failed")
	}
	defer func() {
		if err := admin.Close(context.Background()); err != nil {
			t.Error("owned fixture control close failed")
		}
	}()
	database := "billing_entitlement_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var fixtureDatabase *cifixture.Database
	identifier := pgx.Identifier{database}.Sanitize()
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, admin, database, "CREATE DATABASE "+identifier+" OWNER billing_schema_owner TEMPLATE template0")
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal("owned fixture creation failed")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		control, e := pgx.ConnectConfig(clean, config)
		if e != nil {
			t.Error("owned cleanup control connection failed")
			return
		}
		defer func() {
			if err := control.Close(context.Background()); err != nil {
				t.Error("owned cleanup control close failed")
			}
		}()
		if e := waitOwnedBillingFixtureSessionsClosed(clean, control, database); e != nil {
			t.Errorf("owned guard retained fixture: %s", e)
			return
		}
		if _, e := cifixture.DropDatabase(clean, control, fixtureDatabase, "billing_schema_owner"); e != nil {
			t.Error("owned fixture drop failed")
		}
	})
	assertExists := func() {
		t.Helper()
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", database).Scan(&exists); err != nil || !exists {
			t.Fatal("guard must preserve database")
		}
	}
	t.Run("active-session-retained-with-bounded-timeout", func(t *testing.T) {
		// A deadline can close its pgx query connection. Keep this failure-path
		// control separate from the control used by later identity assertions.
		observer, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal("owned observation control connection failed")
		}
		defer func() {
			if err := observer.Close(context.Background()); err != nil {
				t.Error("owned observation control close failed")
			}
		}()
		heldConfig := config.Copy()
		heldConfig.Database = database
		held, err := pgx.ConnectConfig(ctx, heldConfig)
		if err != nil {
			t.Fatal("owned active session failed")
		}
		defer func() {
			if err := held.Close(context.Background()); err != nil {
				t.Error("owned active session close failed")
			}
		}()
		observe, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		if err := waitOwnedBillingFixtureSessionsClosed(observe, observer, database); err != errOwnedBillingCleanupSessions && err != errOwnedBillingCleanupQuery {
			t.Fatal("active session must prevent cleanup")
		}
		var stillActive bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND pid=$2)", database, held.PgConn().PID()).Scan(&stillActive); err != nil || !stillActive {
			t.Fatal("guard must not terminate active session")
		}
		assertExists()
	})
	t.Run("owner-mismatch-retained", func(t *testing.T) {
		if _, err := admin.Exec(ctx, "ALTER DATABASE "+identifier+" OWNER TO billing_service"); err != nil {
			t.Fatal("owned owner-mismatch setup failed")
		}
		defer func() {
			if _, err := admin.Exec(ctx, "ALTER DATABASE "+identifier+" OWNER TO billing_schema_owner"); err != nil {
				t.Error("owned owner restore failed")
			}
		}()
		if err := waitOwnedBillingFixtureSessionsClosed(ctx, admin, database); err != errOwnedBillingCleanupIdentity {
			t.Fatal("wrong owner must fail closed")
		}
		assertExists()
	})
	t.Run("query-error-retained", func(t *testing.T) {
		closed, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal("owned query-error setup failed")
		}
		if err := closed.Close(ctx); err != nil {
			t.Fatal("owned query-error close failed")
		}
		if err := waitOwnedBillingFixtureSessionsClosed(ctx, closed, database); err != errOwnedBillingCleanupQuery {
			t.Fatal("query failure must fail closed")
		}
		assertExists()
	})
	t.Run("zero-sessions-qualified", func(t *testing.T) {
		if err := waitOwnedBillingFixtureSessionsClosed(ctx, admin, database); err != nil {
			t.Fatalf("closed owned sessions not qualified: %s", err)
		}
		assertExists()
	})
}
