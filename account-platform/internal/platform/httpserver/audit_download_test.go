package httpserver

import (
	"bytes"
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type auditTestWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	writes    []int
}

func (w *auditTestWriter) Write(buffer []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, len(buffer))
	w.mu.Unlock()
	return w.ResponseRecorder.Write(buffer)
}

func (w *auditTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
func (w *auditTestWriter) ReadFrom(io.Reader) (int64, error) {
	panic("ReadFrom bypassed progress observation")
}

type auditTestReader struct{ *bytes.Reader }

func (r auditTestReader) Close() error { return nil }
func (r auditTestReader) WriteTo(io.Writer) (int64, error) {
	panic("WriteTo bypassed progress observation")
}

func TestAuditDownloadProgressContract(t *testing.T) {
	t.Run("chunks and optimization interfaces cannot bypass observation", func(t *testing.T) {
		w := &auditTestWriter{ResponseRecorder: httptest.NewRecorder()}
		stream, err := NewAuditDownloadStream(w, httptest.NewRequest("GET", "/", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if stream.budget != 30*time.Second {
			t.Fatal("production budget drift")
		}
		payload := bytes.Repeat([]byte("public"), 20000)
		if err = stream.Start(200); err != nil {
			t.Fatal(err)
		}
		if err = stream.CopyFrom(auditTestReader{bytes.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.Body.Bytes(), payload) {
			t.Fatal("payload truncated")
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.writes) < 4 {
			t.Fatalf("not chunked: %d writes", len(w.writes))
		}
		for _, size := range w.writes {
			if size > auditDownloadChunkSize {
				t.Fatalf("unbounded write: %d", size)
			}
		}
	})
	t.Run("unsupported before any response is selected", func(t *testing.T) {
		w := httptest.NewRecorder()
		if _, err := NewAuditDownloadStream(w, httptest.NewRequest("GET", "/", nil)); !errors.Is(err, http.ErrNotSupported) {
			t.Fatal(err)
		}
		if w.Body.Len() != 0 || w.Flushed {
			t.Fatal("unsupported preparation emitted a body")
		}
	})
	for _, kind := range []string{"stall", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			w := &auditTestWriter{ResponseRecorder: httptest.NewRecorder()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := newAuditDownloadStream(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx), 40*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			reader, writer := io.Pipe()
			defer writer.Close()
			result := make(chan error, 1)
			go func() { result <- stream.CopyFrom(reader) }()
			if kind == "cancel" {
				cancel()
			}
			select {
			case err := <-result:
				if kind == "stall" && !errors.Is(err, ErrAuditDownloadIdle) {
					t.Fatal(err)
				}
				if kind == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked read survived its owner context")
			}
		})
	}
	t.Run("repeated positive low-rate reads renew budget without total cap", func(t *testing.T) {
		w := &auditTestWriter{ResponseRecorder: httptest.NewRecorder()}
		stream, err := newAuditDownloadStream(w, httptest.NewRequest("GET", "/", nil), 80*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		reader, writer := io.Pipe()
		defer writer.Close()
		go func() {
			defer writer.Close()
			for range 12 {
				time.Sleep(20 * time.Millisecond)
				if _, err := writer.Write([]byte("p")); err != nil {
					return
				}
			}
		}()
		started := time.Now()
		if err = stream.CopyFrom(reader); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) < 200*time.Millisecond || w.Body.String() != "pppppppppppp" {
			t.Fatal("did not traverse multiple idle budgets")
		}
	})
}

type auditZeroReader struct{}

func (auditZeroReader) Read([]byte) (int, error) { return 0, nil }
func (auditZeroReader) Close() error             { return nil }
func TestAuditDownloadZeroReadsCannotRenewBudget(t *testing.T) {
	w := &auditTestWriter{ResponseRecorder: httptest.NewRecorder()}
	stream, err := newAuditDownloadStream(w, httptest.NewRequest("GET", "/", nil), 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start := time.Now()
	if err = stream.CopyFrom(auditZeroReader{}); !errors.Is(err, ErrAuditDownloadIdle) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("zero-byte reader renewed its idle budget")
	}
}

type auditZeroWriter struct{ *auditTestWriter }

func (w *auditZeroWriter) Write([]byte) (int, error) { return 0, nil }

type auditPartialWriter struct{ *auditTestWriter }

func (w *auditPartialWriter) Write(buffer []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return w.ResponseRecorder.Write(buffer[:1])
}
func TestAuditDownloadWriteProgressIsPositiveBytes(t *testing.T) {
	t.Run("zero writes never renew", func(t *testing.T) {
		writer := &auditZeroWriter{&auditTestWriter{ResponseRecorder: httptest.NewRecorder()}}
		stream, err := newAuditDownloadStream(writer, httptest.NewRequest("GET", "/", nil), 30*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		start := time.Now()
		err = stream.CopyFrom(io.NopCloser(bytes.NewReader([]byte("p"))))
		if !errors.Is(err, ErrAuditDownloadIdle) || time.Since(start) > time.Second {
			t.Fatalf("zero write escaped budget: %v %v", time.Since(start), err)
		}
	})
	t.Run("partial positive writes renew without a total timer", func(t *testing.T) {
		writer := &auditPartialWriter{&auditTestWriter{ResponseRecorder: httptest.NewRecorder()}}
		stream, err := newAuditDownloadStream(writer, httptest.NewRequest("GET", "/", nil), 40*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		payload := []byte("public-bytes")
		start := time.Now()
		err = stream.CopyFrom(io.NopCloser(bytes.NewReader(payload)))
		if err != nil || !bytes.Equal(writer.Body.Bytes(), payload) || time.Since(start) < 100*time.Millisecond {
			t.Fatalf("continuous partial writes failed: %v", err)
		}
	})
}

func TestAuditDownloadBlockedNetworkWriteIsBounded(t *testing.T) {
	result := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream, err := newAuditDownloadStream(w, r, 80*time.Millisecond)
		if err != nil {
			result <- err
			return
		}
		defer stream.Close()
		if err = stream.Start(200); err == nil {
			err = stream.CopyFrom(io.NopCloser(bytes.NewReader(make([]byte, 8<<20))))
		}
		result <- err
	}), ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		return ctx
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = server.Serve(listener); close(done) }()
	defer func() { server.Close(); <-done }()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	_, err = io.WriteString(connection, "GET / HTTP/1.1\r\nHost: public.invalid\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("stalled receiver accepted full payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked network write exceeded its finite budget")
	}
}

type auditGapWriter struct {
	*auditTestWriter
	flushes int
	delayAt int
}

func (w *auditGapWriter) FlushError() error {
	w.flushes++
	if w.flushes == w.delayAt {
		time.Sleep(70 * time.Millisecond)
	}
	return nil
}

type auditGapReader struct {
	reads int
	eof   bool
}

func (r *auditGapReader) Read(buffer []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		buffer[0] = 'p'
		return 1, nil
	}
	if r.reads == 2 {
		time.Sleep(70 * time.Millisecond)
		if !r.eof {
			buffer[0] = 'p'
			return 1, nil
		}
	}
	return 0, io.EOF
}
func (*auditGapReader) Close() error { return nil }

func TestAuditDownloadIdleBudgetSpansFlushAndRead(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(map[bool]string{false: "late positive read", true: "late EOF"}[eof], func(t *testing.T) {
			writer := &auditGapWriter{auditTestWriter: &auditTestWriter{ResponseRecorder: httptest.NewRecorder()}, delayAt: 2}
			stream, err := newAuditDownloadStream(writer, httptest.NewRequest("GET", "/", nil), 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if err = stream.Start(200); err != nil {
				t.Fatal(err)
			}
			err = stream.CopyFrom(&auditGapReader{eof: eof})
			if !errors.Is(err, ErrAuditDownloadIdle) {
				t.Fatalf("70ms flush plus 70ms read renewed 100ms no-progress budget: %v", err)
			}
			if writer.Body.String() != "p" {
				t.Fatal("accepted bytes returned after idle deadline")
			}
		})
	}
}

type auditFlushFaultWriter struct {
	http.ResponseWriter
	fault   error
	flushes int
}

func (w *auditFlushFaultWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *auditFlushFaultWriter) FlushError() error {
	w.flushes++
	if w.flushes == 2 && w.fault != nil {
		return w.fault
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestAuditDownloadGinFlushPreservesHeadersAndFinalFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "header status remains committed", true: "last body flush aborts"}[fail], func(t *testing.T) {
			sentinel := errors.New("public controlled final flush failure")
			result := make(chan error, 1)
			router := NewRouter()
			router.GET("/public-download", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
				stream, err := NewAuditDownloadStream(w, r)
				if err != nil {
					result <- err
					return
				}
				defer stream.Close()
				w.Header().Set("X-Public-Header", "preserved")
				err = stream.Start(http.StatusCreated)
				if err == nil {
					state := w.(interface {
						Written() bool
						Status() int
					})
					if !state.Written() || state.Status() != http.StatusCreated {
						err = errors.New("Gin delayed header state was bypassed")
					}
				}
				if err == nil {
					err = stream.CopyFrom(io.NopCloser(strings.NewReader("public-final-chunk")))
				}
				result <- err
				if err != nil {
					panic(http.ErrAbortHandler)
				}
			}))
			common := HandlerWithApplication(config.Config{Environment: "test", ServiceName: "audit-stream-contract"}, router, nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var fault error
				if fail {
					fault = sentinel
				}
				common.ServeHTTP(&auditFlushFaultWriter{ResponseWriter: w, fault: fault}, r)
			}))
			defer server.Close()
			response, err := http.Get(server.URL + "/public-download")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			copyErr := <-result
			if response.StatusCode != http.StatusCreated || response.Header.Get("X-Public-Header") != "preserved" {
				t.Fatal("Gin/header/status semantics changed")
			}
			if fail {
				if !errors.Is(copyErr, sentinel) || readErr == nil {
					t.Fatalf("last flush appeared successful: copy=%v read=%v body=%q", copyErr, readErr, body)
				}
				if strings.Contains(string(body), "INTERNAL_ERROR") || strings.Contains(string(body), "request_id") {
					t.Fatal("abort appended JSON")
				}
			} else if copyErr != nil || readErr != nil || string(body) != "public-final-chunk" {
				t.Fatalf("header/flush success changed: %v %v %q", copyErr, readErr, body)
			}
		})
	}
}
