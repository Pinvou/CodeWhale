package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

// Storage/cancellation commits and billing receipts are controlled here. The
// public chat handler, Service dispatch/registry handshake and HTTP adapter are
// production code. This is NOT PostgreSQL, a real worker transaction, provider
// cancellation confirmation, or a five-minute approval-window end-to-end test.
type localHandshakeRepository struct {
	*chatBudgetRepository
	service                                 *Service
	marked, resumeMark, reading, resumeRead chan struct{}
	readOnce                                sync.Once
	mode                                    string
	mu                                      sync.Mutex
	origin                                  string
	emergencyRequested                      bool
	userCommandFingerprint                  string
	userCommands                            int
}

func (r *localHandshakeRepository) MarkUpstream(ctx context.Context, id uuid.UUID) error {
	if err := r.chatBudgetRepository.MarkUpstream(ctx, id); err != nil {
		return err
	}
	close(r.marked)
	select {
	case <-r.resumeMark:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *localHandshakeRepository) GetUsage(ctx context.Context, id uuid.UUID) (UsageRequest, error) {
	if _, registered := r.service.activeUpstreamCancels.Load(id); registered {
		r.readOnce.Do(func() { close(r.reading) })
		if r.mode == "during_persistent_read" {
			select {
			case <-r.resumeRead:
			case <-ctx.Done():
				return UsageRequest{}, ctx.Err()
			}
		}
		if r.mode == "read_failure_preserves_funds" {
			return UsageRequest{}, errors.New("controlled cancellation observation unavailable")
		}
	}
	usage := r.usage
	r.mu.Lock()
	origin := r.origin
	emergencyRequested := r.emergencyRequested
	r.mu.Unlock()
	usage.CancelStatus = "NOT_REQUESTED"
	usage.EmergencyCancellationRequested = emergencyRequested
	if origin != "" {
		usage.CancellationOrigin = origin
		usage.CancelStatus = "UNSUPPORTED"
		usage.DeliveryStatus = "USER_STOPPED"
	}
	return usage, nil
}

func (r *localHandshakeRepository) cancel(origin string) {
	// Commit observation precedes abort, matching the real worker's ordering.
	r.mu.Lock()
	r.origin = origin
	if origin == "MODEL_EMERGENCY" {
		r.emergencyRequested = true
	}
	r.mu.Unlock()
}

// Controlled first-command writer semantics from requestCancellation: an
// in-flight USER command sets UNSUPPORTED/USER_STOPPED and overwrites the
// mutable origin, while immutable emergency REQUESTED evidence is untouched.
// The paired PG delta invokes the ACTUAL production writer and verifies this
// combination; this seam is not itself database/worker evidence.
func (r *localHandshakeRepository) requestUserStop(id, user uuid.UUID, fingerprint string) error {
	if id != r.usage.ID || !cancellationOwnerMatches(r.usage, user) {
		return ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.userCommandFingerprint != "" {
		if r.userCommandFingerprint != fingerprint {
			return ErrIdempotencyConflict
		}
		return nil
	}
	r.userCommandFingerprint = fingerprint
	r.userCommands++
	r.origin = "USER"
	return nil
}

type localHandshakeTransport struct {
	base            http.RoundTripper
	entered, resume chan struct{}
}

func (t localHandshakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(t.entered)
	select {
	case <-t.resume:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return t.base.RoundTrip(req)
}

func localHandshakeWait(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("required production barrier was not reached")
	}
}

func TestLocalModelCancellationHandshakeHTTP(t *testing.T) {
	for _, mode := range []string{"before_registration", "during_persistent_read", "after_persistent_read", "read_failure_preserves_funds", "normal", "user_stop_preserves_generation", "emergency_then_user_before_registration"} {
		t.Run(mode, func(t *testing.T) {
			f := newChatBudgetHTTPFixture(t)
			repo := &localHandshakeRepository{chatBudgetRepository: f.repo, mode: mode, marked: make(chan struct{}), resumeMark: make(chan struct{}), reading: make(chan struct{}), resumeRead: make(chan struct{})}
			user, wallet := f.repo.route.OwnerID, f.repo.route.WalletID
			authority := httptest.NewServer(&authorizationDecisionRecorder{allowed: true, userID: user})
			t.Cleanup(authority.Close)
			authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
			if err != nil {
				t.Fatal(err)
			}
			policy, err := NewProviderURLPolicy("test", []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			adapter := NewHTTPUpstreamClient()
			transportEntered, transportResume := make(chan struct{}), make(chan struct{})
			if mode == "after_persistent_read" {
				adapter.client.Transport = localHandshakeTransport{base: adapter.client.Transport, entered: transportEntered, resume: transportResume}
			}
			service := &Service{repository: repo, contexts: fixedBillingContextResolver{AuthorizationContext{UserID: user, SessionID: "budget-session", ContextType: "PERSONAL"}}, wallets: chatBudgetWallet{wallet}, billing: f.billing, cancellationBilling: f.billing, executionBilling: f.billing, executionUpstream: adapter, defaultRoutes: f05DeadlineReadyRoute{}, authorizer: authorizer, providerURLs: policy, managementEnabled: true, personalExecutionEnabled: true, defaultMaxOutputTokens: 4096, maximumOutputTokens: 5000, maximumInputTokens: 5000, breaker: resilience.NewBreaker()}
			repo.service = service
			handler := service.Handler(successfulBillingVerify)
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":2}`))
				req.Header.Set("Authorization", "Bearer controlled-user")
				req.Header.Set("Idempotency-Key", "local-cancellation-handshake-original")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				return w
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request() }()
			localHandshakeWait(t, repo.marked)
			id := repo.usage.ID
			if id == uuid.Nil || repo.usage.HoldID == nil || f.billing.reserves != 1 {
				t.Fatal("barrier did not follow accepted usage and real dispatch setup")
			}
			switch mode {
			case "before_registration":
				repo.cancel("MODEL_EMERGENCY")
				if service.abortActiveUpstream(id) {
					t.Fatal("expected cancellation before registry insertion")
				}
			case "user_stop_preserves_generation":
				if err := repo.requestUserStop(id, repo.usage.UserID, cancellationFingerprint(id)); err != nil {
					t.Fatal(err)
				}
			case "emergency_then_user_before_registration":
				repo.cancel("MODEL_EMERGENCY")
				if service.abortActiveUpstream(id) {
					t.Fatal("expected emergency cancellation before registration")
				}
				if err := repo.requestUserStop(id, repo.usage.UserID, cancellationFingerprint(id)); err != nil {
					t.Fatal(err)
				}
				if err := repo.requestUserStop(id, repo.usage.UserID, cancellationFingerprint(id)); err != nil {
					t.Fatal(err)
				}
				repo.mu.Lock()
				origin, evidence, commands := repo.origin, repo.emergencyRequested, repo.userCommands
				repo.mu.Unlock()
				if origin != "USER" || !evidence || commands != 1 {
					t.Fatal("composition did not retain REQUESTED across first USER command and replay")
				}
			}
			close(repo.resumeMark)
			if mode == "during_persistent_read" {
				localHandshakeWait(t, repo.reading)
				repo.cancel("MODEL_EMERGENCY")
				if !service.abortActiveUpstream(id) {
					t.Fatal("registered cancellation was missed during persistent read")
				}
				close(repo.resumeRead)
			}
			if mode == "after_persistent_read" {
				localHandshakeWait(t, transportEntered)
				localHandshakeWait(t, repo.reading)
				repo.cancel("MODEL_EMERGENCY")
				if !service.abortActiveUpstream(id) {
					t.Fatal("registered cancellation was missed after persistent read")
				}
				close(transportResume)
			}
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("public request failed to finish after barrier release")
			}
			if _, registered := service.activeUpstreamCancels.Load(id); registered {
				t.Fatal("completed request left an active cancellation registration")
			}
			success := mode == "normal" || mode == "user_stop_preserves_generation"
			if success {
				if response.Code != http.StatusOK || f.dispatches.Load() != 1 || f.billing.commitCalls != 1 || !repo.committed {
					t.Fatalf("normal/USER execution changed: status=%d upstream=%d commits=%d", response.Code, f.dispatches.Load(), f.billing.commitCalls)
				}
			} else {
				if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "UPSTREAM_RESULT_UNKNOWN") || f.dispatches.Load() != 0 || repo.reviewReason != "upstream_transport_unknown" {
					t.Fatalf("cancel/read failure escaped no-dispatch handshake: status=%d upstream=%d review=%s", response.Code, f.dispatches.Load(), repo.reviewReason)
				}
				if f.billing.commitCalls != 0 || repo.committed || repo.released || f.billing.releaseCalls != 0 || repo.releaseCalls != 0 {
					t.Fatal("uncertain cancellation fabricated financial terminal or released reservation")
				}
			}
			// A subsequent public idempotent request must not reserve or dispatch
			// again. These are controlled receipt counters, not PG ledger proof.
			repo.existing = &repo.usage
			repo.replaySnapshot = ReplaySnapshot{Usage: repo.usage}
			beforeReserve, beforeCommit, beforeRelease, beforeDispatch := f.billing.reserves, f.billing.commitCalls, f.billing.releaseCalls, f.dispatches.Load()
			_ = request()
			if f.billing.reserves != beforeReserve || f.billing.commitCalls != beforeCommit || f.billing.releaseCalls != beforeRelease || f.dispatches.Load() != beforeDispatch {
				t.Fatal("next public retry repeated a financial or upstream action")
			}
		})
	}
}
