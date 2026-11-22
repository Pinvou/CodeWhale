package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type auditDeadlineRecorder struct{ *httptest.ResponseRecorder }

func (w *auditDeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }
func newAuditDeadlineRecorder() *auditDeadlineRecorder {
	return &auditDeadlineRecorder{httptest.NewRecorder()}
}

type auditNetworkDeadlineSpy struct {
	http.ResponseWriter
	calls *atomic.Int32
}

func (w auditNetworkDeadlineSpy) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w auditNetworkDeadlineSpy) SetWriteDeadline(deadline time.Time) error {
	w.calls.Add(1)
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}
func accountAuditNetworkServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httpserver.NewApplicationServer(config.Config{}, handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { server.Serve(listener); close(done) }()
	t.Cleanup(func() { server.Close(); <-done })
	return "http://" + listener.Addr().String()
}

func TestAuditDownloadActualAccountRouterPreservesDeadlineCapability(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://public@127.0.0.1:1/public")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("network forbidden") }
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	owner := &AuditExportService{repository: &PostgresRepository{pool: pool}}
	// Authorization and database outage are controlled boundaries. The actual
	// registered route, Gin Adapt, common metrics/recovery and response deadline
	// capability are exercised over TCP without replacing admission logic.
	application := NewService(&auditAuthorityRepository{allowed: true}, nil).WithAuditExports(owner).Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://public.invalid", Subject: "public-actor", SessionID: "public-session"}, nil
	})
	handler := httpserver.HandlerWithApplication(config.Config{Environment: "test", ServiceName: "account-service"}, application, nil)
	var calls atomic.Int32
	origin := accountAuditNetworkServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(auditNetworkDeadlineSpy{w, &calls}, r) }))
	request, _ := http.NewRequest("GET", origin+"/admin/v1/audit-exports/00000000-0000-4000-8000-000000000001/download", nil)
	request.Header.Set("Authorization", "Bearer public-fixture")
	request.Header.Set("X-Pinvou-Download-Token", strings.Repeat("x", 43))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 || strings.Contains(string(body), "AUDIT_DOWNLOAD_UNAVAILABLE") || response.StatusCode != 500 || !strings.Contains(string(body), "INTERNAL_ERROR") {
		t.Fatalf("actual router hid transport support: calls=%d status=%d body=%s", calls.Load(), response.StatusCode, body)
	}
}

func TestAuditDownloadActualGinRecoveryAbortsWithoutAppendingJSON(t *testing.T) {
	router := httpserver.NewRouter()
	router.GET("/public-abort", httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		stream, err := httpserver.NewAuditDownloadStream(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer stream.Close()
		if err = stream.Start(200); err != nil {
			return
		}
		io.WriteString(w, "public-prefix\n")
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	}))
	handler := httpserver.HandlerWithApplication(config.Config{Environment: "test", ServiceName: "account-service"}, router, nil)
	response, err := http.Get(accountAuditNetworkServer(t, handler) + "/public-abort")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err == nil || string(body) != "public-prefix\n" {
		t.Fatalf("abort became a successful or rewritten body: %q %v", body, err)
	}
}

func TestAuditDownloadUnsupportedTransportPrecedesOwnerConsumption(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/audit-exports/00000000-0000-4000-8000-000000000001/download", nil)
	request.SetPathValue("export_id", "00000000-0000-4000-8000-000000000001")
	request.Header.Set("X-Pinvou-Download-Token", "public-synthetic-ticket-which-must-not-be-consumed")
	recorder := httptest.NewRecorder()
	// A nil concrete owner would panic if invoked. No replacement for the real
	// admission decision or export service is installed by this test.
	(&Service{}).downloadAuditExport(recorder, request)
	if recorder.Code != 503 || !strings.Contains(recorder.Body.String(), "AUDIT_DOWNLOAD_UNAVAILABLE") {
		t.Fatalf("unsupported transport reached owner or changed error: %d", recorder.Code)
	}
}
