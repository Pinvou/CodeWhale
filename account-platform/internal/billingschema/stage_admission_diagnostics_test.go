package billingschema

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestExistingBrokerAdmissionWait(t *testing.T) {
	t.Run("observes zero clients before admitting writes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			samples := 0
			facts, err := awaitBrokerAdmission(t.Context(), func(context.Context) (brokerAdmissionFacts, error) {
				samples++
				if samples == 1 {
					return brokerAdmissionFacts{Clients: 1}, nil
				}
				return brokerAdmissionFacts{}, nil
			})
			if err != nil || !facts.clean() || samples != 2 {
				t.Fatalf("did not wait for a clean sample: facts=%+v samples=%d err=%v", facts, samples, err)
			}
		})
	})
	t.Run("catalog drift on a later sample refuses admission", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			samples := 0
			want := brokerAdmissionFacts{Databases: 1}
			facts, err := awaitBrokerAdmission(t.Context(), func(context.Context) (brokerAdmissionFacts, error) {
				samples++
				if samples == 1 {
					return brokerAdmissionFacts{Clients: 1}, nil
				}
				return want, nil
			})
			if err != nil || facts != want || facts.clean() || samples != 2 {
				t.Fatalf("catalog drift was not returned on its first observation: facts=%+v samples=%d err=%v", facts, samples, err)
			}
		})
	})
	t.Run("persistent client reaches the fixed deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			_, err := awaitBrokerAdmission(t.Context(), func(context.Context) (brokerAdmissionFacts, error) {
				return brokerAdmissionFacts{Clients: 1}, nil
			})
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 15*time.Second {
				t.Fatalf("unexpected wait result: elapsed=%s err=%v", time.Since(start), err)
			}
		})
	})
	t.Run("query or identity error is not retried", func(t *testing.T) {
		failure := errors.New("fixture observation failure")
		samples := 0
		_, err := awaitBrokerAdmission(t.Context(), func(context.Context) (brokerAdmissionFacts, error) {
			samples++
			return brokerAdmissionFacts{}, failure
		})
		if !errors.Is(err, failure) || samples != 1 {
			t.Fatalf("observation failure was not final: samples=%d err=%v", samples, err)
		}
	})
	t.Run("cancelled observation cannot admit writes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, err := awaitBrokerAdmission(ctx, func(context.Context) (brokerAdmissionFacts, error) {
			cancel()
			return brokerAdmissionFacts{}, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admission accepted a cancelled sample: %v", err)
		}
	})
}

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
