package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func escalatedCompletionFixture(t *testing.T, corruptedField ...string) (*PostgresRepository, context.Context, ForcedRefundCase, uuid.UUID, uuid.UUID) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	if len(corruptedField) > 0 {
		_, otherOrder, otherTransaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
		otherForced, err := r.RecordAndFreezeForcedRefund(ctx, otherTransaction, "other-forced-refund", 50, nil)
		if err != nil {
			t.Fatal(err)
		}
		column, value := "", uuid.Nil
		switch corruptedField[0] {
		case "order-link":
			column, value = "payment_order_id", otherOrder.ID
		case "transaction-link":
			column, value = "payment_transaction_id", otherTransaction
		case "evidence-link":
			column, value = "payment_evidence_id", otherForced.PaymentEvidenceID
		default:
			t.Fatal("unsupported reference fault")
		}
		// Inject an inconsistent producer reference at INSERT through the real
		// domain writer. Existing FK and UPDATE immutability guards remain active;
		// this is a controlled stored-evidence fault, not a public mutation path.
		ddl := fmt.Sprintf(`CREATE FUNCTION forced_completion_reference_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.%s := '%s'::uuid; RETURN NEW; END $$; CREATE TRIGGER forced_completion_reference_fault BEFORE INSERT ON payment_anomaly_cases FOR EACH ROW WHEN (NEW.anomaly_type='FORCED_REFUND') EXECUTE FUNCTION forced_completion_reference_fault()`, column, value)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "atomic-escalated-refund", 50, nil)
	if err != nil || forced.ManualReviewCaseID == nil {
		t.Fatal("missing forced-refund review", err)
	}
	if len(corruptedField) > 0 {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER forced_completion_reference_fault ON payment_anomaly_cases; DROP FUNCTION forced_completion_reference_fault()`); err != nil {
			t.Fatal(err)
		}
	}
	first, second := uuid.New(), uuid.New()
	if _, err := r.ClaimPaymentAnomaly(ctx, *forced.ManualReviewCaseID, first, "atomic-escalation-claim"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, first, "atomic-escalation-resolve", "ESCALATE_FINANCIAL_REPAIR", "controlled forced refund escalation", "PENDING"); err != nil {
		t.Fatal(err)
	}
	return r, ctx, forced, first, second
}

func forcedCompletionFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	facts := map[string]json.RawMessage{}
	// Closed fixture table names. Preserve financial subjects, amounts, versions,
	// audit receipts and events, rather than comparing only a net balance/count.
	for _, table := range []string{"payment_orders", "payment_transactions", "payment_evidence", "wallets", "credit_lots", "member_quota_grants", "entitlement_packages", "entitlement_components", "journal_entries", "ledger_postings", "receivables", "forced_refund_cases", "payment_anomaly_cases", "payment_anomaly_commands", "manual_review_cases", "manual_review_case_events", "outbox_events"} {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text),'[]'::jsonb) FROM `+table+` item`).Scan(&raw); err != nil {
			t.Fatal(fmt.Errorf("snapshot %s: %w", table, err))
		}
		facts[table] = raw
	}
	body, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertEscalatedCompletionFacts(t *testing.T, ctx context.Context, r *PostgresRepository, forced ForcedRefundCase) {
	t.Helper()
	var status, action, manual string
	var posted, revoked int64
	var journals, commands, events int
	err := r.pool.QueryRow(ctx, `SELECT forced.status,anomaly.financial_action_status,manual.status,wallet.posted_microcredits,forced.revoked_base_microcredits,
(SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND' AND business_reference=$2),
(SELECT count(*) FROM payment_anomaly_commands WHERE payment_anomaly_case_id=anomaly.id AND action='COMPLETE_ESCALATED_FORCED_REFUND'),
(SELECT count(*) FROM manual_review_case_events WHERE case_id=manual.id AND event_type='COMPLETE_ESCALATED_FORCED_REFUND')
FROM forced_refund_cases forced JOIN wallets wallet ON wallet.id=forced.wallet_id JOIN payment_anomaly_cases anomaly ON anomaly.forced_refund_case_id=forced.id JOIN manual_review_cases manual ON manual.id=anomaly.id WHERE forced.id=$1`, forced.ID, "forced-refund:"+forced.ID.String()).Scan(&status, &action, &manual, &posted, &revoked, &journals, &commands, &events)
	if err != nil || status != "RESOLVED" || action != "APPLIED" || manual != "RESOLVED" || posted != 500000 || revoked != 500000 || journals != 1 || commands != 1 || events != 1 {
		t.Fatal("incomplete or duplicated forced completion", status, action, manual, posted, revoked, journals, commands, events, err)
	}
}

func TestEscalatedForcedRefundWalletLockOrderPostgres(t *testing.T) {
	r, ctx, forced, _, reviewer := escalatedCompletionFixture(t)
	caseID := *forced.ManualReviewCaseID
	worker := func(label string) (*PostgresRepository, string) {
		t.Helper()
		config := r.pool.Config()
		name := label + uuid.NewString()
		config.ConnConfig.RuntimeParams["application_name"] = name
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return NewPostgresRepositoryWithPool(pool), name
	}
	completion, completionName := worker("refund-complete-")
	claim, claimName := worker("anomaly-claim-")
	waitWallet := func(name string) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			if err := r.pool.QueryRow(wait, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 AND query ILIKE '%FROM wallets%FOR UPDATE%' AND cardinality(pg_blocking_pids(pid))>0)`, name).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				return
			}
			select {
			case <-wait.Done():
				t.Fatal("operation did not wait at the wallet lock", name)
			case <-tick.C:
			}
		}
	}
	blocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := getWallet(ctx, blocker, forced.WalletID, true); err != nil {
		t.Fatal(err)
	}
	const completionKey, claimKey = "lock-order-completion", "lock-order-claim"
	const reason = "controlled forced completion lock order"
	claimActor := uuid.New()
	finished := make(chan error, 1)
	go func() {
		_, err := completion.CompleteEscalatedForcedRefund(ctx, caseID, reviewer, completionKey, reason)
		finished <- err
	}()
	waitWallet(completionName)
	claimed := make(chan error, 1)
	go func() {
		_, err := claim.ClaimPaymentAnomaly(ctx, caseID, claimActor, claimKey)
		claimed <- err
	}()
	waitWallet(claimName)
	// A transaction already holding the wallet must still be able to acquire
	// the anomaly/manual rows. NOWAIT makes the old reverse order deterministic.
	if _, err := blocker.Exec(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual NOWAIT`, caseID); err != nil {
		t.Fatal("completion locked the review before the wallet", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal("completion failed after releasing the wallet", err)
	}
	var pgErr *pgconn.PgError
	if err := <-claimed; !errors.Is(err, ErrConflict) && (!errors.As(err, &pgErr) || pgErr.Code != "40001") {
		t.Fatal("claim should reject the escalated case or lose its serialization snapshot", err)
	}
	assertEscalatedCompletionFacts(t, ctx, r, forced)
	after := forcedCompletionFacts(t, ctx, r)
	if _, err := claim.ClaimPaymentAnomaly(ctx, caseID, claimActor, claimKey); !errors.Is(err, ErrConflict) {
		t.Fatal("rejected claim did not retain its meaning on retry", err)
	}
	if _, err := completion.CompleteEscalatedForcedRefund(ctx, caseID, reviewer, completionKey, reason); err != nil || forcedCompletionFacts(t, ctx, r) != after {
		t.Fatal("completion replay or claim retry changed committed facts", err)
	}
}

func TestEscalatedForcedRefundAtomicRollbackPostgres(t *testing.T) {
	for _, point := range []string{"terminal-statement", "receipt-statement", "deferred-commit"} {
		t.Run(point, func(t *testing.T) {
			r, ctx, forced, _, second := escalatedCompletionFixture(t)
			before := forcedCompletionFacts(t, ctx, r)
			ddl := `CREATE FUNCTION forced_completion_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='58000',MESSAGE='controlled forced completion failure'; END $$; `
			table := "payment_anomaly_commands"
			if point == "terminal-statement" {
				table = "payment_anomaly_cases"
				ddl += `CREATE TRIGGER forced_completion_reject BEFORE UPDATE OF financial_action_status ON payment_anomaly_cases FOR EACH ROW WHEN (NEW.financial_action_status='APPLIED') EXECUTE FUNCTION forced_completion_reject()`
			} else if point == "receipt-statement" {
				ddl += `CREATE TRIGGER forced_completion_reject BEFORE INSERT ON payment_anomaly_commands FOR EACH ROW EXECUTE FUNCTION forced_completion_reject()`
			} else {
				ddl += `CREATE CONSTRAINT TRIGGER forced_completion_reject AFTER INSERT ON payment_anomaly_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION forced_completion_reject()`
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, ddl); err != nil {
				t.Fatal(err)
			}
			key, reason := "atomic-completion-retry", "independent controlled forced completion"
			if _, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, second, key, reason); err == nil || forcedCompletionFacts(t, ctx, r) != before {
				t.Fatal("failed completion persisted partial financial or receipt state", err)
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER forced_completion_reject ON `+table+`; DROP FUNCTION forced_completion_reject()`); err != nil {
				t.Fatal(err)
			}
			if _, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, second, key, reason); err != nil {
				t.Fatal("same command could not complete after known database rejection", err)
			}
			assertEscalatedCompletionFacts(t, ctx, r, forced)
		})
	}
}

func TestEscalatedForcedRefundEvidenceAndReceiptGuardsPostgres(t *testing.T) {
	for _, mismatch := range []string{"manual-status", "manual-link", "order-link", "transaction-link", "evidence-link"} {
		t.Run(mismatch, func(t *testing.T) {
			var references []string
			if mismatch != "manual-status" && mismatch != "manual-link" {
				references = []string{mismatch}
			}
			r, ctx, forced, _, second := escalatedCompletionFixture(t, references...)
			var err error
			switch mismatch {
			case "manual-status":
				_, err = r.pool.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=clock_timestamp() WHERE id=$1`, *forced.ManualReviewCaseID)
			case "manual-link":
				_, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET manual_review_case_id=NULL WHERE id=$1`, forced.ID)
			}
			if err != nil {
				t.Fatal("cannot prepare stored mismatch fixture", err)
			}
			before := forcedCompletionFacts(t, ctx, r)
			if _, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, second, "mismatched-completion", "independent controlled forced completion"); !errors.Is(err, ErrConflict) || forcedCompletionFacts(t, ctx, r) != before {
				t.Fatal("mismatched stored evidence was accepted or changed facts", mismatch, err)
			}
		})
	}
	t.Run("receipt-identity-and-terminal-replay", func(t *testing.T) {
		r, ctx, forced, first, second := escalatedCompletionFixture(t)
		key, reason := "receipt-completion-key", "independent controlled forced completion"
		before := forcedCompletionFacts(t, ctx, r)
		if _, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, first, key, reason); !errors.Is(err, ErrConflict) || forcedCompletionFacts(t, ctx, r) != before {
			t.Fatal("same actor completion changed facts", err)
		}
		completed, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, second, key, reason)
		if err != nil {
			t.Fatal("first completion failed", err)
		}
		assertEscalatedCompletionFacts(t, ctx, r, forced)
		after := forcedCompletionFacts(t, ctx, r)
		pool, err := pgxpool.NewWithConfig(ctx, r.pool.Config())
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		restarted := NewPostgresRepositoryWithPool(pool)
		replay, err := restarted.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, second, key, reason)
		if err != nil || replay.ID != completed.ID || replay.Version != completed.Version || forcedCompletionFacts(t, ctx, r) != after {
			t.Fatal("persisted receipt did not survive repository/connection replacement", replay, err)
		}
		for _, input := range []struct {
			actor       uuid.UUID
			key, reason string
		}{{first, key, reason}, {second, key, reason + " changed"}, {second, "different-terminal-key", reason}} {
			if _, err := restarted.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, input.actor, input.key, input.reason); !errors.Is(err, ErrConflict) || forcedCompletionFacts(t, ctx, r) != after {
				t.Fatal("terminal or receipt mismatch changed facts", err)
			}
		}
	})
}

func TestOrdinaryForcedRefundAppliedReceiptRecoveryPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "ordinary-receipt-recovery", 50, nil)
	if err != nil || forced.ManualReviewCaseID == nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	retainedClaimAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor)
	applied, err := r.ApplyReviewedForcedRefund(ctx, forced.ID)
	if err != nil || applied.RevokedBase != 500000 {
		t.Fatal("ordinary financial apply failed", applied, err)
	}
	// The ordinary writer deliberately precedes anomaly finalization; recover
	// this persisted financial receipt after the caller loses the first result.
	before := forcedCompletionFacts(t, ctx, r)
	replay, err := r.ApplyReviewedForcedRefund(ctx, forced.ID)
	if err != nil || replay.RevokedBase != applied.RevokedBase || forcedCompletionFacts(t, ctx, r) != before {
		t.Fatal("ordinary receipt replay changed or rejected committed finance", replay, err)
	}
	resolved, err := r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, "ordinary-resolution-key", "CONFIRM_FORCED_REFUND", "confirmed original forced refund receipt", "APPLIED")
	if err != nil || resolved.FinancialActionStatus != "APPLIED" {
		t.Fatal("ordinary receipt could not complete its anomaly", resolved, err)
	}
}
