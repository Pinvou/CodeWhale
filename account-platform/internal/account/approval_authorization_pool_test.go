package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApprovalWriterConfigurationFailsClosed(t *testing.T) {
	for _, dsn := range []string{"", "  ", "postgres://%invalid", "postgres://account_service:private-credential@127.0.0.1/account", "postgres://account_approval_backfill@127.0.0.1/account"} {
		repository := &PostgresRepository{}
		if err := repository.ConfigureApprovalWriter(context.Background(), dsn); err != ErrApprovalWriterUnavailable {
			t.Fatalf("configuration did not fail with redacted stable error: %v", err)
		}
		if repository.approvalWriterPool != nil {
			t.Fatal("invalid configuration retained a writer pool")
		}
		if err := repository.ApprovalWriterReady(context.Background()); err != ErrApprovalWriterUnavailable {
			t.Fatal("missing writer did not fail closed")
		}
		repository.Close()
	}
}

func TestApprovalWriterUnavailableHTTPError(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/", nil), fmt.Errorf("private underlying context: %w", ErrApprovalWriterUnavailable))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"APPROVAL_WRITER_UNAVAILABLE"`) || strings.Contains(recorder.Body.String(), "private") {
		t.Fatalf("unexpected or unredacted writer error: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "5" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing failure headers: %v", recorder.Header())
	}
}

func TestApprovalWriterUnavailablePoolLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repository := &PostgresRepository{}
	if err := repository.ConfigureApprovalWriter(ctx, "postgres://account_approval_writer@127.0.0.1:1/isolated?sslmode=disable"); err != ErrApprovalWriterUnavailable {
		t.Fatalf("cancelled startup should remain unavailable: %v", err)
	}
	pool := repository.approvalWriterPool
	if pool == nil {
		t.Fatal("valid configuration did not retain a recoverable independent pool")
	}
	if err := repository.ConfigureApprovalWriter(ctx, "postgres://account_approval_writer@127.0.0.1:1/other?sslmode=disable"); err != ErrApprovalWriterUnavailable || repository.approvalWriterPool != pool {
		t.Fatal("startup-only configuration replaced or leaked the first pool")
	}
	repository.Close()
	if connection, err := pool.Acquire(context.Background()); err == nil {
		connection.Release()
		t.Fatal("repository Close did not close its dedicated writer pool")
	}
}

func TestApprovalBackfillCredentialIsSeparate(t *testing.T) {
	for _, dsn := range []string{"", "postgres://account_service@127.0.0.1/account", "postgres://account_approval_writer@127.0.0.1/account"} {
		pool, err := NewApprovalBackfillPool(context.Background(), dsn)
		if pool != nil || !errors.Is(err, ErrApprovalBackfillUnavailable) {
			t.Fatalf("backfill accepted another process identity: %v", err)
		}
	}
}

func TestApprovalWriterReadinessDoesNotBlockOtherRoutes(t *testing.T) {
	repository := &PostgresRepository{}
	application := repository.WithApprovalWriterReadiness(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/health/ready/approval-writer", http.StatusServiceUnavailable},
		{http.MethodPost, "/health/ready/approval-writer", http.StatusMethodNotAllowed},
		{http.MethodGet, "/account/admin/v1/approval-requests", http.StatusAccepted},
		{http.MethodPost, "/account/admin/v1/approval-requests/id/actions/withdraw", http.StatusAccepted},
	} {
		recorder := httptest.NewRecorder()
		application.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != test.status {
			t.Fatalf("%s %s returned %d", test.method, test.path, recorder.Code)
		}
		if test.status == http.StatusServiceUnavailable && (recorder.Body.String() != `{"status":"not_ready"}` || recorder.Header().Get("Cache-Control") != "no-store") {
			t.Fatal("readiness exposed details or omitted no-store")
		}
	}
}
