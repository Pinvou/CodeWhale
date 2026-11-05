package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

// NewApplicationServer constructs the bounded transport used by every workload.
// Streaming is an explicit application-handler opt-in; defaults remain unchanged.
func NewApplicationServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{Addr: cfg.Address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
}

const streamWriteTimeout = 30 * time.Second

// WithStreamingWriteDeadline replaces the absolute response deadline only after
// an opted-in handler selects text/event-stream. Every write and flush remains
// bounded; unsupported deadline transports fail closed instead of streaming.
func WithStreamingWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		writer := &streamDeadlineWriter{ResponseWriter: w, controller: http.NewResponseController(w), cancel: cancel}
		next.ServeHTTP(writer, r.WithContext(ctx))
	})
}

type streamDeadlineWriter struct {
	http.ResponseWriter
	controller  *http.ResponseController
	cancel      context.CancelFunc
	wroteHeader bool
	stream      bool
	err         error
}

func (w *streamDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamDeadlineWriter) fail(err error) error {
	if err != nil {
		w.err = errors.Join(w.err, err)
		w.cancel()
	}
	return w.err
}

func (w *streamDeadlineWriter) beginWrite() error {
	if w.err != nil {
		return w.err
	}
	return w.fail(w.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)))
}

func (w *streamDeadlineWriter) endWrite(err error) error {
	// Idle generation is governed by the existing upstream context, not a
	// response deadline measured from request admission. Blocked writes still
	// have their own bounded budget.
	return w.fail(errors.Join(err, w.controller.SetWriteDeadline(time.Time{})))
}

func (w *streamDeadlineWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	mediaType, _, _ := strings.Cut(w.Header().Get("Content-Type"), ";")
	w.stream = strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
	if !w.stream {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if err := w.beginWrite(); err != nil {
		// A wrapper that hides deadline support must not silently produce an
		// unbounded stream. Preserve the transport error for Write/Flush callers.
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_ = w.endWrite(nil)
}

func (w *streamDeadlineWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.stream {
		return w.ResponseWriter.Write(p)
	}
	if err := w.beginWrite(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	return n, w.endWrite(err)
}

func (w *streamDeadlineWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.stream {
		return w.controller.Flush()
	}
	if err := w.beginWrite(); err != nil {
		return err
	}
	return w.endWrite(w.controller.Flush())
}

func (w *streamDeadlineWriter) Flush() { _ = w.FlushError() }
