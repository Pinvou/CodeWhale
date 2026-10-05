package account

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordResetRejectsInvalidBodyBeforeAnyServiceCall(t *testing.T) {
	service := &Service{}
	server := httptest.NewServer(http.HandlerFunc(service.resetForgottenPassword))
	defer server.Close()
	for _, scenario := range []struct{ name, body string }{
		{"malformed", "{"},
		{"unknown_field", `{"unexpected":true}`},
		{"invalid_uuid", `{"challenge_id":"not-a-uuid"}`},
		{"missing_challenge", `{}`},
		{"multiple_json", `{} {}`},
		{"oversized", `{"code":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			response, err := server.Client().Post(server.URL, "application/json", strings.NewReader(scenario.body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			var problem struct {
				Code string `json:"code"`
			}
			if err != nil || json.Unmarshal(data, &problem) != nil || response.StatusCode != http.StatusBadRequest || problem.Code != "INVALID_REQUEST" {
				t.Fatal("invalid input must be rejected with the static 400 contract")
			}
		})
	}
}
