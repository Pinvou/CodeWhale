package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func reconciliationFrozenInput(p ReconciliationFinancialRepairPlan, v int64) ReconciliationFinancialRepairExecutionInput {
	id := uuid.New()
	return ReconciliationFinancialRepairExecutionInput{ApprovalRequestID: id, AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "reconciliation-repair:" + id.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: v, Command: ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: p.CaseID, PaymentEvidenceID: p.PaymentEvidenceID, BusinessDate: p.BusinessDate}, Frozen: ReconciliationFinancialRepairFrozenFacts{RunID: p.RunID, AmountFen: p.AmountFen, Currency: p.Currency, ProviderTransactionID: p.ProviderTransactionID, ProviderSnapshotDigest: p.ProviderSnapshotDigest, FactsDigest: p.FactsDigest}}
}

// Every forbidden financial table is read as exact JSONB, not merely an amount
// aggregate. Cases/manual-review are allowed to close only on the success path.
func reconciliationEffectSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository, withCases bool) string {
	t.Helper()
	var raw string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM receivables x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_evidence x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM refunds x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM chargeback_cases x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM forced_refund_cases x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_holds x),
 CASE WHEN $1 THEN (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_cases x) END,
 CASE WHEN $1 THEN (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_cases x) END,
 CASE WHEN $1 THEN (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_repair_cases x) END)::text`, withCases).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReconciliationFrozenExecutionPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	r, ctx := retainedFinanceRepository(t)
	set := reconciliationFixtureActivation(t, ctx, r.pool)
	v := set("ACTIVE")
	plan := func(f reconciliationRepairFixture) ReconciliationFinancialRepairPlan {
		p, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: f.EvidenceID, BusinessDate: f.BusinessDate}, 1, 1, v)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	reject := func(t *testing.T, i ReconciliationFinancialRepairExecutionInput) {
		t.Helper()
		before := reconciliationEffectSnapshot(t, ctx, r, true)
		_, err := r.ApplyReconciliationFinancialRepair(ctx, i)
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("fresh rejection %v", err)
		}
		if reconciliationEffectSnapshot(t, ctx, r, true) != before {
			t.Fatal("rejection wrote effects")
		}
	}
	t.Run("empty-digest-zero-effect", func(t *testing.T) {
		i := reconciliationFrozenInput(plan(seedProviderPaidInternalMissing(t, ctx, r, "empty")), v)
		i.Frozen.FactsDigest = ""
		reject(t, i)
	})
	t.Run("activation-version-receipt-manual-and-case-version", func(t *testing.T) {
		f := seedProviderPaidInternalMissing(t, ctx, r, "receipt")
		i := reconciliationFrozenInput(plan(f), v)
		set("INACTIVE")
		reject(t, i)
		v = set("ACTIVE")
		reject(t, i)
		i = reconciliationFrozenInput(plan(f), v)
		before := reconciliationEffectSnapshot(t, ctx, r, false)
		var oldVersion int64
		if err := r.pool.QueryRow(ctx, `SELECT version FROM reconciliation_cases WHERE id=$1`, f.CaseID).Scan(&oldVersion); err != nil {
			t.Fatal(err)
		}
		i.ExecutionKey = "race:reconciliation-repair"
		good, err := reconciliationFinancialWithDeactivationRace(t, ctx, r, i)
		if err != nil {
			t.Fatal(err)
		}
		var version int64
		var status string
		var resolved *time.Time
		if err := r.pool.QueryRow(ctx, `SELECT c.version,m.status,m.resolved_at FROM reconciliation_cases c JOIN manual_review_cases m ON m.id=c.manual_review_case_id WHERE c.id=$1`, f.CaseID).Scan(&version, &status, &resolved); err != nil || version != oldVersion+1 || status != "RESOLVED" || resolved == nil {
			t.Fatalf("case/manual %d %s %v %v", version, status, resolved, err)
		}
		if reconciliationEffectSnapshot(t, ctx, r, false) != before {
			t.Fatal("zero-accounting contract violated")
		}
		set("INACTIVE")
		before = reconciliationEffectSnapshot(t, ctx, r, true)
		same, err := r.ApplyReconciliationFinancialRepair(ctx, i)
		if err != nil || same != good || reconciliationEffectSnapshot(t, ctx, r, true) != before {
			t.Fatalf("receipt replay %v %v", same, err)
		}
		foreign := i
		foreign.AuthorizationEventID = uuid.New()
		if _, err := r.ApplyReconciliationFinancialRepair(ctx, foreign); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign identity %v", err)
		}
		v = set("ACTIVE")
	})
	t.Run("same-channel-date-cross-run-recheck", func(t *testing.T) {
		f := seedProviderPaidInternalMissing(t, ctx, r, "crossrun")
		other := uuid.New()
		if _, err := r.pool.Exec(ctx, `INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at,provider_id,provider_version,source_mode) VALUES($1,'ALIPAY',$2::date,'COMPLETED',1,1,now(),now(),'test-provider','v1','MANUAL_API')`, other, f.BusinessDate); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,merchant_order_no,case_type,status,severity,provider_snapshot,created_at,updated_at) VALUES($1,$2,$3,'PROVIDER_PAID_INTERNAL_MISSING','OPEN','P1','{}',now(),now())`, uuid.New(), other, f.MerchantOrderNo); err != nil {
			t.Fatal(err)
		}
		before := reconciliationEffectSnapshot(t, ctx, r, true)
		_, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: f.EvidenceID, BusinessDate: f.BusinessDate}, 1, 1, v)
		if !errors.Is(err, ErrConflict) || reconciliationEffectSnapshot(t, ctx, r, true) != before {
			t.Fatalf("cross run recheck %v", err)
		}
	})
	t.Run("proof-claim-same-amount-case-version-drift", func(t *testing.T) {
		v = set("ACTIVE")
		f := seedProviderPaidInternalMissing(t, ctx, r, "claim")
		src, err := r.ReconciliationFinancialRepairProofInput(ctx, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: f.EvidenceID, BusinessDate: f.BusinessDate})
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
			t.Fatalf("actual proof %v %v", proof, err)
		}
		b := ApprovalAuthorizationBinding{ApprovalRequestID: id, EventID: eventID, ApprovalEpoch: 1, CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		e := approvalencoding.AuthorizationEvent{ApprovalRequestID: id, EventID: eventID, ApprovalControlEpoch: 1, CommandType: b.CommandType, PolicyVersion: 1, ExecutorIdentity: b.ExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: art.CommandDigest, SnapshotDigest: art.SnapshotDigest, DomainRevision: proof.DomainRevision}
		c := ApprovedCommand{ApprovalRequestID: id, CommandType: b.CommandType, PolicyVersion: 1, ExecutorIdentity: b.ExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent reconciliation facts review", ExecutionIdempotencyKey: "reconciliation-repair:" + id.String()}
		d := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := d.claim
		after := ""
		d.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_cases SET version=version+1 WHERE id=$1`, f.CaseID); err != nil {
				return err
			}
			after = reconciliationEffectSnapshot(t, ctx, r, true)
			return nil
		}
		a := d.ExecuteAuthorizationAttempt(ctx, b, e, c)
		if after == "" || !a.FailureIsDefinite || a.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(a.Result.DomainResult) != 0 || reconciliationEffectSnapshot(t, ctx, r, true) != after {
			t.Fatalf("stale same-amount facts accepted %+v", a)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("immutable claim %d %v", n, err)
		}
	})
	t.Run("evidence-provider-version-and-success-vocabulary", func(t *testing.T) {
		f := seedProviderPaidInternalMissing(t, ctx, r, "identity")
		for _, change := range []struct{ name, sql string }{{"foreign-provider", `'other-provider',provider_version,provider_status,payload_snapshot`}, {"foreign-version", `provider_id,'v2',provider_status,payload_snapshot`}, {"pending", `provider_id,provider_version,'PENDING',jsonb_set(payload_snapshot,'{status}','"PENDING"')`}, {"processing", `provider_id,provider_version,'PROCESSING',jsonb_set(payload_snapshot,'{status}','"PROCESSING"')`}, {"unknown", `provider_id,provider_version,'UNRECOGNIZED',jsonb_set(payload_snapshot,'{status}','"UNRECOGNIZED"')`}} {
			id := uuid.New()
			q := `INSERT INTO payment_evidence(id,evidence_type,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_event_at,received_at,payload_digest,merge_result,provider_id,provider_version,provider_status,payload_snapshot) SELECT $2,evidence_type,$3,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_event_at,received_at,payload_digest,merge_result,` + change.sql + ` FROM payment_evidence WHERE id=$1`
			if _, err := r.pool.Exec(ctx, q, f.EvidenceID, id, "negative:"+id.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: id, BusinessDate: f.BusinessDate}, 1, 1, v); !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatalf("%s accepted %v", change.name, err)
			}
		}
	})
	t.Run("both-real-bill-writers-refund-reference-and-no-provider-call", func(t *testing.T) {
		f := seedProviderPaidInternalMissing(t, ctx, r, "writer")
		provider := &paymentRepairStubProvider{}
		p := &PaymentCoordinator{repository: r, provider: provider, now: time.Now}
		row := ProviderBillEntry{EntryType: "REFUND", ProviderReferenceID: "  refund-direct-" + uuid.NewString() + "  ", ChannelTransactionID: f.ChannelTransactionID, MerchantOrderNo: f.MerchantOrderNo, Channel: "ALIPAY", AmountFen: f.AmountFen, Currency: "CNY", Status: "SUCCESS", PaidAt: time.Now().UTC()}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := p.mergeProviderBillEvidenceWithinTransaction(ctx, tx, row, nil, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var ref, txn string
		if err := r.pool.QueryRow(ctx, `SELECT provider_refund_id,provider_transaction_id FROM payment_evidence WHERE evidence_type='BILL' AND merchant_order_no=$1 AND provider_refund_id=$2`, f.MerchantOrderNo, billReference(row)).Scan(&ref, &txn); err != nil || ref != billReference(row) || txn != f.ChannelTransactionID || provider.calls != 0 {
			t.Fatalf("genuine writer %s %s calls=%d err=%v", ref, txn, provider.calls, err)
		}
		// The bounded production pipeline already populated the first evidence; its
		// normalized reference is a real REFUND source, not a hand-entered receipt.
		if err := r.pool.QueryRow(ctx, `SELECT provider_refund_id FROM payment_evidence WHERE id=$1`, f.EvidenceID).Scan(&ref); err != nil || ref != f.ProviderRefundID {
			t.Fatalf("pipeline writer %s %v", ref, err)
		}
	})
}
