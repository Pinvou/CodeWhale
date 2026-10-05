package token

import (
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Existing administration fixtures now pass through the production Account
// permission client. Only the fixture issuer/subject pair is granted; public
// callers are not promoted by a realm role in the production service.
func tokenTestAuthorizer(t *testing.T) *authorization.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/internal/v1/authorization/check" || r.Header.Get("Authorization") != "Bearer fixture-authority-service-token" {
			t.Error("unexpected authority request")
			w.WriteHeader(403)
			return
		}
		var body struct {
			Issuer     string `json:"issuer"`
			Subject    string `json:"subject"`
			Permission string `json:"permission"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		allowed := ((body.Issuer == "https://fixture-authority.invalid" && body.Subject == "fixture-platform-operator") || (body.Issuer == "fixture-issuer" && body.Subject == "fixture-admin")) && body.Permission != ""
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": uuid.MustParse("11111111-1111-4111-8111-111111111111")})
	}))
	t.Cleanup(server.Close)
	client, err := authorization.NewClient(server.URL, "fixture-authority-service-token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}
