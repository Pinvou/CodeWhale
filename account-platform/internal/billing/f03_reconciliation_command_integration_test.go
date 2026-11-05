package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Original owned UUID database/actor fixture, public handlers and repository.
// The provider and SQLSTATE trigger are controlled; not live vendor acceptance.
type f03BudgetProvider struct {
	trackedBillProvider
	observe bool
	wait    bool
	seen    context.Context
}

func (p *f03BudgetProvider) Bill(ctx context.Context, channel string, date time.Time) ([]ProviderBillEntry, error) {
	if p.observe {
		p.seen = ctx
		if p.wait {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return p.trackedBillProvider.Bill(ctx, channel, date)
}

type f03BudgetRepository struct {
	Repository
	r     *PostgresRepository
	seen  context.Context
	calls int
}

func (r *f03BudgetRepository) UpdateReconciliationCase(ctx context.Context, id uuid.UUID, command ReconciliationCaseCommand) (AdminReconciliationCase, error) {
	r.seen, r.calls = ctx, r.calls+1
	return r.r.UpdateReconciliationCase(ctx, id, command)
}

func f03CommandCase(t *testing.T) (*PostgresRepository, context.Context, *PaymentCoordinator, *f03BudgetProvider, uuid.UUID, uuid.UUID) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	provider := &f03BudgetProvider{trackedBillProvider: trackedBillProvider{reconciliationFixtureProvider: reconciliationFixtureProvider{rows: []ProviderBillEntry{pipelineRefundRow(order, r.now())}}}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	run, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now().In(time.FixedZone("Asia/Shanghai", 8*60*60)))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := r.AdminReconciliationCases(ctx, run.ID, 10)
	if err != nil || len(cases) != 1 {
		t.Fatalf("controlled original case missing: %v %v", cases, err)
	}
	id := cases[0].ID
	if _, err := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled original case claim"}); err != nil {
		t.Fatal(err)
	}
	return r, ctx, c, provider, id, actor
}

func f03RetryFault(t *testing.T, r *PostgresRepository, ctx context.Context, code string, failures int) {
	t.Helper()
	if code != "40001" && code != "40P01" && code != "23505" {
		t.Fatal("unexpected controlled SQLSTATE")
	}
	// Sequence increments are deliberately nontransactional: aborted attempts
	// remain observable without committing any business mutation or receipt.
	statement := fmt.Sprintf(`CREATE SEQUENCE f03_case_attempt;
GRANT USAGE,SELECT ON SEQUENCE f03_case_attempt TO billing_service;
CREATE FUNCTION f03_case_retry_fault() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status <> 'RESOLVED' AND NEW.repair_action IS NOT DISTINCT FROM OLD.repair_action THEN RETURN NEW; END IF;
 IF nextval('f03_case_attempt') <= %d THEN RAISE EXCEPTION 'controlled aborted case transaction' USING ERRCODE='%s'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER f03_case_retry_fault BEFORE UPDATE ON reconciliation_cases FOR EACH ROW EXECUTE FUNCTION f03_case_retry_fault()`, failures, code)
	if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
}

func TestF03PublicCaseCommandRetriesDefiniteDatabaseAbort(t *testing.T) {
	for _, code := range []string{"40001", "40P01", "23505", "exhausted"} {
		t.Run(code, func(t *testing.T) {
			r, ctx, _, provider, id, actor := f03CommandCase(t)
			faultCode, failures := code, 2
			if code == "exhausted" {
				faultCode, failures = "40001", 10
			}
			f03RetryFault(t, r, ctx, faultCode, failures)
			command := ReconciliationCaseCommand{Action: "SET_REPAIR_ACTION", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled immutable case operation", RepairAction: "controlled manual follow-up"}
			beforeCalls := provider.bills
			item, err := r.UpdateReconciliationCase(ctx, id, command)
			var attempts, receipts int
			if queryErr := r.pool.QueryRow(ctx, `SELECT last_value FROM f03_case_attempt`).Scan(&attempts); queryErr != nil {
				t.Fatal(queryErr)
			}
			if queryErr := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_case_commands WHERE reconciliation_case_id=$1 AND idempotency_key=$2`, id, command.IdempotencyKey).Scan(&receipts); queryErr != nil {
				t.Fatal(queryErr)
			}
			if provider.bills != beforeCalls {
				t.Fatal("pure database retry invoked supplier")
			}
			if code == "40001" || code == "40P01" {
				if err != nil || attempts != 3 || receipts != 1 || item.ID != id || item.RepairAction == nil || *item.RepairAction != command.RepairAction {
					t.Fatalf("aborted transaction was not retried with original command: %v attempts=%d receipts=%d item=%v", err, attempts, receipts, item)
				}
				replay, replayErr := r.UpdateReconciliationCase(ctx, id, command)
				if replayErr != nil || replay.ID != item.ID || replay.Version != item.Version {
					t.Fatal("same-key immutable receipt changed", replayErr)
				}
				return
			}
			var pgerr *pgconn.PgError
			wantAttempts := 1
			if code == "exhausted" {
				wantAttempts = 4
			}
			if !errors.As(err, &pgerr) || pgerr.Code != faultCode || attempts != wantAttempts || receipts != 0 {
				t.Fatalf("nonretryable/bounded rollback changed: %v attempts=%d receipts=%d", err, attempts, receipts)
			}
		})
	}
}

func TestF03ResolveSharesBudgetAndPreservesReplay(t *testing.T) {
	t.Run("25s_validation_and_write_then_original_receipt", func(t *testing.T) {
		r, ctx, c, provider, id, actor := f03CommandCase(t)
		provider.rows, provider.observe = nil, true
		f03RetryFault(t, r, ctx, "40001", 2)
		repository := &f03BudgetRepository{Repository: r, r: r}
		s := NewService(repository).WithPayments(c)
		key := uuid.NewString()
		call := func() *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-cases/"+id.String()+"/commands", strings.NewReader(`{"action":"RESOLVE","review_result":"controlled verified absent difference","reason":"controlled verified original resolution"}`)).WithContext(ctx)
			request.SetPathValue("case_id", id.String())
			request.Header.Set("Authorization", "Bearer controlled-human")
			request.Header.Set("Idempotency-Key", key)
			out := httptest.NewRecorder()
			identity.Middleware(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
			}, http.HandlerFunc(s.adminUpdateReconciliationCase)).ServeHTTP(out, request)
			return out
		}
		started, beforeCalls := time.Now(), provider.bills
		out := call()
		var item AdminReconciliationCase
		if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &item) != nil || item.ID != id || item.Status != "RESOLVED" {
			t.Fatalf("real resolve command failed: %d %s", out.Code, out.Body.String())
		}
		deadline, ok := provider.seen.Deadline()
		if !ok || deadline.Before(started.Add(24*time.Second)) || deadline.After(started.Add(25*time.Second+time.Second)) || provider.seen != repository.seen || provider.bills != beforeCalls+1 || repository.calls != 1 {
			t.Fatal("supplier verification and retrying database write did not share original25s budget exactly once")
		}
		provider.failBill = true
		beforeCalls = provider.bills
		replayed := call()
		var receipt AdminReconciliationCase
		if replayed.Code != http.StatusOK || json.Unmarshal(replayed.Body.Bytes(), &receipt) != nil || receipt.ID != item.ID || receipt.Version != item.Version || provider.bills != beforeCalls {
			t.Fatal("saved receipt revalidation reached supplier or changed original result")
		}
	})
	t.Run("short_parent_cancels_before_write", func(t *testing.T) {
		r, ctx, c, provider, id, actor := f03CommandCase(t)
		provider.observe, provider.wait = true, true
		repository := &f03BudgetRepository{Repository: r, r: r}
		s := NewService(repository).WithPayments(c)
		parent, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "/controlled", strings.NewReader(`{"action":"RESOLVE","review_result":"controlled review","reason":"controlled short parent budget"}`)).WithContext(parent)
		request.SetPathValue("case_id", id.String())
		request.Header.Set("Authorization", "Bearer controlled-human")
		request.Header.Set("Idempotency-Key", uuid.NewString())
		out := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
		}, http.HandlerFunc(s.adminUpdateReconciliationCase)).ServeHTTP(out, request)
		if out.Code == http.StatusOK || provider.seen == nil || !errors.Is(provider.seen.Err(), context.DeadlineExceeded) || repository.calls != 0 {
			t.Fatal("short caller deadline did not cancel provider before any command write")
		}
	})
}
