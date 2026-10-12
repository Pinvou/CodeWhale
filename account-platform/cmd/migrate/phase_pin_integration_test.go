package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A child test process executes the real CLI entry point. This confines its
// log.Fatal exit to the child and requires no alternate migration implementation.
func TestPhaseMigrationCLIChild(t *testing.T) {
	if os.Getenv("PINVOU_SCHEMA_CLI_CHILD") != "owned-phase-down" {
		t.Skip("migration CLI child process only")
	}
	flag.CommandLine = flag.NewFlagSet("migrate", flag.ExitOnError)
	os.Args = []string{"migrate", "--direction", "down"}
	main()
	os.Exit(0)
}

func TestPhaseMigrationCLIPinsBeforeDown(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL schema fixture required")
	}
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	control, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect isolated CLI fixture")
	}
	defer control.Close(context.Background())
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse isolated CLI fixture")
	}
	for _, fixture := range []struct{ domain, alias string }{{"billing", "billing"}, {"payment-simulator", "payment-simulator"}, {"billing", "owned-renamed-domain"}, {"payment-simulator", "owned-renamed-domain"}, {"billing", "owned-renamed-source"}, {"payment-simulator", "owned-renamed-source"}} {
		t.Run(fixture.domain+"/"+fixture.alias, func(t *testing.T) {
			domain := fixture.domain
			name := "f02_cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if _, err := control.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Fatal("create owned CLI database")
			}
			t.Cleanup(func() {
				clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var owner string
				var active int
				if err := control.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); err != nil || owner != config.User || active != 0 {
					t.Error("owned CLI database cleanup guard failed")
					return
				}
				if _, err := control.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
					t.Error("nonforce owned CLI database cleanup failed")
				}
			})
			owned := config.Copy()
			owned.Database = name
			connection, err := pgx.ConnectConfig(ctx, owned)
			if err != nil {
				t.Fatal("connect owned CLI database")
			}
			defer connection.Close(context.Background())
			loaded, err := migrations.Load(filepath.Join("../../migrations", domain))
			if err != nil {
				t.Fatal(err)
			}
			selected := loaded
			if domain == "billing" {
				selected = nil
				for _, m := range loaded {
					if m.Version == 1 || m.Version == 6 || m.Version == 900001 {
						selected = append(selected, m)
					}
				}
				err = billingschema.Up(ctx, connection, selected)
			} else {
				err = simulatorschema.Up(ctx, connection, selected)
			}
			if err != nil {
				t.Fatal(err)
			}
			migrations.Unlock(ctx, connection)
			var before string
			if err := connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(t.TempDir(), fixture.alias)
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for _, m := range selected {
				name := m.Name
				if fixture.alias == "owned-renamed-source" {
					name = "custom_" + name
				}
				for _, direction := range []string{"up", "down"} {
					body := m.UpSQL
					if direction == "up" && fixture.alias == "owned-renamed-source" {
						body += "\nSELECT 1;\n"
					}
					if direction == "down" {
						body = m.DownSQL
						if m.Version == 900001 || domain == "payment-simulator" && m.Version == 6 {
							if domain == "billing" {
								body = "ALTER TABLE outbox_events DROP COLUMN publication_generation;"
							} else {
								body = "ALTER TABLE simulator_orders DROP COLUMN merchant_customer_key;"
							}
						}
					}
					p := filepath.Join(directory, fmt.Sprintf("%06d_%s.%s.sql", m.Version, name, direction))
					if err := os.WriteFile(p, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			parsed, err := url.Parse(raw)
			if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
				t.Fatal("CLI fixture requires a PostgreSQL URL")
			}
			parsed.Path = "/" + name
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, executable, "-test.run=^TestPhaseMigrationCLIChild$")
			for _, value := range os.Environ() {
				key, _, _ := strings.Cut(value, "=")
				if key != "DATABASE_URL" && key != "MIGRATIONS_DIR" && key != "MIGRATION_ROLE" && key != "PINVOU_SCHEMA_CLI_CHILD" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" {
					child.Env = append(child.Env, value)
				}
			}
			child.Env = append(child.Env, "DATABASE_URL="+parsed.String(), "MIGRATIONS_DIR="+directory, "MIGRATION_ROLE=", "PINVOU_DEPLOYMENT_ENVIRONMENT=test", "PINVOU_SCHEMA_CLI_CHILD=owned-phase-down")
			output, err := child.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
				t.Fatal("real migration CLI did not reject edited rollback before execution")
			}
			var after string
			if err := connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("rejected CLI rollback changed ledger")
			}
			column := "publication_generation"
			table := "outbox_events"
			if domain == "payment-simulator" {
				column = "merchant_customer_key"
				table = "simulator_orders"
			}
			var exists bool
			if err := connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, table, column).Scan(&exists); err != nil || !exists {
				t.Fatal("rejected CLI rollback removed durable identity column")
			}
		})
	}
}
