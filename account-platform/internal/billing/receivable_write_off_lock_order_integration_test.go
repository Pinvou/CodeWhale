package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestReceivableWriteOffRechargeLockOrderPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	coordinator, order, _, _ := retainedFinanceOrder(t, r, ctx, 1_000_000, false)
	coordinator.provider = &callbackBoundProvider{coordinator.provider.(*retainedFinancialProvider), order.ProviderID, order.ProviderVersion}
	notification := uuid.New()
	body, signature := signedCoreCallback(t, coordinator, order, notification)
	if _, created, err := coordinator.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil || !created {
		t.Fatalf("persist signed payment notification: created=%v err=%v", created, err)
	}

	seed, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	receivable, err := createReceivableWithinTransaction(ctx, seed, order.WalletID, "USAGE_OVERAGE", "lock-order:"+uuid.NewString(), 1_250_000, nil, nil, nil, nil, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	activation := writeOffFixtureActivation(t, ctx, r.pool)("ACTIVE")
	approvedInput := func(version int64) ReceivableWriteOffExecutionInput {
		t.Helper()
		command := ReceivableWriteOffCommandInput{ReceivableID: receivable.ID, WriteOffMicrocredits: 200_000, ExpectedReceivableVersion: version}
		frozen, err := receivableWriteOffPlanFrom(ctx, r.pool, command, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		return ReceivableWriteOffExecutionInput{
			ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1,
			ExecutionKey: uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation,
			Command: command, Frozen: ReceivableWriteOffFrozenFacts{
				ReceivableVersion: frozen.ReceivableVersion, WriteOffMicrocredits: frozen.WriteOffMicrocredits,
				OutstandingMicrocredits: frozen.OutstandingMicrocredits, SettledMicrocredits: frozen.SettledMicrocredits,
				OriginalMicrocredits: frozen.OriginalMicrocredits, SourceType: frozen.SourceType,
				SourceReference: frozen.SourceReference, WalletReceivableMicrocredits: frozen.WalletReceivableMicrocredits,
				FactsDigest: frozen.FactsDigest,
			},
		}
	}
	input := approvedInput(receivable.Version)

	// The real callback has locked its order and wallet when it inserts the
	// recharge journal, before it tries to lock any open receivables.
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fixture_pause_recharge_journal() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_advisory_xact_lock(2300,1); RETURN NEW; END $$;
 CREATE TRIGGER fixture_pause_recharge_journal BEFORE INSERT ON journal_entries
 FOR EACH ROW WHEN (NEW.entry_type='RECHARGE') EXECUTE FUNCTION fixture_pause_recharge_journal()`); err != nil {
		t.Fatal(err)
	}
	gate, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(2300,1)`); err != nil {
		t.Fatal(err)
	}
	var gatePID int32
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	findWaiter := func(blocker int32) int32 {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var pid int32
			err := r.pool.QueryRow(workerCtx, `SELECT COALESCE(min(pid),0) FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid))`, blocker).Scan(&pid)
			if err != nil {
				t.Fatal(err)
			}
			if pid != 0 {
				return pid
			}
			select {
			case <-ticker.C:
			case <-workerCtx.Done():
				t.Fatal("expected transaction did not reach its lock barrier")
			}
		}
	}

	paid := make(chan error, 1)
	go func() { paid <- coordinator.ProcessPersistedNotification(workerCtx, notification) }()
	paidPID := findWaiter(gatePID)
	writtenOff := make(chan error, 1)
	go func() { _, err := r.ApplyReceivableWriteOff(workerCtx, input); writtenOff <- err }()
	findWaiter(paidPID)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	paidErr, writeOffErr := <-paid, <-writtenOff
	for _, err := range []error{paidErr, writeOffErr} {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "40P01" {
			t.Fatalf("recharge and approved write-off deadlocked: recharge=%v write-off=%v", paidErr, writeOffErr)
		}
	}
	if paidErr != nil {
		t.Fatal(paidErr)
	}
	var serialization *pgconn.PgError
	if !errors.As(writeOffErr, &serialization) || serialization.Code != "40001" {
		t.Fatalf("concurrent frozen approval must lose its serializable snapshot: %v", writeOffErr)
	}
	if err := coordinator.ProcessPersistedNotification(ctx, notification); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyReceivableWriteOff(ctx, input); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("same approval accepted changed frozen facts: %v", err)
	}
	newInput := approvedInput(receivable.Version + 1)
	result, err := r.ApplyReceivableWriteOff(ctx, newInput)
	if err != nil || result.AmountMicrocredits != 200_000 || result.StatusAfter != "PARTIALLY_SETTLED" {
		t.Fatalf("fresh approved write-off: result=%+v err=%v", result, err)
	}
	if replay, err := r.ApplyReceivableWriteOff(ctx, newInput); err != nil || replay != result {
		t.Fatalf("same-key replay changed result: replay=%+v err=%v", replay, err)
	}
	var posted, walletDue, remaining, settled, cash, writeOff int64
	var payments, packages, cases, journals int
	err = r.pool.QueryRow(ctx, `SELECT wallet.posted_microcredits,wallet.receivable_microcredits,receivable.outstanding_microcredits,receivable.settled_microcredits,
 (SELECT COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE receivable_id=$1 AND source_type='CASH_RECHARGE_BASE'),
 (SELECT COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE receivable_id=$1 AND source_type='APPROVED_WRITE_OFF'),
 (SELECT count(*) FROM payment_transactions WHERE payment_order_id=$2),
 (SELECT count(*) FROM entitlement_packages WHERE payment_order_id=$2),
 (SELECT count(*) FROM receivable_write_off_cases WHERE receivable_id=$1),
 (SELECT count(*) FROM journal_entries WHERE entry_type IN ('RECHARGE','ADJUSTMENT'))
 FROM receivables receivable JOIN wallets wallet ON wallet.id=receivable.wallet_id WHERE receivable.id=$1`, receivable.ID, order.ID).
		Scan(&posted, &walletDue, &remaining, &settled, &cash, &writeOff, &payments, &packages, &cases, &journals)
	if err != nil || posted != 0 || walletDue != 50_000 || remaining != walletDue || settled != 1_200_000 || cash != 1_000_000 || writeOff != 200_000 || payments != 1 || packages != 1 || cases != 1 || journals != 2 {
		t.Fatalf("financial state changed or duplicated: posted=%d due=%d remaining=%d settled=%d cash=%d writeOff=%d payments=%d packages=%d cases=%d journals=%d err=%v", posted, walletDue, remaining, settled, cash, writeOff, payments, packages, cases, journals, err)
	}
}
