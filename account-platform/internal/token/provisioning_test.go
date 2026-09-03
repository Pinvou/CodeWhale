package token

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProvisioningBackoffIsBoundedAndDeterministic(t *testing.T) {
	id := uuid.MustParse("018f47b4-e124-7dca-9c85-6f7688436600")
	first := provisioningBackoff(id, 1)
	if first < 4*time.Second || first > 6*time.Second {
		t.Fatalf("first backoff outside jitter bound: %s", first)
	}
	if repeated := provisioningBackoff(id, 1); repeated != first {
		t.Fatalf("backoff must be deterministic: %s != %s", repeated, first)
	}
	maximum := provisioningBackoff(id, 20)
	if maximum < 240*time.Second || maximum > 360*time.Second {
		t.Fatalf("maximum backoff outside jitter bound: %s", maximum)
	}
}

func TestProvisioningStatusUsesStableAdmissionErrors(t *testing.T) {
	cases := []struct {
		status string
		want   error
	}{
		{status: "PENDING", want: ErrRouteProvisioningPending},
		{status: "FAILED", want: ErrRouteProvisioningFailed},
		{status: "REPAIR_REQUIRED", want: ErrRouteRepairRequired},
		{status: "READY", want: nil},
	}
	for _, item := range cases {
		if got := provisioningError(RouteProvisioning{Status: item.status}); !errors.Is(got, item.want) {
			t.Fatalf("status %s returned %v, want %v", item.status, got, item.want)
		}
	}
}

func TestProvisioningDesiredSignatureFreezesProtocolVersion(t *testing.T) {
	provisioner := &PostgresDefaultRouteProvisioner{config: DefaultRouteConfig{Provider: "PROVIDER", AdapterVersion: "adapter-v1", BaseURL: "https://provider.invalid", Secret: []byte("test-secret"), Priority: 100}}
	scopeID, walletID, modelID := uuid.New(), uuid.New(), uuid.New()
	v1 := CatalogModel{ModelID: modelID, ProtocolCode: "CHAT", ProtocolDefinitionID: uuid.New(), UpstreamModelRef: "stable-model"}
	v2 := v1
	v2.ProtocolDefinitionID = uuid.New()
	if first, second := provisioner.desiredSignature("PERSONAL", scopeID, walletID, v1), provisioner.desiredSignature("PERSONAL", scopeID, walletID, v2); first == second {
		t.Fatalf("protocol version did not change desired signature: %s", first)
	}
}
