package token

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type workerDiagnosticRecords chan string

func (records workerDiagnosticRecords) Write(record []byte) (int, error) {
	select {
	case records <- string(record):
	default:
	}
	return len(record), nil
}

func captureWorkerDiagnostics(t *testing.T) workerDiagnosticRecords {
	t.Helper()
	records := make(workerDiagnosticRecords, 32)
	previous := slog.Default()
	slog.SetDefault(observability.NewLogger(records, observability.Config{Service: "token-test"}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return records
}

func (records workerDiagnosticRecords) expect(t *testing.T, event, stage, code string) {
	t.Helper()
	select {
	case record := <-records:
		var fields map[string]any
		if err := json.Unmarshal([]byte(record), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["event"] != event || fields["stage"] != stage || fields["error_code"] != code ||
			strings.Contains(record, "private diagnostic test value") {
			t.Fatalf("unexpected diagnostic: %s", record)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("missing diagnostic %s/%s", event, stage)
	}
}

func runDiagnosticWorker(t *testing.T, parent context.Context, run func(context.Context)) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		run(ctx)
	}()
	stop := func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("worker did not exit on cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

type projectionDiagnosticProbe struct {
	calls     int
	recovered chan struct{}
}

func (probe *projectionDiagnosticProbe) BackfillUsageHistoryProjection(ctx context.Context, _ int) (int, bool, error) {
	probe.calls++
	if probe.calls == 1 {
		return 0, false, errors.New("private diagnostic test value")
	}
	close(probe.recovered)
	<-ctx.Done()
	return 0, false, ctx.Err()
}

func TestUsageHistoryProjectionDiagnostics(t *testing.T) {
	records := captureWorkerDiagnostics(t)
	recovered := make(chan struct{})
	worker := NewUsageHistoryProjectionWorker(&projectionDiagnosticProbe{recovered: recovered}, 20)
	stop := runDiagnosticWorker(t, t.Context(), worker.Run)
	records.expect(t, "usage_history_projection_failed", "backfill", "WORKER_OPERATION_FAILED")
	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("projection did not retry")
	}
	stop()
	select {
	case extra := <-records:
		t.Fatalf("shutdown produced a failure diagnostic: %s", extra)
	default:
	}
}

type cancellationFaultTracer struct {
	stages []string
}

func (tracer *cancellationFaultTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if len(tracer.stages) == 0 || !strings.HasPrefix(q.SQL, tracer.stages[0]) {
		return ctx
	}
	tracer.stages = tracer.stages[1:]
	failed, cancel := context.WithCancel(ctx)
	cancel()
	return failed
}

func (*cancellationFaultTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestEmergencyCancellationWorkerDiagnosticsPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	_, target := f.preparedExpiry(t, "hold")
	config := f.r.pool.Config()
	config.ConnConfig.Tracer = &cancellationFaultTracer{stages: []string{
		"INSERT INTO model_emergency_cancellations(",
		"WITH expired AS (",
		"WITH candidates AS (",
		"SELECT usage_request_id FROM model_emergency_cancellations",
		"WITH changed AS (\n    UPDATE model_emergency_cancellations SET status=CASE WHEN retry_attempts",
	}}
	pool, err := pgxpool.NewWithConfig(f.ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := *f.r
	repository.pool = pool
	records := captureWorkerDiagnostics(t)
	worker := NewEmergencyModelCancellationWorker(&repository, f.service, 20*time.Millisecond)
	stop := runDiagnosticWorker(t, f.ctx, worker.Run)
	for _, stage := range []string{"sync_targets", "review_expired_claims", "claim_targets", "request_cancellation", "persist_retry"} {
		records.expect(t, "model_emergency_cancellation_failed", stage, "WORKER_CANCELED")
	}
	var status string
	var attempts int
	if err = f.r.pool.QueryRow(f.ctx, `SELECT status,attempts FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&status, &attempts); err != nil || status != "LEASED" || attempts != 1 {
		t.Fatalf("failed retry changed lease ownership: %s %d %v", status, attempts, err)
	}
	if _, err := f.r.pool.Exec(f.ctx, `UPDATE model_emergency_cancellations SET lease_until=statement_timestamp()-interval '1 second',next_attempt_at=statement_timestamp()-interval '1 second' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for status != "CONVERGED" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if err := f.r.pool.QueryRow(f.ctx, `SELECT status,attempts FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	if status != "CONVERGED" || attempts != 2 {
		t.Fatalf("worker did not recover with a new claim: %s %d", status, attempts)
	}
}
