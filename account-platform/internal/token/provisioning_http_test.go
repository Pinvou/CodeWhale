package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type provisioningHTTPFake struct {
	repairCalls int
}

func (*provisioningHTTPFake) Ensure(context.Context, AuthorizationContext, uuid.UUID, CatalogModel) (RouteProvisioning, error) {
	return RouteProvisioning{}, nil
}
func (*provisioningHTTPFake) List(context.Context, string, int) ([]RouteProvisioning, error) {
	return nil, nil
}
func (*provisioningHTTPFake) Detail(context.Context, uuid.UUID) (ProvisioningDetail, error) {
	return ProvisioningDetail{}, nil
}
func (f *provisioningHTTPFake) Repair(_ context.Context, id, _ uuid.UUID, _ string, _ string) (RouteProvisioning, error) {
	f.repairCalls++
	return RouteProvisioning{ID: id, Status: "PENDING"}, nil
}
func (*provisioningHTTPFake) ValidateModelReady(context.Context, uuid.UUID, string) error { return nil }
func (*provisioningHTTPFake) ContextEligible(context.Context, AuthorizationContext, uuid.UUID, string) (bool, string, error) {
	return true, "", nil
}

func TestAdminRepairProvisioningValidatesIdempotencyKeyAndUnicodeReason(t *testing.T) {
	provisioningID := uuid.MustParse("0199a200-0001-7000-8000-000000000020")
	for _, item := range []struct {
		name   string
		key    string
		reason string
		status int
		calls  int
	}{
		{name: "key too short", key: strings.Repeat("k", 15), reason: "valid", status: http.StatusBadRequest},
		{name: "minimum key", key: strings.Repeat("k", 16), reason: "理", status: http.StatusAccepted, calls: 1},
		{name: "maximum key and Unicode reason", key: strings.Repeat("k", 128), reason: strings.Repeat("😀", 500), status: http.StatusAccepted, calls: 1},
		{name: "key too long", key: strings.Repeat("k", 129), reason: "valid", status: http.StatusBadRequest},
		{name: "blank reason", key: strings.Repeat("k", 16), reason: " \n", status: http.StatusBadRequest},
		{name: "reason too long", key: strings.Repeat("k", 16), reason: strings.Repeat("理", 501), status: http.StatusBadRequest},
	} {
		t.Run(item.name, func(t *testing.T) {
			fake := &provisioningHTTPFake{}
			service := &Service{defaultRoutes: fake}
			payload, err := json.Marshal(map[string]string{"reason": item.reason})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/provisionings/"+provisioningID.String()+"/repair", bytes.NewReader(payload))
			request.SetPathValue("provisioning_id", provisioningID.String())
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", item.key)
			response := httptest.NewRecorder()
			service.adminRepairProvisioning(response, request)
			if response.Code != item.status || fake.repairCalls != item.calls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, fake.repairCalls, response.Body.String())
			}
		})
	}
}
