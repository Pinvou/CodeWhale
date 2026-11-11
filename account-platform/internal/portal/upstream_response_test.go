package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestOrdinaryPortalResponseCompleteness(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    int
		chunked bool
		status  int
	}{
		{"declared-overflow", int(ordinaryAPIResponseLimit) + 1, false, http.StatusOK},
		{"chunked-overflow", int(ordinaryAPIResponseLimit) + 1, true, http.StatusOK},
		{"exact-limit-complete", int(ordinaryAPIResponseLimit), false, http.StatusOK},
		{"original-forbidden-complete", 64, false, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), test.size)
			payload[len(payload)-1] = 'Z'
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", `attachment; filename="not-an-audit-download"`)
				if !test.chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				}
				w.WriteHeader(test.status)
				if test.chunked {
					w.(http.Flusher).Flush()
				}
				// An early overflow refusal closes this controlled connection; the
				// resulting server write error is expected, and never retried.
				_, _ = w.Write(payload)
			}))
			defer upstream.Close()
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			r := financeBrowserRequest(service, session, http.MethodGet, "/api/admin/account/admin/v1/users", "")
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if calls.Load() != 1 {
				t.Fatal("response handling repeated the upstream operation", calls.Load())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") != "" {
				t.Fatal("ordinary response escaped its metadata contract")
			}
			if test.size > int(ordinaryAPIResponseLimit) {
				var failure struct {
					Code string `json:"code"`
				}
				if w.Code != http.StatusBadGateway || json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure.Code != "UPSTREAM_RESPONSE_TOO_LARGE" || w.Body.Len() > 1024 {
					t.Fatal("oversize response was exposed as a complete upstream result", w.Code, w.Body.Len())
				}
				return
			}
			if w.Code != test.status || !bytes.Equal(w.Body.Bytes(), payload) || w.Header().Get("Content-Type") != "application/octet-stream" {
				t.Fatal("complete response lost its original status, type or final bytes", w.Code, w.Body.Len())
			}
		})
	}
}

type incompletePortalResponse struct {
	read   bool
	closed bool
}

func (b *incompletePortalResponse) Read(target []byte) (int, error) {
	if b.read {
		return 0, errors.New("controlled-private-upstream-read-error")
	}
	b.read = true
	return copy(target, "controlled-incomplete-upstream-prefix"), nil
}

func (b *incompletePortalResponse) Close() error { b.closed = true; return nil }

func TestOrdinaryPortalReadFailureDoesNotCommitPartialSuccess(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	body := &incompletePortalResponse{}
	client := *service.client
	client.Transport = materialProxyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: body, ContentLength: -1}, nil
	})
	service.client = &client
	w := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(w, financeBrowserRequest(service, session, http.MethodGet, "/api/admin/account/admin/v1/users", ""))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"UPSTREAM_UNAVAILABLE"`) || strings.Contains(w.Body.String(), "controlled-") || !body.closed {
		t.Fatal("read failure leaked partial/private output or kept the upstream body", w.Code, body.closed)
	}
}

func TestOrdinaryPortalHeadDoesNotApplyBodyLimitToRepresentationMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(ordinaryAPIResponseLimit+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	r, err := http.NewRequest(http.MethodHead, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := upstream.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	w := httptest.NewRecorder()
	forwardOrdinaryAPIResponse(w, r, response)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatal("HEAD representation metadata was treated as a transmitted oversized body")
	}
}

func TestOrdinaryPortalBodylessStatusUsesRealHTTPWriter(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusNotModified} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				length := int64(0)
				if status == http.StatusNotModified {
					// A 304 may describe a large representation without sending it.
					length = ordinaryAPIResponseLimit + 1
				}
				forwardOrdinaryAPIResponse(w, r, &http.Response{StatusCode: status, ContentLength: length, Body: io.NopCloser(strings.NewReader(""))})
			}))
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal("complete bodyless response aborted", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != status || len(body) != 0 {
				t.Fatal("bodyless response changed wire semantics", response.StatusCode, err)
			}
		})
	}
}

type portalShortResponseWriter struct {
	header http.Header
	writes int
}

func (w *portalShortResponseWriter) Header() http.Header { return w.header }
func (w *portalShortResponseWriter) WriteHeader(int)     {}
func (w *portalShortResponseWriter) Write(body []byte) (int, error) {
	w.writes++
	return len(body) - 1, nil
}

func TestOrdinaryPortalShortDeliveryAborts(t *testing.T) {
	writer := &portalShortResponseWriter{header: make(http.Header)}
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatal("short response write did not abort HTTP", recovered)
		}
		if writer.writes != 1 {
			t.Fatalf("continued writing after short delivery: %d calls", writer.writes)
		}
	}()
	body := strings.NewReader(strings.Repeat("complete-original-body", 256))
	forwardOrdinaryAPIResponse(writer, httptest.NewRequest(http.MethodGet, "/", nil), &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), ContentLength: -1})
}
