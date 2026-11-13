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

func TestAliyunSMSPurposeTemplateCompatibility(t *testing.T) {
	type sent struct{ action, template string }
	requests := make(chan sent, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- sent{r.Header.Get("x-acs-action"), r.URL.Query().Get("TemplateCode")}:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Code": "OK", "Success": true, "RequestId": "template-fixture",
			"Model": map[string]any{"BizId": "template-biz", "OutId": r.URL.Query().Get("OutId")},
		})
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, profile, reset, wantProfile, wantReset string
	}{
		{"legacy-empty", "", "", "100002", "100002"},
		{"legacy-whitespace", " \t", "\n ", "100002", "100002"},
		{"independent-purpose-templates", " 100003 ", " 100004 ", "100003", "100004"},
		{"profile-only", "100003", "", "100003", "100002"},
		{"reset-only", "", "100004", "100002", "100004"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := NewAliyunSMSProvider(AliyunSMSConfig{
				AccessKeyID: "fixture-only-id", AccessKeySecret: strings.Repeat("s", 32), SignName: "fixture-sign",
				RegisterTemplate: "100001", LoginTemplate: "100002", ProfileMobileTemplate: tc.profile,
				PasswordResetTemplate: tc.reset, Endpoint: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			provider.client = server.Client()
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			for _, purpose := range []struct {
				value SMSPurpose
				want  string
			}{{SMSPurposeRegister, "100001"}, {SMSPurposeLogin, "100002"}, {SMSPurposeProfileMobile, tc.wantProfile}, {SMSPurposePasswordReset, tc.wantReset}} {
				if _, err := provider.SendCode(ctx, "+8613800138000", purpose.value, uuid.New()); err != nil {
					t.Fatalf("actual provider send %s: %v", purpose.value, err)
				}
				select {
				case got := <-requests:
					if got.action != "SendSmsVerifyCode" || got.template != purpose.want {
						t.Fatalf("purpose %s sent %s/%s, want template %s", purpose.value, got.action, got.template, purpose.want)
					}
				case <-ctx.Done():
					t.Fatal("provider fixture did not observe the request")
				}
			}
		})
	}
}
