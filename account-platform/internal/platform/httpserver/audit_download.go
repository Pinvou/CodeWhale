package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"
)

const auditDownloadIdleTimeout = 30 * time.Second
const auditDownloadChunkSize = 32 << 10

var ErrAuditDownloadIdle = errors.New("audit download made no progress")

// AuditDownloadStream is an explicit opt-in for an already authorized, bounded
// one-use audit download. It does not change ordinary HTTP or SSE policy.
// Progress means positive bytes returned by a bounded Read or Write; zero-byte
// reads/writes do not renew the budget. Each flush is bounded as well.
type AuditDownloadStream struct {
	writer        http.ResponseWriter
	controller    *http.ResponseController
	flushResponse func() error
	ctx           context.Context
	cancel        context.CancelFunc
	budget        time.Duration
	idleUntil     time.Time
	mu            sync.Mutex
	err           error
	closed        bool
	started       bool
	stopCancel    func() bool
}

// NewAuditDownloadStream must precede any irreversible owner consumption.
// Testing deadline support here prevents consuming a ticket on an unsupported
// transport. Keep a finite header deadline until Start selects streaming.
func NewAuditDownloadStream(w http.ResponseWriter, r *http.Request) (*AuditDownloadStream, error) {
	return newAuditDownloadStream(w, r, auditDownloadIdleTimeout)
}

func newAuditDownloadStream(w http.ResponseWriter, r *http.Request, budget time.Duration) (*AuditDownloadStream, error) {
	flushResponse, err := auditDownloadFlush(w)
	if err != nil {
		return nil, err
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(budget)); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	stream := &AuditDownloadStream{writer: w, controller: controller, flushResponse: flushResponse, ctx: ctx, cancel: cancel, budget: budget, idleUntil: time.Now().Add(budget)}
	stream.stopCancel = context.AfterFunc(ctx, func() {
		stream.mu.Lock()
		defer stream.mu.Unlock()
		if !stream.closed && stream.started {
			_ = controller.SetWriteDeadline(time.Now())
		}
	})
	return stream, nil
}

// Gin exposes Flush(), which discards errors from the metric writer's
// FlushError(). Prefer the first error-aware implementation in the unwrap
// chain. Commit deferred headers at intervening framework writers first, so
// bypassing only their lossy Flush does not bypass their header/status state.
func auditDownloadFlush(writer http.ResponseWriter) (func() error, error) {
	if flusher, ok := writer.(interface{ FlushError() error }); ok {
		return flusher.FlushError, nil
	}
	if unwrapped, ok := writer.(interface{ Unwrap() http.ResponseWriter }); ok {
		flush, err := auditDownloadFlush(unwrapped.Unwrap())
		if err != nil {
			return nil, err
		}
		return func() error {
			if headers, ok := writer.(interface{ WriteHeaderNow() }); ok {
				headers.WriteHeaderNow()
			}
			return flush()
		}, nil
	}
	if flusher, ok := writer.(http.Flusher); ok {
		return func() error { flusher.Flush(); return nil }, nil
	}
	return nil, http.ErrNotSupported
}

func (s *AuditDownloadStream) Context() context.Context { return s.ctx }

func (s *AuditDownloadStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return s.ctx.Err()
}

// Cancel makes failures sticky and interrupts a blocked downstream write.
func (s *AuditDownloadStream) Cancel(err error) error {
	if err == nil {
		return nil
	}
	s.mu.Lock()
	s.err = errors.Join(s.err, err)
	result := s.err
	s.mu.Unlock()
	s.cancel()
	return result
}

func (s *AuditDownloadStream) setDeadline(deadline time.Time) error {
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	err := s.controller.SetWriteDeadline(deadline)
	s.mu.Unlock()
	return s.Cancel(err)
}

// The same idle deadline spans Read, Write and Flush. Only an operation that
// returns positive bytes before that deadline can advance it; phase changes,
// headers, flush completion and (0,nil) never count as progress.
func (s *AuditDownloadStream) progressDeadline(positive bool) (time.Time, error) {
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return time.Time{}, err
	}
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return time.Time{}, err
	}
	now := time.Now()
	if !now.Before(s.idleUntil) {
		s.mu.Unlock()
		return time.Time{}, s.Cancel(ErrAuditDownloadIdle)
	}
	if positive {
		s.idleUntil = now.Add(s.budget)
	}
	deadline := s.idleUntil
	s.mu.Unlock()
	return deadline, nil
}

func (s *AuditDownloadStream) flush() error {
	deadline, err := s.progressDeadline(false)
	if err != nil {
		return err
	}
	if err := s.setDeadline(deadline); err != nil {
		return err
	}
	if err := s.flushResponse(); err != nil {
		return s.Cancel(err)
	}
	_, err = s.progressDeadline(false)
	return err
}

// Start flushes headers under the existing finite idle budget, then replaces
// the server's absolute deadline with the shared progress deadline. No total
// transfer timer is installed, and header/flush completion never renews it.
func (s *AuditDownloadStream) Start(status int) error {
	s.mu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.started = true
	s.mu.Unlock()
	deadline, err := s.progressDeadline(false)
	if err != nil {
		return err
	}
	if err := s.setDeadline(deadline); err != nil {
		return err
	}
	s.writer.WriteHeader(status)
	return s.flush()
}

func (s *AuditDownloadStream) writeAll(buffer []byte) error {
	for len(buffer) > 0 {
		deadline, err := s.progressDeadline(false)
		if err != nil {
			return err
		}
		if err := s.setDeadline(deadline); err != nil {
			return err
		}
		n, err := s.writer.Write(buffer)
		if n < 0 || n > len(buffer) {
			return s.Cancel(io.ErrShortWrite)
		}
		if _, progressErr := s.progressDeadline(n > 0); progressErr != nil {
			return progressErr
		}
		if n > 0 {
			buffer = buffer[n:]
		}
		if err != nil {
			return s.Cancel(err)
		}
		if n == 0 {
			runtime.Gosched()
		}
	}
	return s.flush()
}

func (s *AuditDownloadStream) read(reader io.Reader, buffer []byte) (int, error) {
	deadline, err := s.progressDeadline(false)
	if err != nil {
		return 0, err
	}
	timer := time.AfterFunc(time.Until(deadline), func() { s.Cancel(ErrAuditDownloadIdle) })
	defer timer.Stop()
	for {
		if _, err := s.progressDeadline(false); err != nil {
			return 0, err
		}
		n, err := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return 0, s.Cancel(errors.New("invalid audit download read count"))
		}
		if n > 0 || err != nil {
			if !timer.Stop() {
				return 0, s.Cancel(ErrAuditDownloadIdle)
			}
			if _, failure := s.progressDeadline(n > 0); failure != nil {
				return 0, failure
			}
			return n, err
		}
		// A broken reader returning (0,nil) cannot extend the original idle budget.
		runtime.Gosched()
	}
}

// CopyFrom deliberately avoids io.Copy/ReadFrom/WriteTo: neither an optimized
// ReaderFrom nor WriterTo may bypass bounded chunks and progress observation.
// Closing the reader on context cancellation interrupts an upstream HTTP read.
func (s *AuditDownloadStream) CopyFrom(reader io.ReadCloser) error {
	var once sync.Once
	closeReader := func() { once.Do(func() { _ = reader.Close() }) }
	stop := context.AfterFunc(s.ctx, closeReader)
	defer func() { stop(); closeReader() }()
	buffer := make([]byte, auditDownloadChunkSize)
	defer clear(buffer)
	for {
		n, err := s.read(reader, buffer)
		if n > 0 {
			if writeErr := s.writeAll(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return s.Cancel(err)
		}
	}
}

// Close stops cancellation callbacks before retiring a successful context.
// Keep the final chunk/trailer flush within the remaining progress budget;
// successful cleanup must not renew idle time without positive bytes either.
func (s *AuditDownloadStream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	deadline := s.idleUntil
	if s.started && (s.err != nil || s.ctx.Err() != nil) {
		deadline = time.Now()
	}
	_ = s.controller.SetWriteDeadline(deadline)
	s.mu.Unlock()
	s.stopCancel()
	s.cancel()
}
