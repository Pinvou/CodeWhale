package accountschema

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestOutboxGenerationBridgeIdentityAndOrder(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	bridge := slices.IndexFunc(ordered, func(m migrations.Migration) bool { return m.Version == outboxGenerationVersion })
	replay := slices.IndexFunc(ordered, func(m migrations.Migration) bool { return m.Version == 94 })
	if bridge < 0 || replay <= bridge {
		t.Fatal("forward column bridge must precede its first legacy consumer")
	}
	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			changed := slices.Clone(loaded)
			for i := range changed {
				if changed[i].Version == outboxGenerationVersion {
					if direction == "up" {
						changed[i].UpSQL += "\nSELECT 1;"
					} else {
						changed[i].DownSQL += "\nSELECT 1;"
					}
				}
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("modified bridge with unchanged checksum accepted")
			}
		})
	}
}

func TestProductionAccountEventDeliveryUpgrade(t *testing.T) {
	legacyRoot := os.Getenv("PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY")
	if legacyRoot == "" || os.Getenv("F17_TEST_DATABASE_URL") == "" {
		t.Skip("owned PostgreSQL and exact published migration package required")
	}
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	f := newAuditExportGuardFixture(t, ctx, os.Getenv("F17_TEST_DATABASE_URL"))
	c := f.database(t, ctx, "account", "account_service")
	legacy, err := migrations.Load(filepath.Join(legacyRoot, "account"))
	if err != nil || len(legacy) != 87 || legacy[2].Checksum != originalEventDeliveryChecksum {
		t.Fatal("exact published Account 1..87 package required", err)
	}
	if err := migrations.Up(ctx, c, legacy); err != nil {
		t.Fatal("install published Account package", err)
	}
	migrations.Unlock(ctx, c)
	var missing bool
	if err := c.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='outbox_events'::regclass AND attname='publication_generation' AND NOT attisdropped)`).Scan(&missing); err != nil || !missing {
		t.Fatal("published 003 must actually lack the generation column", err)
	}
	_, err = c.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
 VALUES('11111111-1111-4111-8111-111111111111','account.fixture','11111111-1111-4111-8111-111111111112',1,'{"preserved":true}','published-upgrade',now())`)
	if err != nil {
		t.Fatal(err)
	}
	var history, event string
	if err := c.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s),(SELECT to_jsonb(o)::text FROM outbox_events o LIMIT 1)`).Scan(&history, &event); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	withoutBridge := slices.DeleteFunc(slices.Clone(loaded), func(m migrations.Migration) bool { return m.Version == outboxGenerationVersion })
	if err := Up(ctx, c, withoutBridge); err == nil {
		t.Fatal("legacy history accepted without forward column bridge")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("published Account upgrade", err)
		}
		migrations.Unlock(ctx, c)
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("domain verification must accept preserved original 003", err)
		}
		var afterHistory, afterEvent string
		var generation int64
		if err := c.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s WHERE version<=87),
 (SELECT (to_jsonb(o)-'publication_generation')::text FROM outbox_events o LIMIT 1),(SELECT publication_generation FROM outbox_events LIMIT 1)`).Scan(&afterHistory, &afterEvent, &generation); err != nil || afterHistory != history || afterEvent != event || generation != 0 {
			t.Fatal("upgrade changed historical ledger/event facts", err)
		}
	}
	if _, err := migrations.VerifyApplied(ctx, c, loaded); err == nil {
		t.Fatal("generic verifier must remain strict about the different 003")
	}
	if _, err := c.Exec(ctx, `UPDATE outbox_events SET publication_generation=-1`); err == nil {
		t.Fatal("generation nonnegative constraint missing")
	}
}
