package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Actual runtime PostgreSQL failures must not become definite proof drift.
// The owned UUID fixture and its original actor guard remain unchanged; this
// does not claim a full broker/approval dispatch or production outage replay.
func TestLedgerRepairClaimActivationReadErrorsPostgres(t *testing.T) {
	financialFacts := func(t *testing.T, ctx context.Context, r *PostgresRepository) string {
		t.Helper()
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),(SELECT jsonb_agg(to_jsonb(c) ORDER BY approval_request_id) FROM billing_approval_proof_claims c))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("actual-runtime-permission-error-is-not-frozen-fact-drift", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		before := financialFacts(t, ctx, r)
		// Fault only this fixture's own installed table, using its existing
		// migration operator. No cluster actor, migration or data row changes.
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `REVOKE SELECT ON public.billing_approval_command_activations FROM billing_service`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if _, err := retainedFinanceOwnerExec(t, r, cleanup, `GRANT SELECT ON public.billing_approval_command_activations TO billing_service`); err != nil {
				t.Error("restore owned-table runtime privilege", err)
			}
		})
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = (billingTxProofFacts{repository: r, tx: tx}).ledgerRepairPlan(ctx, LedgerRepairCommandInput{})
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "42501" || errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("real activation SELECT failure was converted to definite drift: %T %v", err, err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if financialFacts(t, ctx, r) != before {
			t.Fatal("failed activation read changed wallet, journal or proof claim")
		}
	})
	t.Run("controlled-empty-session-projection-remains-mismatch", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		before := financialFacts(t, ctx, r)
		activationFacts := func() string {
			t.Helper()
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.command_type,a.policy_version),'[]'::jsonb)::text FROM public.billing_approval_command_activations a`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		beforeActivation := activationFacts()
		var beforePublicOID uint32
		if err := r.pool.QueryRow(ctx, `SELECT 'public.billing_approval_command_activations'::regclass::oid`).Scan(&beforePublicOID); err != nil {
			t.Fatal(err)
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// A controlled empty session projection makes the real QueryRow return
		// ErrNoRows without deleting an immutable production activation. This
		// is not evidence that an original migration prefix omitted Ledger.
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE billing_approval_command_activations (LIKE public.billing_approval_command_activations INCLUDING ALL) ON COMMIT DROP`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SET LOCAL search_path TO pg_temp,public`); err != nil {
			t.Fatal(err)
		}
		var tempProjection bool
		if err = tx.QueryRow(ctx, `SELECT to_regclass('billing_approval_command_activations')=to_regclass('pg_temp.billing_approval_command_activations')
			AND (SELECT relnamespace=pg_my_temp_schema() FROM pg_class WHERE oid=to_regclass('billing_approval_command_activations'))
			AND to_regclass('billing_approval_command_activations')<>$1::oid`, beforePublicOID).Scan(&tempProjection); err != nil || !tempProjection {
			t.Fatal("activation read must resolve only to this session's temporary projection", err)
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_temp.billing_approval_command_activations`).Scan(&count); err != nil || count != 0 {
			t.Fatal("controlled session projection must be empty", err)
		}
		if _, err = (billingTxProofFacts{repository: r, tx: tx}).ledgerRepairPlan(ctx, LedgerRepairCommandInput{}); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("missing activation projection stopped rejecting", err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var afterPublicOID uint32
		if err = r.pool.QueryRow(ctx, `SELECT 'public.billing_approval_command_activations'::regclass::oid`).Scan(&afterPublicOID); err != nil {
			t.Fatal(err)
		}
		if beforePublicOID != afterPublicOID || financialFacts(t, ctx, r) != before || activationFacts() != beforeActivation {
			t.Fatal("controlled missing activation read changed public activation or financial/claim facts")
		}
	})
	t.Run("installed-inactive-ledger-activation-remains-mismatch", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		var state string
		if err := r.pool.QueryRow(ctx, `SELECT state FROM billing_approval_command_activations WHERE command_type='LEDGER_REPAIR' AND policy_version=1`).Scan(&state); err != nil || state != "INACTIVE" {
			t.Fatal("original fresh Ledger contract must be inactive", err)
		}
		before := financialFacts(t, ctx, r)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = (billingTxProofFacts{repository: r, tx: tx}).ledgerRepairPlan(ctx, LedgerRepairCommandInput{}); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("inactive activation stopped rejecting", err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if financialFacts(t, ctx, r) != before {
			t.Fatal("inactive activation read wrote financial/claim facts")
		}
	})
}
