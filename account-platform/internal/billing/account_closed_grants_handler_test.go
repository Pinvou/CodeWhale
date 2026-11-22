package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/google/uuid"
)

// These unit tests start after the verified-delegation middleware. Existing
// workload/delegation tests and PostgreSQL reclaim tests cover those boundaries.
type closedGrantHandlerRepository struct {
	Repository
	grant                       MemberQuotaGrant
	version                     int64
	candidateErr, effectErr     error
	candidateCalls, effectCalls int
	actor                       uuid.UUID
	org, id                     uuid.UUID
	reason, key                 string
	observedVersion             int64
}

func (r *closedGrantHandlerRepository) AccountClosedGrantCandidate(_ context.Context, org, id uuid.UUID) (MemberQuotaGrant, int64, error) {
	r.candidateCalls++
	r.org, r.id = org, id
	return r.grant, r.version, r.candidateErr
}
func (r *closedGrantHandlerRepository) ReclaimAccountClosedGrant(_ context.Context, org, id, actor uuid.UUID, version int64, reason, key string) (AccountClosedGrantReclaim, error) {
	r.effectCalls++
	r.org, r.id, r.actor = org, id, actor
	r.observedVersion, r.reason, r.key = version, reason, key
	return AccountClosedGrantReclaim{Grant: r.grant, AmountMicrocredits: 30, SubjectLifecycleVersion: version, Replayed: true}, r.effectErr
}

type closedGrantHandlerState struct {
	calls   int
	user    uuid.UUID
	version int64
	err     error
}

func (s *closedGrantHandlerState) RequireAnonymized(_ context.Context, user uuid.UUID, version int64) error {
	s.calls++
	s.user, s.version = user, version
	return s.err
}
func TestAccountClosedGrantBillingHandler(t *testing.T) {
	org, grantID, actor, member := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name                              string
		delegated, verifier               bool
		id, reason, key                   string
		candidateErr, stateErr, effectErr error
		want, candidates, states, effects int
	}{
		{name: "verified-actor-and-authoritative-lifecycle", delegated: true, verifier: true, id: grantID.String(), reason: "controlled reclaim", key: "controlled-reclaim-key", want: 200, candidates: 1, states: 1, effects: 1},
		{name: "missing-delegation", verifier: true, id: grantID.String(), reason: "reason", key: "controlled-reclaim-key", want: 400},
		{name: "invalid-grant", delegated: true, verifier: true, id: "invalid", reason: "reason", key: "controlled-reclaim-key", want: 400},
		{name: "reason-bound", delegated: true, verifier: true, id: grantID.String(), reason: strings.Repeat("理", 501), key: "controlled-reclaim-key", want: 400},
		{name: "missing-key", delegated: true, verifier: true, id: grantID.String(), reason: "reason", want: 400},
		{name: "missing-closure-authority", delegated: true, id: grantID.String(), reason: "reason", key: "controlled-reclaim-key", want: 503},
		{name: "missing-candidate", delegated: true, verifier: true, id: grantID.String(), reason: "reason", key: "controlled-reclaim-key", candidateErr: ErrNotFound, want: 404, candidates: 1},
		{name: "subject-not-anonymized", delegated: true, verifier: true, id: grantID.String(), reason: "reason", key: "controlled-reclaim-key", stateErr: ErrConflict, want: 409, candidates: 1, states: 1},
		{name: "effect-conflict", delegated: true, verifier: true, id: grantID.String(), reason: "reason", key: "controlled-reclaim-key", effectErr: ErrConflict, want: 409, candidates: 1, states: 1, effects: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := &closedGrantHandlerRepository{grant: MemberQuotaGrant{ID: grantID, OrganizationID: org, MemberUserID: member}, version: 7, candidateErr: tc.candidateErr, effectErr: tc.effectErr}
			state := &closedGrantHandlerState{err: tc.stateErr}
			service := NewService(repository)
			if tc.verifier {
				service.WithAccountClosureStateVerifier(state)
			}
			body, _ := json.Marshal(map[string]string{"reason": tc.reason})
			request := httptest.NewRequest(http.MethodPost, "/controlled-reclaim", strings.NewReader(string(body)))
			request.SetPathValue("organization_id", org.String())
			request.SetPathValue("grant_id", tc.id)
			request.Header.Set("Idempotency-Key", tc.key)
			if tc.delegated {
				request = request.WithContext(humandelegation.WithClaims(request.Context(), humandelegation.Claims{HumanUserID: actor}))
			}
			response := httptest.NewRecorder()
			service.reclaimAccountClosedGrantInternal(response, request)
			if response.Code != tc.want || repository.candidateCalls != tc.candidates || state.calls != tc.states || repository.effectCalls != tc.effects {
				t.Fatalf("status/candidate/state/effect=%d/%d/%d/%d", response.Code, repository.candidateCalls, state.calls, repository.effectCalls)
			}
			if tc.states > 0 && (state.user != member || state.version != 7) {
				t.Fatal("closure authority did not receive real candidate identity/version")
			}
			if tc.effects > 0 && (repository.org != org || repository.id != grantID || repository.actor != actor || repository.observedVersion != 7 || repository.key != tc.key || repository.reason != tc.reason) {
				t.Fatal("financial command lost verified actor/candidate lifecycle/body/key")
			}
			if tc.want == 200 {
				var receipt AccountClosedGrantReclaim
				if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.Grant.ID != grantID || receipt.AmountMicrocredits != 30 || receipt.SubjectLifecycleVersion != 7 || !receipt.Replayed {
					t.Fatal("authoritative receipt was not returned")
				}
			}
		})
	}
}
