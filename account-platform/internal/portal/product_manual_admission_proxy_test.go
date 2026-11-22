package portal

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestManualProductAdmissionProxyAllowlist(t *testing.T) {
	base := "/admin/v1/recharge-products/" + uuid.NewString() + "/manual-admission-submissions"
	verification := uuid.NewString()
	for _, test := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, base, true},
		{http.MethodGet, base + "/" + verification, true},
		{http.MethodHead, base + "/" + verification, true},
		{http.MethodPost, base + "/" + verification + "/decisions", true},
		{http.MethodDelete, base + "/" + verification, false},
		{http.MethodPost, base + "/" + verification + "/publish", false},
		{http.MethodGet, base + "/not-a-uuid", false},
		{http.MethodPost, "/admin/v1/recharge-products/not-a-uuid/manual-admission-submissions", false},
	} {
		if got := manualProductAdmissionPathAllowed(test.method, test.path); got != test.allowed {
			t.Fatalf("%s %s: got %v, want %v", test.method, test.path, got, test.allowed)
		}
	}
}
