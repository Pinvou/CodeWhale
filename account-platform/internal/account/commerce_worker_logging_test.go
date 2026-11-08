package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type commerceWorkerFaultStore struct {
	job                             *CommerceCustomerSyncJob
	claimErr, retryErr, completeErr error
	calls                           []string
	lease                           time.Duration
	provider, version, reason       string
	retryAt                         time.Time
	persistenceContextError         error
}

func commerceWorkerLogJob(version string) *CommerceCustomerSyncJob {
	return &CommerceCustomerSyncJob{ID: uuid.New(), MappingID: uuid.New(), MappingVersion: 1, SubjectType: "USER", SubjectID: uuid.New(), SubjectLifecycleVersion: 1, ProviderID: "provider-a", AdapterVersion: version, TaskCategory: "CUSTOMER_PROVISIONING", ContactMobile: "+8613800001234", Attempts: 2}
}

func (s *commerceWorkerFaultStore) ClaimCommerceCustomerSyncJob(_ context.Context, provider, version string, lease time.Duration) (*CommerceCustomerSyncJob, error) {
	s.calls = append(s.calls, "claim")
	s.provider, s.version, s.lease = provider, version, lease
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	return s.job, nil
}
func (s *commerceWorkerFaultStore) RetryCommerceCustomerSyncJob(ctx context.Context, _ CommerceCustomerSyncJob, reason string, at time.Time) error {
	s.calls = append(s.calls, "retry")
	s.reason, s.retryAt, s.persistenceContextError = reason, at, ctx.Err()
	return s.retryErr
}
func (s *commerceWorkerFaultStore) CompleteCommerceCustomerSyncJob(ctx context.Context, _ CommerceCustomerSyncJob, _ commerce.CustomerProvisionResult) error {
	s.calls = append(s.calls, "complete")
	s.persistenceContextError = ctx.Err()
	return s.completeErr
}

type commerceWorkerFaultAdapter struct {
	err      error
	onEnsure func()
	requests []commerce.CustomerProvisionRequest
}

func (a *commerceWorkerFaultAdapter) EnsureCustomer(_ context.Context, request commerce.CustomerProvisionRequest) (commerce.CustomerProvisionResult, error) {
	a.requests = append(a.requests, request)
	if a.onEnsure != nil {
		a.onEnsure()
	}
	return commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1234"}, a.err
}

type commerceWorkerLog struct {
	Event   string `json:"event"`
	Code    string `json:"error_code"`
	Result  string `json:"result"`
	Service string `json:"service"`
}

func captureCommerceWorkerLogs(t *testing.T, safe bool, run func()) ([]commerceWorkerLog, string) {
	t.Helper()
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	if safe {
		logger = observability.NewLogger(&output, observability.Config{Service: "account", Environment: "test"})
	}
	previous := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(previous)
	run()
	var records []commerceWorkerLog
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	for {
		var record commerceWorkerLog
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records, output.String()
}

func TestCommerceWorkerFailureEventsUseDefaultSafeLogger(t *testing.T) {
	for _, stage := range []string{"claim", "retry", "complete", "idle", "cancelled-claim", "provider-retry-recorded"} {
		t.Run(stage, func(t *testing.T) {
			job := commerceWorkerLogJob("adapter-v2")
			secret := errors.New("controlled-secret Authorization Bearer private-provider-message +8613800001234")
			store, adapter := &commerceWorkerFaultStore{job: job}, &commerceWorkerFaultAdapter{}
			ctx := context.Background()
			expectedEvent, expectedCode := "", ""
			switch stage {
			case "claim":
				store.claimErr = secret
				expectedEvent, expectedCode = "commerce_customer_sync_claim_failed", "COMMERCE_SYNC_CLAIM_FAILED"
			case "retry":
				adapter.err, store.retryErr = secret, secret
				expectedEvent, expectedCode = "commerce_customer_sync_retry_failed", "COMMERCE_SYNC_RETRY_FAILED"
			case "complete":
				store.completeErr = secret
				expectedEvent, expectedCode = "commerce_customer_sync_complete_failed", "COMMERCE_SYNC_COMPLETE_FAILED"
			case "idle":
				store.job = nil
			case "cancelled-claim":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				store.claimErr = context.Canceled
			case "provider-retry-recorded":
				adapter.err = secret
			}
			worker := NewCommerceCustomerSyncWorker(store, adapter, " provider-a ", " adapter-v2 ", time.Second)
			before := time.Now().UTC()
			records, body := captureCommerceWorkerLogs(t, true, func() { worker.runOne(ctx) })
			after := time.Now().UTC()
			if expectedEvent == "" {
				if len(records) != 0 {
					t.Fatal("idle, recorded provider retry or normal shutdown emitted a worker persistence failure", records)
				}
			} else if len(records) != 1 || records[0].Event != expectedEvent || records[0].Code != expectedCode || records[0].Result != "failure" || records[0].Service != "account" {
				t.Fatal("failure did not survive the actual default safe handler", records)
			}
			for _, private := range []string{"controlled-secret", "private-provider-message", job.ContactMobile, job.SubjectID.String(), job.ID.String()} {
				if strings.Contains(body, private) {
					t.Fatal("worker log exposed source data")
				}
			}
			if store.provider != "provider-a" || store.version != "adapter-v2" || store.lease != 30*time.Second {
				t.Fatal("logging changed claim scope or lease")
			}
			expectedCalls := []string{"claim"}
			if stage == "retry" || stage == "provider-retry-recorded" {
				expectedCalls = append(expectedCalls, "retry")
				if store.reason != "COMMERCE_PROVIDER_UNAVAILABLE" || store.retryAt.Before(before.Add(4*time.Minute)) || store.retryAt.After(after.Add(4*time.Minute)) || store.persistenceContextError != nil {
					t.Fatal("logging changed durable retry identity/backoff/context")
				}
			} else if stage == "complete" {
				expectedCalls = append(expectedCalls, "complete")
			}
			if !reflect.DeepEqual(store.calls, expectedCalls) {
				t.Fatal("logging changed business steps", store.calls)
			}
		})
	}
}

func TestCommerceWorkerUnknownCompletionKeepsOriginalCustomerIdentity(t *testing.T) {
	job := commerceWorkerLogJob("v1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &commerceWorkerFaultStore{job: job, completeErr: errors.New("controlled local completion ACK lost")}
	adapter := &commerceWorkerFaultAdapter{onEnsure: cancel}
	worker := NewCommerceCustomerSyncWorker(store, adapter, "provider-a", "v1", time.Second)
	records, _ := captureCommerceWorkerLogs(t, true, func() { worker.runOne(ctx) })
	if len(records) != 1 || records[0].Code != "COMMERCE_SYNC_COMPLETE_FAILED" || store.persistenceContextError != nil {
		t.Fatal("parent cancellation hid the real WithoutCancel persistence failure", records, store.persistenceContextError)
	}
	// The store controls a later claim after its existing lease; the worker must
	// preserve the provider's original subject key, not invent a dispatch ID.
	store.completeErr = nil
	adapter.onEnsure = nil
	records, _ = captureCommerceWorkerLogs(t, true, func() { worker.runOne(context.Background()) })
	if len(records) != 0 || len(adapter.requests) != 2 || adapter.requests[0] != adapter.requests[1] || !reflect.DeepEqual(store.calls, []string{"claim", "complete", "claim", "complete"}) {
		t.Fatal("unknown completion changed the next claim/provider identity", store.calls, adapter.requests)
	}
}

type commerceBrokenLogWriter struct{ calls int }

func (w *commerceBrokenLogWriter) Write([]byte) (int, error) { w.calls++; return 0, io.ErrClosedPipe }
func TestCommerceWorkerLogWriteFailureDoesNotChangeBusinessFlow(t *testing.T) {
	store := &commerceWorkerFaultStore{job: commerceWorkerLogJob("v1"), retryErr: errors.New("controlled persistence failure")}
	adapter := &commerceWorkerFaultAdapter{err: errors.New("controlled provider failure")}
	worker := NewCommerceCustomerSyncWorker(store, adapter, "provider-a", "v1", time.Second)
	writer := &commerceBrokenLogWriter{}
	previous := slog.Default()
	slog.SetDefault(observability.NewLogger(writer, observability.Config{Service: "account"}))
	defer slog.SetDefault(previous)
	worker.runOne(context.Background())
	adapter.err = nil
	store.completeErr = errors.New("controlled completion failure")
	worker.runOne(context.Background())
	if writer.calls != 2 || !reflect.DeepEqual(store.calls, []string{"claim", "retry", "claim", "complete"}) {
		t.Fatal("failed optional log sink blocked the original flow", writer.calls, store.calls)
	}
}

func TestCommerceWorkerLogArgumentsAreSafeWithoutFiltering(t *testing.T) {
	for _, stage := range []string{"claim", "retry", "complete"} {
		t.Run(stage, func(t *testing.T) {
			private := "controlled-private-token +8613800001234"
			store := &commerceWorkerFaultStore{job: commerceWorkerLogJob("v1")}
			adapter := &commerceWorkerFaultAdapter{}
			switch stage {
			case "claim":
				store.claimErr = errors.New(private)
			case "retry":
				store.retryErr = errors.New(private)
				adapter.err = errors.New(private)
			case "complete":
				store.completeErr = errors.New(private)
			}
			worker := NewCommerceCustomerSyncWorker(store, adapter, "provider-a", "v1", time.Second)
			_, body := captureCommerceWorkerLogs(t, false, func() { worker.runOne(context.Background()) })
			if body == "" || strings.Contains(body, private) || strings.Contains(body, store.job.SubjectID.String()) || strings.Contains(body, store.job.ID.String()) {
				t.Fatal("raw logger arguments included source identities or provider/storage errors")
			}
		})
	}
}
