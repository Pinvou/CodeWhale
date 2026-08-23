package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAliyunSMSProviderSendAndVerify(t *testing.T) {
	t.Helper()
	challengeID := uuid.MustParse("018f0f76-3f5d-7d43-b630-163e3f635e5a")
	actions := make([]string, 0, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("x-acs-action")
		actions = append(actions, action)
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "ACS3-HMAC-SHA256 Credential=test-id,") {
			t.Fatalf("authorization header does not use ACS3 signing: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-acs-version") != "2017-05-25" {
			t.Fatalf("unexpected API version %q", r.Header.Get("x-acs-version"))
		}
		query := r.URL.Query()
		if query.Get("PhoneNumber") != "13800138000" || query.Get("CountryCode") != "86" || query.Get("OutId") != challengeID.String() {
			t.Fatalf("unexpected common parameters: %v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "SendSmsVerifyCode":
			if query.Get("SignName") != "gifted-sign" || query.Get("TemplateCode") != "100001" {
				t.Fatalf("unexpected sign or template: %v", query)
			}
			if query.Get("TemplateParam") != `{"code":"##code##","min":"5"}` || query.Get("ReturnVerifyCode") != "false" {
				t.Fatalf("provider-generated verification was not requested: %v", query)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Code": "OK", "Success": true, "RequestId": "request-send",
				"Model": map[string]any{"BizId": "biz-1", "OutId": challengeID.String()},
			})
		case "CheckSmsVerifyCode":
			if query.Get("VerifyCode") != "123456" {
				t.Fatalf("unexpected verification code parameter")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Code": "OK", "Success": true, "RequestId": "request-check",
				"Model": map[string]any{"VerifyResult": "PASS", "OutId": challengeID.String()},
			})
		default:
			t.Fatalf("unexpected action %q", action)
		}
	}))
	defer server.Close()

	provider, err := NewAliyunSMSProvider(AliyunSMSConfig{
		AccessKeyID: "test-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "gifted-sign",
		RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.client = server.Client()
	provider.now = func() time.Time { return time.Date(2026, time.August, 20, 3, 0, 0, 0, time.UTC) }

	delivery, err := provider.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, challengeID)
	if err != nil || delivery.MessageID != "biz-1" {
		t.Fatalf("send delivery=%#v err=%v", delivery, err)
	}
	verified, err := provider.VerifyCode(context.Background(), "+8613800138000", challengeID, SMSVerificationData{Provider: provider.Descriptor()}, " 123456 ")
	if err != nil || !verified {
		t.Fatalf("verify result=%v err=%v", verified, err)
	}
	if len(actions) != 2 || actions[0] != "SendSmsVerifyCode" || actions[1] != "CheckSmsVerifyCode" {
		t.Fatalf("unexpected actions: %v", actions)
	}
}

func TestAliyunSMSProviderTreatsUnknownAsInvalidCode(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Code": "OK", "Success": true, "Model": map[string]any{"VerifyResult": "UNKNOWN"},
		})
	}))
	defer server.Close()
	provider, err := NewAliyunSMSProvider(AliyunSMSConfig{
		AccessKeyID: "test-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "gifted-sign",
		RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.client = server.Client()
	verified, err := provider.VerifyCode(context.Background(), "+8613800138000", uuid.New(), SMSVerificationData{Provider: provider.Descriptor()}, "000000")
	if err != nil || verified {
		t.Fatalf("UNKNOWN must be a normal failed verification: verified=%v err=%v", verified, err)
	}
}

func TestAliyunSMSProviderRequiresHTTPS(t *testing.T) {
	_, err := NewAliyunSMSProvider(AliyunSMSConfig{
		AccessKeyID: "test-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "gifted-sign",
		RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: "http://dypnsapi.aliyuncs.com",
	})
	if err == nil {
		t.Fatal("expected an insecure endpoint to be rejected")
	}
}
