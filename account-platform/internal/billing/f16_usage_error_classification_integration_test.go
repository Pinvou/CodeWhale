package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func f16UsageCASFullSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var memberAudit string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e),(SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM member_quota_grants g),(SELECT jsonb_agg(to_jsonb(a) ORDER BY hold_id,grant_id) FROM member_quota_hold_allocations a),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a))::text`).Scan(&memberAudit); err != nil {
		t.Fatal(err)
	}
	return usageRepairEffectSnapshot(t, ctx, r) + "\n" + reconciliationEffectSnapshot(t, ctx, r, false) + "\n" + memberAudit
}

// Controlled SQLSTATE injection occurs at the late hold update, after the actual
// lot/component/allocation/wallet/journal and overage-receivable writes. This
// proves whole transaction rollback, not concurrent deadlock/serialization or
// provider delivery. The original ordinary personal fixture remains unchanged.
func TestF16UsageCASInfrastructurePostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	for _, code := range []string{"40001", "40P01", "58000", "zero-rows"} {
		t.Run(code, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			activation := usageRepairFixtureActivation(t, ctx, r.pool)("ACTIVE")
			fixture := seedUnsettledUsageLoss(t, ctx, r, "f16-cas-fault", 150, 100, 100)
			input := usageRepairInput(t, ctx, r, fixture, activation)
			plan, err := usageSettlementRepairPlanFrom(ctx, r.pool, input.Command, 1, 1, activation)
			if err != nil || plan.MemberBacked || plan.ExpectedMicrocredits != 150 || plan.SettledMicrocredits != 100 || plan.OverageMicrocredits != 50 {
				t.Fatal("original personal usage overage preflight", err)
			}
			// Match the canonical UTC wire projection without changing either instant.
			input.Frozen.HoldUpdatedAt = input.Frozen.HoldUpdatedAt.UTC()
			input.Frozen.LossReviewUpdatedAt = input.Frozen.LossReviewUpdatedAt.UTC()
			ownerConnection := f13RefundCASFaultOwner(t, ctx, r)
			body := "RAISE EXCEPTION USING ERRCODE = '" + code + "', MESSAGE = 'controlled usage CAS fixture fault';"
			if code == "zero-rows" {
				body = "RETURN NULL;"
			}
			sql := fmt.Sprintf(`CREATE FUNCTION public.f16_usage_cas_fault() RETURNS trigger LANGUAGE plpgsql AS $fixture$ BEGIN %s END; $fixture$; CREATE TRIGGER f16_usage_cas_fault BEFORE UPDATE OF usage_snapshot ON public.usage_holds FOR EACH ROW EXECUTE FUNCTION public.f16_usage_cas_fault();`, body)
			if _, err := ownerConnection.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ownerConnection.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS f16_usage_cas_fault ON public.usage_holds; DROP FUNCTION IF EXISTS public.f16_usage_cas_fault();`); err != nil {
					t.Error("fixture trigger cleanup failed")
				}
			})
			before := f16UsageCASFullSnapshot(t, ctx, r)
			var produced error
			owner := &f16UsageFailureOwner{apply: func(ctx context.Context, in UsageSettlementRepairExecutionInput) (UsageSettlementRepairResult, error) {
				result, err := r.ApplyUsageSettlementRepair(ctx, in)
				produced = err
				return result, err
			}}
			attempt := f16UsageFailureAttempt(t, ctx, plan, input, owner)
			if produced == nil || !strings.Contains(produced.Error(), "hold_cas") {
				t.Fatalf("actual post-financial-write hold CAS not reached: %v", produced)
			}
			if code == "zero-rows" {
				if !errors.Is(produced, ErrConflict) {
					t.Fatal("zero-row owner CAS lost conflict identity")
				}
			} else {
				var got *pgconn.PgError
				if !errors.As(produced, &got) || got.Code != code || errors.Is(produced, ErrConflict) {
					t.Fatalf("actual SQL error acquired definite identity: %v", produced)
				}
			}
			f16AssertUsageFailure(t, attempt, code == "zero-rows")
			if after := f16UsageCASFullSnapshot(t, ctx, r); after != before {
				t.Fatal("failed usage repair changed complete financial, hold/allocation/loss/repair or member/audit facts")
			}
			if _, err := r.UsageSettlementRepairForRequest(ctx, input.ApprovalRequestID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed CAS installed success receipt: %v", err)
			}
		})
	}
}
