package testdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func TestPostgresSchemaIsolationAndCleanup(t *testing.T) {
	if strings.TrimSpace(os.Getenv(DatabaseURLEnv)) == "" {
		t.Skip(DatabaseURLEnv + " is not configured; real PostgreSQL test not executed")
	}
	for _, test := range []struct {
		domain  string
		version int64
	}{
		{"account", 5},
		{"billing", 7},
		{"token", 7},
		{"payment-simulator", 4},
	} {
		t.Run(test.domain, func(t *testing.T) {
			var schemas []string
			var adminConfig *pgx.ConnConfig
			t.Run("create-two-fixtures", func(t *testing.T) {
				for range 2 {
					pool := Open(t, test.domain)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					var schema string
					var version int64
					if err := pool.QueryRow(ctx, "SELECT current_schema(), max(version) FROM schema_migrations").Scan(&schema, &version); err != nil {
						t.Fatalf("read actual migration ledger: %v", err)
					}
					if !strings.HasPrefix(schema, "pinvou_a01_") || version != test.version {
						t.Fatalf("unexpected fixture schema/version: %s/%d", schema, version)
					}
					if len(schemas) > 0 && schema == schemas[0] {
						t.Fatal("two fixtures share a schema")
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
			admin, err := pgx.ConnectConfig(ctx, adminConfig)
			if err != nil {
				t.Fatalf("reconnect after fixture cleanup: %v", err)
			}
			defer admin.Close(ctx)
			for _, schema := range schemas {
				var remains bool
				if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&remains); err != nil || remains {
					t.Fatalf("fixture schema survived cleanup: %s/%v", schema, err)
				}
			}
		})
	}
}
