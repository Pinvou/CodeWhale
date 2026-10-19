package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

func refundFrozenInput(p RefundExceptionRepairPlan) RefundExceptionRepairExecutionInput {
	id := uuid.New()
	return RefundExceptionRepairExecutionInput{ApprovalRequestID: id, AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "refund-repair:" + id.String(), PolicyVersion: 1, SchemaVersion: 1, Command: RefundExceptionRepairCommandInput{RefundID: p.RefundID, PaymentEvidenceID: p.EvidenceID, AnomalyCaseID: p.AnomalyCaseID}, Frozen: RefundExceptionRepairFrozenFacts{RefundUpdatedAt: p.RefundUpdatedAt, AnomalyVersion: p.AnomalyVersion, AmountFen: p.AmountFen, CreditMicrocredits: p.CreditMicrocredits, ProviderRefundID: p.ProviderRefundID, RevokedMicrocredits: p.RevokedMicrocredits, ShortfallMicrocredits: p.ShortfallMicrocredits, WalletID: p.WalletID, WalletVersion: p.WalletVersion, PackageID: p.PackageID, FactsDigest: p.FactsDigest}}
}
func refundEffectSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var raw string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM refunds r),(SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM member_quota_grants g),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM payment_anomaly_cases c),(SELECT count(*) FROM refund_exception_repair_cases),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT count(*) FROM receivables),(SELECT count(*) FROM outbox_events))::text`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestRefundExceptionFrozenExecutionPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	r, ctx := retainedFinanceRepository(t)
	set := refundFixtureActivation(t, ctx, r.pool)
	v := set("ACTIVE")
	plan := func(f refundExceptionFixture) RefundExceptionRepairPlan {
		p, err := refundExceptionRepairPlanFrom(ctx, r.pool, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID}, 1, 1, v)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	reject := func(t *testing.T, i RefundExceptionRepairExecutionInput) {
		t.Helper()
		b := refundEffectSnapshot(t, ctx, r)
		_, err := r.ApplyRefundExceptionRepair(ctx, i)
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("fresh rejection %v", err)
		}
		if refundEffectSnapshot(t, ctx, r) != b {
			t.Fatal("rejection wrote effects")
		}
	}
	t.Run("empty-digest-zero-effect", func(t *testing.T) {
		i := refundFrozenInput(plan(seedRefundException(t, ctx, r, false)))
		i.ActivationVersion = v
		i.Frozen.FactsDigest = ""
		reject(t, i)
	})
	t.Run("nullable-evidence-definite-deny", func(t *testing.T) {
		f := seedRefundException(t, ctx, r, false)
		id := uuid.New()
		_, err := r.pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result) SELECT $1,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key||':null',merchant_order_no,provider_transaction_id,NULL,amount_fen,currency,NULL,received_at,payload_digest,payload_snapshot,merge_result FROM payment_evidence WHERE id=$2`, id, f.EvidenceID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = refundExceptionRepairPlanFrom(ctx, r.pool, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: id, AnomalyCaseID: f.AnomalyCaseID}, 1, 1, v)
		if !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("NULL evidence classification %v", err)
		}
	})
	t.Run("fresh-state-version-and-receipt-first", func(t *testing.T) {
		i := refundFrozenInput(plan(seedRefundException(t, ctx, r, false)))
		i.ActivationVersion = v
		set("INACTIVE")
		reject(t, i)
		v = set("ACTIVE")
		reject(t, i)
		i.ActivationVersion = v
		// Activation is part of the frozen digest; re-read after changing its version.
		p, err := refundExceptionRepairPlanFrom(ctx, r.pool, i.Command, 1, 1, v)
		if err != nil {
			t.Fatal(err)
		}
		i.Frozen = refundFrozenInput(p).Frozen
		good, err := r.ApplyRefundExceptionRepair(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		set("INACTIVE")
		b := refundEffectSnapshot(t, ctx, r)
		same, err := r.ApplyRefundExceptionRepair(ctx, i)
		if err != nil || same != good || refundEffectSnapshot(t, ctx, r) != b {
			t.Fatalf("stored receipt changed %+v %v", same, err)
		}
		other := i
		other.AuthorizationEventID = uuid.New()
		if _, err := r.ApplyRefundExceptionRepair(ctx, other); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign identity %v", err)
		}
		v = set("ACTIVE")
	})
	t.Run("proof-claim-same-amount-grant-version-drift", func(t *testing.T) {
		f := seedRefundException(t, ctx, r, false)
		var component uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1`, f.PackageID).Scan(&component); err != nil {
			t.Fatal(err)
		}
		grant := uuid.New()
		member := uuid.New()
		if _, err := r.pool.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,suspension_reason,membership_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',1,1,'SUSPENDED','REFUND_PENDING',1,$3,$1::uuid::text,repeat('a',64),now()+interval '1 day',now(),now())`, grant, uuid.New(), member, f.PackageID, component); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=99,suspended_grant_microcredits=1 WHERE id=$1`, component); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE refund_component_reservations SET available_microcredits=99,suspended_grant_microcredits=1 WHERE refund_id=$1`, f.RefundID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO refund_grant_reservations(refund_id,grant_id,component_id,available_microcredits,prior_status,prior_suspension_reason,created_at) VALUES($1,$2,$3,1,'ACTIVE',NULL,now())`, f.RefundID, grant, component); err != nil {
			t.Fatal(err)
		}
		src, err := r.RefundExceptionRepairProofInput(ctx, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID})
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
		b := ApprovalAuthorizationBinding{ApprovalRequestID: id, EventID: eventID, ApprovalEpoch: 1, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		e := approvalencoding.AuthorizationEvent{ApprovalRequestID: id, EventID: eventID, ApprovalControlEpoch: 1, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: art.CommandDigest, SnapshotDigest: art.SnapshotDigest, DomainRevision: proof.DomainRevision}
		c := ApprovedCommand{ApprovalRequestID: id, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent frozen refund review", ExecutionIdempotencyKey: "refund-repair:" + id.String()}
		d := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := d.claim
		after := ""
		d.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			if _, err := r.pool.Exec(ctx, `UPDATE member_quota_grants SET allocation_version=allocation_version+1 WHERE id=$1`, grant); err != nil {
				return err
			}
			after = refundEffectSnapshot(t, ctx, r)
			return nil
		}
		a := d.ExecuteAuthorizationAttempt(ctx, b, e, c)
		if after == "" || !a.FailureIsDefinite || a.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(a.Result.DomainResult) != 0 || after != refundEffectSnapshot(t, ctx, r) {
			t.Fatalf("same amount stale grant accepted %+v", a)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("immutable claim %d %v", n, err)
		}
	})
	t.Run("proof-claim-same-amount-anomaly-version-drift", func(t *testing.T) {
		f := seedRefundException(t, ctx, r, false)
		src, err := r.RefundExceptionRepairProofInput(ctx, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID})
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
		b := ApprovalAuthorizationBinding{ApprovalRequestID: id, EventID: eventID, ApprovalEpoch: 1, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		e := approvalencoding.AuthorizationEvent{ApprovalRequestID: id, EventID: eventID, ApprovalControlEpoch: 1, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: art.CommandDigest, SnapshotDigest: art.SnapshotDigest, DomainRevision: proof.DomainRevision}
		c := ApprovedCommand{ApprovalRequestID: id, CommandType: refundExceptionRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent frozen refund review", ExecutionIdempotencyKey: "refund-repair:" + id.String()}
		snapshot := func() string {
			var raw string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
(SELECT to_jsonb(claim) FROM billing_approval_proof_claims claim WHERE approval_request_id=$1),
(SELECT jsonb_agg(to_jsonb(component) ORDER BY id) FROM entitlement_components component WHERE package_id=$2),
(SELECT jsonb_agg(to_jsonb(lot) ORDER BY id) FROM credit_lots lot WHERE wallet_id=$3))::text`, id, f.PackageID, f.WalletID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			return refundEffectSnapshot(t, ctx, r) + raw
		}
		d := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := d.claim
		after := ""
		d.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			// Exercise the existing claim-to-execution boundary without changing
			// amounts, status or other owner facts, or weakening Serializable.
			tag, err := r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET version=version+1 WHERE id=$1
AND status='RESOLVED' AND resolution_code='ESCALATE_FINANCIAL_REPAIR' AND financial_action_status='PENDING'`, f.AnomalyCaseID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				t.Fatal("the claimed anomaly must advance exactly once")
			}
			after = snapshot()
			return nil
		}
		a := d.ExecuteAuthorizationAttempt(ctx, b, e, c)
		if after == "" || !a.FailureIsDefinite || a.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(a.Result.DomainResult) != 0 || after != snapshot() {
			t.Fatalf("same amount stale anomaly accepted %+v", a)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("immutable claim %d %v", n, err)
		}
	})
}

// This executes the real ordinary-refund local-conflict writer with a result
// obtained from the controlled original provider. No external provider or
// operator resolution API is claimed; fixture-owned escalation precedes proof.
func TestRefundSuccessConflictEvidencePostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	for _, shortfall := range []bool{false, true} {
		name := "full-reclaim"
		if shortfall {
			name = "with-shortfall"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, o, tr, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, o.ID, o.RequesterID, tr, pack, uuid.NewString(), "controlled original refund")
			if err != nil {
				t.Fatal(err)
			}
			reference := " verified-original-refund "
			if shortfall {
				// The accepted TrimSpace identity also includes Unicode whitespace;
				// the stored original provider payload must remain byte-for-byte intact.
				reference = " \u3000verified-original-refund\u0085 "
			}
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: reference}}
			result, err := provider.Refund(ctx, o.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen)
			if err != nil {
				t.Fatal(err)
			}
			if shortfall {
				if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-1,used_microcredits=used_microcredits+1 WHERE package_id=$1`, pack); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-1,version=version+1 WHERE id=$1`, o.WalletID); err != nil {
				t.Fatal(err)
			}
			finished, _, err := c.finishRefund(ctx, refund, result, false)
			if err != nil || finished.Status != "MANUAL_REVIEW" {
				t.Fatalf("real local conflict %+v %v", finished, err)
			}
			var evidence, anomaly uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT e.id,a.id FROM payment_evidence e JOIN payment_anomaly_cases a ON a.refund_id=$1 AND a.dedupe_key='refund:'||$1::text||':local-conflict' WHERE e.payment_order_id=$2 AND e.payment_transaction_id=$3 AND e.provider_refund_id=$4 AND e.provider_status='SUCCESS'`, refund.ID, o.ID, tr, strings.TrimSpace(result.ChannelRefundID)).Scan(&evidence, &anomaly); err != nil {
				t.Fatalf("producer evidence absent %v", err)
			}
			// Idempotent writer readback may not create a second observation.
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			same, err := c.insertRefundSuccessEvidence(ctx, tx, o, refund, result, time.Now().UTC())
			if err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil || same != evidence {
				t.Fatalf("evidence replay %s %v", same, err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=now() WHERE id=$1`, anomaly); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET status='RESOLVED',resolution_code='ESCALATE_FINANCIAL_REPAIR',financial_action_status='PENDING',version=version+1,resolved_at=now() WHERE id=$1`, anomaly); err != nil {
				t.Fatal(err)
			}
			if !shortfall {
				// The ordinary writer saw an insufficient wallet and retained its actual
				// verified SUCCESS evidence. Restore only this controlled fixture fault;
				// all original reservations remain fully reclaimable for the repair.
				if _, err := r.pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+1,version=version+1 WHERE id=$1`, o.WalletID); err != nil {
					t.Fatal(err)
				}
			}
			v := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
			cmd := RefundExceptionRepairCommandInput{RefundID: refund.ID, PaymentEvidenceID: evidence, AnomalyCaseID: anomaly}
			p, err := refundExceptionRepairPlanFrom(ctx, r.pool, cmd, 1, 1, v)
			if err != nil {
				t.Fatal(err)
			}
			i := refundFrozenInput(p)
			i.ActivationVersion = v
			good, err := r.ApplyRefundExceptionRepair(ctx, i)
			expectedShortfall, expectedRevoked := int64(0), int64(1000000)
			if shortfall {
				expectedShortfall, expectedRevoked = 1000000, 0
			}
			if err != nil || good.ShortfallMicrocredits != expectedShortfall || good.RevokedMicrocredits != expectedRevoked {
				t.Fatalf("writer-produced repair %+v %v", good, err)
			}
			if provider.calls != 1 {
				t.Fatal("repair called provider")
			}
			var n int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE provider_refund_id=$1`, strings.TrimSpace(result.ChannelRefundID)).Scan(&n); err != nil || n != 1 {
				t.Fatalf("evidence count %d %v", n, err)
			}
			// The emitted payload is immutable JSON with the exact original amount.
			var raw []byte
			if err := r.pool.QueryRow(ctx, `SELECT payload_snapshot FROM payment_evidence WHERE id=$1`, evidence).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var payload ProviderRefund
			if json.Unmarshal(raw, &payload) != nil || payload.AmountFen != refund.AmountFen || payload.ChannelRefundID != result.ChannelRefundID || !strings.EqualFold(payload.Status, "SUCCESS") {
				t.Fatal("payload drift")
			}
			assertRefundRepairFourWayFacts(t, r, ctx, c, o, tr, refund)
		})
	}
}

func assertRefundRepairFourWayFacts(t *testing.T, r *PostgresRepository, ctx context.Context, c *PaymentCoordinator, order PaymentOrder, transaction uuid.UUID, refund Refund) {
	t.Helper()
	var paymentReference, refundReference string
	if err := r.pool.QueryRow(ctx, `SELECT provider_transaction_id FROM payment_transactions WHERE id=$1`, transaction).Scan(&paymentReference); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT channel_refund_id FROM refunds WHERE id=$1`, refund.ID).Scan(&refundReference); err != nil {
		t.Fatal(err)
	}
	observedAt := r.now()
	controlledBill := []ProviderBillEntry{
		{EntryType: "PAYMENT", ProviderReferenceID: paymentReference, ChannelTransactionID: paymentReference, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: observedAt},
		{EntryType: "REFUND", ProviderReferenceID: refundReference, ChannelTransactionID: refundReference, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: refund.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: observedAt},
	}
	// Use the same trusted API-bill persistence path as production. Passing an
	// incoming row alone does not prove the supplier's occurrence date.
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: *c.provider.(*retainedFinancialProvider), rows: controlledBill}
	api, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	local := observedAt.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	run, err := api.RunAdminReconciliation(ctx, uuid.New(), order.Channel, normalizedReconciliationBusinessDate(local))
	if err != nil || run.Status != "COMPLETED" || run.CaseCount != 0 {
		t.Fatalf("actual API reconciliation failed: %+v %v", run, err)
	}
	start, end := refundRepairAuditBusinessWindow(observedAt)
	facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected original payment and repaired refund, got %d facts", len(facts))
	}
	if err := auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
		t.Fatal(err)
	}
	seenRepair := false
	for _, fact := range facts {
		if !fact.LedgerPresent || !fact.LedgerValid || fact.IntegrityCaseType != "" {
			t.Fatalf("real repaired financial fact failed integrity: kind=%s present=%v valid=%v case=%s", fact.EntryType, fact.LedgerPresent, fact.LedgerValid, fact.IntegrityCaseType)
		}
		if fact.EntryType == "REFUND" {
			if !fact.EventDateVerified || fact.EventDateConflict || fact.ProviderReferenceID != strings.TrimSpace(refundReference) {
				t.Fatalf("trusted refund date or canonical identity missing: %+v", fact)
			}
			var source reconciliationAuditRecord
			if err := json.Unmarshal(fact.RefundSnapshot, &source); err != nil || auditString(source, "id") != refund.ID.String() {
				t.Fatal("reconciliation matched another refund", err)
			}
			seenRepair = true
		}
	}
	if !seenRepair {
		t.Fatal("the actual repaired refund was not selected")
	}
	if differences := CompareFourWayReconciliation(controlledBill, facts); len(differences) != 0 {
		t.Fatalf("actual repaired refund generated differences: %+v", differences)
	}
}

// The reconciliation input is a Shanghai calendar date, not the UTC date of
// the event. Keep the real query's half-open business-day window unchanged.
func refundRepairAuditBusinessWindow(now time.Time) (time.Time, time.Time) {
	local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	return reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(local))
}

func TestRefundRepairAuditBusinessWindowIncludesEvent(t *testing.T) {
	for _, value := range []string{"2026-10-20T15:59:59Z", "2026-10-20T16:00:00Z", "2026-10-20T19:29:00Z"} {
		t.Run(value, func(t *testing.T) {
			event, err := time.Parse(time.RFC3339, value)
			if err != nil {
				t.Fatal(err)
			}
			start, end := refundRepairAuditBusinessWindow(event)
			local := event.In(time.FixedZone("Asia/Shanghai", 8*60*60))
			if event.Before(start) || !event.Before(end) || end.Sub(start) != 24*time.Hour || start.In(local.Location()).Day() != local.Day() {
				t.Fatalf("event %s absent from Shanghai business window [%s,%s)", event, start, end)
			}
		})
	}
}
