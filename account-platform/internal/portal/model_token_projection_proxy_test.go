package portal

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestModelTokenReadProjectionsAreIndependentlyAllowlisted(t *testing.T) {
	modelID := uuid.NewString()
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/operations-overview") {
		t.Fatal("Token-local operations overview is not available through its own proxy")
	}
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/models/"+modelID+"/price-projection") {
		t.Fatal("Billing model price projection is not available through its own proxy")
	}
	for _, invalid := range []struct {
		method, path string
	}{
		{http.MethodPost, "/admin/v1/operations-overview"},
		{http.MethodPost, "/admin/v1/models/" + modelID + "/price-projection"},
		{http.MethodGet, "/admin/v1/models/not-a-uuid/price-projection"},
		{http.MethodGet, "/admin/v1/models/" + modelID + "/price-projection/write"},
	} {
		if tokenAdminPathAllowed(invalid.method, invalid.path) || billingAdminPathAllowed(invalid.method, invalid.path) {
			t.Fatalf("unsafe cross-domain projection path accepted: %s %s", invalid.method, invalid.path)
		}
	}
}
