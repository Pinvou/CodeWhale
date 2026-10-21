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
			// Keep an actual F09 prefix. Later financial cases and their owner
			// capabilities form dependent phases; retaining only half of a later
			// phase is correctly rejected by the production migration guards.
			if m.Version < 900000 && m.Version > maximum {
				continue
			}
			if m.Version >= 900013 && (m.Version != 900013 || maximum < 59) {
				continue
			} // Original lock function is delivered after 059.
			selected = append(selected, m)
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
		type phaseSnapshot struct {
			owner, caller, ledger     string
			read, insert, update, del bool
		}
		snapshot := func() phaseSnapshot {
			t.Helper()
			var state phaseSnapshot
			if err := other.QueryRow(ctx, `SELECT pg_get_userbyid(relowner),current_user,
 (SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations),
 has_table_privilege('billing_service',oid,'SELECT'),has_table_privilege('billing_service',oid,'INSERT'),
 has_table_privilege('billing_service',oid,'UPDATE'),has_table_privilege('billing_service',oid,'DELETE')
 FROM pg_class WHERE oid='public.entitlement_component_corrections'::regclass`).Scan(&state.owner, &state.caller, &state.ledger, &state.read, &state.insert, &state.update, &state.del); err != nil {
				t.Fatal("read custom phase authority and complete history", err)
			}
			return state
		}
		beforeReserved := snapshot()
		if beforeReserved.owner != "billing_schema_owner" || beforeReserved.caller != "billing_service" || !beforeReserved.read || beforeReserved.insert || beforeReserved.update || beforeReserved.del {
			t.Fatal("custom58 changed the original prepare phase authority")
		}
		// F10 now reserves the former generic fixture number for the Ledger lock.
		// An unrelated body must be refused without changing any original state.
		reserved := migrations.Migration{Version: 900014, Name: "later_owned_test", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;", Checksum: sqlChecksum("SELECT 1;")}
		if err := Up(ctx, other, append(input, reserved)); err == nil || err.Error() != "ledger repair cases require the owner activation lock capability" {
			t.Fatal("unrelated body did not reach the reserved Ledger capability guard", err)
		}
		if snapshot() != beforeReserved {
			t.Fatal("reserved migration rejection changed ownership, caller, privileges or history")
		}
		var reservedRows int
		if err := other.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=900014`).Scan(&reservedRows); err != nil || reservedRows != 0 {
			t.Fatal("rejected reserved migration entered the ledger", err)
		}
		// A genuine generic later hardening pass still must use the exact effect
		// identity, not MAX(version). This number belongs only to this fresh fixture.
		const laterTestVersion int64 = 1900014
		for _, migration := range loaded {
			if migration.Version >= laterTestVersion {
				t.Fatal("generic control must be beyond every installed-source identity")
			}
		}
		later := reserved
		later.Version = laterTestVersion
		if err := Up(ctx, other, append(input, later)); err != nil {
			t.Fatal("public generic later hardening rejected", err)
		}
		var laterRows int
		if err := other.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=$1 AND checksum=$2`, laterTestVersion, later.Checksum).Scan(&laterRows); err != nil || laterRows != 1 {
			t.Fatal("generic later hardening did not actually enter the ledger", err)
		}
		if err := other.QueryRow(ctx, `SELECT has_table_privilege('billing_service','public.entitlement_component_corrections','INSERT')`).Scan(&writable); err != nil || writable {
			t.Fatal("custom ledger version opened original effect privileges", err)
		}
		afterLater := snapshot()
		if afterLater.owner != beforeReserved.owner || afterLater.caller != beforeReserved.caller || afterLater.read != beforeReserved.read || afterLater.insert != beforeReserved.insert || afterLater.update != beforeReserved.update || afterLater.del != beforeReserved.del {
			t.Fatal("generic later hardening changed original prepare authority")
		}
	})
}
