package billingschema

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

// Genuine numeric history through017, never a current ledger rewritten into
// an old one. Retained proof is synthetic legal metadata, not an approval or
// financial execution claim. No provider or financial posting executes here.
func TestBillingManualAdjustmentProofRollbackPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive owned PostgreSQL schema fixture required")
	}
	ctx, stop := context.WithTimeout(context.Background(), 180*time.Second)
	defer stop()
	goName, binaryName := "go", "migrate"
	if runtime.GOOS == "windows" {
		goName += ".exe"
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "build", "-tags=nomsgpack", "-mod=readonly", "-trimpath", "-o", binary, "./cmd/migrate")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual migration CLI: %v\n%s", err, output)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var legacy []migrations.Migration
	for _, migration := range loaded {
		if migration.Version <= 17 {
			legacy = append(legacy, migration)
		}
	}
	// Only the genuine001–017 package under a deployment mount alias: no later
	// protected Billing identity may accidentally supply the domain binding.
	aliased := filepath.Join(t.TempDir(), "retained-release")
	if err := os.Mkdir(aliased, 0700); err != nil {
		t.Fatal(err)
	}
	for _, migration := range legacy {
		for direction, sql := range map[string]string{"up": migration.UpSQL, "down": migration.DownSQL} {
			name := fmt.Sprintf("%06d_%s.%s.sql", migration.Version, migration.Name, direction)
			if err := os.WriteFile(filepath.Join(aliased, name), []byte(sql), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := newStageFixture(t, ctx, raw)
	for _, seeded := range []bool{false, true} {
		name := "empty"
		if seeded {
			name = "retained-proof"
		}
		t.Run(name, func(t *testing.T) {
			for _, entry := range []string{"public-api", "actual-cli", "renamed-history-cli"} {
				t.Run(entry, func(t *testing.T) {
					c := f.database(t, ctx, "billing", "billing_schema_owner")
					if err := migrations.Up(ctx, c, legacy); err != nil {
						t.Fatal("original historical installation", err)
					}
					migrations.Unlock(ctx, c)
					if _, err := migrations.VerifyApplied(ctx, c, legacy); err != nil {
						t.Fatal(err)
					}
					if stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`) != "17" {
						t.Fatal("historical maximum not017")
					}
					if seeded {
						_, err := c.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at)
 VALUES('11111111-1111-4111-8111-111111111111','PERSONAL','22222222-2222-4222-8222-222222222222','ACTIVE','2026-01-01','2026-01-01');
 INSERT INTO manual_adjustments(id,wallet_id,amount_microcredits,reason,requested_by,approved_by,status,created_at,updated_at,decision_reason,decided_at,
 version,request_idempotency_key,approval_request_id,execution_idempotency_key)
 VALUES('33333333-3333-4333-8333-333333333333','11111111-1111-4111-8111-111111111111',137,'controlled retained metadata',
 '22222222-2222-4222-8222-222222222222','44444444-4444-4444-8444-444444444444','APPROVED','2026-01-01','2026-01-01',
 'controlled independent decision','2026-01-01',7,'controlled-original-request','55555555-5555-4555-8555-555555555555','controlled-original-execution')`)
						if err != nil {
							t.Fatal("legal retained proof fixture", err)
						}
					}
					before := retainedDataRollbackSnapshot(t, ctx, c)
					if entry == "public-api" {
						err := DownOne(ctx, c, loaded)
						migrations.Unlock(ctx, c)
						if err == nil || !strings.Contains(err.Error(), "rollback migration 17:") || !strings.Contains(err.Error(), "approval proof rollback is forward-only") {
							t.Fatal("public historical017 rollback was not explicitly refused", err)
						}
					} else {
						directory, err := filepath.Abs("../../migrations/billing")
						if err != nil {
							t.Fatal(err)
						}
						if entry == "renamed-history-cli" {
							directory = aliased
						}
						childCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
						defer cancel()
						child := exec.CommandContext(childCtx, binary, "--direction", "down", "--dir", directory)
						for _, value := range os.Environ() {
							key, _, _ := strings.Cut(value, "=")
							switch key {
							case "DATABASE_URL", "MIGRATIONS_DIR", "MIGRATION_ROLE", "PINVOU_DEPLOYMENT_ENVIRONMENT":
								continue
							}
							child.Env = append(child.Env, value)
						}
						child.Env = append(child.Env, "DATABASE_URL="+f.serviceURL(t, c, "billing_migrator"), "MIGRATION_ROLE=billing_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
						output, err := child.CombinedOutput()
						if err == nil || childCtx.Err() != nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
							t.Fatal("actual migration CLI did not refuse historical017 Down")
						}
					}
					if retainedDataRollbackSnapshot(t, ctx, c) != before {
						t.Fatal("rejected rollback changed data, schema or original migration ledger")
					}
					// Verify the original trigger still rejects a new posted row
					// lacking execution proof, then roll back the failed statement.
					tx, err := c.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at)
 VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','PERSONAL','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','ACTIVE','2026-01-01','2026-01-01');
 INSERT INTO manual_adjustments(id,wallet_id,amount_microcredits,reason,requested_by,approved_by,status,decision_reason,decided_at,created_at,updated_at)
 VALUES('cccccccc-cccc-4ccc-8ccc-cccccccccccc','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',17,'controlled invalid new proof',
 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','dddddddd-dddd-4ddd-8ddd-dddddddddddd','POSTED','controlled independent decision','2026-01-01','2026-01-01','2026-01-01')`)
					var failure *pgconn.PgError
					refused := errors.As(err, &failure) && failure.Code == "23514" && strings.Contains(failure.Message, "approved execution")
					if err := tx.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
					if !refused {
						t.Fatal("original approved-execution trigger was not preserved")
					}
					if retainedDataRollbackSnapshot(t, ctx, c) != before {
						t.Fatal("rejected new write changed retained facts")
					}
					if err := migrations.Up(ctx, c, legacy); err != nil {
						t.Fatal("next forward installation/replay after refusal", err)
					}
					migrations.Unlock(ctx, c)
					if retainedDataRollbackSnapshot(t, ctx, c) != before {
						t.Fatal("forward replay changed retained evidence")
					}
				})
			}
		})
	}
}
