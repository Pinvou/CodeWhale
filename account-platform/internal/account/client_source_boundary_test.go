package account

import (
	"net/http/httptest"
	"testing"
)

func TestClientSourceRequiresInternalAuthorityProof(t *testing.T) {
	for _, scenario := range []struct{ name, configured, proof, source, want string }{
		{"missing", "controlled", "", "203.0.113.50", "192.0.2.1"},
		{"forged", "controlled", "incorrect", "203.0.113.50", "192.0.2.1"},
		{"unconfigured", "", "", "203.0.113.50", "192.0.2.1"},
		{"trusted", "controlled", "controlled", "203.0.113.50", "203.0.113.50"},
		{"invalid_address", "controlled", "controlled", "arbitrary", "192.0.2.1"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := NewService(nil, nil).WithServiceToken(scenario.configured)
			request := httptest.NewRequest("POST", "/controlled", nil)
			request.RemoteAddr = "192.0.2.1:4321"
			request.Header.Set("X-Pinvou-Client-Source", scenario.source)
			request.Header.Set("X-Pinvou-Client-Source-Authorization", scenario.proof)
			if service.requestSource(request) != scenario.want {
				t.Fatal("untrusted source replaced peer authority")
			}
		})
	}
}
