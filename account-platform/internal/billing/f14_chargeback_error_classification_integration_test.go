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

// Controlled SQLSTATE injection exercises the real Apply after its first case
// update and financial writes. It proves error propagation and transaction
// rollback; it does not claim an actual competing serialization/deadlock race.
func TestF14ChargebackCASInfrastructurePostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	for _, code := range []string{"40001", "40P01", "58000", "zero-rows"} {
		t.Run(code, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			activation := chargebackFixtureActivation(t, ctx, r.pool)("ACTIVE")
			fixture := seedChargebackShortfall(t, ctx, r, false)
			plan, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, ChargebackFinancialRepairCommandInput{ChargebackCaseID: fixture.ChargebackCaseID, PaymentEvidenceID: fixture.EvidenceID}, 1, 1, activation)
			if err != nil {
				t.Fatal("original chargeback preflight", err)
			}
			input := chargebackFrozenInput(plan, activation)
			// Reuse F13's exact owned UUID/database/schema-owner guard after adjacent
			// propagation rather than copying another permission-sensitive connection.
			ownerConnection := f13RefundCASFaultOwner(t, ctx, r)
			body := "RAISE EXCEPTION USING ERRCODE = '" + code + "', MESSAGE = 'controlled chargeback CAS fixture fault';"
			if code == "zero-rows" {
				body = "RETURN NULL;"
			}
			sql := fmt.Sprintf(`CREATE FUNCTION public.f14_chargeback_cas_fault() RETURNS trigger LANGUAGE plpgsql AS $fixture$ BEGIN %s END; $fixture$; CREATE TRIGGER f14_chargeback_cas_fault BEFORE UPDATE OF receivable_id ON public.chargeback_cases FOR EACH ROW EXECUTE FUNCTION public.f14_chargeback_cas_fault();`, body)
			if _, err := ownerConnection.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ownerConnection.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS f14_chargeback_cas_fault ON public.chargeback_cases; DROP FUNCTION IF EXISTS public.f14_chargeback_cas_fault();`); err != nil {
					t.Error("fixture trigger cleanup failed")
				}
			})
			before := chargebackEffectSnapshot(t, ctx, r)
			var produced error
			owner := &f14ChargebackFailureOwner{apply: func(ctx context.Context, in ChargebackFinancialRepairExecutionInput) (ChargebackFinancialRepairResult, error) {
				result, err := r.ApplyChargebackFinancialRepair(ctx, in)
				produced = err
				return result, err
			}}
			attempt := f14ChargebackFailureAttempt(t, ctx, plan, input, owner)
			if produced == nil || !strings.Contains(produced.Error(), "chargeback_case_receivable") {
				t.Fatalf("actual post-financial-write CAS path not reached: %v", produced)
			}
			if code == "zero-rows" {
				if !errors.Is(produced, ErrConflict) {
					t.Fatal("zero-row CAS lost definite conflict identity")
				}
			} else {
				var got *pgconn.PgError
				if !errors.As(produced, &got) || got.Code != code || errors.Is(produced, ErrConflict) {
					t.Fatalf("actual SQL error acquired conflict identity: %v", produced)
				}
			}
			f14AssertChargebackFailure(t, attempt, code == "zero-rows")
			if after := chargebackEffectSnapshot(t, ctx, r); after != before {
				t.Fatal("failed repair changed financial facts, case/order projection, journal, audit or outbox")
			}
			if _, err := r.ChargebackFinancialRepairForRequest(ctx, input.ApprovalRequestID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed CAS installed success receipt: %v", err)
			}
		})
	}
}
