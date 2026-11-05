package token

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type streamTransportRepository struct {
	*streamRepositoryFake
	usage    UsageRequest
	envelope DispatchEnvelope
	mode     string
	loads    int
}

func (r *streamTransportRepository) FindUsageByIdempotency(context.Context, uuid.UUID, string) (UsageRequest, error) {
	return r.usage, nil
}
func (r *streamTransportRepository) AcquireDispatch(context.Context, uuid.UUID, string, time.Duration) (DispatchEnvelope, bool, error) {
	return r.envelope, true, nil
}
func (r *streamTransportRepository) LoadReplay(ctx context.Context, _ uuid.UUID, after int64) (ReplaySnapshot, error) {
	r.loads++
	if r.mode == "live" && r.loads == 1 {
		return ReplaySnapshot{Usage: UsageRequest{ExecutionStatus: "STREAMING"}, Events: []ReplayEvent{{Sequence: 1, EventType: "CHUNK", Payload: json.RawMessage(`{"part":"first"}`)}}}, nil
	}
	select {
	case <-time.After(31500 * time.Millisecond):
	case <-ctx.Done():
		return ReplaySnapshot{}, ctx.Err()
	}
	events := []ReplayEvent{{Sequence: 2, EventType: "CHUNK", Payload: json.RawMessage(`{"part":"late"}`)}, {Sequence: 3, EventType: "DONE", Payload: []byte("[DONE]")}}
	return ReplaySnapshot{Usage: UsageRequest{ExecutionStatus: "SUCCEEDED", Status: "COMMITTED"}, Result: &ReplayResult{Kind: "STREAM_AGGREGATE"}, Events: events}, nil
}
func streamTransportService(t *testing.T, mode string, provider *httptest.Server) (*Service, *streamTransportRepository, []byte) {
	t.Helper()
	service, _, closeAuthority := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: uuid.New(), ContextType: "PERSONAL"})
	t.Cleanup(closeAuthority)
	body := []byte(`{"model":"controlled","stream":true,"messages":[],"max_tokens":10}`)
	logical, err := normalizeInferenceBody(body, true)
	if err != nil {
		t.Fatal(err)
	}
	hold := uuid.New()
	usage := UsageRequest{ID: uuid.New(), HoldID: &hold, CanonicalVersion: 1, RequestFingerprint: inferenceFingerprint("CHAT", 1, logical), ExecutionStatus: "DISPATCH_INTENT_PERSISTED", RequestSnapshot: json.RawMessage(`{"provider_id":"controlled","adapter_version":"` + service.executionUpstream.AdapterVersion() + `"}`)}
	if mode == "live" {
		usage.ExecutionStatus = "STREAMING"
	}
	if mode == "stored" {
		usage.ExecutionStatus = "SUCCEEDED"
	}
	repository := &streamTransportRepository{streamRepositoryFake: &streamRepositoryFake{}, usage: usage, mode: mode}
	if provider != nil {
		repository.envelope = DispatchEnvelope{BaseURL: provider.URL, Provider: "controlled", AdapterVersion: service.executionUpstream.AdapterVersion(), Credential: []byte("controlled-local-fixture"), RequestBody: body, Price: PriceVersion{OutputPerMillion: 1000000}, Stream: true, MaximumCost: 100}
	}
	service.repository = repository
	service.executionBilling = &streamBillingFake{}
	service.breaker = resilience.NewBreaker()
	return service, repository, body
}
func startStreamTransportServer(t *testing.T, service *Service, observers ...func(*http.Request)) (string, <-chan struct{}) {
	t.Helper()
	cfg := config.Config{ServiceName: "token-gateway", Environment: "test", RateLimitRPS: 100, RateLimitBurst: 200}
	application := httpserver.WithStreamingWriteDeadline(service.Handler(successfulBillingVerify))
	wrapped := httpserver.HandlerWithApplication(cfg, application, nil)
	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		for _, observe := range observers {
			observe(r)
		}
		wrapped.ServeHTTP(w, r)
	})
	server := httpserver.NewApplicationServer(cfg, handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return "http://" + listener.Addr().String(), done
}
func streamTransportRequest(t *testing.T, url string, body []byte) (*http.Response, error) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer controlled-oidc")
	r.Header.Set("Idempotency-Key", "controlled-stream-transport-key")
	r.Header.Set("Content-Type", "application/json")
	return (&http.Client{Timeout: 40 * time.Second}).Do(r)
}

// Actual shared server constructor/middleware, public authenticated Token route,
// protocol adapter and SSE delivery/persistence logic. Repository, authorization
// facts and Billing are controlled in memory; this is not a real funds/PG E2E.
func TestTokenSSESharedHTTPExceedsThirtySeconds(t *testing.T) {
	for _, mode := range []string{"dispatch", "live", "stored"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var provider *httptest.Server
			var calls atomic.Int64
			if mode == "dispatch" {
				provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/v1/chat/completions" {
						t.Error("wrong actual upstream route")
					}
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"part\":\"first\"}\n\n")
					http.NewResponseController(w).Flush()
					select {
					case <-time.After(31500 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					io.WriteString(w, "data: {\"part\":\"late\",\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
				}))
				defer provider.Close()
			}
			service, repository, body := streamTransportService(t, mode, provider)
			url, done := startStreamTransportServer(t, service)
			start := time.Now()
			response, err := streamTransportRequest(t, url, body)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
				payload, _ := io.ReadAll(response.Body)
				t.Fatalf("headers=%v status=%d body=%s", response.Header, response.StatusCode, payload)
			}
			reader := bufio.NewReader(response.Body)
			first, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if mode != "stored" && time.Since(start) > 5*time.Second {
				t.Fatal("first frame was buffered until late provider output")
			}
			rest, err := io.ReadAll(reader)
			<-done
			payload := first + string(rest)
			if err != nil || !strings.Contains(payload, "late") || !strings.Contains(payload, "[DONE]") || time.Since(start) < 31*time.Second {
				t.Fatalf("truncated long %s stream: payload=%q err=%v elapsed=%v", mode, payload, err, time.Since(start))
			}
			if mode == "dispatch" {
				billing := service.executionBilling.(*streamBillingFake)
				if calls.Load() != 1 || !repository.resultSaved || !repository.committed || repository.reviewReason != "" || billing.commitCalls != 1 {
					t.Fatalf("unexpected actual dispatch/persistence/settlement: calls=%d saved=%v committed=%v review=%q commitCalls=%d", calls.Load(), repository.resultSaved, repository.committed, repository.reviewReason, billing.commitCalls)
				}
			}
		})
	}
}

func TestTokenSSETransportRetainsRegisteredUpstreamCancellation(t *testing.T) {
	providerCancelled := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"part\":\"first\"}\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(providerCancelled)
	}))
	defer provider.Close()
	service, repository, body := streamTransportService(t, "dispatch", provider)
	url, done := startStreamTransportServer(t, service)
	response, err := streamTransportRequest(t, url, body)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if _, err = reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Uses the same registered upstream cancellation function as the emergency
	// cancellation worker, not a new inferred provider-cancel capability.
	if !service.abortActiveUpstream(repository.usage.ID) {
		t.Fatal("active upstream cancellation missing")
	}
	select {
	case <-providerCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP provider context was not canceled")
	}
	io.Copy(io.Discard, reader)
	response.Body.Close()
	<-done
	if repository.committed || repository.reviewReason != "stream_terminal_missing" {
		t.Fatalf("unknown canceled usage fabricated success: committed=%v review=%q", repository.committed, repository.reviewReason)
	}
}

func TestTokenSSETransportDisconnectRetainsDetachedDispatch(t *testing.T) {
	release := make(chan struct{})
	var premature atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"part\":\"first\"}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			premature.Store(true)
			return
		case <-release:
		}
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	service, repository, body := streamTransportService(t, "dispatch", provider)
	requestCancelled := make(chan struct{})
	url, done := startStreamTransportServer(t, service, func(r *http.Request) { go func() { <-r.Context().Done(); close(requestCancelled) }() })
	response, err := streamTransportRequest(t, url, body)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if _, err = reader.ReadString('\n'); err != nil {
		close(release)
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-requestCancelled:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("client disconnect not propagated to request")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("detached dispatch failed to finish")
	}
	if premature.Load() || !repository.committed || repository.reviewReason != "" || !strings.Contains(strings.Join(repository.deliveries, ","), "DISCONNECTED") {
		t.Fatalf("disconnect changed detached settlement policy: premature=%v committed=%v review=%q deliveries=%v", premature.Load(), repository.committed, repository.reviewReason, repository.deliveries)
	}
}
