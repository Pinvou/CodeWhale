package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

type terminalResponseRepository struct {
	*streamRepositoryFake
	missingReferenceDeadlineStore
	current            UsageRequest
	afterAuthorization bool
	terminal           bool
	finalErr           error
	finalCalls         int
}

func (r *terminalResponseRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	current := r.current
	if !r.terminal {
		current.Status = "UPSTREAM_ACTIVE"
	}
	return current, nil
}
func (r *terminalResponseRepository) PreserveMissingReferenceLateUsage(context.Context, uuid.UUID, Usage) (bool, error) {
	if r.afterAuthorization {
		r.terminal = true
		return true, nil
	}
	return false, nil
}
func (r *terminalResponseRepository) RecordFinalUsage(_ context.Context, id uuid.UUID, provider, adapter string, usage Usage) error {
	r.finalCalls++
	if id != r.current.ID || provider != "test" || adapter != "openai-compatible-v1" || usage.OutputTokens != 10 {
		return ErrForbidden
	}
	return r.finalErr
}

// The real upstream HTTP adapter/dispatch/replay producer is used. The terminal
// transition is controlled at the repository boundary, not a PostgreSQL race.
func TestNonStreamingTerminalRaceReturnsOnlyRetainedProjection(t *testing.T) {
	for _, name := range []string{"status pending", "stored original", "expired original", "stale dispatch phase", "stale streaming phase", "deadline during authorization", "evidence failure"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"NEW_LATE_BODY"}}],"usage":{"prompt_tokens":0,"completion_tokens":10}}`))
			}))
			defer server.Close()
			usageID, holdID := uuid.New(), uuid.New()
			current := UsageRequest{ID: usageID, Status: "RELEASED", ExecutionStatus: "REVIEW", RequestSnapshot: json.RawMessage(`{"provider_id":"test","adapter_version":"openai-compatible-v1"}`)}
			repository := &terminalResponseRepository{streamRepositoryFake: &streamRepositoryFake{}, current: current, terminal: true}
			wantStatus, wantBody := http.StatusAccepted, "USAGE_REVIEW"
			switch name {
			case "stored original":
				repository.current.ExecutionStatus = "SUCCEEDED"
				repository.replaySnapshot.Result = &ReplayResult{Kind: "JSON", HTTPStatus: 200, ContentType: "application/json", Payload: []byte(`{"content":"OLD_RETAINED_BODY"}`)}
				wantStatus, wantBody = 200, "OLD_RETAINED_BODY"
			case "expired original":
				repository.replaySnapshot.ResultExpired = true
				wantStatus, wantBody = 410, "IDEMPOTENCY_RESULT_EXPIRED"
			case "stale dispatch phase":
				repository.current.ExecutionStatus = "RECEIVED"
				wantBody = "USAGE_RECEIVED"
			case "stale streaming phase":
				repository.current.ExecutionStatus = "STREAMING"
				wantBody = "USAGE_RESULT_PENDING"
			case "deadline during authorization":
				repository.terminal = false
				repository.afterAuthorization = true
			case "evidence failure":
				repository.finalErr = errors.New("controlled evidence persistence error")
				wantStatus, wantBody = 503, "RESULT_PERSISTENCE_FAILED"
			}
			money := &streamBillingFake{}
			service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient(), breaker: resilience.NewBreaker()}
			out := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			service.dispatchUsage(out, request, UsageRequest{ID: usageID, HoldID: &holdID, RequestSnapshot: current.RequestSnapshot}, DispatchEnvelope{leaseOwner: "controlled-worker", BaseURL: server.URL, Provider: "test", AdapterVersion: "openai-compatible-v1", Credential: []byte("controlled-credential"), RequestBody: []byte(`{"model":"test"}`), Price: PriceVersion{OutputPerMillion: 1000000}, MaximumCost: 10, ContextType: "PERSONAL"})
			if out.Code != wantStatus || !strings.Contains(out.Body.String(), wantBody) || strings.Contains(out.Body.String(), "NEW_LATE_BODY") {
				t.Fatalf("terminal response: status=%d body=%q want=%d/%q", out.Code, out.Body.String(), wantStatus, wantBody)
			}
			if repository.finalCalls != 1 || repository.resultSaved || repository.committed || repository.released || money.extendCalls != 0 || money.commitCalls != 0 || money.releaseCalls != 0 {
				t.Fatalf("terminal evidence reopened financial/body processing: final=%d repository=%+v money=%+v", repository.finalCalls, repository.streamRepositoryFake, money)
			}
			if name != "evidence failure" && (out.Header().Get("Cache-Control") != "no-store" || out.Header().Get("X-Pinvou-Usage-Request-ID") != usageID.String()) {
				t.Fatalf("terminal status lost original replay headers: %v", out.Header())
			}
		})
	}
}
