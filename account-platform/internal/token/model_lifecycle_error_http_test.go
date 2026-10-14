package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelLifecycleErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		model  bool
	}{
		{"unavailable", ErrModelUnavailable, http.StatusConflict, "MODEL_UNAVAILABLE", true},
		{"wrapped-unavailable", fmt.Errorf("catalog: %w", ErrModelUnavailable), http.StatusConflict, "MODEL_UNAVAILABLE", true},
		{"deprecated", ErrModelDeprecated, http.StatusConflict, "MODEL_DEPRECATED", true},
		{"permission-unchanged", ErrForbidden, http.StatusForbidden, "TOKEN_FORBIDDEN", false},
		{"wrapped-deprecated", fmt.Errorf("catalog: %w", ErrModelDeprecated), http.StatusConflict, "MODEL_DEPRECATED", true},
		{"not-found-unchanged", ErrNotFound, http.StatusNotFound, "TOKEN_NOT_FOUND", false},
		{"unknown-cannot-produce-marker", errors.New("MODEL_DEPRECATED do-not-echo"), http.StatusInternalServerError, "INTERNAL_ERROR", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Service{}).writeError(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), tc.err)
			var result struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || result.Code != tc.code {
				t.Fatalf("status/code=%d/%s", w.Code, result.Code)
			}
			want := ""
			if tc.model {
				want = tc.code
			}
			if got := w.Header().Get("X-Pinvou-Model-Rejection-v1"); got != want {
				t.Fatalf("model marker=%q want=%q", got, want)
			}
			if result.Message == "" || result.Message == tc.err.Error() || strings.Contains(result.Message, "do-not-echo") {
				t.Fatal("raw catalog or unknown error exposed")
			}
		})
	}
}

func TestUpstreamCannotProduceModelRejectionMarker(t *testing.T) {
	upstream := http.Header{
		"X-Pinvou-Model-Rejection-v1": {"MODEL_DEPRECATED"},
		"x-pinvou-model-rejection-v1": {"MODEL_UNAVAILABLE"},
		"Content-Type":                {"application/json"},
		"X-Ordinary-Provider-Header":  {"preserved"},
	}
	result := make(http.Header)
	(&HTTPUpstreamClient{}).CopyResponseHeaders(result, upstream)
	if result.Get("X-Pinvou-Model-Rejection-v1") != "" {
		t.Fatal("supplier forged gateway lifecycle authority")
	}
	if result.Get("Content-Type") != "application/json" || result.Get("X-Ordinary-Provider-Header") != "preserved" {
		t.Fatal("ordinary provider headers changed")
	}
}
