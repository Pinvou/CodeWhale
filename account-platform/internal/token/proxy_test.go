package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type streamRepositoryFake struct {
	Repository
	events        []string
	deliveries    []string
	reviewReason  string
	responseSaved bool
	resultSaved   bool
	committed     bool
	increaseCalls int
	increasedBy   int64
}

func (r *streamRepositoryFake) AppendReplayEvent(_ context.Context, _ uuid.UUID, eventType string, _ []byte) (int64, error) {
	r.events = append(r.events, eventType)
	return int64(len(r.events)), nil
}

func (r *streamRepositoryFake) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	return UsageRequest{DeliveryStatus: "DELIVERING"}, nil
}

func (r *streamRepositoryFake) MarkDelivery(_ context.Context, _ uuid.UUID, status string) error {
	r.deliveries = append(r.deliveries, status)
	return nil
}

func (r *streamRepositoryFake) MarkReview(_ context.Context, _ uuid.UUID, reason string, _ map[string]any) error {
	r.reviewReason = reason
	return nil
}

func (r *streamRepositoryFake) MarkResponseReceived(context.Context, uuid.UUID) error {
	r.responseSaved = true
	return nil
}

func (r *streamRepositoryFake) SaveReplayResult(context.Context, uuid.UUID, ReplayResult) error {
	r.resultSaved = true
	return nil
}

func (r *streamRepositoryFake) MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error {
	r.committed = true
	return nil
}

func (r *streamRepositoryFake) IncreaseMaximum(_ context.Context, _ uuid.UUID, additional int64) error {
	r.increaseCalls++
	r.increasedBy += additional
	return nil
}

type streamBillingFake struct {
	BillingClient
	extendCalls          int
	extendedBy           int64
	extendErr            error
	commitCalls          int
	terminalSeenAtExtend func() bool
	terminalBeforeExtend bool
}

func (b *streamBillingFake) Extend(_ context.Context, _ uuid.UUID, _ uuid.UUID, additional int64) error {
	b.extendCalls++
	b.extendedBy += additional
	if b.terminalSeenAtExtend != nil {
		b.terminalBeforeExtend = b.terminalSeenAtExtend()
	}
	return b.extendErr
}

func (b *streamBillingFake) ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, HoldPricingContext) (BillingHold, error) {
	return BillingHold{}, nil
}

func (b *streamBillingFake) Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error {
	b.commitCalls++
	return nil
}

func TestGatewayAdaptersOwnProtocolAndReferenceMapping(t *testing.T) {
	response := &http.Response{Header: http.Header{"X-Request-Id": []string{"generic-request"}, "X-Oneapi-Request-Id": []string{"newapi-request"}}}
	generic := NewHTTPUpstreamClient()
	newAPI := NewNewAPIGatewayAdapter()
	if generic.UpstreamRequestID(response) != "generic-request" || newAPI.UpstreamRequestID(response) != "newapi-request" {
		t.Fatal("provider response headers escaped their gateway adapters")
	}
	forwarded := http.Header{}
	newAPI.CopyResponseHeaders(forwarded, response.Header)
	if forwarded.Get("X-Oneapi-Request-Id") != "" || forwarded.Get("X-Request-Id") != "generic-request" {
		t.Fatal("provider-specific evidence headers must not escape the selected adapter")
	}
	protocols := newAPI.Protocols()
	if len(protocols) != 1 || protocols[0].ProtocolCode != "CHAT" || protocols[0].ProtocolVersion != 1 || !protocols[0].Streaming {
		t.Fatalf("unexpected protocol capabilities: %#v", protocols)
	}
}

func TestNormalizeLogicalTurnID(t *testing.T) {
	valid := "pinvou-turn-0123456789abcdef"
	if got, err := normalizeLogicalTurnID(valid); err != nil || got != valid {
		t.Fatalf("logical turn ID = %q, %v", got, err)
	}
	if got, err := normalizeLogicalTurnID(""); err != nil || got != "" {
		t.Fatalf("empty logical turn ID = %q, %v", got, err)
	}
	for _, invalid := range []string{"short", "contains spaces and punctuation!"} {
		if _, err := normalizeLogicalTurnID(invalid); err == nil {
			t.Fatalf("accepted invalid logical turn ID %q", invalid)
		}
	}
}

func TestParseUsageJSON(t *testing.T) {
	usage, ok := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if !ok || usage.InputTokens != 8 || usage.OutputTokens != 7 || usage.CachedInputTokens != 3 {
		t.Fatalf("unexpected usage: %#v, %v", usage, ok)
	}
}

func TestParseUsageJSONRejectsMissingAndInvalidUsage(t *testing.T) {
	for _, payload := range []string{`{"choices":[]}`, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`} {
		if _, ok := parseUsageJSON([]byte(payload)); ok {
			t.Fatalf("accepted invalid usage: %s", payload)
		}
	}
}

func TestInferenceFingerprintIsCanonicalAndTransportIndependent(t *testing.T) {
	first, err := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"temperature":0.5}`), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeInferenceBody([]byte(`{"temperature":0.5,"messages":[],"model":"example-model"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if inferenceFingerprint("CHAT", 1, first) != inferenceFingerprint("CHAT", 1, second) {
		t.Fatal("equivalent JSON requests produced different fingerprints")
	}
	changed, _ := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"temperature":0.7}`), false)
	if inferenceFingerprint("CHAT", 1, first) == inferenceFingerprint("CHAT", 1, changed) {
		t.Fatal("semantic request changes must change the fingerprint")
	}
}

func TestStreamingCanonicalizationForcesUsageEvidence(t *testing.T) {
	body, err := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"stream":true}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"messages":[],"model":"example-model","stream":true,"stream_options":{"include_usage":true}}` {
		t.Fatalf("unexpected canonical stream request: %s", body)
	}
}

func TestProxyStreamDoesNotChargeSSEEnvelopeBytesBeyondReservedOutput(t *testing.T) {
	chunk, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": strings.Repeat("思", 10_000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := bytes.NewBuffer(nil)
	stream.WriteString("data: ")
	stream.Write(chunk)
	stream.WriteString("\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// The request's full maximum cost is already reserved, so transport
	// framing must never trigger another authorization or truncate the response.
	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 100)

	if billing.extendCalls != 0 {
		t.Fatalf("SSE transport bytes triggered %d billing extension(s)", billing.extendCalls)
	}
	if !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatal("validated stream terminal was not delivered")
	}
	if repository.reviewReason != "" || !repository.responseSaved || !repository.resultSaved || !repository.committed {
		t.Fatalf("stream did not settle cleanly: %#v", repository)
	}
	if got := strings.Join(repository.events, ","); got != "CHUNK,CHUNK,DONE" {
		t.Fatalf("persisted event order = %q", got)
	}
}

func TestProxyStreamWithholdsDoneWhenUsageEvidenceIsMissing(t *testing.T) {
	repository := &streamRepositoryFake{}
	service := &Service{repository: repository, billing: &streamBillingFake{}, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{}, strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"), make(chan struct{}), 0)

	if strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatal("unvalidated terminal must not be delivered as a successful stream")
	}
	if repository.reviewReason != "stream_usage_missing" {
		t.Fatalf("review reason = %q", repository.reviewReason)
	}
	if repository.responseSaved || repository.resultSaved || repository.committed {
		t.Fatalf("invalid stream was settled: %#v", repository)
	}
}

func TestProxyStreamMarksMissingTerminalForReconciliation(t *testing.T) {
	repository := &streamRepositoryFake{}
	service := &Service{repository: repository, billing: &streamBillingFake{}, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{}, strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10}}\n\n"), make(chan struct{}), 0)

	if repository.reviewReason != "stream_terminal_missing" {
		t.Fatalf("review reason = %q", repository.reviewReason)
	}
	if repository.responseSaved || repository.resultSaved || repository.committed {
		t.Fatalf("unterminated stream was settled: %#v", repository)
	}
}

func TestProxyStreamWithholdsDoneWhenAdditionalAuthorizationFails(t *testing.T) {
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{extendErr: errors.New("insufficient balance")}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := strings.NewReader("data: {\"choices\":[]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 1)

	if billing.extendCalls != 1 || billing.extendedBy != 9 {
		t.Fatalf("additional authorization = %d calls, %d microcredits", billing.extendCalls, billing.extendedBy)
	}
	if strings.Contains(recorder.Body.String(), "data: [DONE]") || strings.Contains(strings.Join(repository.events, ","), "DONE") {
		t.Fatal("failed additional authorization persisted or delivered a successful terminal")
	}
	if repository.reviewReason != "additional_preauthorization_failed" || repository.increaseCalls != 0 {
		t.Fatalf("failed authorization state = %#v", repository)
	}
	if repository.responseSaved || repository.resultSaved || repository.committed || billing.commitCalls != 0 {
		t.Fatalf("unauthorized stream was settled: repository=%#v billing=%#v", repository, billing)
	}
}

func TestProxyStreamAuthorizesAdditionalUsageBeforeDone(t *testing.T) {
	repository := &streamRepositoryFake{}
	recorder := httptest.NewRecorder()
	billing := &streamBillingFake{terminalSeenAtExtend: func() bool {
		return strings.Contains(recorder.Body.String(), "data: [DONE]") || strings.Contains(strings.Join(repository.events, ","), "DONE")
	}}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := strings.NewReader("data: {\"choices\":[]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 1)

	if billing.extendCalls != 1 || billing.extendedBy != 9 || billing.terminalBeforeExtend {
		t.Fatalf("additional authorization order = %#v", billing)
	}
	if repository.increaseCalls != 1 || repository.increasedBy != 9 {
		t.Fatalf("local maximum update = %#v", repository)
	}
	if !strings.Contains(recorder.Body.String(), "data: [DONE]") || repository.reviewReason != "" {
		t.Fatalf("authorized terminal was not delivered: body=%q repository=%#v", recorder.Body.String(), repository)
	}
	if !repository.responseSaved || !repository.resultSaved || !repository.committed || billing.commitCalls != 1 {
		t.Fatalf("authorized stream was not settled: repository=%#v billing=%#v", repository, billing)
	}
}
