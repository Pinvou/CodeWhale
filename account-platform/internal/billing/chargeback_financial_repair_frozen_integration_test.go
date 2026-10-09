package billing

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func chargebackFrozenInput(p ChargebackFinancialRepairPlan, version int64) ChargebackFinancialRepairExecutionInput {
	id := uuid.New()
	return ChargebackFinancialRepairExecutionInput{ApprovalRequestID: id, AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "chargeback-repair:" + id.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: version, Command: ChargebackFinancialRepairCommandInput{ChargebackCaseID: p.ChargebackCaseID, PaymentEvidenceID: p.EvidenceID}, Frozen: ChargebackFinancialRepairFrozenFacts{CaseVersion: p.CaseVersion, BaseShortfallMicrocredits: p.BaseShortfallMicrocredits, ComponentDigest: p.ComponentDigest, WalletID: p.WalletID, WalletVersion: p.WalletVersion, PackageID: p.PackageID, FactsDigest: p.FactsDigest}}
}

func chargebackEffectSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var raw string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM payment_orders o),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_cases c),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM credit_lots l),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM payment_anomaly_cases a),(SELECT count(*) FROM chargeback_financial_repair_cases),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT count(*) FROM receivables),(SELECT count(*) FROM outbox_events))::text`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestChargebackFrozenExecutionPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	r, ctx := retainedFinanceRepository(t)
	set := chargebackFixtureActivation(t, ctx, r.pool)
	v := set("ACTIVE")
	plan := func(f chargebackRepairFixture) ChargebackFinancialRepairPlan {
		p, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, ChargebackFinancialRepairCommandInput{ChargebackCaseID: f.ChargebackCaseID, PaymentEvidenceID: f.EvidenceID}, 1, 1, v)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	reject := func(t *testing.T, i ChargebackFinancialRepairExecutionInput) {
		t.Helper()
		before := chargebackEffectSnapshot(t, ctx, r)
		_, err := r.ApplyChargebackFinancialRepair(ctx, i)
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("fresh rejection %v", err)
		}
		if chargebackEffectSnapshot(t, ctx, r) != before {
			t.Fatal("rejection wrote effects")
		}
	}
	t.Run("empty-digest-zero-effect", func(t *testing.T) {
		i := chargebackFrozenInput(plan(seedChargebackShortfall(t, ctx, r, false)), v)
		i.Frozen.FactsDigest = ""
		reject(t, i)
	})
	t.Run("activation-version-receipt-and-order-summary", func(t *testing.T) {
		f := seedChargebackShortfall(t, ctx, r, true)
		i := chargebackFrozenInput(plan(f), v)
		var originalSummary int64
		if err := r.pool.QueryRow(ctx, `SELECT summary_version FROM payment_orders WHERE id=$1`, f.OrderID).Scan(&originalSummary); err != nil {
			t.Fatal(err)
		}
		set("INACTIVE")
		reject(t, i)
		v = set("ACTIVE")
		reject(t, i)
		i = chargebackFrozenInput(plan(f), v)
		good, err := r.ApplyChargebackFinancialRepair(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		var open, loss, summary int64
		var attention string
		if err := r.pool.QueryRow(ctx, `SELECT (chargeback_summary->>'open_count')::bigint,(chargeback_summary->>'lost_amount_fen')::bigint,attention_summary,summary_version FROM payment_orders WHERE id=$1`, f.OrderID).Scan(&open, &loss, &attention, &summary); err != nil || open != 1 || loss != 100 || attention != "RECEIVABLE_OPEN" || summary != originalSummary+1 {
			t.Fatalf("order summary %d/%d/%s/%d err=%v", open, loss, attention, summary, err)
		}
		set("INACTIVE")
		before := chargebackEffectSnapshot(t, ctx, r)
		same, err := r.ApplyChargebackFinancialRepair(ctx, i)
		if err != nil || same != good || chargebackEffectSnapshot(t, ctx, r) != before {
			t.Fatalf("stored receipt changed %+v %v", same, err)
		}
		other := i
		other.AuthorizationEventID = uuid.New()
		if _, err = r.ApplyChargebackFinancialRepair(ctx, other); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign identity %v", err)
		}
		v = set("ACTIVE")
	})
	t.Run("proof-claim-same-effective-component-tuple-drift", func(t *testing.T) {
		v = set("ACTIVE")
		f := seedChargebackShortfall(t, ctx, r, false)
		src, err := r.ChargebackFinancialRepairProofInput(ctx, ChargebackFinancialRepairCommandInput{ChargebackCaseID: f.ChargebackCaseID, PaymentEvidenceID: f.EvidenceID})
		if err != nil {
			t.Fatal(err)
		}
		art, err := approvalencoding.ConvertLegacyProofInput(src)
		if err != nil {
			t.Fatal(err)
		}
		id, eventID := uuid.New(), uuid.New()
		service := NewService(r)
		proof, err := service.billingApprovalProof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: id, Artifacts: art})
		if err != nil || proof.Verdict != "ALLOW" {
			t.Fatalf("actual proof %+v %v", proof, err)
		}
		b := ApprovalAuthorizationBinding{ApprovalRequestID: id, EventID: eventID, ApprovalEpoch: 1, CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		e := approvalencoding.AuthorizationEvent{ApprovalRequestID: id, EventID: eventID, ApprovalControlEpoch: 1, CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: art.CommandDigest, SnapshotDigest: art.SnapshotDigest, DomainRevision: proof.DomainRevision}
		c := ApprovedCommand{ApprovalRequestID: id, CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent frozen chargeback review", ExecutionIdempotencyKey: "chargeback-repair:" + id.String()}
		d := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := d.claim
		after := ""
		d.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			// The effective original (and thus BASE shortfall) stays 100. Both
			// original and reclassified amounts move, which the old aggregate hid.
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET original_microcredits=original_microcredits+1,reclassified_out_microcredits=reclassified_out_microcredits+1 WHERE id=$1`, f.ComponentID); err != nil {
				return err
			}
			after = chargebackEffectSnapshot(t, ctx, r)
			return nil
		}
		a := d.ExecuteAuthorizationAttempt(ctx, b, e, c)
		if after == "" || !a.FailureIsDefinite || a.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(a.Result.DomainResult) != 0 || after != chargebackEffectSnapshot(t, ctx, r) {
			t.Fatalf("same effective stale component accepted %+v", a)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("immutable claim %d %v", n, err)
		}
	})
	t.Run("wallet-source-lock-before-final-recompute", func(t *testing.T) {
		f := seedChargebackShortfall(t, ctx, r, false)
		i := chargebackFrozenInput(plan(f), v)
		holder, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		var pid int
		var id uuid.UUID
		if err := holder.QueryRow(ctx, `SELECT pg_backend_pid(),id FROM wallets WHERE id=$1 FOR UPDATE`, f.WalletID).Scan(&pid, &id); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := r.ApplyChargebackFinancialRepair(ctx, i); done <- err }()
		deadline := time.Now().Add(3 * time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("actual owner was not blocked on source wallet")
		}
		if _, err := holder.Exec(ctx, `UPDATE wallets SET version=version+1 WHERE id=$1`, f.WalletID); err != nil {
			t.Fatal(err)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		after := chargebackEffectSnapshot(t, ctx, r)
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("waited source revision was accepted")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owner did not finish after source release")
		}
		if chargebackEffectSnapshot(t, ctx, r) != after {
			t.Fatal("waited rejection wrote effects")
		}
	})
}
