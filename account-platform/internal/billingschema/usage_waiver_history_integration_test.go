package billingschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestUsageWaiverStaticValidationPrecedesDatabase(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"waiver-up", "waiver-forged-checksum", "waiver-down", "026-body", "026-checksum", "026-name", "missing-026"} {
		t.Run(kind, func(t *testing.T) {
			input := append([]migrations.Migration(nil), loaded...)
			for i := range input {
				if input[i].Version == usageWaiverVersion {
					switch kind {
					case "waiver-up":
						input[i].UpSQL += "\nSELECT 1;"
					case "waiver-forged-checksum":
						input[i].UpSQL += "\nSELECT 1;"
						input[i].Checksum = sqlChecksum(input[i].UpSQL)
					case "waiver-down":
						input[i].DownSQL = "SELECT 1;"
					}
				}
				if input[i].Version == 26 {
					switch kind {
					case "026-body":
						input[i].UpSQL += "\nSELECT 1;"
					case "026-checksum":
						input[i].Checksum = strings.Repeat("a", 64)
					case "026-name":
						input[i].Name = "unrelated"
					}
				}
			}
			if kind == "missing-026" {
				for i := range input {
					if input[i].Version == 26 {
						input = append(input[:i], input[i+1:]...)
						break
					}
				}
			}
			// Exercise both public entrypoints. A database read with this
			// deliberately absent connection would panic instead of rejecting SQL.
			if err := Up(context.Background(), nil, input); err == nil {
				t.Fatal("invalid waiver delivery reached database access")
			}
			if _, err := VerifyApplied(context.Background(), nil, input); err == nil {
				t.Fatal("invalid waiver verification reached database access")
			}
		})
	}
}

func TestUsageWaiverMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"changed-up", "forged-up-checksum", "changed-down", "moved-version"} {
		t.Run(kind, func(t *testing.T) {
			modified := append([]migrations.Migration(nil), loaded...)
			for i := range modified {
				if modified[i].Version == usageWaiverVersion {
					switch kind {
					case "changed-up":
						modified[i].UpSQL += "\nSELECT 1;"
					case "forged-up-checksum":
						modified[i].UpSQL += "\nSELECT 1;"
						modified[i].Checksum = sqlChecksum(modified[i].UpSQL)
					case "changed-down":
						modified[i].DownSQL = "SELECT 1;"
					case "moved-version":
						modified[i].Version++
					}
				}
			}
			if _, err := Ordered(modified); err == nil {
				t.Fatal("changed waiver identity accepted")
			}
		})
	}
}

func TestUsageWaiverHistoricalSchemaPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fresh-exact-history-append-only-runtime-and-forward-only", func(t *testing.T) {
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		var original string
		if err := c.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=26`).Scan(&original); err != nil || original != mainlineWaiverEvidenceChecksum {
			t.Fatal("fresh ledger invented fixed026", original, err)
		}
		runtime, err := pgx.Connect(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(ctx)
		for _, table := range []string{"usage_waiver_commands", "usage_waiver_results", "usage_waiver_rejections"} {
			var valid bool
			if err := runtime.QueryRow(ctx, `SELECT pg_get_userbyid(relowner)='billing_schema_owner' AND has_table_privilege('billing_service',oid,'SELECT') AND has_table_privilege('billing_service',oid,'INSERT') AND NOT has_table_privilege('billing_service',oid,'UPDATE') AND NOT has_table_privilege('billing_service',oid,'DELETE') AND NOT has_table_privilege('billing_service',oid,'TRUNCATE') FROM pg_class WHERE oid=to_regclass($1)`, table).Scan(&valid); err != nil || !valid {
				t.Fatal("evidence owner/ACL", table, err)
			}
			for _, sql := range []string{"DELETE FROM " + pgx.Identifier{table}.Sanitize(), "TRUNCATE " + pgx.Identifier{table}.Sanitize(), "ALTER TABLE " + pgx.Identifier{table}.Sanitize() + " ADD COLUMN unsafe TEXT"} {
				if _, err := runtime.Exec(ctx, sql); err == nil {
					t.Fatal("runtime evidence mutation allowed", table, sql)
				}
			}
		}
		before := stageShape(t, ctx, c)
		ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
		assertHistoricalBillingRollback(t, ctx, f.database(t, ctx, "billing", "billing_schema_owner"), loaded, 900024, "P0001", "administrator waiver commands and outcomes are forward-only")
		assertBillingCurrentReplay(t, ctx, c, loaded)
		migrations.Unlock(ctx, c)
		if before != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) {
			t.Fatal("failed rollback changed shape/ledger")
		}
	})
	for _, kind := range []string{"old-LF", "old-CRLF", "unknown-checksum", "wrong-legacy-shape"} {
		t.Run(kind, func(t *testing.T) {
			c := f.database(t, ctx, "billing", "billing_schema_owner")
			var base []migrations.Migration
			for _, m := range loaded {
				if m.Version <= 25 || m.Version == 900001 || m.Version == 900011 {
					base = append(base, m)
				}
			}
			if err := Up(ctx, c, base); err != nil {
				t.Fatal("public real026 predecessor", err)
			}
			migrations.Unlock(ctx, c)
			legacy := migrations.Migration{Version: 26, Name: "hold_operations_evidence", UpSQL: oldWaiverEvidenceSQL, Checksum: oldWaiverEvidenceChecksum}
			if kind == "old-CRLF" {
				legacy.UpSQL = strings.ReplaceAll(oldWaiverEvidenceSQL, "\n", "\r\n")
				legacy.Checksum = oldWaiverEvidenceCRLFChecksum
			}
			if err := migrations.UpWithHooks(ctx, c, []migrations.Migration{legacy}, migrations.UpHooks{LedgerRole: "billing_schema_owner"}); err != nil {
				t.Fatal("exact original fixed026 SQL", err)
			}
			migrations.Unlock(ctx, c)
			actor := billing.UsageWaiverActor{ID: uuid.New(), Issuer: "https://legacy-controlled.invalid", Subject: "operator", AuthorizationVersion: 19}
			// Controlled historical rows use the original immutable command/action
			// schema and fingerprint. This fixture proves preservation/read/block, not
			// a live old-generation coordinator or a newly authorized legacy resumption.
			repo, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, c, "billing_service"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			holdKeys := map[uuid.UUID]string{}
			for _, completed := range []bool{false, true} {
				wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repo.Credit(ctx, wallet.ID, "historical-waiver:"+wallet.ID.String(), 100, false); err != nil {
					t.Fatal(err)
				}
				hold, err := repo.Reserve(ctx, uuid.New(), wallet.ID, 40, nil, time.Now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				key := uuid.NewString()
				holdKeys[hold.ID] = key
				facts, _ := json.Marshal([]string{hold.ID.String(), "original historical reason", actor.Issuer + "|" + actor.Subject})
				digest := sha256.Sum256(facts)
				fingerprint := hex.EncodeToString(digest[:])
				if _, err := c.Exec(ctx, `INSERT INTO usage_hold_admin_commands(idempotency_key,hold_id,request_fingerprint,reason,actor,before_status,created_at) VALUES($1,$2,$3,'original historical reason',$4,'ACTIVE',now())`, key, hold.ID, fingerprint, actor.Issuer+"|"+actor.Subject); err != nil {
					t.Fatal(err)
				}
				if completed {
					if _, err := repo.Release(ctx, hold.UsageRequestID, hold.ID, "historical controlled completed waiver"); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Exec(ctx, `UPDATE usage_holds SET actual_microcredits=0 WHERE id=$1`, hold.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Exec(ctx, `INSERT INTO usage_hold_admin_actions(id,hold_id,action,idempotency_key,request_fingerprint,reason,actor,before_status,after_status,allocation_snapshot,created_at) VALUES($1,$2,'SAFE_RELEASE',$3,$4,'original historical reason',$5,'ACTIVE','RELEASED','{"original_history":true}',now())`, uuid.New(), hold.ID, key, fingerprint, actor.Issuer+"|"+actor.Subject); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "unknown-checksum" {
				if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; UPDATE schema_migrations SET checksum=repeat('f',64) WHERE version=26; SET ROLE billing_service`); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "wrong-legacy-shape" {
				if _, err := c.Exec(ctx, `ALTER TABLE usage_hold_admin_commands ADD COLUMN unregistered TEXT`); err != nil {
					t.Fatal(err)
				}
			}
			data := func() string {
				return stageValue(t, ctx, c, `SELECT jsonb_build_object('commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY idempotency_key) FROM usage_hold_admin_commands x),'actions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_hold_admin_actions x),'holds',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_holds x),'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x))::text`)
			}
			originalData := data()
			original26 := stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=26`)
			beforeShape := stageShape(t, ctx, c)
			beforeLedger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
			err = Up(ctx, c, loaded)
			if kind == "unknown-checksum" || kind == "wrong-legacy-shape" {
				if err == nil {
					t.Fatal("unknown history/shape accepted")
				}
				if beforeShape != stageShape(t, ctx, c) || beforeLedger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) || originalData != data() {
					t.Fatal("rejected upgrade left durable effects")
				}
				return
			}
			if err != nil {
				t.Fatal("recognized deployed026 upgrade", err)
			}
			if originalData != data() || original26 != stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=26`) {
				t.Fatal("upgrade rewrote original history or facts")
			}
			if _, err := VerifyApplied(ctx, c, loaded); err != nil {
				t.Fatal("known fixed026 exact Billing history rejected", err)
			}
			if _, err := migrations.VerifyApplied(ctx, c, loaded); err == nil {
				t.Fatal("generic verifier was relaxed for alternative026")
			}
			for hold, key := range holdKeys {
				record, err := repo.LegacyUsageWaiver(ctx, hold, "original historical reason", key, actor)
				if err != nil || !record.Legacy || record.HoldID != hold {
					t.Fatal("old immutable fact unreadable", record, err)
				}
				if _, err := repo.PrepareUsageWaiver(ctx, hold, "original historical reason", key, actor); !errors.Is(err, billing.ErrLegacyWaiverRequiresReview) {
					t.Fatal("old key invented modern authority", err)
				}
				if !record.Completed {
					if _, err := repo.PrepareUsageWaiver(ctx, hold, "replacement decision", uuid.NewString(), actor); !errors.Is(err, billing.ErrLegacyHoldWaiverPending) {
						t.Fatal("new key bypassed unresolved old intent", err)
					}
				}
			}
			if originalData != data() {
				t.Fatal("read/block changed legacy data")
			}
		})
	}
}
