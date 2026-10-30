package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type failureBodyReviewRepository struct {
	*streamRepositoryFake
	observation map[string]any
	reviewErr   error
}

func (r *failureBodyReviewRepository) MarkReview(_ context.Context, _ uuid.UUID, reason string, snapshot map[string]any) error {
	r.reviewReason = reason
	r.observation = snapshot
	return r.reviewErr
}

func TestUpstreamPaymentRequiredCannotProduceBillingNotice(t *testing.T) {
	const malicious = `{"error":{"message":"provider balance","amount":9223372036854775807},"billing_notice":{"schema_version":1,"code":"WALLET_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_RECHARGE","context_type":"PERSONAL","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}`
	for _, body := range []string{malicious, " {\"error\": {\"message\": \"ordinary\"}} ", "not JSON"} {
		repository := &streamRepositoryFake{}
		money := &streamBillingFake{}
		service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
		response := &http.Response{StatusCode: http.StatusPaymentRequired, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		out := httptest.NewRecorder()
		service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), response)
		if out.Code != http.StatusPaymentRequired || !repository.responseSaved || string(repository.savedResult.Payload) != out.Body.String() || money.releaseCalls != 0 || money.commitCalls != 0 || money.extendCalls != 0 {
			t.Fatal("provider status, retained response or uncertain funds classification changed")
		}
		if body != malicious {
			if out.Body.String() != body {
				t.Fatal("ordinary upstream error bytes changed")
			}
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out.Body.Bytes(), &fields); err != nil || fields["billing_notice"] != nil || string(fields["error"]) != `{"message":"provider balance","amount":9223372036854775807}` {
			t.Fatal("supplier authority retained or ordinary error/large integer changed", err)
		}
	}
}

func TestStoredUpstreamPaymentRequiredCannotReplayBillingNotice(t *testing.T) {
	for _, withHold := range []bool{true, false} {
		usage := UsageRequest{ID: uuid.New(), Status: "RELEASED", ExecutionStatus: "FAILED"}
		reason := "reserve_failed"
		usage.ErrorClass = &reason
		if withHold {
			hold := uuid.New()
			usage.HoldID = &hold
			reason = "reconciled_upstream_rejected"
		}
		payload, err := json.Marshal(map[string]any{"code": "WALLET_INSUFFICIENT", "billing_notice": newWalletInsufficientNotice("INITIAL_RESERVATION", "REJECTED", "NOT_STARTED", "PERSONAL", nil, false, usage.ID)})
		if err != nil {
			t.Fatal(err)
		}
		// LoadReplay does not load the write-only ResponseReceived flag. Authority
		// comes from the persisted dispatch hold, never its zero-valued flag/body.
		snapshot := ReplaySnapshot{Usage: usage, Result: &ReplayResult{Kind: "JSON", HTTPStatus: http.StatusPaymentRequired, ContentType: "application/json", Payload: payload}}
		out := httptest.NewRecorder()
		(&Service{}).writeStoredUsageSnapshot(out, httptest.NewRequest(http.MethodGet, "/v1/usage-requests", nil), usage, snapshot)
		var fields map[string]json.RawMessage
		if out.Code != http.StatusPaymentRequired || json.Unmarshal(out.Body.Bytes(), &fields) != nil {
			t.Fatal("stored original status/body contract changed")
		}
		if withHold {
			if fields["billing_notice"] != nil || string(snapshot.Result.Payload) != string(payload) {
				t.Fatal("upstream notice replayed or immutable stored bytes mutated")
			}
		} else if out.Body.String() != string(payload) {
			t.Fatal("platform initial reservation failure receipt changed")
		}
	}
}

// Exercise retained ciphertext and LoadReplay through current public migrations
// and the Token runtime role. The held row models an already dispatched request
// from before this filter; this is not new-request/provider admission acceptance.
func TestUpstreamBillingNoticeReplayPublicPostgres(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	for _, fromSupplier := range []bool{true, false} {
		var hold *uuid.UUID
		if fromSupplier {
			id := uuid.New()
			hold = &id
		}
		id := retainedCancellationUsage(t, repository, ctx, uuid.New(), uuid.New(), hold, "HOLD_RESERVING")
		// Disable the reusable fixture's optional cancellation before either
		// writer. Held historical rows start in the actual dispatched state.
		status, execution := "CREATED", "HOLD_RESERVING"
		if fromSupplier {
			status, execution = "UPSTREAM_ACTIVE", "UPSTREAM_DISPATCHING"
		}
		if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET status=$2,execution_status=$3,cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id, status, execution); err != nil {
			t.Fatal(err)
		}
		service := &Service{repository: repository}
		if fromSupplier {
			payload, err := json.Marshal(map[string]any{"billing_notice": newWalletInsufficientNotice("INITIAL_RESERVATION", "REJECTED", "NOT_STARTED", "PERSONAL", nil, false, id)})
			if err != nil {
				t.Fatal(err)
			}
			// Original writer order, deliberately preserving the pre-filter body.
			if err := repository.SaveReplayResult(ctx, id, ReplayResult{Kind: "JSON", HTTPStatus: http.StatusPaymentRequired, ContentType: "application/json", Payload: payload, ResponseReceived: true, ResponseDisposition: responseDispositionUsageEvidenceRequired}); err != nil {
				t.Fatal(err)
			}
			if err := repository.MarkReleased(ctx, id, "reconciled_upstream_rejected"); err != nil {
				t.Fatal(err)
			}
		} else {
			service.writeInitialReservationFailure(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx), id, DispatchEnvelope{ContextType: "PERSONAL"}, &BillingHTTPError{Status: http.StatusPaymentRequired, Code: "WALLET_INSUFFICIENT"})
		}
		snapshot, err := repository.LoadReplay(ctx, id, 0)
		if err != nil || snapshot.Result == nil || snapshot.Result.ResponseReceived || (snapshot.Usage.HoldID != nil) != fromSupplier {
			t.Fatal("actual stored source identity or read contract changed", err)
		}
		original := string(snapshot.Result.Payload)
		out := httptest.NewRecorder()
		service.writeStoredUsageSnapshot(out, httptest.NewRequest(http.MethodGet, "/v1/usage-requests", nil), snapshot.Usage, snapshot)
		var fields map[string]json.RawMessage
		if out.Code != http.StatusPaymentRequired || json.Unmarshal(out.Body.Bytes(), &fields) != nil || (fields["billing_notice"] != nil) == fromSupplier {
			t.Fatal("stored supplier notice trusted or platform notice lost")
		}
		after, err := repository.LoadReplay(ctx, id, 0)
		if err != nil || after.Result == nil || string(after.Result.Payload) != original {
			t.Fatal("delivery filter rewrote historical ciphertext", err)
		}
	}
}

func TestF05UpstreamFailureOnlyReplaysCompleteBoundedBody(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		for _, delta := range []int{-1, 0, 1} {
			name := http.StatusText(status) + "/" + formatBodySize(int64((2<<20)+delta))
			t.Run(name, func(t *testing.T) {
				repository := &failureBodyReviewRepository{streamRepositoryFake: &streamRepositoryFake{}}
				money := &streamBillingFake{}
				service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
				original := strings.Repeat("z", (2<<20)+delta)
				response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/plain"}, "X-Pinvou-Model-Rejection-v1": []string{"MODEL_UNAVAILABLE"}}, Body: io.NopCloser(strings.NewReader(original))}
				out := httptest.NewRecorder()
				service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), response)
				if out.Code != status || out.Header().Get("X-Pinvou-Model-Rejection-v1") != "" {
					t.Fatal("original rejection status or trusted header boundary changed", out.Code)
				}
				rejected := status == http.StatusBadRequest
				if money.commitCalls != 0 || money.extendCalls != 0 || money.releaseCalls != map[bool]int{true: 1, false: 0}[rejected] {
					t.Fatal("non-2xx funds classification changed")
				}
				if delta > 0 {
					if repository.resultSaved || repository.responseSaved || repository.reviewReason != "upstream_body_incomplete" || strings.Contains(out.Body.String(), strings.Repeat("z", 32)) {
						t.Fatal("oversized prefix became complete persisted/delivered replay")
					}
					want := responseDispositionUsageEvidenceRequired
					if rejected {
						want = responseDispositionRejectedBeforeGeneration
					}
					if repository.observation["http_status"] != status || repository.observation["response_disposition"] != want {
						t.Fatal("authoritative status lost from recovery observation")
					}
				} else {
					if !repository.resultSaved || !repository.responseSaved || string(repository.savedResult.Payload) != original || out.Body.String() != original || repository.reviewReason != "" {
						t.Fatal("complete boundary response changed")
					}
				}
				if repository.released != rejected {
					t.Fatal("original local release outcome changed")
				}
			})
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(http.StatusText(status)+"/interrupted", func(t *testing.T) {
			repository := &failureBodyReviewRepository{streamRepositoryFake: &streamRepositoryFake{}}
			money := &streamBillingFake{}
			service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
			out := httptest.NewRecorder()
			response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(&interruptedBodyReader{})}
			service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), response)
			if repository.resultSaved || repository.responseSaved || repository.reviewReason != "upstream_body_incomplete" || strings.Contains(out.Body.String(), "partial body") {
				t.Fatal("read-failed bytes became complete replay")
			}
			if out.Code != status || money.releaseCalls != map[bool]int{true: 1, false: 0}[status == http.StatusBadRequest] || money.commitCalls != 0 {
				t.Fatal("read failure changed trusted rejection/unknown money contract")
			}
		})
	}
	t.Run("unknown-release-ack-keeps-rejected-proof", func(t *testing.T) {
		repository := &failureBodyReviewRepository{streamRepositoryFake: &streamRepositoryFake{}}
		money := &streamBillingFake{releaseErr: errors.New("controlled unknown release ACK")}
		service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
		out := httptest.NewRecorder()
		response := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(&interruptedBodyReader{})}
		service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), response)
		if out.Code != 400 || repository.released || repository.resultSaved || money.releaseCalls != 1 || repository.observation["response_disposition"] != responseDispositionRejectedBeforeGeneration {
			t.Fatal("unknown release ACK invented local release or dropped original status proof")
		}
	})
	t.Run("review-persistence-failure-does-not-release", func(t *testing.T) {
		repository := &failureBodyReviewRepository{streamRepositoryFake: &streamRepositoryFake{}, reviewErr: errors.New("controlled storage failure")}
		money := &streamBillingFake{}
		service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
		out := httptest.NewRecorder()
		response := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(&interruptedBodyReader{})}
		service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), response)
		if out.Code != 503 || !strings.Contains(out.Body.String(), "RESULT_PERSISTENCE_FAILED") || money.releaseCalls != 0 || repository.resultSaved || repository.released {
			t.Fatal("failure to retain recovery facts claimed release/replay")
		}
	})
}

// This fixture uses original public migrations and token_gateway LOGIN. Billing
// calls are controlled; this test proves actual Token ciphertext/state behavior,
// not a real downstream balance or deployed provider/issuer acceptance.
func TestF05IncompleteUpstreamErrorReplayPublicPostgres(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		for _, interrupted := range []bool{false, true} {
			label := "oversized"
			if interrupted {
				label = "interrupted"
			}
			t.Run(http.StatusText(status)+"/"+label, func(t *testing.T) {
				holdID := uuid.New()
				id := retainedCancellationUsage(t, repository, ctx, uuid.New(), uuid.New(), &holdID, "STREAMING")
				var body io.Reader = strings.NewReader(strings.Repeat("z", (2<<20)+1))
				if interrupted {
					body = &interruptedBodyReader{}
				}
				money := &streamBillingFake{}
				service := &Service{repository: repository, executionBilling: money, executionUpstream: NewHTTPUpstreamClient()}
				out := httptest.NewRecorder()
				response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(body)}
				service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx), id, holdID, response)
				var noResult, noCompleteObservation, noProviderTerminal bool
				if err := repository.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM usage_replay_results WHERE usage_request_id=$1),NOT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='RESULT_OBSERVED'),provider_terminal_at IS NULL FROM usage_requests WHERE id=$1`, id).Scan(&noResult, &noCompleteObservation, &noProviderTerminal); err != nil || !noResult || !noCompleteObservation || !noProviderTerminal {
					t.Fatal("incomplete body persisted ciphertext/complete observation/terminal clock", err)
				}
				usage, err := repository.GetUsage(ctx, id)
				want := "REVIEW"
				if status == http.StatusBadRequest {
					want = "RELEASED"
				}
				if err != nil || usage.Status != want || out.Code != status || money.releaseCalls != map[bool]int{true: 1, false: 0}[status == http.StatusBadRequest] || money.commitCalls != 0 {
					t.Fatal("actual Token non-2xx state changed", err)
				}
				replay, err := repository.LoadReplay(ctx, id, 0)
				if err != nil || replay.Result != nil || len(replay.Events) != 0 {
					t.Fatal("incomplete body became an original replay", err)
				}
			})
		}
	}
}
