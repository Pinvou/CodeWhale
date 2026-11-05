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

func f15ReconciliationCASFullSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var audit string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM manual_review_case_events e),(SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e))::text`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	return reconciliationEffectSnapshot(t, ctx, r, true) + "\n" + audit
}

// The controlled fault is in the real manual-review update after the actual case
// update. SQLSTATE injection proves typed errors and whole target rollback, not
// a real competing serialization/deadlock race or live provider/issuer delivery.
func TestF15ReconciliationCASInfrastructurePostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	for _, code := range []string{"40001", "40P01", "58000", "zero-rows"} {
		t.Run(code, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			activation := reconciliationFixtureActivation(t, ctx, r.pool)("ACTIVE")
			fixture := seedProviderPaidInternalMissing(t, ctx, r, "f15-cas-fault")
			plan, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: fixture.CaseID, PaymentEvidenceID: fixture.EvidenceID, BusinessDate: fixture.BusinessDate}, 1, 1, activation)
			if err != nil || plan.ManualReviewCaseID == nil || *plan.ManualReviewCaseID != fixture.CaseID {
				t.Fatal("original reconciliation manual target preflight", err)
			}
			input := reconciliationFrozenInput(plan, activation)
			// Inherited F13 guard permits only the original owned UUID/database/actor;
			// no permission-sensitive connection implementation is copied here.
			ownerConnection := f13RefundCASFaultOwner(t, ctx, r)
			body := "RAISE EXCEPTION USING ERRCODE = '" + code + "', MESSAGE = 'controlled reconciliation CAS fixture fault';"
			if code == "zero-rows" {
				body = "RETURN NULL;"
			}
			sql := fmt.Sprintf(`CREATE FUNCTION public.f15_reconciliation_cas_fault() RETURNS trigger LANGUAGE plpgsql AS $fixture$ BEGIN %s END; $fixture$; CREATE TRIGGER f15_reconciliation_cas_fault BEFORE UPDATE ON public.manual_review_cases FOR EACH ROW EXECUTE FUNCTION public.f15_reconciliation_cas_fault();`, body)
			if _, err := ownerConnection.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ownerConnection.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS f15_reconciliation_cas_fault ON public.manual_review_cases; DROP FUNCTION IF EXISTS public.f15_reconciliation_cas_fault();`); err != nil {
					t.Error("fixture trigger cleanup failed")
				}
			})
			before := f15ReconciliationCASFullSnapshot(t, ctx, r)
			var produced error
			owner := &f15ReconciliationFailureOwner{apply: func(ctx context.Context, in ReconciliationFinancialRepairExecutionInput) (ReconciliationFinancialRepairResult, error) {
				result, err := r.ApplyReconciliationFinancialRepair(ctx, in)
				produced = err
				return result, err
			}}
			attempt := f15ReconciliationFailureAttempt(t, ctx, plan, input, owner)
			if produced == nil || !strings.Contains(produced.Error(), "manual_review_cas") {
				t.Fatalf("actual second target CAS path not reached: %v", produced)
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
			f15AssertReconciliationFailure(t, attempt, code == "zero-rows")
			if after := f15ReconciliationCASFullSnapshot(t, ctx, r); after != before {
				t.Fatal("failed closure changed case/manual/repair/audit or fourteen complete financial tables")
			}
			if _, err := r.ReconciliationFinancialRepairForRequest(ctx, input.ApprovalRequestID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed CAS installed success receipt: %v", err)
			}
		})
	}
}
