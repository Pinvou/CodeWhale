package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

type manualWorkerFaultStore struct {
	fault       string
	faultErr    error
	calls       []string
	badLimit    bool
	gotSchedule manualreview.OnCallSchedule
	onStep      func(string)
}

func (s *manualWorkerFaultStore) step(ctx context.Context, name string, limit int) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if limit != 100 {
		s.badLimit = true
	}
	s.calls = append(s.calls, name)
	if s.onStep != nil {
		s.onStep(name)
	}
	if s.fault == name {
		if s.faultErr != nil {
			return 0, s.faultErr
		}
		return 0, errors.New("controlled-private-repository-token +8613800001234")
	}
	return 1, nil
}
func (s *manualWorkerFaultStore) SyncManualReviewCases(ctx context.Context, limit int) (int, error) {
	return s.step(ctx, "sync", limit)
}
func (s *manualWorkerFaultStore) EscalateOverdueManualReviews(ctx context.Context, schedule manualreview.OnCallSchedule, limit int) (int, error) {
	s.gotSchedule = schedule
	return s.step(ctx, "escalate", limit)
}

type manualWorkerFailingLogWriter struct{ calls int }

func (w *manualWorkerFailingLogWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, io.ErrClosedPipe
}

func TestManualReviewWorkerFailuresRemainVisibleAndIndependent(t *testing.T) {
	expectedSteps := []string{"sync", "escalate"}
	expectedEvents := map[string]string{"sync": "manual_review_sync_failed", "escalate": "manual_review_escalation_failed"}
	for _, fault := range expectedSteps {
		t.Run(fault, func(t *testing.T) {
			schedule, err := manualreview.ParseOnCallSchedule(uuid.NewString(), true)
			if err != nil {
				t.Fatal(err)
			}
			store := &manualWorkerFaultStore{fault: fault}
			worker := &ManualReviewSLAWorker{repository: store, schedule: schedule, interval: time.Minute}
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "account", Environment: "test"}))
			defer slog.SetDefault(previous)
			worker.runOne(context.Background())
			var record struct {
				Event   string `json:"event"`
				Code    string `json:"error_code"`
				Result  string `json:"result"`
				Service string `json:"service"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil || record.Event != expectedEvents[fault] || record.Code != strings.ToUpper(expectedEvents[fault]) || record.Result != "failure" || record.Service != "account" {
				t.Fatal("actual safe logger lost the worker failure", record, err)
			}
			if strings.Contains(output.String(), "controlled-private") || strings.Contains(output.String(), "+8613800001234") {
				t.Fatal("raw repository error leaked")
			}
			store.fault = ""
			worker.runOne(context.Background())
			if !reflect.DeepEqual(store.calls, append(append([]string{}, expectedSteps...), expectedSteps...)) || store.badLimit || !reflect.DeepEqual(store.gotSchedule, schedule) {
				t.Fatal("failure logging changed the independent steps, batch limit or on-call schedule", store.calls)
			}
			before := output.String()
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			worker.runOne(cancelled)
			if output.String() != before {
				t.Fatal("normal shutdown cancellation became a worker failure")
			}
		})
	}
}

func TestManualReviewWorkerRunsStartupAndLaterPassAfterFailure(t *testing.T) {
	expectedSteps := []string{"sync", "escalate"}
	schedule, err := manualreview.ParseOnCallSchedule(uuid.NewString(), true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &manualWorkerFaultStore{fault: expectedSteps[0]}
	store.onStep = func(name string) {
		if name == expectedSteps[len(expectedSteps)-1] {
			if len(store.calls) == len(expectedSteps) {
				store.fault = ""
			} else if len(store.calls) == 2*len(expectedSteps) {
				cancel()
			}
		}
	}
	worker := &ManualReviewSLAWorker{repository: store, schedule: schedule, interval: time.Nanosecond}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "account"}))
	defer slog.SetDefault(previous)
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned worker did not acknowledge cancellation")
		}
		t.Fatal("real startup/ticker loop stopped progressing")
	}
	if !reflect.DeepEqual(store.calls, append(append([]string{}, expectedSteps...), expectedSteps...)) || store.badLimit {
		t.Fatal("startup fault prevented the later complete pass", store.calls)
	}
}

func TestManualReviewWorkerBrokenLogSinkPreservesRemainingSteps(t *testing.T) {
	expectedSteps := []string{"sync", "escalate"}
	store := &manualWorkerFaultStore{fault: expectedSteps[0]}
	writer := &manualWorkerFailingLogWriter{}
	previous := slog.Default()
	slog.SetDefault(observability.NewLogger(writer, observability.Config{Service: "account"}))
	defer slog.SetDefault(previous)
	worker := &ManualReviewSLAWorker{repository: store, interval: time.Minute}
	worker.runOne(context.Background())
	store.fault = ""
	worker.runOne(context.Background())
	if writer.calls != 1 || !reflect.DeepEqual(store.calls, append(append([]string{}, expectedSteps...), expectedSteps...)) {
		t.Fatal("unavailable optional logging blocked domain work", writer.calls, store.calls)
	}
}

func TestManualReviewWorkerArgumentsRemainPrivateWithoutFiltering(t *testing.T) {
	expectedSteps := []string{"sync", "escalate"}
	for _, fault := range expectedSteps {
		store := &manualWorkerFaultStore{fault: fault}
		worker := &ManualReviewSLAWorker{repository: store, interval: time.Minute}
		var output bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		worker.runOne(context.Background())
		slog.SetDefault(previous)
		if output.Len() == 0 || strings.Contains(output.String(), "controlled-private") || strings.Contains(output.String(), "+8613800001234") {
			t.Fatal("worker passed a raw repository error to its logger")
		}
	}
}

func TestManualReviewWorkerShutdownKeepsIndependentStorageFailure(t *testing.T) {
	expectedSteps := []string{"sync", "escalate"}
	expectedEvents := map[string]string{"sync": "manual_review_sync_failed", "escalate": "manual_review_escalation_failed"}
	for _, tc := range []struct {
		name             string
		err              error
		cancelDuringStep bool
		wantLog          bool
	}{
		{"shutdown-independent-storage-failure", errors.New("controlled-private-repository-token +8613800001234"), true, true},
		{"shutdown-wrapped-cancellation", fmt.Errorf("controlled wrapped cancellation: %w", context.Canceled), true, false},
		{"active-dependency-cancellation", context.Canceled, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &manualWorkerFaultStore{fault: expectedSteps[0], faultErr: tc.err}
			if tc.cancelDuringStep {
				store.onStep = func(name string) {
					if name == expectedSteps[0] {
						cancel()
					}
				}
			}
			worker := &ManualReviewSLAWorker{repository: store, interval: time.Minute}
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "account"}))
			defer slog.SetDefault(previous)
			worker.runOne(ctx)
			if tc.wantLog {
				var record struct {
					Event string `json:"event"`
					Code  string `json:"error_code"`
				}
				if err := json.Unmarshal(output.Bytes(), &record); err != nil || record.Event != expectedEvents[expectedSteps[0]] || record.Code != strings.ToUpper(expectedEvents[expectedSteps[0]]) {
					t.Fatal("shutdown suppressed an independent operation failure", record, err)
				}
			} else if output.Len() != 0 {
				t.Fatal("actual wrapped shutdown cancellation emitted a failure")
			}
			if strings.Contains(output.String(), "controlled-private") || strings.Contains(output.String(), "+8613800001234") {
				t.Fatal("shutdown failure log exposed the repository error")
			}
			firstCalls := len(store.calls)
			before := output.String()
			store.fault = ""
			store.onStep = nil
			worker.runOne(context.Background())
			if !reflect.DeepEqual(store.calls[firstCalls:], expectedSteps) || store.badLimit || output.String() != before {
				t.Fatal("shutdown observation changed the next independent pass", store.calls)
			}
		})
	}
}
