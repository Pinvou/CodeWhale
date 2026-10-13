package billingschema

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestF09HardenedEntitlementUpgradeKeepsPhasePrivileges(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	c := f.database(t, ctx, "billing", "billing_schema_owner")
	stage := func(maximum int64) []migrations.Migration {
		var selected []migrations.Migration
		for _, m := range loaded {
			if m.Version == 900013 && maximum < 59 {
				continue
			} // Original lock function is delivered after 059.
			if m.Version < 56 || m.Version > 59 || m.Version <= maximum {
				selected = append(selected, m)
			}
		}
		return selected
	}
	if err := Up(ctx, c, stage(55)); err != nil {
		t.Fatal("original hardened predecessor", err)
	}
	var hardened bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('public.billing_runtime_hardening') IS NOT NULL`).Scan(&hardened); err != nil || !hardened {
		t.Fatal("predecessor was not actually hardened")
	}
	for _, version := range []int64{56, 57, 58, 59} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			if err := Up(ctx, c, stage(version)); err != nil {
				t.Fatal("ordinary public upgrade", err)
			}
			var owner string
			var read, insert, update, del bool
			if err := c.QueryRow(ctx, `SELECT pg_get_userbyid(relowner),has_table_privilege('billing_service',oid,'SELECT'),has_table_privilege('billing_service',oid,'INSERT'),has_table_privilege('billing_service',oid,'UPDATE'),has_table_privilege('billing_service',oid,'DELETE') FROM pg_class WHERE oid='public.entitlement_component_corrections'::regclass`).Scan(&owner, &read, &insert, &update, &del); err != nil || owner != "billing_schema_owner" || !read || insert != (version >= 58) || update || del {
				t.Fatalf("phase%d owner=%s R/I/U/D=%v/%v/%v/%v err=%v", version, owner, read, insert, update, del, err)
			}
			var installed int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version BETWEEN 56 AND $1`, version).Scan(&installed); err != nil || installed != int(version-55) {
				t.Fatal("phase ledger incomplete", err)
			}
			// Migration wrappers must return the original caller role.
			var current string
			if err := c.QueryRow(ctx, `SELECT current_user`).Scan(&current); err != nil || current != "billing_service" {
				t.Fatal("migration leaked owner identity")
			}
		})
	}
	t.Run("unrelated-version-58-cannot-open-receipt-writes", func(t *testing.T) {
		other := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, other, stage(57)); err != nil {
			t.Fatal(err)
		}
		unrelated := migrations.Migration{Version: 58, Name: "unrelated_owned_test", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;", Checksum: sqlChecksum("SELECT 1;")}
		input := append(stage(57), unrelated)
		if err := Up(ctx, other, input); err != nil {
			t.Fatal("unrelated custom migration rejected", err)
		}
		var writable bool
		if err := other.QueryRow(ctx, `SELECT has_table_privilege('billing_service','public.entitlement_component_corrections','INSERT')`).Scan(&writable); err != nil || writable {
			t.Fatal("custom version opened original effect privileges", err)
		}
		// A later hardening pass also must use the identity, not just ledger 58.
		if err := Up(ctx, other, append(input, migrations.Migration{Version: 900014, Name: "later_owned_test", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;", Checksum: sqlChecksum("SELECT 1;")})); err != nil {
			t.Fatal(err)
		}
		if err := other.QueryRow(ctx, `SELECT has_table_privilege('billing_service','public.entitlement_component_corrections','INSERT')`).Scan(&writable); err != nil || writable {
			t.Fatal("custom ledger version opened original effect privileges", err)
		}
	})
}
