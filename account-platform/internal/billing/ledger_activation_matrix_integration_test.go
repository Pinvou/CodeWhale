package billing

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"
)

// Every shape runs under the installed runtime identity in a separately owned database.
func TestLedgerRepairOwnedEightShapeMatrix(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	repo, _ := newAdminPaginationIntegrationRepository(t, ctx, raw)
	pool := repo.pool
	operator := ledgerActivationFixtureOperator(t, ctx, pool)
	activationVersion := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	var adjustment, revenue, clear, recv, payment, refund uuid.UUID
	for code, id := range map[string]*uuid.UUID{"platform:adjustment": &adjustment, "platform:usage-revenue": &revenue, "platform:chargeback-clearing": &clear, "platform:receivables": &recv, "platform:payment-clearing": &payment, "platform:refund-clearing": &refund} {
		if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code=$1 AND wallet_id IS NULL`, code).Scan(id); err != nil {
			t.Fatal("system account", code, err)
		}
	}
	// A real wallet leg is needed for EXPIRY; every other example is the
	// documented platform-only shape. Fixtures use the public installed schema.
	wallet, walletAccount := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',0,0,1,now(),now());`, wallet, uuid.New()); err != nil {
		t.Fatal("seed actual wallet", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,code,account_type,wallet_id,created_at) VALUES($1,$2,'WALLET_LIABILITY',$3,now())`, walletAccount, "wallet:"+wallet.String(), wallet); err != nil {
		t.Fatal("seed actual wallet liability", err)
	}
	for _, tc := range []struct {
		name, kind          string
		delta               int64
		wrong, right, other uuid.UUID
	}{
		{"negative-target-original-platform-only", "CHARGEBACK", -30, adjustment, recv, clear},
		{"positive-target-original-usage", "USAGE", 30, adjustment, revenue, recv},
		{"negative-clearing-recharge", "RECHARGE", -30, adjustment, payment, recv},
		{"positive-clearing-refund", "REFUND", 30, adjustment, refund, recv},
		{"negative-adjustment-write-off", "ADJUSTMENT", -30, revenue, adjustment, recv},
		{"positive-adjustment-expiry", "EXPIRY", 30, revenue, adjustment, walletAccount},
		{"negative-clearing-repair-credit", "REPAIR_CREDIT", -30, adjustment, payment, recv},
		{"positive-clearing-forced-refund", "FORCED_REFUND", 30, adjustment, refund, recv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			jid, pid := uuid.New(), uuid.New()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'owned signed direction fixture',$4)`, jid, tc.kind, "owned-signed:"+jid.String(), now); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,$4,$5),($6,$2,$7,$8,$5)`, pid, jid, tc.wrong, tc.delta, now, uuid.New(), tc.other, -tc.delta); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			command := LedgerRepairCommandInput{TargetJournalEntryID: jid, TargetPostingID: pid, WrongLedgerAccountID: tc.wrong, CorrectLedgerAccountID: tc.right, AmountMicrocredits: 30}
			plan, err := repo.LedgerRepairPlanForCommand(ctx, command, 1, 1, activationVersion)
			if err != nil {
				t.Fatal("actual owner plan", err)
			}
			exec := LedgerRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "owned-ledger-" + jid.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: jid, TargetPostingID: pid, WrongLedgerAccountID: tc.wrong, CorrectLedgerAccountID: tc.right, EntryType: tc.kind, AmountMicrocredits: 30, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
			counts := func() string {
				return ledgerActivationTestValue(t, ctx, pool, `SELECT jsonb_build_array((SELECT count(*) FROM ledger_repair_cases),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT COALESCE(sum(posted_microcredits),0) FROM wallets),(SELECT COALESCE(sum(reserved_microcredits),0) FROM wallets))::text`)
			}
			// Ledger attribution must leave every other financial fact intact,
			// including wallet revision/timestamps and individual lot values.
			financialFacts := func() string {
				var facts []string
				for _, table := range []string{"wallets", "credit_lots", "usage_holds", "usage_hold_allocations", "entitlement_packages", "entitlement_components", "receivables", "receivable_allocations", "refunds", "payment_orders", "chargeback_cases", "forced_refund_cases"} {
					facts = append(facts, ledgerActivationTestValue(t, ctx, pool, `SELECT COALESCE(jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text),'[]'::jsonb)::text FROM `+table+` f`))
				}
				return strings.Join(facts, "\n")
			}
			originalFinancialFacts := financialFacts()
			t.Run("noncanonical-platform-destination-refused", func(t *testing.T) {
				rogue := uuid.New()
				if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,code,account_type,created_at) SELECT $1,$2,account_type,now() FROM ledger_accounts WHERE id=$3`, rogue, "noncanonical:"+rogue.String(), tc.right); err != nil {
					t.Fatal(err)
				}
				bad := command
				bad.CorrectLedgerAccountID = rogue
				before := counts()
				if _, err := repo.LedgerRepairPlanForCommand(ctx, bad, 1, 1, activationVersion); err == nil {
					t.Fatal("noncanonical platform destination accepted")
				}
				if counts() != before || financialFacts() != originalFinancialFacts {
					t.Fatal("rejected destination changed financial facts")
				}
			})
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
			var result LedgerRepairResult
			if tc.kind == "CHARGEBACK" {
				result = ledgerRepairWithDeactivationRace(t, ctx, repo, exec)
				activationVersion = setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
			} else {
				result, err = repo.ApplyLedgerRepair(ctx, exec)
				if err != nil {
					t.Fatal("actual apply", err)
				}
			}
			if financialFacts() != originalFinancialFacts {
				t.Fatal("ledger attribution changed non-ledger financial facts")
			}
			var description string
			if err := pool.QueryRow(ctx, `SELECT description FROM journal_entries WHERE id=$1`, result.JournalEntryID).Scan(&description); err != nil || description != "ledger repair: owned-signed:"+jid.String() {
				t.Fatalf("REVERSAL description does not preserve target reference: %q (%v)", description, err)
			}
			t.Run("exact-account-direction", func(t *testing.T) {
				var wd, rd, sum int64
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0),COALESCE(sum(delta_microcredits) FILTER(WHERE ledger_account_id=$2),0),COALESCE(sum(delta_microcredits) FILTER(WHERE ledger_account_id=$3),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID, tc.wrong, tc.right).Scan(&count, &sum, &wd, &rd); err != nil {
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
				setLedgerActivationFixtureState(t, ctx, operator, "INACTIVE")
				got, err = repo.ApplyLedgerRepair(ctx, exec)
				if err != nil || got != result || counts() != before {
					t.Fatal("deactivation invalidated immutable receipt or replay wrote effects")
				}
				activationVersion = setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
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
	t.Run("noncanonical-platform-source-refused", func(t *testing.T) {
		rogue, journal, posting := uuid.New(), uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES($1,$2,'ADJUSTMENT',now())`, rogue, "noncanonical:"+rogue.String()); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'USAGE',$2,'noncanonical source fixture',now())`, journal, "noncanonical:"+journal.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,30,now()),($4,$2,$5,-30,now())`, posting, journal, rogue, uuid.New(), recv); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		before := ledgerActivationTestValue(t, ctx, pool, `SELECT count(*)::text FROM ledger_repair_cases`)
		command := LedgerRepairCommandInput{TargetJournalEntryID: journal, TargetPostingID: posting, WrongLedgerAccountID: rogue, CorrectLedgerAccountID: revenue, AmountMicrocredits: 30}
		if _, err := repo.LedgerRepairPlanForCommand(ctx, command, 1, 1, activationVersion); err == nil {
			t.Fatal("noncanonical platform source accepted")
		}
		if ledgerActivationTestValue(t, ctx, pool, `SELECT count(*)::text FROM ledger_repair_cases`) != before {
			t.Fatal("rejected source produced a repair case")
		}
	})
}

func ledgerActivationTestValue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) string {
	t.Helper()
	var value string
	if err := pool.QueryRow(ctx, query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
