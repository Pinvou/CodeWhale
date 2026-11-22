package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Original strict owned-instance fixture and real reserve/extend/settle writers.
// No provider calls, disabled constraints or replacement financial executors.
func TestHoldExtensionIdempotencePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	originalClock := r.now
	fixture := func(t *testing.T) (Wallet, Hold, HoldExtension) {
		t.Helper()
		r.now = originalClock
		wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		wallet, err = r.Credit(ctx, wallet.ID, "extension-fixture-"+uuid.NewString(), 10000, false)
		if err != nil {
			t.Fatal(err)
		}
		hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, r.now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		command := HoldExtension{UsageRequestID: hold.UsageRequestID, HoldID: hold.ID, OperationID: uuid.New(), LeaseSeconds: 900, AdditionalMicrocredits: 40}
		return wallet, hold, command
	}
	snapshot := func(t *testing.T) string {
		t.Helper()
		state := map[string]json.RawMessage{}
		for _, table := range []string{"wallets", "ledger_accounts", "credit_lots", "entitlement_packages", "entitlement_components", "usage_holds", "usage_hold_allocations", "member_quota_grants", "member_quota_hold_allocations", "journal_entries", "ledger_postings", "receivables", "usage_loss_reviews", "usage_hold_lease_events", "idempotency_records"} {
			var body []byte
			query := `SELECT COALESCE(jsonb_agg(to_jsonb(entry) ORDER BY to_jsonb(entry)::text),'[]'::jsonb) FROM ` + pgx.Identifier{table}.Sanitize() + ` entry`
			if err := r.pool.QueryRow(ctx, query).Scan(&body); err != nil {
				t.Fatalf("snapshot %s: %v", table, err)
			}
			state[table] = body
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	assertMoney := func(t *testing.T, wallet, hold uuid.UUID, maximum int64, events, receipts int) {
		t.Helper()
		var gotMaximum, reserved int64
		var gotEvents, gotReceipts int
		err := r.pool.QueryRow(ctx, `SELECT h.max_microcredits,w.reserved_microcredits,(SELECT count(*) FROM usage_hold_lease_events WHERE hold_id=h.id),(SELECT count(*) FROM idempotency_records WHERE scope='usage.hold.extend' AND response_body->>'hold_id'=h.id::text) FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1 AND w.id=$2`, hold, wallet).Scan(&gotMaximum, &reserved, &gotEvents, &gotReceipts)
		if err != nil || gotMaximum != maximum || reserved != maximum || gotEvents != events || gotReceipts != receipts {
			t.Fatalf("reservation/lease/receipt mismatch: max=%d reserved=%d events=%d receipts=%d err=%v", gotMaximum, reserved, gotEvents, gotReceipts, err)
		}
	}
	t.Run("success-replay-and-every-fact-conflict", func(t *testing.T) {
		wallet, hold, command := fixture(t)
		first, err := r.ExtendHold(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		assertMoney(t, wallet.ID, hold.ID, 140, 2, 1)
		before := snapshot(t)
		same := command
		same.LeaseSeconds = 0
		replay, err := r.ExtendHold(ctx, same)
		if err != nil || !reflect.DeepEqual(replay, first) || snapshot(t) != before {
			t.Fatal("same command changed original result or financial facts", err)
		}
		for _, change := range []struct {
			name   string
			mutate func(*HoldExtension)
		}{
			{"usage", func(c *HoldExtension) { c.UsageRequestID = uuid.New() }},
			{"hold", func(c *HoldExtension) { c.HoldID = uuid.New() }},
			{"amount", func(c *HoldExtension) { c.AdditionalMicrocredits++ }},
			{"lease", func(c *HoldExtension) { c.LeaseSeconds++ }},
		} {
			t.Run(change.name, func(t *testing.T) {
				different := command
				change.mutate(&different)
				_, err := r.ExtendHold(ctx, different)
				if !errors.Is(err, ErrConflict) || snapshot(t) != before {
					t.Fatal("same operation accepted changed facts or changed state", err)
				}
			})
		}
	})
	t.Run("different-operations-same-amount-remain-distinct", func(t *testing.T) {
		wallet, hold, command := fixture(t)
		first, err := r.ExtendHold(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		next := command
		next.OperationID = uuid.New()
		if _, err = r.ExtendHold(ctx, next); err != nil {
			t.Fatal(err)
		}
		assertMoney(t, wallet.ID, hold.ID, 180, 3, 2)
		before := snapshot(t)
		same, err := r.ExtendHold(ctx, command)
		if err != nil || !reflect.DeepEqual(same, first) || snapshot(t) != before {
			t.Fatal("old ACK changed to the newer extension or reapplied funds", err)
		}
	})
	t.Run("terminal-hold-success-is-receipt-only", func(t *testing.T) {
		for _, terminal := range []string{"COMMITTED", "RELEASED"} {
			t.Run(terminal, func(t *testing.T) {
				_, hold, command := fixture(t)
				first, err := r.ExtendHold(ctx, command)
				if err != nil {
					t.Fatal(err)
				}
				if terminal == "COMMITTED" {
					_, err = r.Commit(ctx, hold.UsageRequestID, hold.ID, 80, map[string]any{"controlled": "extension-terminal"})
				} else {
					_, err = r.Release(ctx, hold.UsageRequestID, hold.ID, "controlled-extension-release")
				}
				if err != nil {
					t.Fatal(err)
				}
				before := snapshot(t)
				replay, err := r.ExtendHold(ctx, command)
				if err != nil || !reflect.DeepEqual(replay, first) || snapshot(t) != before {
					t.Fatal("terminal replay changed or renewed financial state", err)
				}
				current, err := getHoldByUsage(ctx, r.pool, hold.UsageRequestID, false)
				if err != nil || current.Status != terminal {
					t.Fatal("terminal hold reactivated", err)
				}
			})
		}
	})
	t.Run("expired-success-replays-but-new-operation-cannot-renew", func(t *testing.T) {
		_, _, command := fixture(t)
		first, err := r.ExtendHold(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		now := first.ExpiresAt.Add(time.Minute)
		r.now = func() time.Time { return now }
		defer func() { r.now = originalClock }()
		before := snapshot(t)
		same, err := r.ExtendHold(ctx, command)
		if err != nil || !reflect.DeepEqual(same, first) || snapshot(t) != before {
			t.Fatal("expired successful operation was not a read-only replay", err)
		}
		command.OperationID = uuid.New()
		if _, err = r.ExtendHold(ctx, command); !errors.Is(err, ErrConflict) || snapshot(t) != before {
			t.Fatal("new operation renewed expired hold", err)
		}
	})
	t.Run("concurrent-same-operation-reserves-once", func(t *testing.T) {
		wallet, hold, command := fixture(t)
		limited, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		type answer struct {
			hold Hold
			err  error
		}
		done := make(chan answer, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() { <-start; h, err := r.ExtendHold(limited, command); done <- answer{h, err} }()
		}
		close(start)
		a, b := <-done, <-done
		if a.err != nil || b.err != nil || !reflect.DeepEqual(a.hold, b.hold) {
			t.Fatalf("concurrent replay mismatch: %v %v", a.err, b.err)
		}
		assertMoney(t, wallet.ID, hold.ID, 140, 2, 1)
	})
	t.Run("insufficient-funds-roll-back-all-facts-and-no-receipt", func(t *testing.T) {
		_, _, command := fixture(t)
		command.AdditionalMicrocredits = 10001
		before := snapshot(t)
		if _, err := r.ExtendHold(ctx, command); !errors.Is(err, ErrInsufficientBalance) || snapshot(t) != before {
			t.Fatal("rejected extension changed money, leases or receipts", err)
		}
	})
	t.Run("zero-renewal-retains-original-no-receipt-contract", func(t *testing.T) {
		wallet, hold, command := fixture(t)
		command.OperationID = uuid.Nil
		command.AdditionalMicrocredits = 0
		renewed, err := r.ExtendHold(ctx, command)
		if err != nil || !renewed.ExpiresAt.After(hold.ExpiresAt) {
			t.Fatal("ordinary renewal failed", err)
		}
		assertMoney(t, wallet.ID, hold.ID, 100, 2, 0)
	})
	t.Run("receipt-snapshot-cannot-substitute-wallet", func(t *testing.T) {
		_, _, command := fixture(t)
		if _, err := r.ExtendHold(ctx, command); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE idempotency_records SET response_body=jsonb_set(response_body,'{wallet_id}',to_jsonb($2::text)) WHERE scope='usage.hold.extend' AND idempotency_key=$1`, command.OperationID.String(), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t)
		if _, err := r.ExtendHold(ctx, command); !errors.Is(err, ErrConflict) || snapshot(t) != before {
			t.Fatal("corrupt receipt substituted owner facts", err)
		}
	})
	t.Run("commit-serialization-is-a-confirmed-full-abort", func(t *testing.T) {
		for _, failures := range []int{1, 4} {
			t.Run(fmt.Sprintf("first-%d-commits-abort", failures), func(t *testing.T) {
				wallet, hold, command := fixture(t)
				// Deployment-owner fault setup only in the original fixture's UUID
				// database. No production constraints/triggers are disabled. The
				// nontransactional sequence counts actual deferred COMMIT attempts.
				statement := fmt.Sprintf(`
CREATE SEQUENCE public.fixture_hold_extension_commit_attempts;
GRANT USAGE,SELECT ON SEQUENCE public.fixture_hold_extension_commit_attempts TO billing_service;
CREATE FUNCTION public.fixture_hold_extension_commit_abort() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.scope='usage.hold.extend' AND NEW.idempotency_key=TG_ARGV[0] THEN
  IF nextval('public.fixture_hold_extension_commit_attempts')<=TG_ARGV[1]::bigint THEN
   RAISE EXCEPTION 'controlled deferred commit serialization abort' USING ERRCODE='40001';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER fixture_hold_extension_commit_abort
 AFTER INSERT ON public.idempotency_records DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION public.fixture_hold_extension_commit_abort('%s','%d');`, command.OperationID.String(), failures)
				if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
					t.Fatal("install controlled deferred failure", err)
				}
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := retainedFinanceOwnerExec(t, r, cleanup, `DROP TRIGGER fixture_hold_extension_commit_abort ON public.idempotency_records; DROP FUNCTION public.fixture_hold_extension_commit_abort(); DROP SEQUENCE public.fixture_hold_extension_commit_attempts;`); err != nil {
						t.Error("remove controlled deferred failure", err)
					}
				}()
				base := originalClock().Truncate(time.Microsecond)
				var clock atomic.Int64
				r.now = func() time.Time { return base.Add(time.Duration(clock.Add(1)-1) * time.Second) }
				defer func() { r.now = originalClock }()
				before := snapshot(t)
				result, err := r.ExtendHold(ctx, command)
				var attempts int
				if scanErr := r.pool.QueryRow(ctx, `SELECT last_value FROM public.fixture_hold_extension_commit_attempts`).Scan(&attempts); scanErr != nil {
					t.Fatal("read actual commit attempt count", scanErr)
				}
				if failures == 1 {
					if err != nil || attempts != 2 || !result.ExpiresAt.Equal(base.Add(900*time.Second)) {
						t.Fatalf("known abort did not retry with original deadline: attempts=%d expiry=%s err=%v", attempts, result.ExpiresAt, err)
					}
					assertMoney(t, wallet.ID, hold.ID, 140, 2, 1)
				} else {
					var rejected *pgconn.PgError
					if !errors.As(err, &rejected) || rejected.Code != "40001" || attempts != 4 || snapshot(t) != before {
						t.Fatalf("bounded commit abort retries leaked financial/lease/receipt facts: attempts=%d err=%v", attempts, err)
					}
				}
			})
		}
	})
}
