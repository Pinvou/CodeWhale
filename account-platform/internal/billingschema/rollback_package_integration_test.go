package billingschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// The existing exclusive fixture supplies genuine non-superuser deployment
// actors and UUID databases. Use original public 001, not handwritten copies
// of its DDL; its old checksum/ledger shape must remain sufficient for upgrade.
func TestPublishedRollbackPackageIntegrityPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual exclusive schema PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	first := selectVersions(loaded, []int64{1})
	if len(first) != 1 {
		t.Fatal("original public foundation is required")
	}
	t.Run("published-down-legacy-crlf-no-create", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := migrations.Up(ctx, c, first); err != nil {
			t.Fatal(err)
		}
		defer migrations.Unlock(context.Background(), c)
		crlf := strings.ReplaceAll(strings.ReplaceAll(first[0].UpSQL, "\r\n", "\n"), "\n", "\r\n")
		digest := sha256.Sum256([]byte(crlf))
		if _, err := c.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=1;`, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO inbox_messages(event_id,event_type,aggregate_version) VALUES('11111111-1111-4111-8111-111111111111','retained.fact',7); SET ROLE billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM billing_service; SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, c, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x),'inbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM inbox_messages x),'outbox',(SELECT count(*) FROM outbox_events))::text`)
		changed := append([]migrations.Migration(nil), first...)
		changed[0].DownSQL = "DELETE FROM inbox_messages;"
		for _, operation := range []struct {
			name string
			run  func() error
		}{
			{"up", func() error { return migrations.Up(ctx, c, changed) }},
			{"verify", func() error { _, e := migrations.VerifyApplied(ctx, c, changed); return e }},
			{"down", func() error { return migrations.DownOne(ctx, c, changed) }},
		} {
			if err := operation.run(); err == nil || !strings.Contains(err.Error(), "rollback differs from published package") {
				t.Fatal("changed published rollback accepted by "+operation.name, err)
			}
			if actual := stageValue(t, ctx, c, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x),'inbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM inbox_messages x),'outbox',(SELECT count(*) FROM outbox_events))::text`); actual != before {
				t.Fatal("rejected " + operation.name + " changed retained facts or ledger")
			}
		}
		if err := migrations.Up(ctx, c, first); err != nil {
			t.Fatal("original restricted upgrade after refusal", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, first); err != nil {
			t.Fatal("legacy CRLF original checksum no longer verifies", err)
		}
		if actual := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`); !strings.Contains(actual, hex.EncodeToString(digest[:])) {
			t.Fatal("upgrade rewrote legacy checksum instead of preserving history")
		}
	})
	t.Run("recorded-up-identity-before-down", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := migrations.Up(ctx, c, first); err != nil {
			t.Fatal(err)
		}
		defer migrations.Unlock(context.Background(), c)
		if _, err := c.Exec(ctx, `UPDATE schema_migrations SET checksum=repeat('0',64) WHERE version=1`); err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, c, `SELECT to_jsonb(x)::text FROM schema_migrations x`)
		if err := migrations.DownOne(ctx, c, first); err == nil || !strings.Contains(err.Error(), "checksum changed before rollback") {
			t.Fatal("rollback ignored applied forward identity", err)
		}
		if before != stageValue(t, ctx, c, `SELECT to_jsonb(x)::text FROM schema_migrations x`) || stageValue(t, ctx, c, `SELECT (to_regclass('inbox_messages') IS NOT NULL AND to_regclass('outbox_events') IS NOT NULL)::text`) != "true" {
			t.Fatal("rejected applied identity changed tables or ledger")
		}
		if _, err := c.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=1`, first[0].Checksum); err != nil {
			t.Fatal(err)
		}
		if err := migrations.DownOne(ctx, c, first); err != nil {
			t.Fatal("original reversible rollback after identity restoration", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, nil); err != nil {
			t.Fatal("successful rollback did not remove original ledger row", err)
		}
	})
	for _, representation := range []string{"LF", "CRLF"} {
		t.Run("published-ledger-cannot-become-custom/"+representation, func(t *testing.T) {
			c := f.database(t, ctx, "billing", "billing_schema_owner")
			original := append([]migrations.Migration(nil), first...)
			if representation == "CRLF" {
				original[0].UpSQL = strings.ReplaceAll(strings.ReplaceAll(original[0].UpSQL, "\r\n", "\n"), "\n", "\r\n")
				digest := sha256.Sum256([]byte(original[0].UpSQL))
				original[0].Checksum = hex.EncodeToString(digest[:])
			}
			if err := migrations.Up(ctx, c, original); err != nil {
				t.Fatal("original published writer", err)
			}
			defer migrations.Unlock(context.Background(), c)
			if _, err := c.Exec(ctx, `INSERT INTO inbox_messages(event_id,event_type,aggregate_version) VALUES('22222222-2222-4222-8222-222222222222','retained.fact',9)`); err != nil {
				t.Fatal(err)
			}
			facts := func() string {
				return stageValue(t, ctx, c, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x),'inbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM inbox_messages x),'outbox',(SELECT count(*) FROM outbox_events))::text`)
			}
			before := facts()
			changed := append([]migrations.Migration(nil), original...)
			changed[0].UpSQL += "\n-- edited candidate cannot redefine applied history\n"
			digest := sha256.Sum256([]byte(strings.ReplaceAll(changed[0].UpSQL, "\r\n", "\n")))
			changed[0].Checksum = hex.EncodeToString(digest[:])
			changed[0].DownSQL = "DELETE FROM inbox_messages;"
			if err := migrations.DownOne(ctx, c, changed); err == nil || !strings.Contains(err.Error(), "checksum changed before rollback") {
				t.Fatal("known applied history was downgraded to custom by supplied Up", err)
			}
			if before != facts() {
				t.Fatal("rejected candidate changed published facts or ledger")
			}
			if _, err := migrations.VerifyApplied(ctx, c, original); err != nil {
				t.Fatal("original history no longer verifies after refusal", err)
			}
			if err := migrations.DownOne(ctx, c, original); err != nil {
				t.Fatal("original reversible operation could not continue", err)
			}
		})
	}
	t.Run("unrelated-custom-history", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		custom := []migrations.Migration{{Version: 1, Name: "custom_business", UpSQL: "CREATE TABLE custom_rollback_probe(id integer)", DownSQL: "DROP TABLE custom_rollback_probe"}}
		if err := migrations.Up(ctx, c, custom); err != nil {
			t.Fatal(err)
		}
		defer migrations.Unlock(context.Background(), c)
		if err := migrations.DownOne(ctx, c, custom); err != nil {
			t.Fatal("generic unpopulated-checksum ledger lost compatibility", err)
		}
		if stageValue(t, ctx, c, `SELECT (to_regclass('custom_rollback_probe') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations))::text`) != "true" {
			t.Fatal("custom rollback effects not preserved")
		}
		digest := sha256.Sum256([]byte(custom[0].UpSQL))
		custom[0].Checksum = hex.EncodeToString(digest[:])
		if err := migrations.Up(ctx, c, custom); err != nil {
			t.Fatal("custom populated-checksum writer", err)
		}
		if err := migrations.DownOne(ctx, c, custom); err != nil {
			t.Fatal("valid custom populated-checksum rollback lost compatibility", err)
		}
	})
}
