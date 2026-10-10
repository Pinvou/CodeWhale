package billing

import (
	"context"
	"testing"
	"time"
)

// The journal table gate pauses the real Apply after its activation lock.
// The only operator write is the exact installed command's INACTIVE transition.
func paymentRepairWithDeactivationRace(t *testing.T, ctx context.Context, coordinator *PaymentCoordinator, input PaymentFinancialRepairExecutionInput) (PaymentFinancialRepairResult, error) {
	t.Helper()
	repo := coordinator.repository
	gate := ledgerActivationFixtureOperator(t, ctx, repo.pool)
	operator := ledgerActivationFixtureOperator(t, ctx, repo.pool)
	gateTx, err := gate.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gateTx.Rollback(ctx)
	if _, err := gateTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner; LOCK TABLE public.journal_entries IN SHARE MODE`); err != nil {
		t.Fatal("journal observation gate failed")
	}
	var gatePID int
	if err := gateTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	// Establish the runtime connection within setup, before the original bounded observation.
	warm, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal("runtime observation setup failed")
	}
	warm.Release()
	type outcome struct {
		result PaymentFinancialRepairResult
		err    error
	}
	applied := make(chan outcome, 1)
	go func() {
		result, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
		applied <- outcome{result, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var runtimePID int
	for time.Now().Before(deadline) {
		select {
		case <-applied:
			t.Fatal("Apply returned before the journal gate")
		default:
		}
		if err := gateTx.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename='billing_service' AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, gatePID).Scan(&runtimePID); err != nil {
			t.Fatal("runtime wait observation failed")
		}
		if runtimePID != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtimePID == 0 {
		t.Fatal("real Apply did not reach the post-activation journal gate")
	}
	operatorTx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer operatorTx.Rollback(ctx)
	if _, err := operatorTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	var operatorPID int
	if err := operatorTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&operatorPID); err != nil {
		t.Fatal(err)
	}
	disabled := make(chan error, 1)
	go func() {
		_, err := operatorTx.Exec(ctx, `UPDATE public.billing_approval_command_activations SET state='INACTIVE',version=version+1 WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1`)
		disabled <- err
	}()
	deadline = time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		select {
		case <-disabled:
			t.Fatal("operator disabled command inside the live financial transaction")
		default:
		}
		if err := gateTx.QueryRow(ctx, `SELECT $1=ANY(pg_blocking_pids($2))`, runtimePID, operatorPID).Scan(&blocked); err != nil {
			t.Fatal("operator wait observation failed")
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("operator did not wait on the real Apply activation lock")
	}
	if err := gateTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var result PaymentFinancialRepairResult
	select {
	case got := <-applied:
		if got.err != nil {
			t.Fatal("real Apply failed after gate release", got.err)
		}
		result = got.result
	case <-ctx.Done():
		t.Fatal("Apply did not finish within the fixture budget")
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatal("operator failed after financial commit")
		}
	case <-ctx.Done():
		t.Fatal("operator did not resume")
	}
	if err := operatorTx.Commit(ctx); err != nil {
		t.Fatal("operator disable did not commit")
	}
	got, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
	if err != nil || got != result {
		t.Fatal("operator disable invalidated the original immutable receipt")
	}
	return result, nil
}
