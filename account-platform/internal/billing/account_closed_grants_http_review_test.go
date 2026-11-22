package billing

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/google/uuid"
)

type closedGrantHTTPReviewRepository struct {
	Repository
	t                         *testing.T
	org, grant, actor, member uuid.UUID
	candidateErr              error
	candidateCalls, mutations atomic.Int32
}

func (r *closedGrantHTTPReviewRepository) AccountClosedGrantCandidate(_ context.Context, org, grant uuid.UUID) (MemberQuotaGrant, int64, error) {
	r.candidateCalls.Add(1)
	if org != r.org || grant != r.grant {
		r.t.Error("candidate target changed")
	}
	return MemberQuotaGrant{ID: r.grant, OrganizationID: r.org, MemberUserID: r.member}, 2, r.candidateErr
}

func (r *closedGrantHTTPReviewRepository) ReclaimAccountClosedGrant(_ context.Context, org, grant, actor uuid.UUID, lifecycle int64, reason, key string) (AccountClosedGrantReclaim, error) {
	r.mutations.Add(1)
	if org != r.org || grant != r.grant || actor != r.actor || lifecycle != 2 || reason != "controlled closed-grant reclaim" || key != "f08-http-original-operation-key" {
		r.t.Error("verified reclaim actor, target or original operation facts changed")
	}
	return AccountClosedGrantReclaim{Grant: MemberQuotaGrant{ID: r.grant, OrganizationID: r.org}, AmountMicrocredits: 5, SubjectLifecycleVersion: 2}, nil
}

type closedGrantHTTPReviewVerifier struct {
	t      *testing.T
	member uuid.UUID
	calls  atomic.Int32
}

func (v *closedGrantHTTPReviewVerifier) RequireAnonymized(_ context.Context, member uuid.UUID, lifecycle int64) error {
	v.calls.Add(1)
	if member != v.member || lifecycle != 2 {
		v.t.Error("closure verification target changed")
	}
	return nil
}

// Exercise the installed HTTP route, real delegation signatures and existing
// test-only workload authentication. No external issuer or database is used.
func TestF08ClosedGrantReclaimHTTPCapabilityBoundaries(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := humandelegation.NewSigner("f08-http-fixture", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	humanVerifier, err := humandelegation.NewVerifier(map[string]ed25519.PublicKey{"f08-http-fixture": publicKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, code string
		want       int
	}{
		{"complete", "", http.StatusOK},
		{"missing-closure-verifier", "ACCOUNT_CLOSURE_UNAVAILABLE", http.StatusServiceUnavailable},
		{"missing-repository-capability", "ACCOUNT_CLOSURE_UNAVAILABLE", http.StatusServiceUnavailable},
		{"business-conflict", "BILLING_CONFLICT", http.StatusConflict},
		{"invalid-key", "INVALID_REQUEST", http.StatusBadRequest},
		{"invalid-reason", "INVALID_REQUEST", http.StatusBadRequest},
		{"missing-delegation", "HUMAN_DELEGATION_REQUIRED", http.StatusForbidden},
		{"wrong-human-permission", "HUMAN_DELEGATION_REQUIRED", http.StatusForbidden},
		{"missing-workload", "SERVICE_AUTH_REQUIRED", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &closedGrantHTTPReviewRepository{t: t, org: uuid.New(), grant: uuid.New(), actor: uuid.New(), member: uuid.New()}
			closureVerifier := &closedGrantHTTPReviewVerifier{t: t, member: repo.member}
			var repository Repository = repo
			if test.name == "missing-repository-capability" {
				repository = nil
			}
			if test.name == "business-conflict" {
				repo.candidateErr = ErrConflict
			}
			service := NewService(repository).WithHumanDelegations(humanVerifier)
			if test.name != "missing-closure-verifier" {
				service.WithAccountClosureStateVerifier(closureVerifier)
			}
			server := httptest.NewServer(service.Handler("f08-http-fixture-workload"))
			defer server.Close()
			path := "/internal/v1/organizations/" + repo.org.String() + "/account-closed-grants/" + repo.grant.String() + "/reclaim"
			body := `{"reason":"controlled closed-grant reclaim"}`
			if test.name == "invalid-reason" {
				body = `{"reason":" "}`
			}
			permission := "org.grants.reclaim_closed"
			if test.name == "wrong-human-permission" {
				permission = "org.quota.allocate"
			}
			delegation, err := signer.Sign(humandelegation.Claims{Audience: "billing-service", HumanUserID: repo.actor, Permission: permission, AuthorizationVersion: 1, OrganizationID: &repo.org, ResourceType: "member-quota-grant", ResourceID: repo.grant.String(), HTTPMethod: http.MethodPost, HTTPPath: path, RequestSHA256: humandelegation.RequestDigest([]byte(body))})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "f08-http-original-operation-key")
			if test.name == "invalid-key" {
				request.Header.Set("Idempotency-Key", "short")
			}
			if test.name != "missing-workload" {
				request.Header.Set("Authorization", "Bearer f08-http-fixture-workload")
			}
			if test.name != "missing-delegation" {
				request.Header.Set(humanDelegationHeader, delegation)
			}
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != test.want || (test.code != "" && !strings.Contains(string(payload), `"code":"`+test.code+`"`)) {
				t.Fatalf("installed HTTP status=%d want=%d body=%s error=%v", response.StatusCode, test.want, payload, err)
			}
			wantCandidate, wantMutation, wantVerifier := int32(0), int32(0), int32(0)
			if test.name == "complete" {
				wantCandidate, wantMutation, wantVerifier = 1, 1, 1
			} else if test.name == "business-conflict" {
				wantCandidate = 1
			}
			if repo.candidateCalls.Load() != wantCandidate || repo.mutations.Load() != wantMutation || closureVerifier.calls.Load() != wantVerifier {
				t.Fatal("rejected or unavailable reclaim performed domain work")
			}
		})
	}
}
