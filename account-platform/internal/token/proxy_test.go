package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

type streamRepositoryFake struct {
	Repository
	events          []string
	deliveries      []string
	reviewReason    string
	responseSaved   bool
	resultSaved     bool
	committed       bool
	released        bool
	releaseErr      error
	releaseCalls    int
	replaySnapshot  ReplaySnapshot
	loadReplayCalls int
	savedResult     ReplayResult
	saveResultErr   error
	commitSnapshot  map[string]any
	increaseCalls   int
	increasedBy     int64
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

func (r *streamRepositoryFake) MarkUpstream(context.Context, uuid.UUID) error { return nil }

func (r *streamRepositoryFake) SaveReplayResult(_ context.Context, _ uuid.UUID, result ReplayResult) error {
	if r.saveResultErr != nil {
		return r.saveResultErr
	}
	r.resultSaved = true
	r.savedResult = result
	r.responseSaved = result.ResponseReceived
	return nil
}

func (r *streamRepositoryFake) MarkCommitted(_ context.Context, _ uuid.UUID, _ int64, snapshot map[string]any) error {
	r.committed = true
	r.commitSnapshot = snapshot
	return nil
}

func (r *streamRepositoryFake) MarkReleased(context.Context, uuid.UUID, string) error {
	r.releaseCalls++
	if r.releaseErr != nil {
		return r.releaseErr
	}
	r.released = true
	return nil
}

func (r *streamRepositoryFake) LoadReplay(context.Context, uuid.UUID, int64) (ReplaySnapshot, error) {
	r.loadReplayCalls++
	return r.replaySnapshot, nil
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
	commitErr            error
	terminalSeenAtExtend func() bool
	terminalBeforeExtend bool
	releaseCalls         int
	releaseErr           error
}

type memberQuotaBillingFake struct {
	BillingClient
	membershipVersion int64
}

func (b *memberQuotaBillingFake) ReserveMemberQuotaPriced(_ context.Context, _, _, _, _ uuid.UUID, membershipVersion, _ int64, _ uuid.UUID, _ HoldPricingContext) (BillingHold, error) {
	b.membershipVersion = membershipVersion
	return BillingHold{ID: uuid.New()}, nil
}

type liveReplayRepositoryFake struct {
	Repository
	snapshots  []ReplaySnapshot
	loadCalls  int
	deliveries []string
	beforeLoad func(int)
}

func (r *liveReplayRepositoryFake) LoadReplay(_ context.Context, _ uuid.UUID, after int64) (ReplaySnapshot, error) {
	r.loadCalls++
	if r.beforeLoad != nil {
		r.beforeLoad(r.loadCalls)
	}
	index := min(r.loadCalls-1, len(r.snapshots)-1)
	snapshot := r.snapshots[index]
	filtered := make([]ReplayEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		if event.Sequence > after {
			filtered = append(filtered, event)
		}
	}
	snapshot.Events = filtered
	return snapshot, nil
}

func (r *liveReplayRepositoryFake) MarkDelivery(_ context.Context, _ uuid.UUID, status string) error {
	r.deliveries = append(r.deliveries, status)
	return nil
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
	return b.commitErr
}

func (b *streamBillingFake) Release(context.Context, uuid.UUID, uuid.UUID, string) error {
	b.releaseCalls++
	return b.releaseErr
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

func TestApplyDefaultChatMaximumOutputOnlyWhenBothFieldsAreMissing(t *testing.T) {
	request := chatCompletionRequest{Model: "model"}
	body, err := applyDefaultChatMaximumOutput([]byte(`{"model":"model","messages":[]}`), &request, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if request.MaxCompletionTokens != 4096 || payload["max_completion_tokens"] != float64(4096) {
		t.Fatalf("missing output limit was not defaulted: request=%#v payload=%#v", request, payload)
	}

	explicit := chatCompletionRequest{Model: "model", MaxCompletionTokens: 1024}
	body, err = applyDefaultChatMaximumOutput([]byte(`{"model":"model","max_completion_tokens":1024}`), &explicit, 4096)
	if err != nil || !bytes.Contains(body, []byte(`"max_completion_tokens":1024`)) || explicit.MaxCompletionTokens != 1024 {
		t.Fatalf("explicit output limit changed: request=%#v body=%s err=%v", explicit, body, err)
	}
}

func TestOperationalOutputDefaultDoesNotChangeCanonicalV1Fingerprint(t *testing.T) {
	canonical, err := normalizeInferenceBody([]byte(`{"model":"model","messages":[]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	request := chatCompletionRequest{Model: "model"}
	effective, err := applyDefaultChatMaximumOutput(append([]byte(nil), canonical...), &request, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(canonical, effective) {
		t.Fatal("test did not exercise the operational default")
	}
	const preUpgradeCanonicalV1 = "6ca55eae1a1bdcfd6e0699f7ce9a055571f4681276fbcd5486e575dc5c1447d3"
	if got := inferenceFingerprint("CHAT", 1, canonical); got != preUpgradeCanonicalV1 {
		t.Fatalf("canonical v1 fingerprint changed after operational default: %s", got)
	}
}

func TestBillingNoticeCapabilityAndWalletInsufficientClassification(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(clientCapabilitiesHeader, "other-v1, BILLING-NOTICE-V1")
	if !supportsBillingNotice(request) {
		t.Fatal("versioned billing notice capability was not detected")
	}
	if !walletInsufficient(&BillingHTTPError{Status: http.StatusPaymentRequired, Code: "wallet_insufficient"}) {
		t.Fatal("definitive insufficient-balance response was not classified")
	}
	if walletInsufficient(&BillingHTTPError{Status: http.StatusServiceUnavailable, Code: "WALLET_INSUFFICIENT"}) || walletInsufficient(errors.New("insufficient balance")) {
		t.Fatal("non-contract billing failures must not be classified from text or the wrong status")
	}

	header := http.Header{}
	usageID := uuid.New()
	setBillingNoticeHeaders(header, newWalletInsufficientNotice("ADDITIONAL_RESERVATION", "COMPLETED", "SETTLED", "personal", nil, false, usageID))
	if header.Get("X-Pinvou-Billing-Code") != "WALLET_INSUFFICIENT" || header.Get("X-Pinvou-Billing-Usage-Request-ID") != usageID.String() || header.Get("X-Pinvou-Billing-Context-Type") != "PERSONAL" {
		t.Fatalf("unexpected billing notice headers: %#v", header)
	}
}

func TestOrganizationReservationUsesFrozenOrganizationMembershipVersion(t *testing.T) {
	organizationID, userID := uuid.New(), uuid.New()
	organizationMembershipVersion := int64(7)
	snapshot, err := json.Marshal(frozenUsageSnapshot{ContextType: "ORGANIZATION", OrganizationID: &organizationID, MembershipVersion: 19, OrganizationMembershipVersion: &organizationMembershipVersion})
	if err != nil {
		t.Fatal(err)
	}
	billing := &memberQuotaBillingFake{}
	service := &Service{billing: billing}
	usage := UsageRequest{ID: uuid.New(), UserID: userID, OrganizationID: &organizationID, WalletID: uuid.New(), Model: "model", MaximumMicrocredits: 10, RequestSnapshot: snapshot}
	_, err = service.ensureUsageHold(context.Background(), usage, DispatchEnvelope{Price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New()}})
	if err != nil || billing.membershipVersion != organizationMembershipVersion {
		t.Fatalf("organization membership version = %d, err=%v", billing.membershipVersion, err)
	}

	legacySnapshot, _ := json.Marshal(frozenUsageSnapshot{ContextType: "ORGANIZATION", OrganizationID: &organizationID, MembershipVersion: 19})
	usage.RequestSnapshot = legacySnapshot
	if _, err = service.ensureUsageHold(context.Background(), usage, DispatchEnvelope{Price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New()}}); !errors.Is(err, ErrOrganizationMembershipContext) || !definitiveBillingFailure(err) {
		t.Fatalf("legacy organization snapshot did not fail closed: %v", err)
	}
}

func TestMemberQuotaInsufficientIsClassifiedForAdditionalAuthorization(t *testing.T) {
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{extendErr: &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "MEMBER_QUOTA_INSUFFICIENT"}}
	service := &Service{repository: repository, billing: billing}
	_, code, authorized := service.authorizeAdditional(context.Background(), uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, Usage{OutputTokens: 10}, 1)
	if !authorized || code != "MEMBER_QUOTA_INSUFFICIENT" {
		t.Fatalf("additional member quota result: authorized=%v code=%q", authorized, code)
	}
	organizationID := uuid.New()
	notice := newInsufficientNotice(code, "ADDITIONAL_RESERVATION", "COMPLETED", "SETTLED", "ORGANIZATION", &organizationID, true, uuid.New())
	if notice.Action != "CONTACT_ORG_ADMIN" {
		t.Fatalf("member quota action = %q", notice.Action)
	}
}

func TestInitialWalletInsufficientIsPersistedBeforeTerminalReplay(t *testing.T) {
	repository := &streamRepositoryFake{}
	service := &Service{repository: repository}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("X-Request-ID", "request-initial-insufficient")
	usageID := uuid.New()

	organizationID := uuid.New()
	service.writeInitialReservationFailure(recorder, request, usageID, DispatchEnvelope{ContextType: "ORGANIZATION", OrganizationID: &organizationID}, &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "WALLET_INSUFFICIENT"})

	if recorder.Code != http.StatusPaymentRequired || !repository.resultSaved || !repository.released {
		t.Fatalf("initial failure was not durably completed: code=%d repository=%#v", recorder.Code, repository)
	}
	if repository.savedResult.HTTPStatus != http.StatusPaymentRequired || !bytes.Equal(repository.savedResult.Payload, recorder.Body.Bytes()) {
		t.Fatalf("replay payload differs from first response: %#v body=%s", repository.savedResult, recorder.Body.String())
	}
	var payload struct {
		Code          string        `json:"code"`
		BillingNotice billingNotice `json:"billing_notice"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || payload.Code != "WALLET_INSUFFICIENT" || payload.BillingNotice.ContextType != "ORGANIZATION" || payload.BillingNotice.Action != "CONTACT_ORG_ADMIN" || payload.BillingNotice.OrganizationID == nil || *payload.BillingNotice.OrganizationID != organizationID {
		t.Fatalf("organization notice is unsafe or incomplete: %#v", payload)
	}

	replayRepository := &liveReplayRepositoryFake{snapshots: []ReplaySnapshot{{
		Usage:  UsageRequest{ID: usageID, Status: "RELEASED", ExecutionStatus: "FAILED"},
		Result: &repository.savedResult,
	}}}
	replayRecorder := httptest.NewRecorder()
	service.repository = replayRepository
	service.replayUsage(replayRecorder, request, replayRepository.snapshots[0].Usage, nil)
	if replayRecorder.Code != http.StatusPaymentRequired || !bytes.Equal(replayRecorder.Body.Bytes(), recorder.Body.Bytes()) {
		t.Fatalf("same idempotency result did not replay the original 402: code=%d body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
}

func TestInitialWalletInsufficientRetryRecoversSavedResultBeforeReservingAgain(t *testing.T) {
	repository := &streamRepositoryFake{releaseErr: errors.New("database unavailable")}
	service := &Service{repository: repository}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	usageID := uuid.New()

	first := httptest.NewRecorder()
	service.writeInitialReservationFailure(first, request, usageID, DispatchEnvelope{ContextType: "PERSONAL"}, &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "WALLET_INSUFFICIENT"})
	if first.Code != http.StatusServiceUnavailable || !repository.resultSaved || repository.released {
		t.Fatalf("fault injection did not stop between replay save and release: code=%d repository=%#v", first.Code, repository)
	}

	repository.replaySnapshot = ReplaySnapshot{Usage: UsageRequest{ID: usageID, ExecutionStatus: "HOLD_RESERVING"}, Result: &repository.savedResult}
	repository.releaseErr = nil
	replayed := httptest.NewRecorder()
	service.replayUsage(replayed, request, repository.replaySnapshot.Usage, nil)
	if replayed.Code != http.StatusPaymentRequired || !bytes.Equal(replayed.Body.Bytes(), repository.savedResult.Payload) || !repository.released || repository.releaseCalls != 2 {
		t.Fatalf("retry did not converge the saved 402 before reserve: code=%d body=%s repository=%#v", replayed.Code, replayed.Body.String(), repository)
	}
}

func TestInitialMemberQuotaAndMembershipFailuresRecoverSavedTerminalResult(t *testing.T) {
	cases := []struct {
		name       string
		failure    error
		wantStatus int
		wantCode   string
	}{
		{name: "member quota", failure: &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "MEMBER_QUOTA_INSUFFICIENT"}, wantStatus: http.StatusPaymentRequired, wantCode: "MEMBER_QUOTA_INSUFFICIENT"},
		{name: "membership changed", failure: &BillingHTTPError{Status: http.StatusConflict, Code: "MEMBER_LIFECYCLE_CONFLICT"}, wantStatus: http.StatusConflict, wantCode: "MEMBER_LIFECYCLE_CONFLICT"},
		{name: "legacy snapshot", failure: ErrOrganizationMembershipContext, wantStatus: http.StatusConflict, wantCode: "ORGANIZATION_MEMBERSHIP_CONTEXT_INVALID"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			repository := &streamRepositoryFake{releaseErr: errors.New("database unavailable")}
			service := &Service{repository: repository}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			usageID, organizationID := uuid.New(), uuid.New()
			first := httptest.NewRecorder()
			service.writeInitialReservationFailure(first, request, usageID, DispatchEnvelope{ContextType: "ORGANIZATION", OrganizationID: &organizationID}, test.failure)
			if first.Code != http.StatusServiceUnavailable || repository.savedResult.HTTPStatus != test.wantStatus || !strings.Contains(string(repository.savedResult.Payload), test.wantCode) {
				t.Fatalf("saved terminal result = %#v first=%d", repository.savedResult, first.Code)
			}
			repository.replaySnapshot = ReplaySnapshot{Usage: UsageRequest{ID: usageID, ExecutionStatus: "HOLD_RESERVING"}, Result: &repository.savedResult}
			repository.releaseErr = nil
			replayed := httptest.NewRecorder()
			handled, err := service.recoverInitialReservationFailure(replayed, request, usageID)
			if err != nil || !handled || replayed.Code != test.wantStatus || !repository.released {
				t.Fatalf("recovery handled=%v status=%d released=%v err=%v", handled, replayed.Code, repository.released, err)
			}
		})
	}
}

func TestOrganizationWalletNoticeRoutesPurchasersToStableOrganization(t *testing.T) {
	organizationID := uuid.New()
	usageID := uuid.New()
	notice := newWalletInsufficientNotice("INITIAL_RESERVATION", "REJECTED", "NOT_STARTED", "ORGANIZATION", &organizationID, true, usageID)
	if notice.Action != "OPEN_ORGANIZATION_RECHARGE" || notice.OrganizationID == nil || *notice.OrganizationID != organizationID {
		t.Fatalf("organization purchaser notice = %#v", notice)
	}
	header := http.Header{}
	setBillingNoticeHeaders(header, notice)
	if header.Get("X-Pinvou-Billing-Organization-ID") != organizationID.String() {
		t.Fatalf("stable organization target missing from headers: %#v", header)
	}
	withoutPermission := newWalletInsufficientNotice("INITIAL_RESERVATION", "REJECTED", "NOT_STARTED", "ORGANIZATION", &organizationID, false, usageID)
	if withoutPermission.Action != "CONTACT_ORG_ADMIN" {
		t.Fatalf("non-purchaser organization action = %q", withoutPermission.Action)
	}
}

func TestNonStreamingReplayRestoresBillingNoticeHeaders(t *testing.T) {
	usageID := uuid.New()
	notice := newWalletInsufficientNotice("ADDITIONAL_RESERVATION", "COMPLETED", "SETTLED", "PERSONAL", nil, false, usageID)
	snapshot, _ := json.Marshal(map[string]any{"usage": Usage{}, "billing_notice": notice})
	repository := &liveReplayRepositoryFake{snapshots: []ReplaySnapshot{{
		Usage:  UsageRequest{ID: usageID, Status: "COMMITTED", ExecutionStatus: "SUCCEEDED", UsageSnapshot: snapshot},
		Result: &ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"ok":true}`)},
	}}}
	service := &Service{repository: repository}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(clientCapabilitiesHeader, billingNoticeCapability)

	service.replayUsage(recorder, request, repository.snapshots[0].Usage, nil)

	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Pinvou-Billing-Code") != "WALLET_INSUFFICIENT" || recorder.Header().Get("X-Pinvou-Billing-Action") != "OPEN_RECHARGE" {
		t.Fatalf("replayed response lost billing notice: code=%d headers=%#v", recorder.Code, recorder.Header())
	}
}

func TestSettlementTerminalReplayEventsStayGated(t *testing.T) {
	for _, eventType := range []string{"DONE", "done", billingNoticeEvent} {
		if !terminalReplayEvent(eventType) {
			t.Fatalf("settlement terminal event %q was not gated", eventType)
		}
	}
	if terminalReplayEvent("CHUNK") {
		t.Fatal("ordinary stream chunks must remain replayable before settlement")
	}
}

func TestLiveReplayWithholdsNoticeAndDoneUntilSettlementSucceeds(t *testing.T) {
	events := []ReplayEvent{
		{Sequence: 1, EventType: "CHUNK", Payload: []byte(`{"choices":[]}`)},
		{Sequence: 2, EventType: billingNoticeEvent, Payload: []byte(`{"code":"WALLET_INSUFFICIENT"}`)},
		{Sequence: 3, EventType: "DONE", Payload: []byte(`[DONE]`)},
	}
	recorder := httptest.NewRecorder()
	repository := &liveReplayRepositoryFake{snapshots: []ReplaySnapshot{
		{Usage: UsageRequest{Status: "UPSTREAM_ACTIVE", ExecutionStatus: "STREAMING"}, Events: events},
		{Usage: UsageRequest{Status: "COMMITTED", ExecutionStatus: "SUCCEEDED"}, Events: events},
	}}
	repository.beforeLoad = func(call int) {
		if call == 2 && (strings.Contains(recorder.Body.String(), "billing_notice") || strings.Contains(recorder.Body.String(), "[DONE]")) {
			t.Fatal("settlement terminal escaped during the pending replay snapshot")
		}
	}
	service := &Service{repository: repository}
	request := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)

	service.replayLiveStream(recorder, request, uuid.New(), 0)

	if !strings.Contains(recorder.Body.String(), "event: chunk") || !strings.Contains(recorder.Body.String(), "event: billing_notice") || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("committed replay is incomplete: %q", recorder.Body.String())
	}
	if repository.loadCalls != 2 || !slices.Contains(repository.deliveries, "DELIVERED") {
		t.Fatalf("unexpected replay convergence: %#v", repository)
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

func TestProxyStreamCommitsTrustedUsageAndWarnsWhenAdditionalAuthorizationIsInsufficient(t *testing.T) {
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{extendErr: &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "WALLET_INSUFFICIENT"}}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(clientCapabilitiesHeader, billingNoticeCapability)
	stream := strings.NewReader("data: {\"choices\":[]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 1)

	if billing.extendCalls != 1 || billing.extendedBy != 9 {
		t.Fatalf("additional authorization = %d calls, %d microcredits", billing.extendCalls, billing.extendedBy)
	}
	if !strings.Contains(recorder.Body.String(), "event: billing_notice") || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("settled stream did not deliver its notice and terminal: %q", recorder.Body.String())
	}
	if repository.reviewReason != "" || repository.increaseCalls != 0 {
		t.Fatalf("insufficient extension entered review or changed the reservation: %#v", repository)
	}
	if !repository.responseSaved || !repository.resultSaved || !repository.committed || billing.commitCalls != 1 {
		t.Fatalf("trusted usage was not settled: repository=%#v billing=%#v", repository, billing)
	}
	if persisted, _ := repository.commitSnapshot["billing_notice"].(*billingNotice); persisted == nil || persisted.Code != "WALLET_INSUFFICIENT" {
		t.Fatalf("settled notice was not persisted for replay: %#v", repository.commitSnapshot)
	}
	if got := strings.Join(repository.events, ","); got != "CHUNK,CHUNK,BILLING_NOTICE,DONE" {
		t.Fatalf("persisted event order = %q", got)
	}
}

func TestProxyStreamReportsSettlementPendingWhenCommitFails(t *testing.T) {
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{extendErr: errors.New("billing unavailable"), commitErr: errors.New("billing unavailable")}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 1)

	if strings.Contains(recorder.Body.String(), "data: [DONE]") || !strings.Contains(recorder.Body.String(), "SETTLEMENT_PENDING") {
		t.Fatalf("uncertain settlement terminal = %q", recorder.Body.String())
	}
	if repository.reviewReason != "settlement_pending" || billing.commitCalls != 1 || repository.committed {
		t.Fatalf("pending settlement state = repository=%#v billing=%#v", repository, billing)
	}
	assertSavedSettlementCanReconcile(t, repository.savedResult)
}

func TestProxyStreamReplayFailureLeavesNoSplitResponseState(t *testing.T) {
	repository := &streamRepositoryFake{saveResultErr: errors.New("replay transaction failed")}
	billing := &streamBillingFake{}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 10)

	if repository.responseSaved || repository.resultSaved || repository.committed || billing.commitCalls != 0 {
		t.Fatalf("stream response escaped the failed atomic replay transaction: repository=%#v billing=%#v", repository, billing)
	}
	if repository.reviewReason != "result_persistence_failed" || strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("failed stream replay was not failed closed: body=%q repository=%#v", recorder.Body.String(), repository)
	}
}

func TestNonStreamingResultPersistsReconcilerSnapshotBeforeCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":10}}`))
	}))
	defer server.Close()
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{commitErr: errors.New("billing unavailable")}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient(), breaker: resilience.NewBreaker()}
	holdID := uuid.New()
	usageID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	service.dispatchUsage(recorder, request, UsageRequest{ID: usageID, HoldID: &holdID}, DispatchEnvelope{BaseURL: server.URL, Provider: "test", Credential: []byte("credential"), RequestBody: []byte(`{"model":"test"}`), Price: PriceVersion{OutputPerMillion: 1_000_000}, MaximumCost: 10, ContextType: "PERSONAL"})

	if recorder.Code != http.StatusServiceUnavailable || !repository.resultSaved || repository.savedResult.Kind != "JSON" || repository.savedResult.FinalUsage == nil || repository.savedResult.ActualMicrocredits == nil || repository.savedResult.SettlementSnapshot == nil {
		t.Fatalf("non-stream settlement evidence was not durable before commit: code=%d result=%#v", recorder.Code, repository.savedResult)
	}
	assertSavedSettlementCanReconcile(t, repository.savedResult)
}

func TestNonStreamingReplayFailureLeavesNoSplitResponseState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":10}}`))
	}))
	defer server.Close()
	repository := &streamRepositoryFake{saveResultErr: errors.New("replay transaction failed")}
	billing := &streamBillingFake{}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient(), breaker: resilience.NewBreaker()}
	holdID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()

	service.dispatchUsage(recorder, request, UsageRequest{ID: uuid.New(), HoldID: &holdID}, DispatchEnvelope{BaseURL: server.URL, Provider: "test", Credential: []byte("credential"), RequestBody: []byte(`{"model":"test"}`), Price: PriceVersion{OutputPerMillion: 1_000_000}, MaximumCost: 10, ContextType: "PERSONAL"})

	if recorder.Code != http.StatusServiceUnavailable || repository.responseSaved || repository.resultSaved || repository.committed || billing.commitCalls != 0 {
		t.Fatalf("non-stream response escaped the failed atomic replay transaction: code=%d repository=%#v billing=%#v", recorder.Code, repository, billing)
	}
	if repository.reviewReason != "result_persistence_failed" {
		t.Fatalf("failed non-stream replay review reason = %q", repository.reviewReason)
	}
}

func TestNonStreamingMissingUsagePersistsProviderEvidenceRecoveryState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient(), breaker: resilience.NewBreaker()}
	usageID, holdID := uuid.New(), uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()

	service.dispatchUsage(recorder, request, UsageRequest{ID: usageID, HoldID: &holdID}, DispatchEnvelope{BaseURL: server.URL, Provider: "test", Credential: []byte("credential"), RequestBody: []byte(`{"model":"test"}`), Price: PriceVersion{OutputPerMillion: 1_000_000}, MaximumCost: 10})

	if recorder.Code != http.StatusOK || !repository.resultSaved || repository.savedResult.ResponseDisposition != responseDispositionUsageEvidenceRequired || repository.reviewReason != "" || billing.commitCalls != 0 {
		t.Fatalf("missing usage was not frozen for provider evidence recovery: code=%d repository=%#v billing=%#v", recorder.Code, repository, billing)
	}
	assertResponseDispositionCanReconcile(t, repository.savedResult, UsageEvidence{Usage: &Usage{OutputTokens: 2}}, false)
}

func TestPreGenerationRejectionSurvivesReleaseFailureForReconciliation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_REQUEST"}}`))
	}))
	defer server.Close()
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{releaseErr: errors.New("billing unavailable")}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient(), breaker: resilience.NewBreaker()}
	usageID, holdID := uuid.New(), uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()

	service.dispatchUsage(recorder, request, UsageRequest{ID: usageID, HoldID: &holdID}, DispatchEnvelope{BaseURL: server.URL, Provider: "test", Credential: []byte("credential"), RequestBody: []byte(`{"model":"test"}`)})

	if recorder.Code != http.StatusBadRequest || repository.savedResult.ResponseDisposition != responseDispositionRejectedBeforeGeneration || repository.released || billing.releaseCalls != 1 {
		t.Fatalf("pre-generation rejection lost its retryable release state: code=%d repository=%#v billing=%#v", recorder.Code, repository, billing)
	}
	assertResponseDispositionCanReconcile(t, repository.savedResult, UsageEvidence{}, true)
}

func assertResponseDispositionCanReconcile(t *testing.T, result ReplayResult, evidence UsageEvidence, expectRelease bool) {
	t.Helper()
	snapshot, err := json.Marshal(replayResponseObservation{ResponseDisposition: result.ResponseDisposition, HTTPStatus: result.HTTPStatus})
	if err != nil {
		t.Fatal(err)
	}
	usageID, holdID, priceID := uuid.New(), uuid.New(), uuid.New()
	store := &reconciliationStoreFake{}
	billing := &reconciliationBillingFake{price: PriceVersion{ID: priceID, Model: "test-model", OutputPerMillion: 1_000_000}}
	providerID, adapterVersion, upstreamRequestID := "NEW_API", "new-api-v1", "provider-request-1"
	var provider UsageEvidenceProvider
	if !expectRelease {
		provider = evidenceClientFake{evidence: evidence}
	}
	reconciler := NewUsageReconciler(store, billing, provider, time.Second)
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: usageID, HoldID: &holdID, PriceVersionID: &priceID, Model: "test-model", Status: "REVIEW"}, UsageSnapshot: snapshot, ProviderID: &providerID, AdapterVersion: &adapterVersion, UpstreamRequestID: &upstreamRequestID}
	if err := reconciler.reconcile(context.Background(), item); err != nil {
		t.Fatalf("persisted response disposition did not converge: %v", err)
	}
	if expectRelease {
		if billing.releasedID != usageID || store.releasedID != usageID {
			t.Fatalf("persisted rejection did not release: billing=%#v store=%#v", billing, store)
		}
		return
	}
	if billing.committedUsageID != usageID || store.committedID != usageID {
		t.Fatalf("provider evidence did not commit: billing=%#v store=%#v", billing, store)
	}
}

func assertSavedSettlementCanReconcile(t *testing.T, result ReplayResult) {
	t.Helper()
	if result.FinalUsage == nil || result.ActualMicrocredits == nil || result.SettlementSnapshot == nil {
		t.Fatalf("incomplete settlement replay result: %#v", result)
	}
	snapshot, err := json.Marshal(result.SettlementSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	usageID, holdID, priceID := uuid.New(), uuid.New(), uuid.New()
	store := &reconciliationStoreFake{}
	billing := &reconciliationBillingFake{price: PriceVersion{ID: priceID, OutputPerMillion: 1_000_000}}
	reconciler := NewUsageReconciler(store, billing, nil, time.Second)
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: usageID, HoldID: &holdID, PriceVersionID: &priceID, Status: "SETTLEMENT_PENDING"}, UsageSnapshot: snapshot}
	if err := reconciler.reconcile(context.Background(), item); err != nil {
		t.Fatalf("saved settlement did not auto-converge: %v snapshot=%s", err, snapshot)
	}
	if billing.committedActual != *result.ActualMicrocredits || store.committedActual != *result.ActualMicrocredits {
		t.Fatalf("saved settlement converged with wrong actual: billing=%#v store=%#v", billing, store)
	}
}

func TestProxyStreamRoutesDefinitiveCommitConflictToReview(t *testing.T) {
	repository := &streamRepositoryFake{}
	billing := &streamBillingFake{commitErr: &BillingHTTPError{Status: http.StatusConflict, Code: "HOLD_STATE_CONFLICT"}}
	service := &Service{repository: repository, billing: billing, upstream: NewHTTPUpstreamClient()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")

	service.proxyStream(recorder, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, stream, make(chan struct{}), 1)

	if repository.reviewReason != "settlement_conflict" || !strings.Contains(recorder.Body.String(), "SETTLEMENT_REVIEW_REQUIRED") || strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("definitive commit conflict did not fail closed to review: body=%q repository=%#v", recorder.Body.String(), repository)
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
