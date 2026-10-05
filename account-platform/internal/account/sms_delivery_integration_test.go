package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresSMSUnknownDeliveryRetainsCooldownAndExplicitRejectionAllowsRetry(t *testing.T) {
	for _, adapter := range []string{"http", "aliyun"} {
		for _, outcome := range []string{"lost_ack", "malformed", "oversized", "server_error", "unproven_client_error", "explicit_rejection"} {
			if adapter == "http" && outcome == "explicit_rejection" {
				// The neutral adapter contract has no authoritative negative response.
				continue
			}
			t.Run(adapter+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				repo := accountDatabase(t)
				var dispatches atomic.Int32
				providerHTTP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					dispatches.Add(1)
					switch outcome {
					case "lost_ack":
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("controlled hijack failed")
							return
						}
						connection.Close()
					case "malformed":
						io.WriteString(w, `{`)
					case "oversized":
						io.WriteString(w, `{}`+strings.Repeat(" ", 256<<10)+"tail")
					case "server_error":
						w.WriteHeader(500)
						io.WriteString(w, `{"Code":"INVALID_PARAMETERS","Success":false}`)
					case "unproven_client_error":
						w.WriteHeader(400)
						io.WriteString(w, `{"Code":"UNKNOWN","Success":false}`)
					case "explicit_rejection":
						w.WriteHeader(400)
						io.WriteString(w, `{"Code":"INVALID_PARAMETERS","Success":false}`)
					}
				}))
				defer providerHTTP.Close()
				var provider SMSProvider
				if adapter == "http" {
					configured, err := NewSMSProvider(SMSProviderConfig{Provider: "http", Endpoint: providerHTTP.URL, ServiceToken: strings.Repeat("s", 32)})
					if err != nil {
						t.Fatal(err)
					}
					configured.(*httpSMSProvider).client = providerHTTP.Client()
					provider = configured
				} else {
					configured, err := NewAliyunSMSProvider(AliyunSMSConfig{Endpoint: providerHTTP.URL, AccessKeyID: "controlled", AccessKeySecret: strings.Repeat("s", 32), SignName: "controlled", RegisterTemplate: "controlled", LoginTemplate: "controlled"})
					if err != nil {
						t.Fatal(err)
					}
					configured.client = providerHTTP.Client()
					provider = configured
				}
				auth, err := NewMobileAuthService(repo, provider, &identityProvisionerFake{}, "https://identity.example.test", registrationTokensForTest(t))
				if err != nil {
					t.Fatal(err)
				}
				accountHTTP := httptest.NewServer(NewService(repo, nil).WithMobileAuth(auth, strings.Repeat("s", 32)).Handler(nil))
				defer accountHTTP.Close()
				post := func(path string, body any, expected int) []byte {
					t.Helper()
					encoded, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					response, err := accountHTTP.Client().Post(accountHTTP.URL+path, "application/json", strings.NewReader(string(encoded)))
					if err != nil {
						t.Fatal("controlled Account request failed")
					}
					defer response.Body.Close()
					data, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != expected {
						t.Fatalf("route status=%d expected=%d", response.StatusCode, expected)
					}
					return data
				}
				for attempt := 0; attempt < 2; attempt++ {
					var captcha SliderCaptchaResponse
					if err := json.Unmarshal(post("/v1/auth/captcha/challenges", map[string]any{"mobile": "13800138000", "purpose": "REGISTER"}, 201), &captcha); err != nil {
						t.Fatal(err)
					}
					var offset int
					// Read only this test's own puzzle; this is not human captcha acceptance.
					if err := repo.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&offset); err != nil {
						t.Fatal(err)
					}
					expected := 503
					if attempt == 1 && outcome != "explicit_rejection" {
						expected = 429
					}
					data := post("/v1/auth/sms/challenges", map[string]any{"username": "controlled.user", "mobile": "13800138000", "password_handle": "controlled-reference", "password_policy_version": "user-v1", "purpose": "REGISTER", "captcha_id": captcha.ID, "captcha_offset": offset}, expected)
					var result struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(data, &result); err != nil {
						t.Fatal(err)
					}
					code := "SMS_UNAVAILABLE"
					if expected == 429 {
						code = "SMS_RATE_LIMITED"
					}
					if result.Code != code {
						t.Fatal("unexpected public SMS error classification")
					}
				}
				var created, failed int
				if err := repo.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='CREATED'),count(*) FILTER (WHERE status='FAILED') FROM sms_challenges`).Scan(&created, &failed); err != nil {
					t.Fatal(err)
				}
				if outcome == "explicit_rejection" {
					if dispatches.Load() != 2 || created != 0 || failed != 2 {
						t.Fatal("authoritative rejection did not safely release cooldown")
					}
				} else if dispatches.Load() != 1 || created != 1 || failed != 0 {
					t.Fatal("uncertain delivery bypassed cooldown")
				}
			})
		}
	}
}

func TestAliyunSMSRejectClassificationRequiresCompleteConsistentProof(t *testing.T) {
	for index, body := range []string{
		`{"Code":"INVALID_PARAMETERS"}`,
		`{"Code":"INVALID_PARAMETERS","Success":true}`,
		`{"Code":"INVALID_PARAMETERS","Success":false,"Model":{"BizId":"accepted"}}`,
		`{"Code":"INVALID_PARAMETERS","Success":false} {}`,
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); io.WriteString(w, body) }))
			defer server.Close()
			provider, err := NewAliyunSMSProvider(AliyunSMSConfig{Endpoint: server.URL, AccessKeyID: "controlled", AccessKeySecret: strings.Repeat("s", 32), SignName: "controlled", RegisterTemplate: "controlled", LoginTemplate: "controlled"})
			if err != nil {
				t.Fatal(err)
			}
			provider.client = server.Client()
			_, err = provider.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, uuid.New())
			if err == nil || err == ErrSMSRejected {
				t.Fatal("incomplete or inconsistent response was treated as authoritative rejection")
			}
		})
	}
}
