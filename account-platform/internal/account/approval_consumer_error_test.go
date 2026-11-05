package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalConsumerSerializationCausePreservesClassification(t *testing.T) {
	t.Run("serialization cause and HTTP remain fail closed", func(t *testing.T) {
		cause := &pgconn.PgError{Code: "40001", Message: "controlled serialization conflict"}
		got := approvalConsumerError(context.Background(), cause)
		var recovered *pgconn.PgError
		if !errors.Is(got, ErrApprovalConsumerBindingUnavailable) || !errors.As(got, &recovered) || recovered != cause {
			t.Fatalf("serialization cause or unavailable classification lost: %v", got)
		}
		response := httptest.NewRecorder()
		(&Service{}).writeError(response, httptest.NewRequest(http.MethodPost, "/", nil), got)
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "APPROVAL_AUTHORIZATION_UNAVAILABLE") || strings.Contains(response.Body.String(), cause.Message) {
			t.Fatal("serialization conflict changed public classification or exposed database detail")
		}
	})
	t.Run("other infrastructure retains original sentinel", func(t *testing.T) {
		if got := approvalConsumerError(context.Background(), &pgconn.PgError{Code: "08006"}); got != ErrApprovalConsumerBindingUnavailable {
			t.Fatalf("unrelated infrastructure mapping changed: %v", got)
		}
	})
	t.Run("authorization and input retain original classification", func(t *testing.T) {
		for code, want := range map[string]error{"42501": ErrForbidden, "22023": ErrApprovalConsumerBindingInvalid, "23514": ErrApprovalConsumerBindingInvalid, "55000": ErrApprovalConsumerBindingInvalid, "P0002": ErrApprovalConsumerBindingInvalid, "P0003": ErrApprovalConsumerBindingInvalid} {
			if got := approvalConsumerError(context.Background(), &pgconn.PgError{Code: code}); got != want {
				t.Fatalf("original %s classification changed: %v", code, got)
			}
		}
	})
	t.Run("cancellation keeps precedence", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := approvalConsumerError(ctx, &pgconn.PgError{Code: "40001"}); got != context.Canceled {
			t.Fatalf("cancelled operation was relabelled: %v", got)
		}
	})
}
