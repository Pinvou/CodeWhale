package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSMSAdmissionRetryPreservesFailureBoundaries(t *testing.T) {
	serialization := &pgconn.PgError{Code: "40001", Message: "private database diagnostic"}
	permanent := &pgconn.PgError{Code: "23505", Message: "unique constraint"}
	for _, test := range []struct {
		name        string
		failures    int
		cause       error
		cancel      bool
		wantCalls   int
		unavailable bool
	}{
		{name: "success after confirmed aborts", failures: 2, cause: fmt.Errorf("admission: %w", serialization), wantCalls: 3},
		{name: "confirmed abort budget exhausted", failures: 4, cause: fmt.Errorf("admission: %w", serialization), wantCalls: 3, unavailable: true},
		{name: "permanent database rejection", failures: 4, cause: permanent, wantCalls: 1},
		{name: "unknown commit response", failures: 4, cause: io.ErrUnexpectedEOF, wantCalls: 1},
		{name: "canceled during confirmed abort", failures: 4, cause: serialization, cancel: true, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			result, err := retrySMSAdmission(ctx, func() (int, error) {
				calls++
				if test.cancel {
					cancel()
				}
				if calls <= test.failures {
					return calls, test.cause
				}
				return calls, nil
			})
			if calls != test.wantCalls || result != calls {
				t.Fatalf("calls=%d result=%d, want %d", calls, result, test.wantCalls)
			}
			if errors.Is(err, ErrSMSUnavailable) != test.unavailable {
				t.Fatalf("unavailable=%v, want %v: %v", errors.Is(err, ErrSMSUnavailable), test.unavailable, err)
			}
			if test.failures < calls {
				if err != nil {
					t.Fatalf("successful admission: %v", err)
				}
			} else if !errors.Is(err, test.cause) {
				t.Fatalf("lost original failure: %v", err)
			} else if !test.unavailable && err != test.cause {
				t.Fatalf("unconfirmed/permanent/canceled failure changed: %v", err)
			}
		})
	}
}

func TestSMSAdmissionContentionUsesSafePublicError(t *testing.T) {
	cause := &pgconn.PgError{Code: "40001", Message: "private database diagnostic"}
	_, err := retrySMSAdmission(context.Background(), func() (int, error) { return 0, cause })
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError != cause {
		t.Fatalf("database cause unavailable for internal diagnostics: %v", err)
	}
	service := &Service{}
	for name, write := range map[string]func(http.ResponseWriter, *http.Request, error){
		"registration": service.writeMobileAuthError,
		"profile":      service.writeProfileError,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, httptest.NewRequest(http.MethodPost, "/", nil), err)
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if decodeErr := json.Unmarshal(response.Body.Bytes(), &body); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if response.Code != http.StatusServiceUnavailable || body.Code != "SMS_UNAVAILABLE" || body.Message != "the SMS service is temporarily unavailable" {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), cause.Message) || strings.Contains(response.Body.String(), cause.Code) {
				t.Fatalf("database detail leaked: %s", response.Body.String())
			}
		})
	}
}
