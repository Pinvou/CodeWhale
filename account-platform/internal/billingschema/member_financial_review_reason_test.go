package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMemberFinancialReviewReasonPinned(t *testing.T) {
	full, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"valid", "missing", "bad-name", "bad-up", "bad-down", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			in := append([]migrations.Migration(nil), full...)
			for i := range in {
				if in[i].Version == 900005 {
					switch kind {
					case "missing":
						in = append(in[:i], in[i+1:]...)
					case "bad-name":
						in[i].Name = "other"
					case "bad-up":
						in[i].UpSQL += "\nSELECT 1;"
					case "bad-down":
						in[i].DownSQL = "DROP TABLE member_quota_grants;"
					case "duplicate":
						in = append(in, in[i])
					}
					break
				}
			}
			_, err := Ordered(in)
			if (kind == "valid") != (err == nil) {
				t.Fatal("migration identity guard mismatch", kind, err)
			}
		})
	}
}
func TestMemberFinancialReviewReasonPublicUpgrade(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("dedicated PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	full, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	fresh := f.database(t, ctx, "billing", "billing_schema_owner")
	upgrade := f.database(t, ctx, "billing", "billing_schema_owner")
	ordered, e := Ordered(full)
	if e != nil {
		t.Fatal(e)
	}
	var prior []migrations.Migration
	for _, x := range ordered {
		if x.Version != 900005 {
			prior = append(prior, x)
		}
	}
	// The exact previous public order is retained, omitting only this newly delivered final pair.
	if e = migrations.UpWithHooks(ctx, upgrade, prior, migrations.UpHooks{BeforeApply: protectLegacyRefundMigration}); e != nil {
		t.Fatal(e)
	}
	if _, e = upgrade.Exec(ctx, stageBillingSeed); e != nil {
		t.Fatal(e)
	}
	facts := stageWalletFacts(t, ctx, upgrade)
	if e = Up(ctx, upgrade, full); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, upgrade, full); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, upgrade)
	if facts != stageWalletFacts(t, ctx, upgrade) {
		t.Fatal("upgrade changed retained wallet facts")
	}
	if e = Up(ctx, fresh, full); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, fresh, full); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, fresh)
	if stageShape(t, ctx, fresh) != stageShape(t, ctx, upgrade) {
		t.Fatal("fresh/cumulative schema owner/ACL mismatch")
	}
	if !strings.Contains(stageValue(t, ctx, upgrade, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='member_quota_grants'::regclass AND conname='member_quota_grants_suspension_reason_check'`), "CHARGEBACK_REVIEW") {
		t.Fatal("mature financial reason missing")
	}
	ledger := stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`)
	if _, e = Ordered(full); e != nil {
		t.Fatal(e)
	}
	if e = migrations.DownOne(ctx, upgrade, full); e == nil {
		t.Fatal("financial reason rollback was permitted")
	}
	if ledger != stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`) {
		t.Fatal("rejected rollback changed ledger")
	}
}
