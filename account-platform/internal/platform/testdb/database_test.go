package testdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoopbackHostRejectsExternalAndSocketHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "localhost", "LOCALHOST"} {
		if !loopbackHost(host) {
			t.Errorf("loopback host rejected: %q", host)
		}
	}
	for _, host := range []string{"", "/tmp", "0.0.0.0", "::", "192.0.2.1", "postgres.example", "localhost.example"} {
		if loopbackHost(host) {
			t.Errorf("non-loopback host accepted: %q", host)
		}
	}
}

func TestOwnedConnectionStringPreservesParametersAndRoundTrips(t *testing.T) {
	for _, dsn := range []string{
		"postgresql://fixture:encoded%40password@127.0.0.1:5432/original?sslmode=disable&application_name=fixture&search_path=public&dbname=original",
		"host=127.0.0.1 port=5432 user=fixture password='encoded@password' dbname=original sslmode=disable application_name=fixture search_path=public",
	} {
		config, err := pgxpool.ParseConfig(fixtureConnectionString(dsn, "owned_database", `"owned_schema",pg_catalog`))
		if err != nil {
			t.Fatal("owned configuration did not parse")
		}
		roundTrip, err := pgxpool.ParseConfig(config.ConnString())
		if err != nil {
			t.Fatal("owned connection string did not round trip")
		}
		if roundTrip.ConnConfig.Database != "owned_database" || roundTrip.ConnConfig.RuntimeParams["search_path"] != `"owned_schema",pg_catalog` || roundTrip.ConnConfig.RuntimeParams["application_name"] != "fixture" || roundTrip.ConnConfig.Password != "encoded@password" || roundTrip.ConnConfig.TLSConfig != nil {
			t.Fatal("owned connection string lost isolation or original connection parameters")
		}
	}
}

func TestPostgresSchemaIsolationAndCleanup(t *testing.T) {
	if strings.TrimSpace(os.Getenv(DatabaseURLEnv)) == "" {
		t.Skip(DatabaseURLEnv + " is not configured; real PostgreSQL test not executed")
	}
	for _, domain := range []string{"account", "billing", "token", "payment-simulator"} {
		t.Run(domain, func(t *testing.T) {
			loaded, err := migrations.Load(migrationDirectory(t, domain))
			if err != nil || len(loaded) == 0 {
				t.Fatalf("load expected fixture migrations: %v", err)
			}
			expectedVersion := loaded[len(loaded)-1].Version
			var schemas []string
			var adminConfig *pgx.ConnConfig
			t.Run("create-two-fixtures", func(t *testing.T) {
				for range 2 {
					pool := Open(t, domain)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					var schema string
					var version int64
					if err := pool.QueryRow(ctx, "SELECT current_schema(), max(version) FROM schema_migrations").Scan(&schema, &version); err != nil {
						t.Fatalf("read actual migration ledger: %v", err)
					}
					if !strings.HasPrefix(schema, "pinvou_a01_") || version != expectedVersion {
						t.Fatalf("unexpected fixture schema/version: %s/%d", schema, version)
					}
					if len(schemas) > 0 && schema == schemas[0] {
						t.Fatal("two fixtures share a schema")
					}
					// The helper's exported connection string is also used to create
					// cross-domain HTTP service repositories in integration tests.
					second, err := pgx.Connect(ctx, pool.Config().ConnString())
					if err != nil {
						t.Fatal("reconnect using owned fixture address failed")
					}
					var secondSchema string
					var secondVersion int64
					queryErr := second.QueryRow(ctx, "SELECT current_schema(), max(version) FROM schema_migrations").Scan(&secondSchema, &secondVersion)
					_ = second.Close(ctx)
					if queryErr != nil || secondSchema != schema || secondVersion != version {
						t.Fatal("exported fixture connection string escaped owned schema/database")
					}
					schemas = append(schemas, schema)
					adminConfig = pool.Config().ConnConfig.Copy()
				}
			})
			if len(schemas) == 0 {
				t.Fatal("no PostgreSQL fixtures were created")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			adminConfig.RuntimeParams["search_path"] = "pg_catalog"
			originalConfig, err := pgx.ParseConfig(os.Getenv(DatabaseURLEnv))
			if err != nil {
				t.Fatal("reload original test database configuration failed")
			}
			adminConfig.Database = originalConfig.Database
			admin, err := pgx.ConnectConfig(ctx, adminConfig)
			if err != nil {
				t.Fatalf("reconnect after fixture cleanup: %v", err)
			}
			defer admin.Close(ctx)
			for _, schema := range schemas {
				var remains bool
				if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", schema).Scan(&remains); err != nil || remains {
					t.Fatalf("fixture database survived cleanup: %s/%v", schema, err)
				}
			}
		})
	}
}
