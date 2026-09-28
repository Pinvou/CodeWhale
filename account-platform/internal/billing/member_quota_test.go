package billing

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestMemberQuotaReservationErrorsRemainSpecific(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{err: ErrMemberLifecycleConflict, status: http.StatusConflict, code: "MEMBER_LIFECYCLE_CONFLICT"},
		{err: ErrMemberQuotaInsufficient, status: http.StatusPaymentRequired, code: "MEMBER_QUOTA_INSUFFICIENT"},
	}
	service := &Service{}
	for _, test := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/holds/reserve", nil)
		service.writeError(recorder, request, test.err)
		var payload struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || recorder.Code != test.status || payload.Code != test.code {
			t.Fatalf("error=%v status=%d payload=%q decode=%v", test.err, recorder.Code, recorder.Body.String(), err)
		}
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
