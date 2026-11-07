package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

type recoveryReportingStore struct {
	err        error
	cleanupErr error
	steps      []string
}

func (s *recoveryReportingStore) RecoverUncertainDispatches(context.Context) error {
	s.steps = append(s.steps, "recover")
	return s.err
}

func (s *recoveryReportingStore) CleanupReplayRetention(context.Context) error {
	s.steps = append(s.steps, "cleanup")
	return s.cleanupErr
}

func TestReplayRetentionRecoveryErrorIsVisibleWithoutPrivateCause(t *testing.T) {
	store := &recoveryReportingStore{err: errors.New("controlled-private-database-value")}
	worker := NewReplayRetentionWorker(store, time.Hour)
	var logs bytes.Buffer
	worker.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	worker.runCycle(context.Background())
	if !reflect.DeepEqual(store.steps, []string{"recover", "cleanup"}) {
		t.Fatal("recovery error prevented retention cleanup", store.steps)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal("expected one structured error event", err)
	}
	if event["level"] != "ERROR" || event["error_code"] != "UNCERTAIN_DISPATCH_RECOVERY_RETRY_PENDING" || strings.Contains(logs.String(), store.err.Error()) {
		t.Fatal("missing operational code or private error leaked", logs.String())
	}
}

func TestReplayRetentionDoesNotLogSuccessfulOrCanceledRecovery(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		store := &recoveryReportingStore{}
		if canceled {
			cancel()
			store.err = context.Canceled
		}
		worker := NewReplayRetentionWorker(store, time.Hour)
		var logs bytes.Buffer
		worker.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		worker.runCycle(ctx)
		cancel()
		if logs.Len() != 0 || !reflect.DeepEqual(store.steps, []string{"recover", "cleanup"}) {
			t.Fatalf("success/cancellation produced an error or changed order: canceled=%t logs=%s", canceled, logs.String())
		}
	}
}

func TestReplayRetentionCleanupFailureIsPrivateAndNextCycleContinues(t *testing.T) {
	store := &recoveryReportingStore{cleanupErr: errors.New("controlled-private-retention-value")}
	worker := NewReplayRetentionWorker(store, time.Hour)
	var logs bytes.Buffer
	worker.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	worker.runCycle(context.Background())
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal("expected one structured retention failure", err)
	}
	if event["error_code"] != "REPLAY_RETENTION_CLEANUP_RETRY_PENDING" || strings.Contains(logs.String(), store.cleanupErr.Error()) {
		t.Fatal("retention failure hidden or private error exposed", logs.String())
	}
	store.cleanupErr = nil
	logs.Reset()
	worker.runCycle(context.Background())
	if logs.Len() != 0 || !reflect.DeepEqual(store.steps, []string{"recover", "cleanup", "recover", "cleanup"}) {
		t.Fatal("healthy next cycle did not continue normally", store.steps, logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store.cleanupErr = context.Canceled
	worker.runCycle(ctx)
	if logs.Len() != 0 {
		t.Fatal("shutdown cancellation reported as retention error", logs.String())
	}
}
