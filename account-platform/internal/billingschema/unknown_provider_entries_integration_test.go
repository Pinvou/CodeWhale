package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"
)

// Full public Up, non-super migration actor and actual billing_service writes;
// preserve old known source facts and original migration ledger before append.
func TestUnknownProviderEntryPublicMigrationPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual exclusive schema PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"fresh-public-up", "existing-known-row-upgrade", "historical-forward-only"} {
		t.Run(kind, func(t *testing.T) {
			c := f.database(t, ctx, "billing", "billing_schema_owner")
			target := loaded
			if kind == "historical-forward-only" {
				target = nil
				for _, m := range loaded {
					if m.Version <= unknownProviderEntryVersion {
						target = append(target, m)
					}
				}
			}
			input := target
			if kind != "fresh-public-up" {
				input = nil
				for _, m := range target {
					// A predecessor must not contain objects from later migrations.
					if m.Version < unknownProviderEntryVersion {
						input = append(input, m)
					}
				}
			}
			if err := Up(ctx, c, input); err != nil {
				t.Fatal("public predecessor Up", err)
			}
			run := uuid.New()
			if _, err := c.Exec(ctx, `INSERT INTO reconciliation_runs(id,provider_id,provider_version,channel,business_date,source_mode,status,started_at) VALUES($1,'controlled','v1','ALIPAY',current_date,'MANUAL_API','RUNNING',now())`, run); err != nil {
				t.Fatal(err)
			}
			insert := func(row int, kind string) error {
				_, err := c.Exec(ctx, `INSERT INTO reconciliation_provider_rows(reconciliation_run_id,row_number,entry_type,provider_reference_id,merchant_order_no,channel,amount_fen,currency,status,paid_at,raw_snapshot,created_at) VALUES($1,$2,$3,'original-ref','original-order','ALIPAY',100,'CNY','SUCCESS',now(),'{"original":true}',now())`, run, row, kind)
				return err
			}
			if err := insert(1, "PAYMENT"); err != nil {
				t.Fatal(err)
			}
			known := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(row) ORDER BY row_number)::text FROM reconciliation_provider_rows row`)
			ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<900025`)
			if kind != "fresh-public-up" {
				if err := insert(2, "FUTURE_FEE"); err == nil {
					t.Fatal("predecessor unexpectedly accepts future kind")
				}
				if err := Up(ctx, c, target); err != nil {
					t.Fatal("actual public append upgrade", err)
				}
				if known != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(row) ORDER BY row_number)::text FROM reconciliation_provider_rows row`) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<900025`) {
					t.Fatal("append changed prior source facts or ledger")
				}
			}
			if _, err := VerifyApplied(ctx, c, target); err != nil {
				t.Fatal("exact applied identity", err)
			}
			if err := insert(2, "FUTURE_FEE"); err != nil {
				t.Fatal("bounded future source rejected", err)
			}
			if err := insert(3, strings.Repeat("X", 65)); err == nil {
				t.Fatal("oversized kind accepted")
			}
			if err := insert(3, ""); err == nil {
				t.Fatal("empty kind accepted")
			}
			before := stageValue(t, ctx, c, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(x) ORDER BY row_number) FROM reconciliation_provider_rows x),'ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x))::text`)
			if kind == "historical-forward-only" && stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`) != "900025" {
				t.Fatal("forward-only probe is not at migration 900025")
			}
			downErr := DownOne(ctx, c, target)
			if downErr == nil {
				t.Fatal("forward-only evidence rollback accepted")
			}
			if kind == "historical-forward-only" {
				var rejected *pgconn.PgError
				if !errors.As(downErr, &rejected) || rejected.Code != "P0001" || rejected.Message != "unsupported provider entry evidence is forward-only" {
					t.Fatal("unexpected 900025 rollback rejection", downErr)
				}
			}
			if before != stageValue(t, ctx, c, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(x) ORDER BY row_number) FROM reconciliation_provider_rows x),'ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x))::text`) {
				t.Fatal("rejected rollback left changes")
			}
			if kind == "historical-forward-only" {
				if err := Up(ctx, c, target); err != nil {
					t.Fatal("public replay after refused rollback", err)
				}
				if _, err := VerifyApplied(ctx, c, target); err != nil {
					t.Fatal("applied identity after refused rollback and replay", err)
				}
				if before != stageValue(t, ctx, c, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(x) ORDER BY row_number) FROM reconciliation_provider_rows x),'ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x))::text`) {
					t.Fatal("replay after refused rollback changed source facts or ledger")
				}
			}
		})
	}
}
