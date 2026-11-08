package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type approvalCapabilityRepositoryStub struct {
	Repository
	control AdminApprovalControl
	calls   int
}

func (stub *approvalCapabilityRepositoryStub) ApprovalActionControl(context.Context) (AdminApprovalControl, error) {
	stub.calls++
	return stub.control, nil
}

func TestApprovalCapabilityIsClosedAndUncacheable(t *testing.T) {
	stub := &approvalCapabilityRepositoryStub{control: validAdminApprovalControl(AdminApprovalModeLegacy, AdminApprovalRelayRunning)}
	service := NewService(stub, nil)
	handler := httpserver.CorrelationHandler(http.HandlerFunc(service.adminApprovalCapability))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/v2/approval-capability", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || stub.calls != 1 {
		t.Fatalf("status=%d headers=%v calls=%d body=%s", response.Code, response.Header(), stub.calls, response.Body.String())
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 9 {
		t.Fatalf("capability not closed: %s", response.Body.String())
	}
	for _, key := range []string{"schema_version", "mode", "approval_wire_version", "proof_required", "epoch", "authorization_relay_state", "authorization_relay_epoch", "version", "request_id"} {
		if len(fields[key]) == 0 {
			t.Fatalf("capability missing %s: %s", key, response.Body.String())
		}
	}
	var capability approvalCapability
	if json.Unmarshal(response.Body.Bytes(), &capability) != nil || capability.Epoch != stub.control.ApprovalEpoch ||
		capability.Version != stub.control.Version || capability.RequestID == "" {
		t.Fatalf("incorrect capability: %s", response.Body.String())
	}
}
