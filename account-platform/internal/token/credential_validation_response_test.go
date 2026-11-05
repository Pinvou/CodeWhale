package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCredentialValidationDurableAmbiguityReference(t *testing.T) {
	reference := uuid.MustParse("0199a200-0001-7000-8000-000000000501")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"version_changed_after_probe", ErrCatalogVersionConflict},
		{"authority_changed_after_probe", ErrTokenRouteActivationChanged},
		{"commit_outcome_unknown", errors.New("synthetic secret must not escape")},
		{"entry_deadline_after_durable_claim", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/token/admin/v1/commands", nil)
			// Repository error paths return an empty completed object. Recovery
			// must use the previously persisted command, not this empty result.
			writeCredentialValidationCommandResponse(w, r, reference, TokenCommandResult{}, tc.err)
			var body struct {
				Code    string `json:"code"`
				Details struct {
					Reference uuid.UUID `json:"command_reference"`
					Query     string    `json:"result_query"`
				} `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 503 || body.Code != "COMMAND_RESULT_UNKNOWN" || body.Details.Reference != reference || body.Details.Query != "/token/admin/v1/commands/"+reference.String() {
				t.Fatalf("lost durable recovery identity: status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "synthetic secret") {
				t.Fatal("internal error leaked")
			}
		})
	}
}

func TestCredentialValidationResponseKeepsExistingOutcomes(t *testing.T) {
	reference := uuid.MustParse("0199a200-0001-7000-8000-000000000502")
	for _, tc := range []struct {
		name, status string
		err          error
		httpStatus   int
	}{
		{"live_claim", "PENDING", fmt.Errorf("wrapped: %w", ErrCredentialProbeClaimed), 202},
		{"validated", "SUCCEEDED", nil, 200},
		{"expired_lease_terminal", "FAILED", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := TokenCommandResult{CommandReference: reference, Status: tc.status}
			w := httptest.NewRecorder()
			writeCredentialValidationCommandResponse(w, httptest.NewRequest(http.MethodPost, "/token/admin/v1/commands", nil), reference, result, tc.err)
			var actual TokenCommandResult
			if json.Unmarshal(w.Body.Bytes(), &actual) != nil || w.Code != tc.httpStatus || actual.CommandReference != reference || actual.Status != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("changed original result: %d %s", w.Code, w.Body.String())
			}
			if tc.httpStatus == 202 && w.Header().Get("Retry-After") != "2" {
				t.Fatal("pending retry guidance changed")
			}
		})
	}
}

func TestCredentialValidationUnknownWithoutDurableIdentity(t *testing.T) {
	w := httptest.NewRecorder()
	writeCredentialValidationCommandResponse(w, httptest.NewRequest(http.MethodPost, "/token/admin/v1/commands", nil), uuid.Nil, TokenCommandResult{}, errors.New("storage failed"))
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != 503 || body["code"] != "COMMAND_RESULT_UNKNOWN" {
		t.Fatalf("changed unknown response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := body["details"]; ok {
		t.Fatal("invented durable command identity")
	}
}
