package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Only a fixture-owned UUID database may install this fault trigger. It runs as
// its original schema owner; the actual repair still uses billing_service.
func f13RefundCASFaultOwner(t *testing.T, ctx context.Context, r *PostgresRepository) *pgx.Conn {
	t.Helper()
	config := r.pool.Config().ConnConfig.Copy()
	suffix := strings.TrimPrefix(config.Database, "retained_finance_")
	if suffix == config.Database {
		t.Fatal("refund fault fixture requires owned database")
	}
	id, err := uuid.Parse(suffix)
	if err != nil || id == uuid.Nil || strings.ReplaceAll(id.String(), "-", "") != suffix {
		t.Fatal("refund fault fixture requires exact UUID database")
	}
	config.User, config.Password = "billing_migrator", ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot connect fixture owner")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	var database, actor, owner string
	err = connection.QueryRow(ctx, `SELECT current_database(),session_user,pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()`).Scan(&database, &actor, &owner)
	if err != nil || database != config.Database || actor != "billing_migrator" || owner != "billing_schema_owner" {
		t.Fatal("refund fault fixture database/actor/owner guard failed")
	}
	if _, err = connection.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	return connection
}
func f13RefundCASEffectSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var extra string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM payment_orders o),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM credit_lots c),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM entitlement_packages p),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p))::text`).Scan(&extra)
	if err != nil {
		t.Fatal(err)
	}
	return refundEffectSnapshot(t, ctx, r) + "\n" + extra
}

// SQLSTATE injection uses actual PostgreSQL errors and the actual Apply owner;
// it proves error propagation and rollback, not a real competing deadlock race.
func TestF13RefundExceptionCASInfrastructurePostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("disposable Billing PG not configured")
	}
	for _, code := range []string{"40001", "40P01", "58000", "zero-rows"} {
		t.Run(code, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			activation := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
			fixture := seedRefundException(t, ctx, r, false)
			_, input := f13RefundRepairInput(t, ctx, r, fixture, activation)
			// Match the real public proof projection's canonical UTC time. PostgreSQL
			// values may carry a local zone while naming the same frozen instant.
			input.Frozen.RefundUpdatedAt = input.Frozen.RefundUpdatedAt.UTC()
			ownerConnection := f13RefundCASFaultOwner(t, ctx, r)
			body := "RAISE EXCEPTION USING ERRCODE = '" + code + "', MESSAGE = 'controlled refund CAS fixture fault';"
			if code == "zero-rows" {
				body = "RETURN NULL;"
			}
			// The code list and identifiers are closed fixture constants, never inputs.
			sql := fmt.Sprintf(`CREATE FUNCTION public.f13_refund_cas_fault() RETURNS trigger LANGUAGE plpgsql AS $fixture$ BEGIN %s END; $fixture$; CREATE TRIGGER f13_refund_cas_fault BEFORE UPDATE ON public.credit_lots FOR EACH ROW EXECUTE FUNCTION public.f13_refund_cas_fault();`, body)
			if _, err := ownerConnection.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ownerConnection.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS f13_refund_cas_fault ON public.credit_lots; DROP FUNCTION IF EXISTS public.f13_refund_cas_fault();`); err != nil {
					t.Error("fixture trigger cleanup failed")
				}
			})
			before := f13RefundCASEffectSnapshot(t, ctx, r)
			var produced error
			owner := &f13RefundFailureOwner{apply: func(ctx context.Context, in RefundExceptionRepairExecutionInput) (RefundExceptionRepairResult, error) {
				result, err := r.ApplyRefundExceptionRepair(ctx, in)
				produced = err
				return result, err
			}}
			binding, event, command := f13RefundFailureAuthorization(t, input)
			attempt := f13RefundFailureDomain(owner, activation).ExecuteAuthorizationAttempt(ctx, binding, event, command)
			if owner.calls != 1 || !reflect.DeepEqual(owner.input, input) || produced == nil || !strings.Contains(produced.Error(), "credit_lot_cas") {
				t.Fatalf("actual credit CAS error path not reached: calls=%d definite=%t code=%s err=%v", owner.calls, attempt.FailureIsDefinite, attempt.StableFailureCode, produced)
			}
			if code == "zero-rows" {
				if !errors.Is(produced, ErrConflict) {
					t.Fatal("zero-row CAS conflict identity lost")
				}
			} else {
				var pgError *pgconn.PgError
				if !errors.As(produced, &pgError) || pgError.Code != code || errors.Is(produced, ErrConflict) {
					t.Fatalf("SQL error changed identity: %v", produced)
				}
			}
			f13AssertRefundFailureAttempt(t, attempt, code == "zero-rows")
			if after := f13RefundCASEffectSnapshot(t, ctx, r); after != before {
				t.Fatal("failed repair changed frozen financial facts, order projection, postings, audit or outbox")
			}
			if _, err := r.RefundExceptionRepairForRequest(ctx, input.ApprovalRequestID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed CAS installed a success receipt: %v", err)
			}
		})
	}
}
