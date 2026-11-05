package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPersonalReadinessAttestationRejectsInvalidBodies(t *testing.T) {
	for name, body := range map[string]string{"malformed": `{"artifact_sha256":`, "second": `{} {}`, "unknown": `{"unexpected":true}`, "oversize": `{}` + strings.Repeat(" ", 1<<20)} {
		t.Run(name, func(t *testing.T) {
			service := &Service{clientReadiness: &ClientReadinessService{}}
			response := httptest.NewRecorder()
			service.issueClientReadinessAttestation(response, httptest.NewRequest(http.MethodPost, "/internal/v1/client-readiness/attestations", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
				t.Fatalf("invalid attestation returned success/empty body: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
