package account

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Only the F14 owned test opts into the existing same-package cause. Public
// readiness and the shared fixture's default constructor stay redacted.
func f14DiagnosticRelayConstructor(t *testing.T) func(context.Context, string) (*ApprovalRelayStore, error) {
	t.Helper()
	return func(ctx context.Context, dsn string) (*ApprovalRelayStore, error) {
		store, err := newApprovalRelayStore(ctx, dsn)
		if err != nil {
			t.Fatal(f14RelayFailureClassification(err))
		}
		return store, err
	}
}

func f14RelayFailureClassification(err error) string {
	stage, class, sqlstate := "pool-configuration", "unclassified", "NONE"
	var failure *approvalRelayReadinessError
	if errors.As(err, &failure) {
		switch failure.stage {
		case "acquire", "identity-query", "identity-contract", "sidecar-query":
			stage = failure.stage
		default:
			stage = "unclassified"
		}
	}
	var databaseError *pgconn.PgError
	switch {
	case errors.As(err, &databaseError):
		class = "postgres"
		if len(databaseError.Code) == 5 {
			valid := true
			for _, c := range databaseError.Code {
				valid = valid && (c >= '0' && c <= '9' || c >= 'A' && c <= 'Z')
			}
			if valid {
				sqlstate = databaseError.Code
			}
		}
	case errors.Is(err, context.DeadlineExceeded):
		class = "deadline"
	case errors.Is(err, context.Canceled):
		class = "cancelled"
	case errors.Is(err, errApprovalRelayReadinessContract):
		class = "identity-contract"
	case errors.Is(err, ErrApprovalWriterUnavailable):
		class = "configuration-contract"
	}
	return fmt.Sprintf("genuine relay readiness failed: stage=%s class=%s sqlstate=%s", stage, class, sqlstate)
}

func TestF14RelayDiagnosticClosedClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", &approvalRelayReadinessError{stage: "acquire", cause: context.DeadlineExceeded}, "stage=acquire class=deadline sqlstate=NONE"},
		{"cancelled", &approvalRelayReadinessError{stage: "identity-query", cause: context.Canceled}, "stage=identity-query class=cancelled sqlstate=NONE"},
		{"contract", &approvalRelayReadinessError{stage: "identity-contract", cause: errApprovalRelayReadinessContract}, "stage=identity-contract class=identity-contract sqlstate=NONE"},
		{"postgres-redacted", &approvalRelayReadinessError{stage: "sidecar-query", cause: &pgconn.PgError{Code: "42501", Message: "private-message", Detail: "private-detail"}}, "stage=sidecar-query class=postgres sqlstate=42501"},
		{"invalid-metadata", &approvalRelayReadinessError{stage: "private-stage", cause: &pgconn.PgError{Code: "secret\nvalue", Message: "private-message"}}, "stage=unclassified class=postgres sqlstate=NONE"},
		{"unknown-redacted", errors.New("private-error"), "stage=pool-configuration class=unclassified sqlstate=NONE"},
		{"configuration", ErrApprovalWriterUnavailable, "stage=pool-configuration class=configuration-contract sqlstate=NONE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := f14RelayFailureClassification(test.err)
			if got != "genuine relay readiness failed: "+test.want {
				t.Fatalf("unexpected closed classification: %s", got)
			}
		})
	}
}
