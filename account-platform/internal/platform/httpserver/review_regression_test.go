package httpserver

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func TestInvalidProductionClockFailsClosed(t *testing.T) {
	for _, environment := range []string{"production", "production-new", " Production-Canary "} {
		t.Run(environment, func(t *testing.T) {
			called := false
			handler := HandlerWithApplication(config.Config{ServiceName: "test", Environment: environment},
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }), nil)
			for _, test := range []struct {
				method, path string
				status       int
			}{
				{http.MethodGet, "/health/live", http.StatusOK},
				{http.MethodGet, "/health/ready", http.StatusServiceUnavailable},
				{http.MethodPost, "/write", http.StatusServiceUnavailable},
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
				if response.Code != test.status || response.Header().Get("Strict-Transport-Security") == "" {
					t.Fatalf("%s %s: status=%d HSTS=%q", test.method, test.path, response.Code, response.Header().Get("Strict-Transport-Security"))
				}
			}
			if called {
				t.Fatal("unsafe production clock reached write handler")
			}
		})
	}
}

type panicHijackWriter struct {
	*httptest.ResponseRecorder
	connection net.Conn
}

func (writer *panicHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return writer.connection, bufio.NewReadWriter(bufio.NewReader(writer.connection), bufio.NewWriter(writer.connection)), nil
}

func TestPanicAfterHijackClosesConnectionAndAborts(t *testing.T) {
	connection, peer := net.Pipe()
	t.Cleanup(func() { _ = connection.Close(); _ = peer.Close() })
	writer := &panicHijackWriter{ResponseRecorder: httptest.NewRecorder(), connection: connection}
	defer func() {
		if value := recover(); value != http.ErrAbortHandler {
			t.Fatalf("panic=%v", value)
		}
		_ = peer.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := peer.Write([]byte("probe")); err == nil {
			t.Fatal("hijacked connection remained open")
		}
		if writer.Body.Len() != 0 {
			t.Fatal("recovery wrote HTTP data after hijacking")
		}
	}()
	withRecovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatal(err)
		}
		panic("private")
	})).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/socket", nil))
}

func TestExplicitAbortHandlerSentinelIsPreserved(t *testing.T) {
	writer := httptest.NewRecorder()
	defer func() {
		if value := recover(); value != http.ErrAbortHandler {
			t.Fatalf("panic=%v", value)
		}
		if writer.Body.Len() != 0 {
			t.Fatal("intentional abort replaced by an error response")
		}
	}()
	withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/abort", nil))
}

func TestSuccessfulHijackDoesNotCloseOwnedConnection(t *testing.T) {
	connection, peer := net.Pipe()
	t.Cleanup(func() { _ = connection.Close(); _ = peer.Close() })
	writer := &panicHijackWriter{ResponseRecorder: httptest.NewRecorder(), connection: connection}
	withRecovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/socket", nil))
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	go func() { _, _ = peer.Write([]byte("ping")) }()
	buffer := make([]byte, 4)
	if n, err := connection.Read(buffer); err != nil || string(buffer[:n]) != "ping" {
		t.Fatalf("owned connection unavailable: %v", err)
	}
}

type deadlineResponseWriter struct {
	*httptest.ResponseRecorder
	readDeadline, writeDeadline time.Time
}

func (writer *deadlineResponseWriter) SetReadDeadline(value time.Time) error {
	writer.readDeadline = value
	return nil
}
func (writer *deadlineResponseWriter) SetWriteDeadline(value time.Time) error {
	writer.writeDeadline = value
	return nil
}

func TestRecoveryPreservesResponseControllerAndAbortsAfterFlush(t *testing.T) {
	writer := &deadlineResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	deadline := time.Now().Add(time.Second)
	defer func() {
		if value := recover(); value != http.ErrAbortHandler {
			t.Fatalf("panic=%v", value)
		}
		if writer.readDeadline != deadline || writer.writeDeadline != deadline || !writer.Flushed || writer.Body.Len() != 0 {
			t.Fatal("recovery changed controller or already-flushed response")
		}
	}()
	withRecovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		if err := controller.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		if err := controller.Flush(); err != nil {
			t.Fatal(err)
		}
		panic("private")
	})).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
}

func TestStandardApplicationPanicIsSanitized(t *testing.T) {
	const secret = "private-panic-value"
	handler := HandlerWithApplication(config.Config{ServiceName: "test"}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(secret) }), nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	request.Header.Set("X-Request-ID", "panic-request")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), secret) || !strings.Contains(response.Body.String(), "panic-request") {
		t.Fatalf("unsafe panic response: status=%d body=%s", response.Code, response.Body)
	}
}

func TestPanicAfterResponseStartedAbortsInsteadOfAppendingError(t *testing.T) {
	response := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("abort=%v", recovered)
		}
		if response.Body.String() != "partial" {
			t.Fatalf("response=%s", response.Body)
		}
	}()
	withRecovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("partial")); panic("private") })).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stream", nil))
}
