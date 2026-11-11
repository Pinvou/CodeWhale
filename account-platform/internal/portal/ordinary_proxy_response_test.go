package portal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestOrdinaryProxyValidatesResponseBeforeForwarding(t *testing.T) {
	const limit = 32 << 20
	const idempotencyKey = "register-once"
	chunk := strings.Repeat("x", 64<<10)
	writePayload := func(w io.Writer, size int) {
		for size > 0 {
			n, err := io.WriteString(w, chunk[:min(size, len(chunk))])
			if err != nil {
				return
			}
			size -= n
		}
	}
	tests := []struct {
		name      string
		write     func(http.ResponseWriter)
		status    int
		size      int
		errorCode string
	}{
		{
			name: "exact limit",
			write: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", strconv.Itoa(limit))
				w.WriteHeader(http.StatusCreated)
				writePayload(w, limit)
			},
			status: http.StatusCreated,
			size:   limit,
		},
		{
			name: "declared over limit",
			write: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", strconv.Itoa(limit+1))
				w.WriteHeader(http.StatusCreated)
				writePayload(w, limit+1)
			},
			status:    http.StatusBadGateway,
			errorCode: "UPSTREAM_RESPONSE_TOO_LARGE",
		},
		{
			name: "chunked exact limit",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusCreated)
				w.(http.Flusher).Flush()
				writePayload(w, limit)
			},
			status: http.StatusCreated,
			size:   limit,
		},
		{
			name: "small chunked response",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusCreated)
				w.(http.Flusher).Flush()
				writePayload(w, 3)
			},
			status: http.StatusCreated,
			size:   3,
		},
		{
			name: "chunked over limit",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusCreated)
				w.(http.Flusher).Flush()
				writePayload(w, limit+1)
			},
			status:    http.StatusBadGateway,
			errorCode: "UPSTREAM_RESPONSE_TOO_LARGE",
		},
		{
			name: "decompressed over limit",
			write: func(w http.ResponseWriter) {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusCreated)
				compressed := gzip.NewWriter(w)
				writePayload(compressed, limit+1)
				_ = compressed.Close()
			},
			status:    http.StatusBadGateway,
			errorCode: "UPSTREAM_RESPONSE_TOO_LARGE",
		},
		{
			name: "upstream closes before declared end",
			write: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", "30")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"private":"partial"`)
			},
			status:    http.StatusBadGateway,
			errorCode: "UPSTREAM_UNAVAILABLE",
		},
		{
			name: "valid upstream error",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, "conflict")
			},
			status: http.StatusConflict,
			size:   len("conflict"),
		},
		{
			name:   "no content",
			write:  func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
			status: http.StatusNoContent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/register" || r.Header.Get("Idempotency-Key") != idempotencyKey {
					t.Error("upstream did not receive the original command and idempotency key")
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.Header().Set("Retry-After", "3")
				w.Header().Set("X-Context-Version", "7")
				tc.write(w)
			}))
			defer upstream.Close()
			service := testService(t, newMemoryStore())
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := httptest.NewRequest(http.MethodPost, "/api/account/v1/auth/register", strings.NewReader(`{}`))
			request.Header.Set("Origin", service.config.PublicURL.String())
			request.Header.Set("Idempotency-Key", idempotencyKey)
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)

			if calls.Load() != 1 {
				t.Fatalf("upstream dispatched %d times", calls.Load())
			}
			if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status = %d, cache control = %q, want status %d and no-store", response.Code, response.Header().Get("Cache-Control"), tc.status)
			}
			if tc.status == http.StatusBadGateway {
				var failure map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure["code"] != tc.errorCode || strings.Contains(response.Body.String(), "partial") {
					t.Fatalf("invalid gateway error: %q", response.Body.String())
				}
				if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Retry-After") != "" || response.Header().Get("X-Context-Version") != "" {
					t.Fatalf("upstream headers leaked into the gateway error: %v", response.Header())
				}
				return
			}
			if response.Header().Get("Content-Type") != "application/problem+json" || response.Header().Get("Retry-After") != "3" || response.Header().Get("X-Context-Version") != "7" {
				t.Fatalf("response headers changed: %v", response.Header())
			}
			if response.Body.Len() != tc.size {
				t.Fatalf("body length = %d, want %d", response.Body.Len(), tc.size)
			}
			if tc.size == limit && bytes.Count(response.Body.Bytes(), []byte{'x'}) != limit {
				t.Fatal("body at the exact limit changed")
			}
			if tc.status == http.StatusConflict && response.Body.String() != "conflict" {
				t.Fatal("upstream error body changed")
			}
		})
	}
}

func TestOrdinaryProxyPreservesBodylessResponses(t *testing.T) {
	for _, tc := range []struct {
		method string
		status int
	}{
		{http.MethodHead, http.StatusOK},
		{http.MethodGet, http.StatusNotModified},
	} {
		t.Run(tc.method, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != "/v1/me/authorization-context" {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Length", strconv.Itoa(32<<20+1))
				w.Header().Set("X-Context-Version", "7")
				w.WriteHeader(tc.status)
			}))
			defer upstream.Close()
			store := newMemoryStore()
			service := testService(t, store)
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, "/api/account/v1/me/authorization-context", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != tc.status || response.Body.Len() != 0 || response.Header().Get("X-Context-Version") != "7" {
				t.Fatalf("bodyless response changed: status = %d, body size = %d, headers = %v", response.Code, response.Body.Len(), response.Header())
			}
		})
	}
}

func TestOrdinaryProxyDoesNotReplayCommandAfterLosingResponse(t *testing.T) {
	const idempotencyKey = "register-once"
	for name, payload := range map[string]string{"json": `{}`, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			var commands atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{}`)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != payload || r.Header.Get("Idempotency-Key") != idempotencyKey {
					t.Errorf("upstream command changed: body=%q error=%v", body, err)
				}
				if payload == "" && r.ContentLength != 0 {
					t.Errorf("empty command changed to content length %d", r.ContentLength)
				}
				if commands.Add(1) != 1 {
					w.WriteHeader(http.StatusCreated)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close() // The command arrived; its response never started.
			}))
			defer upstream.Close()
			service := testService(t, newMemoryStore())
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			handler := service.Handler(nil)
			warm := httptest.NewRecorder()
			handler.ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/api/account/v1/auth/registration-operations/"+uuid.NewString(), nil))
			if warm.Code != http.StatusOK {
				t.Fatalf("failed to warm the upstream connection: %d", warm.Code)
			}
			var reused atomic.Bool
			request := httptest.NewRequest(http.MethodPost, "/api/account/v1/auth/register", strings.NewReader(payload))
			request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) {
					if info.Reused {
						reused.Store(true)
					}
				},
			}))
			request.Header.Set("Origin", service.config.PublicURL.String())
			request.Header.Set("Idempotency-Key", idempotencyKey)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if reused.Load() != (payload != "") || commands.Load() != 1 || response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"UPSTREAM_UNAVAILABLE"`) {
				t.Fatalf("reused=%v commands=%d status=%d body=%q", reused.Load(), commands.Load(), response.Code, response.Body.String())
			}
		})
	}
}

func TestOrdinaryProxyPreservesEmptyKeyedDeleteOverHTTP2(t *testing.T) {
	const key = "delete-organization"
	path := "/admin/v1/organizations/" + uuid.NewString()
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Method != http.MethodDelete || r.URL.Path != path || r.Header.Get("Idempotency-Key") != key || r.ContentLength != 0 {
			t.Errorf("unexpected upstream request: %s %s %s, content length=%d", r.Proto, r.Method, r.URL.Path, r.ContentLength)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	service.emptyCommandClient = portalClientForPolicy(transport, portalEmptyCommandHTTP)
	defer service.emptyCommandClient.CloseIdleConnections()
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := financeBrowserRequest(service, session, http.MethodDelete, "/api/admin/account"+path, "")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("bodyless HTTP/2 command returned %d: %s", response.Code, response.Body.String())
	}
}
