package billingschema

import (
	"context"
	billing "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrivateLedgerSignedCorrection(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Fatal("required isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect isolated admin")
	}
	var dd, user string
	var port int
	var actors int
	err = admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_setting('port')::int,(SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'pinvou_wave_test')`).Scan(&dd, &user, &port, &actors)
	_ = admin.Close(ctx)
	if err != nil || port != 55504 || user != "pinvou_wave_test" || actors != 0 || strings.ReplaceAll(dd, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" {
		t.Fatal("exclusive runtime owner/port/datadir/actor guard")
	}
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "billing", "billing_schema_owner")
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := Up(ctx, c, all); err != nil {
		t.Fatal("current private F10 complete public migrations", err)
	}
	if _, err := migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal("public ledger verify", err)
	}
	migrations.Unlock(ctx, c)
	pool, err := pgxpool.New(ctx, f.serviceURL(t, c, "billing_service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var su, cu string
	var super, ddl bool
	if err := pool.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),has_schema_privilege(current_user,'public','CREATE')`).Scan(&su, &cu, &super, &ddl); err != nil || su != "billing_service" || cu != "billing_service" || super {
		t.Fatal("actual non-super runtime role")
	}
	repo := billing.NewPostgresRepositoryWithPool(pool)
	var wrong, revenue, clear, recv uuid.UUID
	for code, id := range map[string]*uuid.UUID{"platform:adjustment": &wrong, "platform:usage-revenue": &revenue, "platform:chargeback-clearing": &clear, "platform:receivables": &recv} {
		if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code=$1 AND wallet_id IS NULL`, code).Scan(id); err != nil {
			t.Fatal("system account", code, err)
		}
	}
	for _, tc := range []struct {
		name, kind   string
		delta        int64
		right, other uuid.UUID
	}{{"negative-target-original-platform-only", "CHARGEBACK", -30, recv, clear}, {"positive-target-original-usage", "USAGE", 30, revenue, recv}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			jid, pid := uuid.New(), uuid.New()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'private signed direction fixture',$4)`, jid, tc.kind, "private-signed:"+jid.String(), now); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,$4,$5),($6,$2,$7,$8,$5)`, pid, jid, wrong, tc.delta, now, uuid.New(), tc.other, -tc.delta); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			command := billing.LedgerRepairCommandInput{TargetJournalEntryID: jid, TargetPostingID: pid, WrongLedgerAccountID: wrong, CorrectLedgerAccountID: tc.right, AmountMicrocredits: 30}
			plan, err := repo.LedgerRepairPlanForCommand(ctx, command, 1, 1, 1)
			if err != nil {
				t.Fatal("actual owner plan", err)
			}
			exec := billing.LedgerRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "private-ledger-" + jid.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1, Command: command, Frozen: billing.LedgerRepairFrozenFacts{TargetJournalEntryID: jid, TargetPostingID: pid, WrongLedgerAccountID: wrong, CorrectLedgerAccountID: tc.right, EntryType: tc.kind, AmountMicrocredits: 30, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
			counts := func() string {
				return stageValue(t, ctx, c, `SELECT jsonb_build_array((SELECT count(*) FROM ledger_repair_cases),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT COALESCE(sum(posted_microcredits),0) FROM wallets),(SELECT COALESCE(sum(reserved_microcredits),0) FROM wallets))::text`)
			}
			t.Run("nonmatching-frozen-no-mutation", func(t *testing.T) {
				before := counts()
				bad := exec
				bad.Frozen.PostingSetDigest = strings.Repeat("f", 64)
				if _, err := repo.ApplyLedgerRepair(ctx, bad); err == nil {
					t.Error("mismatched frozen accepted")
				}
				if after := counts(); after != before {
					t.Error("mismatch changed actual domain facts")
				}
			})
			result, err := repo.ApplyLedgerRepair(ctx, exec)
			if err != nil {
				t.Fatal("actual apply", err)
			}
			t.Run("exact-account-direction", func(t *testing.T) {
				var wd, rd, sum int64
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0),COALESCE(sum(delta_microcredits) FILTER(WHERE ledger_account_id=$2),0),COALESCE(sum(delta_microcredits) FILTER(WHERE ledger_account_id=$3),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID, wrong, tc.right).Scan(&count, &sum, &wd, &rd); err != nil {
					t.Fatal(err)
				}
				if count != 2 || sum != 0 {
					t.Error("original balanced pair invariant")
				}
				if wd != -tc.delta || rd != tc.delta {
					t.Errorf("wrong-account correction=%d expected=%d; correct-account correction=%d expected=%d", wd, -tc.delta, rd, tc.delta)
				}
				var original int64
				if err := pool.QueryRow(ctx, `SELECT delta_microcredits FROM ledger_postings WHERE id=$1`, pid).Scan(&original); err != nil || original != tc.delta {
					t.Error("immutable original altered")
				}
			})
			t.Run("result-first-identical-replay", func(t *testing.T) {
				before := counts()
				got, err := repo.ApplyLedgerRepair(ctx, exec)
				if err != nil || got != result {
					t.Error("same key result-first replay", err)
				}
				if counts() != before {
					t.Error("replay wrote another effect")
				}
				different := exec
				different.ApprovalRequestID = uuid.New()
				different.AuthorizationEventID = uuid.New()
				if _, err := repo.ApplyLedgerRepair(ctx, different); err == nil {
					t.Error("second approval corrected same posting")
				}
				if counts() != before {
					t.Error("second approval changed facts")
				}
			})
		})
	}
}
