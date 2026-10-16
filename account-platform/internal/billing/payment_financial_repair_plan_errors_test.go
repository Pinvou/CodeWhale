package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type f12PlanControlledRow struct {
	t      *testing.T
	values []any
	err    error
}

func (row f12PlanControlledRow) Scan(destination ...any) error {
	row.t.Helper()
	if row.err != nil {
		return row.err
	}
	if len(destination) != len(row.values) {
		row.t.Fatalf("production row projection fields=%d expected=%d", len(destination), len(row.values))
	}
	for index, value := range row.values {
		target := reflect.ValueOf(destination[index])
		if target.Kind() != reflect.Pointer || target.IsNil() || !reflect.ValueOf(value).Type().AssignableTo(target.Elem().Type()) {
			row.t.Fatal("controlled source cannot supply the actual typed projection")
		}
		target.Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

type f12PlanReviewReadControl struct {
	t                 *testing.T
	input             PaymentFinancialRepairCommandInput
	wallet, requester uuid.UUID
	now               time.Time
	product, evidence []byte
	reviewCount       int64
	reviewError       error
	queries           int
}

func (control *f12PlanReviewReadControl) QueryRow(_ context.Context, statement string, arguments ...any) pgx.Row {
	control.t.Helper()
	control.queries++
	var values []any
	switch {
	case strings.Contains(statement, "FROM payment_orders WHERE id=$1"):
		if !reflect.DeepEqual(arguments, []any{control.input.PaymentOrderID}) {
			control.t.Fatal("order identity changed")
		}
		values = []any{"controlled-order", control.wallet, "F12_READ_ERROR", "ALIPAY", int64(100), "CNY", int64(100), "MANUAL_REVIEW", "MANUAL_REVIEW", "controlled-sku", "controlled-provider", "v1", control.now, int64(1), control.product, control.requester, ""}
	case strings.Contains(statement, "FROM payment_evidence WHERE id=$1"):
		if !reflect.DeepEqual(arguments, []any{control.input.PaymentEvidenceID}) {
			control.t.Fatal("persisted evidence identity changed")
		}
		amount, paidAt := int64(100), control.now
		values = []any{control.input.PaymentOrderID, "QUERY", "controlled-provider", "v1", "controlled-order", "controlled-transaction", &amount, "CNY", "PAY_SUCCESS", &paidAt, strings.Repeat("a", 64), "RECORDED", control.evidence, "controlled-evidence-key", control.now}
	case strings.Contains(statement, "EXISTS(SELECT 1 FROM payment_transactions"):
		if !reflect.DeepEqual(arguments, []any{control.input.PaymentOrderID, "controlled-transaction"}) {
			control.t.Fatal("absence facts changed order/evidence tuple")
		}
		values = []any{false, false, false, false, false, control.reviewCount, control.reviewCount}
	case strings.HasPrefix(statement, "SELECT id FROM payment_transactions"):
		if control.queries != 4 || !reflect.DeepEqual(arguments, []any{control.input.PaymentOrderID, "controlled-transaction"}) {
			control.t.Fatal("not the actual post-absence review transaction read")
		}
		return f12PlanControlledRow{t: control.t, err: control.reviewError}
	default:
		control.t.Fatal("production went past the failing review transaction read")
	}
	return f12PlanControlledRow{t: control.t, values: values}
}

func (control *f12PlanReviewReadControl) Query(context.Context, string, ...any) (pgx.Rows, error) {
	control.t.Fatal("no wallet/receivable projection may follow the failed review transaction read")
	return nil, errors.New("unexpected later read")
}

// Drive the original production plan through valid persisted-fact projections,
// inject a failure at its actual fourth QueryRow, then use the production domain
// classifier and proof path. No provider, SQL, or financial writes are executed.
func TestF12PaymentRepairReviewReadErrorsKeepDomainUnknown(t *testing.T) {
	for _, outcome := range []struct {
		name     string
		err      error
		count    int64
		definite bool
	}{
		{"serialization", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}), 1, false},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, 1, false},
		{"storage", &pgconn.PgError{Code: "08006"}, 1, false},
		{"canceled", context.Canceled, 1, false},
		{"deadline", context.DeadlineExceeded, 1, false},
		{"missing-exact-row", pgx.ErrNoRows, 1, true},
		{"wrapped-missing-exact-row", fmt.Errorf("wrapped: %w", pgx.ErrNoRows), 1, true},
		{"zero-review-count", nil, 0, true},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			control := &f12PlanReviewReadControl{t: t, input: PaymentFinancialRepairCommandInput{PaymentOrderID: uuid.New(), PaymentEvidenceID: uuid.New()}, wallet: uuid.New(), requester: uuid.New(), now: time.Now().UTC(), reviewCount: outcome.count, reviewError: outcome.err}
			var err error
			control.product, err = json.Marshal(Product{ProductID: "F12_READ_ERROR", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY"})
			if err != nil {
				t.Fatal(err)
			}
			control.evidence, err = json.Marshal(map[string]any{"external_customer_key": control.requester.String(), "external_sku": "controlled-sku"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := paymentFinancialRepairPlanFrom(context.Background(), control, control.input, 1, 1, 1)
			wantQueries := 4
			if outcome.count == 0 {
				wantQueries = 3
			}
			if control.queries != wantQueries || !reflect.DeepEqual(plan, PaymentFinancialRepairPlan{}) {
				t.Fatalf("actual producer queries=%d want=%d plan=%+v", control.queries, wantQueries, plan)
			}
			if outcome.definite {
				if !errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Fatal("confirmed missing business fact lost its rejection", err)
				}
			} else if err != outcome.err || errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatal("actual producer flattened typed infrastructure failure", err)
			}
			attempt := classifyAuthorizationDomainFailure(err)
			if attempt.FailureIsDefinite != outcome.definite || len(attempt.Result.DomainResult) != 0 || (outcome.definite && attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED") || (!outcome.definite && attempt.StableFailureCode != "") {
				t.Fatalf("actual delivery error classification=%+v", attempt)
			}
			frozen := approvalencoding.LegacyProofInput{CommandType: paymentFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_ORDER", TargetResourceID: control.input.PaymentOrderID.String(), NormalizedCommand: marshalRaw(map[string]any{"payment_order_id": control.input.PaymentOrderID, "payment_evidence_id": control.input.PaymentEvidenceID, "reason_code": paymentFinancialRepairReason})}
			owner := &paymentFactsStub{err: err}
			_, reason, _, proofErr := currentBillingProofInput(context.Background(), owner, frozen)
			if outcome.definite {
				if proofErr != nil || reason != "DOMAIN_INVARIANT_FAILED" {
					t.Fatal("actual proof did not reject the confirmed missing fact", reason, proofErr)
				}
			} else if proofErr != outcome.err || reason != "" {
				t.Fatal("actual proof manufactured a denial for unavailable storage", reason, proofErr)
			}
		})
	}
}
