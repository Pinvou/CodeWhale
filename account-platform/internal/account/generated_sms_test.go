package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHTTPGeneratedSMSProviderSendsAndVerifiesPlatformCode(t *testing.T) {
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
	delivery, err := provider.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, challengeID)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.MessageID != "message-1" || len(delivery.VerificationData) != 32 || delivered.Mobile != "+8613800138000" || delivered.Purpose != "REGISTER" || delivered.ChallengeID != challengeID.String() || len(delivered.Code) != 6 {
		t.Fatal("generated delivery facts are incomplete")
	}
	verification := SMSVerificationData{Provider: provider.Descriptor(), Opaque: delivery.VerificationData}
	verified, err := provider.VerifyCode(context.Background(), delivered.Mobile, challengeID, verification, delivered.Code)
	if err != nil || !verified {
		t.Fatalf("verified=%v err=%v", verified, err)
	}
	wrongCode := "0" + delivered.Code[1:]
	if delivered.Code[0] == '0' {
		wrongCode = "1" + delivered.Code[1:]
	}
	verified, err = provider.VerifyCode(context.Background(), delivered.Mobile, challengeID, verification, wrongCode)
	if err != nil || verified {
		t.Fatalf("wrong code verified=%v err=%v", verified, err)
	}
}

func TestSMSProviderFactoryRejectsUnknownMode(t *testing.T) {
	if _, err := NewSMSProvider(SMSProviderConfig{Mode: "vendor-condition-in-core"}); err == nil {
		t.Fatal("unknown provider mode must fail closed")
	}
}
