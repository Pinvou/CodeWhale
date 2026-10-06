package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"time"
)

// Owned PostgreSQL trigger pauses Commit while it holds wallet and component
// locks. A concurrent review must queue on that same wallet, keeping source FK
// reads compatible; the former package/source-first cycle must not recur.
// No production test hook or user data is involved.
func TestPostgresPaymentReviewAndCommitLockOrder(t *testing.T) {
	for _, kind := range []string{"chargeback-record", "forced-record", "chargeback-apply", "forced-apply"} {
		t.Run(kind, func(t *testing.T) {
			r, wallet, transaction := forcedRefundPaidSource(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var evidence uuid.UUID
			if kind == "chargeback-apply" {
				item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-lock-order", json.RawMessage(`{"confirmed":true}`))
				if err != nil {
					t.Fatal(err)
				}
				evidence = item.PaymentEvidenceID
			}
			if kind == "forced-apply" {
				item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-lock-order", 100, json.RawMessage(`{"confirmed":true}`))
				if err != nil {
					t.Fatal(err)
				}
				evidence = item.ID
			}
			gate := int32(time.Now().UnixNano() % 1000000000)
			owner, err := r.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Release()
			if _, err = owner.Exec(ctx, `SELECT pg_advisory_lock(214711,$1)`, gate); err != nil {
				t.Fatal(err)
			}
			defer owner.Exec(ctx, `SELECT pg_advisory_unlock(214711,$1)`, gate)
			if _, err = r.pool.Exec(ctx, `CREATE FUNCTION controlled_commit_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.used_microcredits>OLD.used_microcredits THEN PERFORM pg_advisory_xact_lock(214711,`+fmt.Sprint(gate)+`); END IF; RETURN NEW; END $$; CREATE TRIGGER controlled_commit_gate BEFORE UPDATE ON entitlement_components FOR EACH ROW EXECUTE FUNCTION controlled_commit_gate()`); err != nil {
				t.Fatal(err)
			}
			commitResult := make(chan error, 1)
			go func() {
				_, err := r.Commit(ctx, hold.UsageRequestID, hold.ID, 50, map[string]any{"controlled_usage": true})
				commitResult <- err
			}()
			var commitPID int
			deadline := time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				err = r.pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid=214711 AND objid=$1 AND NOT granted`, gate).Scan(&commitPID)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if commitPID == 0 {
				t.Fatal("controlled Commit did not reach its locked component gate")
			}
			review := func() error {
				switch kind {
				case "forced-record":
					_, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-lock-order", 50, json.RawMessage(`{"confirmed":true}`))
					return err
				case "chargeback-apply":
					_, err := r.ApplyConfirmedChargeback(ctx, evidence, "controlled-lock-order")
					return err
				case "forced-apply":
					_, err := r.ApplyReviewedForcedRefund(ctx, evidence)
					return err
				default:
					_, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "controlled-lock-order", json.RawMessage(`{"confirmed":true}`))
					return err
				}
			}
			reviewResult := make(chan error, 1)
			go func() { reviewResult <- review() }()
			var reviewBlocked bool
			deadline = time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(pid)))`, commitPID).Scan(&reviewBlocked)
				if err != nil {
					t.Fatal(err)
				}
				if reviewBlocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !reviewBlocked {
				t.Fatal("competing review did not reach the established component/wallet lock")
			}
			if _, err = owner.Exec(ctx, `SELECT pg_advisory_unlock(214711,$1)`, gate); err != nil {
				t.Fatal(err)
			}
			commitErr, reviewErr := <-commitResult, <-reviewResult
			code := func(err error) string {
				if err == nil {
					return "NONE"
				}
				var sql *pgconn.PgError
				if errors.As(err, &sql) {
					return sql.Code
				}
				return "OTHER"
			}
			if code(commitErr) == "40P01" || code(reviewErr) == "40P01" {
				t.Fatalf("financial deadlock: Commit=%s Review=%s", code(commitErr), code(reviewErr))
			}
			// Serializable may reject its pre-wait snapshot after Commit wins. This is
			// an explicit rolled-back attempt, never treated as a successful operation.
			if code(reviewErr) == "40001" {
				reviewErr = review()
			}
			if commitErr != nil || reviewErr != nil {
				t.Fatalf("same logical review did not converge: Commit=%s Review=%s", code(commitErr), code(reviewErr))
			}
			current, err := r.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected, debt := wallet.PostedMicrocredits-50, int64(0)
			if kind == "chargeback-apply" || kind == "forced-apply" {
				expected, debt = 0, 50
			}
			if current.PostedMicrocredits != expected || current.ReservedMicrocredits != 0 || current.ReceivableMicrocredits != debt {
				t.Fatal("concurrent source review broke monetary conservation")
			}
			var usages, reviews int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='USAGE'`).Scan(&usages); err != nil || usages != 1 {
				t.Fatal("usage financial winner duplicated", err)
			}
			table := "chargeback_cases"
			if kind == "forced-record" || kind == "forced-apply" {
				table = "forced_refund_cases"
			}
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&reviews); err != nil || reviews != 1 {
				t.Fatal("source review duplicated", err)
			}
		})
	}
}
