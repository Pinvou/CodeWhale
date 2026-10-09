package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestProtectedMigrationIdentityUsesFrozenFacts(t *testing.T) {
	for _, item := range []struct {
		version                int64
		name, checksum, domain string
	}{
		{900001, "first_delivery_permission_metadata", "", "account"},
		{900001, "outbox_publication_generation", "changed", "billing"},
		{900002, "payment_anomaly_resolution_intents", "changed", "billing"},
		{6, "merchant_customer_key", "changed", "payment-simulator"},
		{5, "renamed", "c10369f3c1201aa37752d686645fac25c435b6e68daba401158f54090ae4e0bb", "payment-simulator"},
		{1, "custom_business", "custom_checksum", ""},
	} {
		if actual := protectedMigrationDomain(item.version, item.name, item.checksum); actual != item.domain {
			t.Fatalf("migration identity domain %q, want %q", actual, item.domain)
		}
	}
}

func TestMigrationDomainRetainsUnrelatedCustomHistory(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect owned domain fixture")
	}
	defer connection.Close(context.Background())
	schema := "f02_custom_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("create owned custom schema")
	}
	defer func() {
		migrations.Unlock(context.Background(), connection)
		_, err := connection.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if err != nil {
			t.Error("cleanup owned custom schema")
		}
	}()
	if _, err = connection.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	loaded := []migrations.Migration{{Version: 1, Name: "custom_business", UpSQL: "CREATE TABLE owned_custom_facts(id BIGINT);", DownSQL: "DROP TABLE owned_custom_facts;"}}
	domain, err := migrationDomain(ctx, connection, loaded, "owned-custom-history")
	if err != nil || domain != "owned-custom-history" {
		t.Fatal("unrelated custom history no longer generic")
	}
	if err = migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	if err = migrations.DownOne(ctx, connection, loaded); err != nil {
		t.Fatal("generic rollback rejected")
	}
	var remains bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('owned_custom_facts') IS NOT NULL OR EXISTS(SELECT 1 FROM schema_migrations)`).Scan(&remains); err != nil || remains {
		t.Fatal("generic rollback did not preserve its original semantics")
	}
	mixed := []migrations.Migration{{Version: 900001, Name: "outbox_publication_generation"}, {Version: 6, Name: "merchant_customer_key"}}
	if _, err = migrationDomain(ctx, connection, mixed, "owned-custom-history"); err == nil {
		t.Fatal("mixed protected identities accepted")
	}
}
