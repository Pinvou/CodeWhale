package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMergedSMSAdaptersKeepUnknownDeliveryAndFullResponseBoundaries(t *testing.T) {
	for _, adapter := range []string{"aliyun-managed", "http", "http-generated"} {
		for _, scenario := range []string{"valid", "trailing", "oversized", "lost_ack", "redirect", "rejected"} {
			t.Run(adapter+"/"+scenario, func(t *testing.T) {
				dispatches := 0
				target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Error("credentials must not follow redirects")
					w.WriteHeader(200)
				}))
				defer target.Close()
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					dispatches++
					if scenario == "lost_ack" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					if scenario == "redirect" {
						http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
						return
					}
					body := `{"message_id":"public-test-delivery"}`
					if adapter == "aliyun-managed" {
						body = `{"Code":"OK","Success":true,"Model":{"BizId":"public-test-delivery"}}`
					}
					if scenario == "rejected" {
						w.WriteHeader(http.StatusBadRequest)
						body = `{"Code":"MOBILE_NUMBER_ILLEGAL","Success":false}`
					}
					if scenario == "trailing" {
						body += ` {"second":true}`
					}
					if scenario == "oversized" {
						body += strings.Repeat(" ", 256<<10) + "invalid"
					}
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				config := SMSProviderConfig{Mode: adapter, Endpoint: server.URL, ServiceToken: strings.Repeat("p", 32), Aliyun: AliyunSMSConfig{AccessKeyID: "public-test-key", AccessKeySecret: "public-test-secret", SignName: "public-test-sign", RegisterTemplate: "public-test-register", LoginTemplate: "public-test-login", Endpoint: server.URL}, HTTPGenerated: HTTPGeneratedSMSConfig{Endpoint: server.URL, BearerToken: strings.Repeat("p", 32), VerificationKey: []byte(strings.Repeat("v", 32))}}
				provider, err := NewSMSProvider(config)
				if err != nil {
					t.Fatal(err)
				}
				// Only trust the owned TLS certificate; preserve the production timeout
				// and redirect policy while using the real adapter request/decoder.
				switch p := provider.(type) {
				case *AliyunSMSProvider:
					p.client.Transport = server.Client().Transport
				case *httpSMSProvider:
					p.client.Transport = server.Client().Transport
				case *HTTPGeneratedSMSProvider:
					p.client.Transport = server.Client().Transport
				}
				delivery, err := provider.SendCode(context.Background(), "+8613800000000", SMSPurposeRegister, uuid.New())
				if dispatches != 1 {
					t.Fatalf("expected one real dispatch, got %d", dispatches)
				}
				if scenario == "valid" {
					if err != nil || delivery.MessageID != "public-test-delivery" {
						t.Fatal("valid delivery must be returned")
					}
					return
				}
				if err == nil {
					t.Fatal("uncertain or rejected response must fail")
				}
				expectedReject := adapter == "aliyun-managed" && scenario == "rejected"
				if errors.Is(err, ErrSMSRejected) != expectedReject {
					t.Fatal("only documented authoritative Aliyun rejection may cancel cooldown")
				}
			})
		}
	}
}

func TestPublishedSMSProviderModesRemainAvailable(t *testing.T) {
	for _, mode := range []string{"http", "aliyun"} {
		config := SMSProviderConfig{Provider: mode, Endpoint: "https://example.invalid", ServiceToken: strings.Repeat("p", 32), Aliyun: AliyunSMSConfig{AccessKeyID: "public-test-key", AccessKeySecret: "public-test-secret", SignName: "public-test-sign", RegisterTemplate: "public-test-register", LoginTemplate: "public-test-login"}}
		provider, err := NewSMSProvider(config)
		if err != nil || provider == nil {
			t.Fatal("published vendor-neutral configuration must remain constructible")
		}
	}
}
