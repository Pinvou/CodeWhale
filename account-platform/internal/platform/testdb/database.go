// Package testdb provides opt-in, isolated PostgreSQL fixtures for domain tests.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DatabaseURLEnv = "PINVOU_ACCOUNT_TEST_DATABASE_URL"

// Open applies the domain's migrations in a random, test-owned database/schema.
// All connections use only that schema and pg_catalog; public is never a fallback.
// Tests opt in through DatabaseURLEnv, which must address a loopback PostgreSQL.
// The configured role must have CREATEDB. Database-wide extensions must never
// belong to another fixture's schema, whose cleanup could remove their functions.
func Open(t *testing.T, domain string) *pgxpool.Pool {
	t.Helper()
	switch domain {
	case "account", "billing", "token", "payment-simulator":
	default:
		t.Fatalf("unsupported test database domain %q", domain)
	}
	dsn := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if dsn == "" {
		t.Skip(DatabaseURLEnv + " is not configured; real PostgreSQL test not executed")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database connection configuration")
	}
	if !loopbackHost(config.ConnConfig.Host) {
		t.Fatal("test database must use a loopback host")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !loopbackHost(fallback.Host) {
			t.Fatal("test database fallback must use a loopback host")
		}
	}
	loaded, err := migrations.Load(migrationDirectory(t, domain))
	if err != nil {
		t.Fatalf("load %s test migrations: %v", domain, err)
	}
	if len(loaded) == 0 {
		t.Fatalf("no migrations loaded for test domain %s", domain)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adminConfig := config.ConnConfig.Copy()
	adminConfig.RuntimeParams["search_path"] = "pg_catalog"
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal("connect to loopback test database failed")
	}
	var schema string
	var database string
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		defer admin.Close(cleanupCtx)
		if database != "" {
			if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
				t.Error("remove test-owned database failed")
			}
		}
	})
	var serverAddress string
	if err := admin.QueryRow(ctx, "SELECT host(inet_server_addr())").Scan(&serverAddress); err != nil || !loopbackHost(serverAddress) {
		t.Fatal("connected test database server is not loopback")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal("generate isolated test schema name failed")
	}
	name := "pinvou_a01_" + strings.ReplaceAll(domain, "-", "_") + "_" + hex.EncodeToString(nonce)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatal("create isolated test database failed; configured role needs CREATEDB")
	}
	// Register ownership only after CREATE succeeds: a collision must never clean
	// up a database created by another test or user.
	database = name
	schema = name
	// Reparse the owned address rather than mutating only exported fields: pgx
	// ConnString otherwise keeps the original configured database/search_path,
	// so a second repository constructed from it would escape this fixture.
	config, err = pgxpool.ParseConfig(fixtureConnectionString(dsn, database, pgx.Identifier{schema}.Sanitize()+",pg_catalog"))
	if err != nil {
		t.Fatal("parse isolated test database configuration failed")
	}
	config.MaxConns = 4
	pool, err = pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open isolated test database pool failed")
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal("acquire isolated test database connection failed")
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("create isolated test schema failed")
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("apply %s test migrations: %v", domain, err)
	}
	return pool
}

func fixtureConnectionString(dsn, database, searchPath string) string {
	if parsed, err := url.Parse(dsn); err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		parsed.Path = "/" + database
		parsed.RawPath = ""
		query := parsed.Query()
		query.Del("dbname")
		query.Set("search_path", searchPath)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	// Database/schema names are generated from fixed ASCII prefixes and hex,
	// not caller input. Libpq keyword duplicates use the last occurrence.
	return dsn + " dbname=" + database + " search_path='" + searchPath + "'"
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func migrationDirectory(t *testing.T, domain string) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal("find test working directory failed")
	}
	for {
		candidate := filepath.Join(directory, "migrations", domain)
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("cannot find migrations for test domain %s", domain)
		}
		directory = parent
	}
}
