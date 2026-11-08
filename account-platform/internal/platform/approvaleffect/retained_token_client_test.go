package approvaleffect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

func TestF04TokenEffectUsesExactOwnerAuthority(t *testing.T) {
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Token effect contacted Billing")
		w.WriteHeader(503)
	}))
	defer billing.Close()
	requestID, fingerprint := uuid.New(), strings.Repeat("a", 64)
	invalid := false
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/approval-effects/"+requestID.String() || r.Header.Get("Authorization") != "Bearer controlled-effect" || r.URL.Query().Get("command_fingerprint") != fingerprint {
			t.Error("effect request binding changed")
		}
		view := Observation{SchemaVersion: 1, Authority: AuthorityToken, ApprovalRequestID: requestID, CommandType: r.URL.Query().Get("command_type"), CommandFingerprint: fingerprint, State: StateAbsent, ObservedAt: time.Now().UTC()}
		if invalid {
			view.Authority = AuthorityBilling
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	}))
	defer token.Close()
	client, err := NewClientWithToken(billing.URL, token.URL, "controlled-effect")
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range approvalconsumer.TokenCommands() {
		t.Run(descriptor.CommandType, func(t *testing.T) {
			invalid = false
			result, err := client.Observe(context.Background(), requestID, descriptor.CommandType, fingerprint)
			if err != nil || result.Authority != AuthorityToken || result.State != StateAbsent {
				t.Fatal("valid Token observation rejected", err)
			}
			invalid = true
			if _, err = client.Observe(context.Background(), requestID, descriptor.CommandType, fingerprint); err == nil {
				t.Fatal("wrong effect authority accepted")
			}
		})
	}
	legacy, _ := NewClient(billing.URL, "controlled-effect")
	if _, err = legacy.Observe(context.Background(), requestID, approvalconsumer.TokenCommands()[0].CommandType, fingerprint); err == nil {
		t.Fatal("missing Token authority accepted")
	}
}
