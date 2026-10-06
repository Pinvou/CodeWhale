package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMemberQuotaAllocationFingerprintIsStableAndRequestBound(t *testing.T) {
	request := MemberQuotaAllocation{OrganizationID: uuid.Must(uuid.NewV7()), MemberUserID: uuid.Must(uuid.NewV7()), PackageID: uuid.Must(uuid.NewV7()), ComponentID: uuid.Must(uuid.NewV7()), ComponentType: " base ", AmountMicrocredits: 80, MembershipVersion: 2, CreatedBy: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-command-key-0001"}
	normalized, first, err := normalizeMemberQuotaAllocation(request)
	if err != nil || normalized.ComponentType != "BASE" {
		t.Fatalf("normalize allocation: %#v %v", normalized, err)
	}
	_, replay, err := normalizeMemberQuotaAllocation(request)
	if err != nil || replay != first {
		t.Fatal("same allocation must retain its request fingerprint")
	}
	request.AmountMicrocredits++
	_, changed, err := normalizeMemberQuotaAllocation(request)
	if err != nil || changed == first {
		t.Fatal("changed amount must change the request fingerprint")
	}
	request.AmountMicrocredits--
	request.CreatedBy = uuid.Must(uuid.NewV7())
	_, changedActor, err := normalizeMemberQuotaAllocation(request)
	if err != nil || changedActor == first {
		t.Fatal("changed actor must change the request fingerprint")
	}
}

func TestMemberQuotaBatchChildKeysAreStableAndIndependent(t *testing.T) {
	item := memberQuotaBatchItem{MemberUserID: uuid.Must(uuid.NewV7()), PackageID: uuid.Must(uuid.NewV7()), ComponentID: uuid.Must(uuid.NewV7())}
	first := deriveMemberQuotaChildKey("stable-batch-key-0001", 0, item)
	if first != deriveMemberQuotaChildKey("stable-batch-key-0001", 0, item) {
		t.Fatal("same batch item must derive the same child idempotency key")
	}
	if first == deriveMemberQuotaChildKey("stable-batch-key-0001", 1, item) {
		t.Fatal("different item indexes must derive independent child keys")
	}
}

func TestMemberQuotaErrorCodesRemainStable(t *testing.T) {
	if got := memberQuotaErrorCode(ErrInsufficientComponentCapacity); got != "INSUFFICIENT_COMPONENT_CAPACITY" {
		t.Fatalf("unexpected capacity code %q", got)
	}
	if got := memberQuotaErrorCode(ErrIdempotencyConflict); got != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("unexpected idempotency code %q", got)
	}
	if !errors.Is(ErrIdempotencyConflict, ErrIdempotencyConflict) {
		t.Fatal("sentinel error must remain comparable")
	}
}

// Only transport gating is controlled here; the PostgreSQL case below checks the real grant.
type quotaReasonRepository struct {
	Repository
	calls int
}

func (r *quotaReasonRepository) AllocateMemberQuota(context.Context, MemberQuotaAllocation) (MemberQuotaGrant, bool, error) {
	panic("not used")
}
func (r *quotaReasonRepository) ListMemberQuotaGrants(context.Context, uuid.UUID, *uuid.UUID, int) ([]MemberQuotaGrant, error) {
	panic("not used")
}
func (r *quotaReasonRepository) RevokeMemberQuota(_ context.Context, org, grant, actor uuid.UUID, reason, key string) (MemberQuotaGrant, error) {
	r.calls++
	return MemberQuotaGrant{ID: grant, OrganizationID: org}, nil
}
func TestMemberQuotaRevokeReasonCharacterBoundary(t *testing.T) {
	for _, count := range []int{500, 501} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			repo := &quotaReasonRepository{}
			service := NewService(repo)
			body, _ := json.Marshal(map[string]any{"actor_user_id": uuid.New(), "reason": strings.Repeat("\u539f", count)})
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			request.SetPathValue("organization_id", uuid.NewString())
			request.SetPathValue("grant_id", uuid.NewString())
			request.Header.Set("Idempotency-Key", uuid.NewString())
			response := httptest.NewRecorder()
			service.revokeMemberQuotaGrant(response, request)
			want, calls := 200, 1
			if count > 500 {
				want, calls = 400, 0
			}
			if response.Code != want || repo.calls != calls {
				t.Fatalf("status=%d calls=%d", response.Code, repo.calls)
			}
		})
	}
}
