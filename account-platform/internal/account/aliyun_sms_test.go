package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
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
			if query.Get("TemplateParam") != `{"code":"##code##","min":"5"}` || query.Get("ReturnVerifyCode") != "false" || query.Get("DuplicatePolicy") != "1" {
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
	if provider.DeliveryRetryPolicy() != SMSDeliveryAtMostOnce {
		t.Fatalf("Aliyun delivery policy=%s, want AT_MOST_ONCE", provider.DeliveryRetryPolicy())
	}
	if verification, err := provider.PrepareVerification(challengeID); err != nil || verification != nil {
		t.Fatalf("Aliyun provider-managed verification=%x err=%v", verification, err)
	}

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

func TestAliyunSMSTransportErrorsOmitRequestInputs(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(observability.NewLogger(&logs, observability.Config{Service: "account-service", Environment: "test"}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	provider, err := NewAliyunSMSProvider(AliyunSMSConfig{
		AccessKeyID: "fixture-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "fixture",
		RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: server.URL,
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	provider.client = server.Client()
	server.Close()
	challenge := uuid.New()
	const callerRequestID = "fixture-caller-request"
	const callerTraceID = "11111111222222223333333344444444"
	ctx := observability.ContextForTrace(context.Background(), callerRequestID, callerTraceID)
	_, sendErr := provider.SendCode(ctx, "+8613800138000", SMSPurposeLogin, challenge)
	var rejected *SMSDeliveryRejectedError
	if sendErr == nil || errors.As(sendErr, &rejected) {
		t.Fatal("send transport failure must leave delivery outcome unknown")
	}
	if !strings.Contains(logs.String(), `"event":"aliyun_sms_send_failed"`) ||
		!strings.Contains(logs.String(), `"stage":"transport"`) {
		t.Fatal("send transport failure was not recorded")
	}
	verified, err := provider.VerifyCode(context.Background(), "+8613800138000", challenge,
		SMSVerificationData{Provider: provider.Descriptor()}, "903489")
	if verified || err == nil || !strings.Contains(logs.String(), `"event":"aliyun_sms_verification_failed"`) ||
		!strings.Contains(logs.String(), `"stage":"transport"`) {
		t.Fatal("verification transport failure was not recorded")
	}
	for _, diagnostic := range []string{sendErr.Error(), err.Error(), logs.String()} {
		for _, sensitive := range []string{"13800138000", "903489", challenge.String(), server.URL, "fixture-id", callerRequestID, callerTraceID} {
			if strings.Contains(diagnostic, sensitive) {
				t.Fatal("transport diagnostics contain request data")
			}
		}
	}
}

func TestAliyunSMSDiagnosticRetainsOnlySafeProviderContext(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(observability.NewLogger(&logs, observability.Config{Service: "account-service", Environment: "test"}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"Code":"fixture-upstream-code","Message":"fixture-upstream-message"}`))
	}))
	defer server.Close()
	provider, err := NewAliyunSMSProvider(AliyunSMSConfig{
		AccessKeyID: "fixture-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "fixture",
		RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.client = server.Client()
	challenge := uuid.New()
	assertDiagnostic := func(event, stage string) {
		t.Helper()
		var diagnostic struct {
			Event  string `json:"event"`
			Stage  string `json:"stage"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal(logs.Bytes(), &diagnostic); err != nil || diagnostic.Event != event ||
			diagnostic.Stage != stage || diagnostic.Status != http.StatusServiceUnavailable {
			t.Fatal("provider diagnostic did not preserve the bounded event, stage and status", err)
		}
		for _, sensitive := range []string{"fixture-upstream-code", "fixture-upstream-message", "13800138000", "903489", challenge.String(), "fixture-id", server.URL} {
			if strings.Contains(logs.String(), sensitive) {
				t.Fatal("provider diagnostic included provider or request data")
			}
		}
	}
	if _, err := provider.SendCode(context.Background(), "+8613800138000", SMSPurposeLogin, challenge); err == nil {
		t.Fatal("provider outage was accepted as successful delivery")
	}
	assertDiagnostic("aliyun_sms_send_failed", "http_status")
	logs.Reset()
	verified, err := provider.VerifyCode(context.Background(), "+8613800138000", challenge,
		SMSVerificationData{Provider: provider.Descriptor()}, "903489")
	if verified || err == nil {
		t.Fatal("provider outage was accepted as successful verification")
	}
	assertDiagnostic("aliyun_sms_verification_failed", "provider_rejected")
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

func TestAliyunSMSProviderClassifiesOnlyExplicitProviderRejection(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "MOBILE_REJECTED", "Success": false})
	}))
	defer server.Close()
	provider, err := NewAliyunSMSProvider(AliyunSMSConfig{AccessKeyID: "test-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "gifted-sign", RegisterTemplate: "100001", LoginTemplate: "100001", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	provider.client = server.Client()
	_, err = provider.SendCode(context.Background(), "+8613800138000", SMSPurposeProfileMobile, uuid.New())
	var rejected *SMSDeliveryRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("explicit provider rejection error=%v", err)
	}
	status = http.StatusServiceUnavailable
	_, err = provider.SendCode(context.Background(), "+8613800138000", SMSPurposeProfileMobile, uuid.New())
	if errors.As(err, &rejected) {
		t.Fatalf("HTTP 503 must remain UNKNOWN, got %v", err)
	}
}
