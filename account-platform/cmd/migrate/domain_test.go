package main

import (
	"context"
	"os"
	"path/filepath"
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
		{17, "approval_execution", "changed", "billing"},
		{17, "renamed", "f4d6ad26e66ab16e8c030adf5b3f22246459c2d4d844116fe50e57d537631b11", "billing"},
		{17, "custom_business", "custom_checksum", ""},
		{32, "audit_export_protection", "changed", "account"},
		{32, "renamed", "d60a688ed9ee49014725543dbe458ac4386940033f09549877491f8f835be5ad", "account"},
		{32, "custom_business", "custom_checksum", ""},
		{900001, "first_delivery_permission_metadata", "", "account"},
		{900001, "outbox_publication_generation", "changed", "billing"},
		{900002, "payment_anomaly_resolution_intents", "changed", "billing"},
		{900004, "refund_dispatch_fencing", "changed", "billing"},
		{900005, "payment_notification_provenance", "changed", "billing"},
		{999999, "refund_dispatch_fencing", "changed", "billing"},
		{999999, "payment_notification_provenance", "changed", "billing"},
		{900004, "renamed", "b21624f92414a8ec464138ab5bab13ac5d6e2d982881cb2b1e72ca22f7e8c694", "billing"},
		{900005, "renamed", "26c694e7b99c7d070654a2bd26d3b2af693d4a15e400c5c677fb93f5f3a88eee", "billing"},
		{900004, "custom_business", "custom_checksum", ""},
		{900005, "custom_business", "custom_checksum", ""},
		{87, "registration_bonus_fingerprint", "changed", "account"},
		{28, "account_closure_cleanup", "changed", "account"},
		{27, "account_closure_saga", "changed", "account"},
		{27, "renamed", "cfd14d187cd1c3fa3aab134de59cbfb88a44a7bbc88bc3e3406d8d6c7d2505db", "account"},
		{27, "account_closure_saga", "cfd14d187cd1c3fa3aab134de59cbfb88a44a7bbc88bc3e3406d8d6c7d2505db", "account"},
		{27, "custom_business", "custom_checksum", ""},
		{26, "custom_business", "custom_checksum", ""},
		{28, "renamed", "05eec7870758d9eb29248236a389f7facfbc4025d1fa711d40775d3fe96855b9", "account"},
		{28, "custom_business", "custom_checksum", ""},
		{900004, "renamed", "ddfdb70370c3900c113efc918fb5586ab1a2cb977337b8df35e565e1a250772f", "account"},
		{900015, "organization_evidence_truncate_guards", "changed", "account"},
		{900015, "renamed", "8ef3aea3b9923be58e6aa4460886e03df786db8771697e53bd86bdeea5809e78", "account"},
		{900015, "custom_business", "custom_checksum", ""},
		{10, "plan_catalog", "changed", "billing"},
		{10, "renamed", "5fb4350e0b63eb7bd6a62e631be87b40027fe8f72162651bb322e14cd8fc65a2", "billing"},
		{14, "payment_repairs", "changed", "billing"},
		{14, "renamed", "416f0d1ff871a17cf2bdf498d8e0aeb01d436fbc9c3d45cc52459ad6a6a27e54", "billing"},
		{27, "targeted_refund_lifecycle", "changed", "billing"},
		{27, "renamed", "65755ba6c88de24886417e30fa3ec0c0c706f302cd7db3362fa5de2c5247181d", "billing"},
		{49, "local_payment_expiry", "changed", "billing"},
		{49, "renamed", "13c2cfd865feb46bca7f4811b2b4ca6be96c9906195f45bfc7aa839da78a5866", "billing"},
		{49, "custom_business", "custom_checksum", ""},
		{900020, "outbox_aggregate_lookup", "changed", "account"},
		{900020, "renamed", "c4ad07bf49d5faa3f7493df352ca7885a3be3c23c49a618a804fcc4825e2e615", "account"},
		{900020, "custom_business", "custom_checksum", ""},
		{900045, "provider_health_recent", "changed", "billing"},
		{900045, "renamed", "0d1b5cc17cce2342540c4c75d15c783f53f37177412247ca5eefcd24d942d961", "billing"},
		{900045, "custom_business", "custom_checksum", ""},
		{79, "registration_bonus", "changed", "billing"},
		{6, "merchant_customer_key", "changed", "payment-simulator"},
		{5, "renamed", "c10369f3c1201aa37752d686645fac25c435b6e68daba401158f54090ae4e0bb", "payment-simulator"},
		{1, "custom_business", "custom_checksum", ""},
	} {
		if actual := protectedMigrationDomain(item.version, item.name, item.checksum); actual != item.domain {
			t.Fatalf("migration identity domain %q, want %q", actual, item.domain)
		}
	}
}

func TestPaymentDispatchProvenanceLoadedDomainAliases(t *testing.T) {
	canonical, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"refund_dispatch_fencing", "payment_notification_provenance"} {
		t.Run(name, func(t *testing.T) {
			var original migrations.Migration
			for _, item := range canonical {
				if item.Name == name {
					original = item
				}
			}
			if original.UpSQL == "" {
				t.Fatal("missing original migration")
			}
			directory := t.TempDir()
			for suffix, body := range map[string]string{"up": original.UpSQL, "down": "SELECT 1;\n"} {
				if err := os.WriteFile(filepath.Join(directory, "999999_custom_alias."+suffix+".sql"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := migrations.Load(directory)
			if err != nil || len(loaded) != 1 {
				t.Fatal("load renamed carrier", err)
			}
			alias := loaded[0]
			if alias.Version != 999999 || alias.Name != "custom_alias" || alias.Checksum != original.Checksum {
				t.Fatal("fixture did not retain the real canonical SQL identity")
			}
			if got := protectedMigrationDomain(alias.Version, alias.Name, alias.Checksum); got != "billing" {
				t.Fatal("canonical SQL identity escaped the Billing domain", got)
			}
		})
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
	// A shared version alone must not turn custom history into Billing/Account.
	for _, version := range []int64{17, 28} {
		loaded[0].Version = version
		if domain, err = migrationDomain(ctx, connection, loaded, "owned-custom-history"); err != nil || domain != "owned-custom-history" {
			t.Fatal("unrelated custom history no longer generic", version)
		}
		if err = migrations.Up(ctx, connection, loaded); err != nil {
			t.Fatal(err)
		}
		if err = migrations.DownOne(ctx, connection, loaded); err != nil {
			t.Fatal("unrelated custom rollback rejected", version)
		}
		if err = connection.QueryRow(ctx, `SELECT to_regclass('owned_custom_facts') IS NOT NULL OR EXISTS(SELECT 1 FROM schema_migrations)`).Scan(&remains); err != nil || remains {
			t.Fatal("unrelated custom rollback changed semantics", version)
		}
	}
	mixed := []migrations.Migration{{Version: 900001, Name: "outbox_publication_generation"}, {Version: 6, Name: "merchant_customer_key"}}
	if _, err = migrationDomain(ctx, connection, mixed, "owned-custom-history"); err == nil {
		t.Fatal("mixed protected identities accepted")
	}
}
