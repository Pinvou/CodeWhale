package httpserver

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deadlineTestWriter struct {
	*httptest.ResponseRecorder
	deadlines                       []time.Time
	deadlineErr, writeErr, flushErr error
}

func (w *deadlineTestWriter) SetWriteDeadline(v time.Time) error {
	w.deadlines = append(w.deadlines, v)
	return w.deadlineErr
}
func (w *deadlineTestWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}
func (w *deadlineTestWriter) FlushError() error { w.ResponseRecorder.Flush(); return w.flushErr }

func TestStreamingWriteDeadlineTransportContract(t *testing.T) {
	t.Run("bounded operations and cleared idle", func(t *testing.T) {
		w := &deadlineTestWriter{ResponseRecorder: httptest.NewRecorder()}
		WithStreamingWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(200)
			if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
				t.Fatal(err)
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Fatal(err)
			}
			if r.Context().Err() != nil {
				t.Fatal("successful stream canceled")
			}
		})).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if len(w.deadlines) != 6 {
			t.Fatalf("deadlines=%v", w.deadlines)
		}
		for i, v := range w.deadlines {
			if i%2 == 0 {
				left := time.Until(v)
				if left < 29*time.Second || left > 30*time.Second {
					t.Fatalf("write budget=%v", left)
				}
			} else if !v.IsZero() {
				t.Fatal("generation idle retained absolute deadline")
			}
		}
	})
	for _, kind := range []string{"unsupported", "write", "flush"} {
		t.Run(kind, func(t *testing.T) {
			sentinel := errors.New("controlled " + kind)
			w := &deadlineTestWriter{ResponseRecorder: httptest.NewRecorder()}
			if kind == "unsupported" {
				sentinel = http.ErrNotSupported
				w.deadlineErr = sentinel
			}
			if kind == "write" {
				w.writeErr = sentinel
			}
			if kind == "flush" {
				w.flushErr = sentinel
			}
			WithStreamingWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				var err error
				if kind == "flush" {
					err = http.NewResponseController(w).Flush()
				} else {
					_, err = w.Write([]byte("data: x\n\n"))
				}
				if !errors.Is(err, sentinel) || r.Context().Err() != context.Canceled {
					t.Fatalf("err=%v context=%v", err, r.Context().Err())
				}
				if _, err = w.Write([]byte("later")); !errors.Is(err, sentinel) {
					t.Fatalf("error not sticky: %v", err)
				}
			})).ServeHTTP(&metricResponseWriter{ResponseWriter: w}, httptest.NewRequest("GET", "/", nil))
			if kind == "unsupported" && w.Code != http.StatusServiceUnavailable {
				t.Fatalf("unsupported status=%d", w.Code)
			}
		})
	}
	t.Run("ordinary response untouched", func(t *testing.T) {
		w := &deadlineTestWriter{ResponseRecorder: httptest.NewRecorder()}
		WithStreamingWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		})).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if len(w.deadlines) != 0 {
			t.Fatal("JSON changed transport policy")
		}
	})
}

func TestSharedApplicationServerTimeoutDefaults(t *testing.T) {
	for _, name := range []string{"token-gateway", "account-service", "billing-service"} {
		s := NewApplicationServer(config.Config{ServiceName: name}, http.NotFoundHandler())
		if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 30*time.Second || s.WriteTimeout != 30*time.Second || s.IdleTimeout != 60*time.Second {
			t.Fatalf("%s transport changed: %+v", name, s)
		}
	}
}

func TestSharedOrdinaryResponseRetainsAbsoluteWriteTimeout(t *testing.T) {
	t.Parallel()
	h := WithStreamingWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "first\n")
		http.NewResponseController(w).Flush()
		time.Sleep(31500 * time.Millisecond)
		io.WriteString(w, "late\n")
		http.NewResponseController(w).Flush()
	}))
	server := NewApplicationServer(config.Config{}, h)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { server.Serve(listener); close(done) }()
	t.Cleanup(func() { server.Close(); <-done })
	client := &http.Client{Timeout: 40 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err == nil || !strings.Contains(string(body), "first") || strings.Contains(string(body), "late") {
		t.Fatalf("ordinary deadline lost: body=%q err=%v", body, err)
	}
}
