package billingschema

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestBrokerAdmissionDiagnosticRedaction(t *testing.T) {
	const private = "private-diagnostic-sentinel-never-output"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"ordinary-error", errors.New(private), "class=other sqlstate=unavailable"},
		{"wrapped-postgres", fmt.Errorf("%s: %w", private, &pgconn.PgError{Code: "42501", Message: private, Detail: private, Hint: private, SchemaName: private, TableName: private}), "class=postgres sqlstate=42501"},
		{"valid-state-letters", &pgconn.PgError{Code: "42P01", Message: private}, "class=postgres sqlstate=42P01"},
		{"canceled", errors.Join(errors.New(private), context.Canceled), "class=canceled sqlstate=unavailable"},
		{"deadline", fmt.Errorf("%s: %w", private, context.DeadlineExceeded), "class=deadline sqlstate=unavailable"},
	}
	for i, code := range []string{"", "4250", "425001", "42p01", "4250\n", "四2501", private} {
		tests = append(tests, struct {
			name string
			err  error
			want string
		}{fmt.Sprintf("invalid-state-%d", i), &pgconn.PgError{Code: code, Message: private, Detail: private}, "class=postgres sqlstate=unavailable"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := brokerAdmissionErrorDiagnostic(test.err)
			if strings.Contains(got, private) {
				t.Fatal("private sentinel escaped admission diagnostic")
			}
			if got != "broker fixture admission query failed: "+test.want {
				t.Fatal("admission error was not reduced to its fixed class and safe SQLSTATE")
			}
		})
	}
}
