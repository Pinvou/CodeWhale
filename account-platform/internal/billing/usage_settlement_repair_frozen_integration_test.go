package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

func usageRepairEffectSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var raw string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('orders',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),'transactions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),'evidence',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_evidence x),'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),'lots',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),'packages',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),'components',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),'holds',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_holds x),'allocations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY hold_id,component_id) FROM usage_hold_allocations x),'reviews',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_loss_reviews x),'receivables',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM receivables x),'journals',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),'repairs',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_settlement_repair_cases x))::text`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
func usageRepairInput(t *testing.T, ctx context.Context, r *PostgresRepository, f usageRepairFixture, v int64) UsageSettlementRepairExecutionInput {
	t.Helper()
	command := UsageSettlementRepairCommandInput{UsageRequestID: f.UsageRequestID, HoldID: f.HoldID, LossReviewID: f.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind}
	p, err := usageSettlementRepairPlanFrom(ctx, r.pool, command, 1, 1, v)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	return UsageSettlementRepairExecutionInput{ApprovalRequestID: id, AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "usage-repair:" + id.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: v, Command: command, Frozen: UsageSettlementRepairFrozenFacts{HoldUpdatedAt: p.HoldUpdatedAt, LossReviewUpdatedAt: p.LossReviewUpdatedAt, ExpectedMicrocredits: p.ExpectedMicrocredits, SettledMicrocredits: p.SettledMicrocredits, WalletID: p.WalletID, WalletVersion: p.WalletVersion, AllocationsDigest: p.AllocationsDigest, FactsDigest: p.FactsDigest}}
}

func TestUsageRepairFrozenExecutionPostgres(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if url == "" {
		t.Skip("owned PostgreSQL opt-in not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, _ := newAdminPaginationIntegrationRepository(t, ctx, url)
	set := usageRepairFixtureActivation(t, ctx, r.pool)
	v := set("ACTIVE")
	for _, state := range []string{"FROZEN", "REVOKED", "EXPIRED"} {
		t.Run("preserve-"+state, func(t *testing.T) {
			f := seedUnsettledUsageLoss(t, ctx, r, state, 50, 100, 100)
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET status=$2 WHERE id=$1`, f.ComponentID, state); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET status=$2 WHERE id=$1`, f.PackageID, state); err != nil {
				t.Fatal(err)
			}
			in := usageRepairInput(t, ctx, r, f, v)
			result, err := r.ApplyUsageSettlementRepair(ctx, in)
			if err != nil || result.SettledMicrocredits != 50 {
				t.Fatalf("apply %v %+v", err, result)
			}
			var component, pack string
			if err := r.pool.QueryRow(ctx, `SELECT c.status,p.status FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE c.id=$1`, f.ComponentID).Scan(&component, &pack); err != nil || component != state || pack != state {
				t.Fatalf("terminal resurrected %s/%s %v", component, pack, err)
			}
		})
	}
	t.Run("pricing-identity-and-claimed", func(t *testing.T) {
		f := seedUnsettledUsageLoss(t, ctx, r, "pricing", 50, 100, 100)
		command := UsageSettlementRepairCommandInput{UsageRequestID: f.UsageRequestID, HoldID: f.HoldID, LossReviewID: f.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind}
		for _, change := range []string{
			`UPDATE usage_loss_reviews SET claimed_actual_microcredits=51 WHERE id=$1`,
			`UPDATE usage_holds SET pricing_snapshot=jsonb_set(pricing_snapshot,'{price_version_id}',to_jsonb('00000000-0000-7000-8000-000000000111'::text)) WHERE id=$1`,
			`UPDATE usage_holds SET pricing_snapshot=NULL WHERE id=$1`,
			`UPDATE usage_loss_reviews SET usage_snapshot='{}' WHERE id=$1`,
		} {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id := f.HoldID
			if strings.Contains(change, "usage_loss_reviews") {
				id = f.LossReviewID
			}
			if _, err := tx.Exec(ctx, change, id); err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			_, err = usageSettlementRepairPlanFrom(ctx, tx, command, 1, 1, v)
			if !errors.Is(err, ErrApprovalSnapshotMismatch) {
				tx.Rollback(ctx)
				t.Fatalf("pricing accepted %s: %v", change, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("insufficient-wallet-proof-and-execution-zero-effect", func(t *testing.T) {
		f := seedUnsettledUsageLoss(t, ctx, r, "wallet", 50, 100, 100)
		in := usageRepairInput(t, ctx, r, f, v)
		if _, err := r.pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=49,version=version+1 WHERE id=$1`, f.WalletID); err != nil {
			t.Fatal(err)
		}
		before := usageRepairEffectSnapshot(t, ctx, r)
		if _, err := r.UsageSettlementRepairProofInput(ctx, in.Command); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("insufficient wallet proof accepted %v", err)
		}
		if _, err := r.ApplyUsageSettlementRepair(ctx, in); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
			t.Fatalf("insufficient wallet execution %v", err)
		}
	})
	t.Run("empty-live-version-and-receipt-first", func(t *testing.T) {
		f := seedUnsettledUsageLoss(t, ctx, r, "live", 50, 100, 100)
		in := usageRepairInput(t, ctx, r, f, v)
		empty := in
		empty.Frozen.FactsDigest = ""
		before := usageRepairEffectSnapshot(t, ctx, r)
		if _, err := r.ApplyUsageSettlementRepair(ctx, empty); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
			t.Fatalf("empty digest accepted %v", err)
		}
		set("INACTIVE")
		if _, err := r.ApplyUsageSettlementRepair(ctx, in); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
			t.Fatalf("inactive accepted %v", err)
		}
		v = set("ACTIVE")
		if _, err := r.ApplyUsageSettlementRepair(ctx, in); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
			t.Fatalf("old activation version accepted %v", err)
		}
		in = usageRepairInput(t, ctx, r, f, v)
		result, err := r.ApplyUsageSettlementRepair(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		after := usageRepairEffectSnapshot(t, ctx, r)
		set("INACTIVE")
		replay, err := r.ApplyUsageSettlementRepair(ctx, in)
		if err != nil || replay.CaseID != result.CaseID || usageRepairEffectSnapshot(t, ctx, r) != after {
			t.Fatalf("committed replay %v %+v", err, replay)
		}
		v = set("ACTIVE")
		var update, del bool
		if err := r.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'usage_settlement_repair_cases','UPDATE'),has_table_privilege(current_user,'usage_settlement_repair_cases','DELETE')`).Scan(&update, &del); err != nil {
			t.Fatal(err)
		}
		t.Logf("runtime ACL update=%v delete=%v; append-only rejection checked independently", update, del)
		for _, sql := range []string{`UPDATE usage_settlement_repair_cases SET facts_digest=facts_digest WHERE id=$1`, `DELETE FROM usage_settlement_repair_cases WHERE id=$1`} {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, sql, result.CaseID)
			if err == nil {
				tx.Rollback(ctx)
				t.Fatalf("immutable receipt permitted %s", sql)
			}
			tx.Rollback(ctx)
		}
	})
	t.Run("actual-proof-claim-same-amount-facts-drift", func(t *testing.T) {
		f := seedUnsettledUsageLoss(t, ctx, r, "claim", 50, 100, 100)
		src, err := r.UsageSettlementRepairProofInput(ctx, UsageSettlementRepairCommandInput{UsageRequestID: f.UsageRequestID, HoldID: f.HoldID, LossReviewID: f.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind})
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
		b := ApprovalAuthorizationBinding{ApprovalRequestID: id, EventID: eventID, ApprovalEpoch: 1, CommandType: usageSettlementRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		e := approvalencoding.AuthorizationEvent{ApprovalRequestID: id, EventID: eventID, ApprovalControlEpoch: 1, CommandType: b.CommandType, PolicyVersion: 1, ExecutorIdentity: b.ExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: art.CommandDigest, SnapshotDigest: art.SnapshotDigest, DomainRevision: proof.DomainRevision}
		c := ApprovedCommand{ApprovalRequestID: id, CommandType: b.CommandType, PolicyVersion: 1, ExecutorIdentity: b.ExecutorIdentity, TargetResourceType: src.TargetResourceType, TargetResourceID: src.TargetResourceID, NormalizedCommand: src.NormalizedCommand, CriticalResourceVersions: src.CriticalResourceVersions, FinancialSnapshot: src.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent usage facts review", ExecutionIdempotencyKey: "usage-repair:" + id.String()}
		d := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := d.claim
		after := ""
		d.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN' WHERE id=$1`, f.PackageID); err != nil {
				return err
			}
			after = usageRepairEffectSnapshot(t, ctx, r)
			return nil
		}
		a := d.ExecuteAuthorizationAttempt(ctx, b, e, c)
		if after == "" || !a.FailureIsDefinite || a.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || usageRepairEffectSnapshot(t, ctx, r) != after {
			t.Fatalf("stale claim accepted %+v", a)
		}
	})
}
