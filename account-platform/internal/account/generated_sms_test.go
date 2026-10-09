package account

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHTTPGeneratedSMSProviderSendsAndVerifiesPlatformCode(t *testing.T) {
	var idempotencyKey string
	var delivered struct {
		Mobile      string `json:"mobile"`
		Purpose     string `json:"purpose"`
		Code        string `json:"code"`
		ChallengeID string `json:"challenge_id"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adapter-token" {
			t.Error("missing adapter authorization")
		}
		idempotencyKey = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&delivered); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "message-1"})
	}))
	defer server.Close()
	provider, err := NewHTTPGeneratedSMSProvider(HTTPGeneratedSMSConfig{Endpoint: server.URL, BearerToken: "adapter-token", VerificationKey: []byte(strings.Repeat("k", 32)), AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	challengeID := uuid.New()
	if provider.DeliveryRetryPolicy() != SMSDeliveryIdempotent {
		t.Fatalf("HTTP generated delivery policy=%s, want IDEMPOTENT", provider.DeliveryRetryPolicy())
	}
	prepared, err := provider.PrepareVerification(challengeID)
	if err != nil || len(prepared) != 32 {
		t.Fatalf("prepared verification=%x err=%v", prepared, err)
	}
	delivery, err := provider.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, challengeID)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.MessageID != "message-1" || len(delivery.VerificationData) != 32 || !hmac.Equal(prepared, delivery.VerificationData) || delivered.Mobile != "+8613800138000" || delivered.Purpose != "REGISTER" || delivered.ChallengeID != challengeID.String() || idempotencyKey != challengeID.String() || len(delivered.Code) != 6 {
		t.Fatalf("delivery=%#v payload=%#v", delivery, delivered)
	}
	firstCode := delivered.Code
	if _, err := provider.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, challengeID); err != nil {
		t.Fatal(err)
	}
	if delivered.Code != firstCode || idempotencyKey != challengeID.String() {
		t.Fatalf("same challenge retry changed code or idempotency key: code=%q key=%q", delivered.Code, idempotencyKey)
	}
	verification := SMSVerificationData{Provider: provider.Descriptor(), Opaque: delivery.VerificationData}
	verified, err := provider.VerifyCode(context.Background(), delivered.Mobile, challengeID, verification, delivered.Code)
	if err != nil || !verified {
		t.Fatalf("verified=%v err=%v", verified, err)
	}
	verified, err = provider.VerifyCode(context.Background(), delivered.Mobile, challengeID, verification, "000000")
	if err != nil || verified {
		t.Fatalf("wrong code verified=%v err=%v", verified, err)
	}
}

func TestSMSProviderFactoryRejectsUnknownMode(t *testing.T) {
	if _, err := NewSMSProvider(SMSProviderConfig{Mode: "vendor-condition-in-core"}); err == nil {
		t.Fatal("unknown provider mode must fail closed")
	}
}

func TestHTTPGeneratedSMSProviderDistinguishesDefiniteRejectionFromUnknownOutcome(t *testing.T) {
	status := http.StatusBadRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer server.Close()
	provider, err := NewHTTPGeneratedSMSProvider(HTTPGeneratedSMSConfig{Endpoint: server.URL, VerificationKey: []byte(strings.Repeat("k", 32)), AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.SendCode(context.Background(), "+8613800138000", SMSPurposeProfileMobile, uuid.New())
	var rejected *SMSDeliveryRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("HTTP 400 error=%v, want definite rejection", err)
	}
	status = http.StatusServiceUnavailable
	_, err = provider.SendCode(context.Background(), "+8613800138000", SMSPurposeProfileMobile, uuid.New())
	if errors.As(err, &rejected) {
		t.Fatalf("HTTP 503 must remain UNKNOWN, got %v", err)
	}
}
