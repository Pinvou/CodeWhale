package portal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type auditDeadlineRecorder struct{ *httptest.ResponseRecorder }

func (w *auditDeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }
func newAuditDeadlineRecorder() *auditDeadlineRecorder {
	return &auditDeadlineRecorder{httptest.NewRecorder()}
}

type auditUnreadBody struct{}

func (auditUnreadBody) Read([]byte) (int, error) { panic("unsupported transport read a request body") }
func (auditUnreadBody) Close() error             { return nil }

func TestAuditDownloadPortalUnsupportedBeforeReadingOrForwarding(t *testing.T) {
	service := testService(t, newMemoryStore())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	request := httptest.NewRequest("GET", "/", nil)
	request.Body = auditUnreadBody{}
	recorder := httptest.NewRecorder()
	service.forward(recorder, request, service.config.AccountURL, "/admin/v1/audit-exports/"+uuid.NewString()+"/download", Session{}, true)
	if recorder.Code != 503 || calls.Load() != 0 {
		t.Fatalf("status=%d owner calls=%d", recorder.Code, calls.Load())
	}
}

func startAuditApplicationServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httpserver.NewApplicationServer(config.Config{}, handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = server.Serve(listener); close(done) }()
	t.Cleanup(func() { server.Close(); <-done })
	return "http://" + listener.Addr().String()
}

type auditPeriodicReader struct {
	remaining int
	delay     time.Duration
	stop      chan struct{}
	once      sync.Once
}

func (r *auditPeriodicReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.stop:
		return 0, context.Canceled
	}
	r.remaining--
	copy(buffer, "public-progress\n")
	return len("public-progress\n"), nil
}
func (r *auditPeriodicReader) Close() error { r.once.Do(func() { close(r.stop) }); return nil }

// This intentionally crosses both historic total budgets with actual TCP,
// production server constructors and the installed Portal forwarding branch.
// The owner supplies controlled post-consumption bytes using the same stream
// primitive as Account; it does not simulate PostgreSQL consumption or IdP.
func TestAuditDownloadProgressAcrossBothApplicationServers(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := startAuditApplicationServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		stream, err := httpserver.NewAuditDownloadStream(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer stream.Close()
		w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
		if err = stream.Start(200); err != nil {
			return
		}
		if err = stream.CopyFrom(&auditPeriodicReader{remaining: 30, delay: 1050 * time.Millisecond, stop: make(chan struct{})}); err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	service.config.AccountURL = parseTestURL(t, upstream)
	portal := startAuditApplicationServer(t, service.Handler(nil))
	id := uuid.NewString()
	request := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
	request.URL = parseTestURL(t, portal+request.URL.String())
	request.RequestURI = ""
	request.Header.Set("X-Pinvou-Download-Token", "public-one-use-proof")
	start := time.Now()
	response, err := (&http.Client{Timeout: 45 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	expected := bytes.Repeat([]byte("public-progress\n"), 30)
	if err != nil || response.StatusCode != 200 || !bytes.Equal(payload, expected) {
		t.Fatalf("stream truncated status=%d bytes=%d err=%v", response.StatusCode, len(payload), err)
	}
	if time.Since(start) < 31*time.Second || calls.Load() != 1 {
		t.Fatalf("elapsed=%v calls=%d", time.Since(start), calls.Load())
	}
	if sha256.Sum256(payload) != sha256.Sum256(expected) {
		t.Fatal("payload digest drift")
	}
	if service.client.Timeout != 15*time.Second || service.auditClient.Timeout != 0 {
		t.Fatal("client policies mixed")
	}
}

func TestAuditDownloadDisconnectStopsOwnerAndDoesNotReplay(t *testing.T) {
	ownerStopped := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		defer close(ownerStopped)
		w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	portal := startAuditApplicationServer(t, service.Handler(nil))
	id := uuid.NewString()
	request := financeBrowserRequest(service, session, "GET", "/api/admin/account/admin/v1/audit-exports/"+id+"/download", "")
	request.URL = parseTestURL(t, portal+request.URL.String())
	request.RequestURI = ""
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-ownerStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected browser left upstream download running")
	}
	if calls.Load() != 1 {
		t.Fatal("one-use download was replayed")
	}
}

func TestAuditDownloadClientRetainsOrdinaryPolicyAndRedirectProtection(t *testing.T) {
	ordinary, err := newPortalHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	download, err := newPortalAuditHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Timeout != 15*time.Second || download.Timeout != 0 {
		t.Fatal("ordinary request policy changed")
	}
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, "public") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	response, err := download.Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 302 || targetCalls.Load() != 0 {
		t.Fatal("redirect exposed a one-use authority")
	}
	if _, err = newPortalAuditHTTPClient("relative-ca.pem"); err == nil {
		t.Fatal("private CA path validation bypassed")
	}
}

func TestAuditDownloadClientPreservesProvidedProxyAndTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "public") }))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	var proxyCalls atomic.Int32
	transport.Proxy = func(*http.Request) (*url.URL, error) { proxyCalls.Add(1); return nil, nil }
	client := portalClientForTransport(transport, true)
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if proxyCalls.Load() != 1 || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("download client replaced provided proxy/TLS policy")
	}
}

func TestAuditDownloadSetupRemainsBounded(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(stopped); <-r.Context().Done() }))
	defer upstream.Close()
	service := testService(t, newMemoryStore())
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	resource := "/admin/v1/audit-exports/" + uuid.NewString() + "/download"
	origin := startAuditApplicationServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.forward(w, r, service.config.AccountURL, resource, Session{}, true)
	}))
	start := time.Now()
	response, err := (&http.Client{Timeout: 20 * time.Second}).Get(origin)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 502 || time.Since(start) > 17*time.Second {
		t.Fatalf("setup budget lost: %d %v %v", response.StatusCode, time.Since(start), err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("setup deadline did not cancel upstream")
	}
	if !strings.Contains(string(body), "UPSTREAM_UNAVAILABLE") {
		t.Fatal("setup error changed")
	}
}
