package identityadapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBootstrapPasswordRejectionProofHTTPBoundary(t *testing.T) {
	const policy = `{"error":"invalidPasswordHistoryMessage","error_description":"localized policy rejection"}`
	for _, test := range []struct {
		name, body, contentType    string
		status                     int
		proof, disconnect, partial bool
	}{
		{name: "history", body: policy, contentType: "application/json", status: 400, proof: true},
		{name: "length", body: `{"error":"invalidPasswordMinLengthMessage","error_description":"密码长度不足"}`, contentType: "application/json; charset=UTF-8", status: 400, proof: true},
		{name: "generic-model-error", body: `{"error":"storageFailure","error_description":"failed"}`, contentType: "application/json", status: 400},
		{name: "description-only", body: `{"error":"unknown","error_description":"invalidPasswordHistoryMessage"}`, contentType: "application/json", status: 400},
		{name: "missing-description", body: `{"error":"invalidPasswordHistoryMessage"}`, contentType: "application/json", status: 400},
		{name: "duplicate", body: `{"error":"unknown","error":"invalidPasswordHistoryMessage","error_description":"rejected"}`, contentType: "application/json", status: 400},
		{name: "truncated", body: strings.TrimSuffix(policy, "}"), contentType: "application/json", status: 400},
		{name: "trailing", body: policy + `{}`, contentType: "application/json", status: 400},
		{name: "oversized", body: policy + strings.Repeat(" ", 4096), contentType: "application/json", status: 400},
		{name: "null", body: `null`, contentType: "application/json", status: 400},
		{name: "wrong-type", body: `{"error":"invalidPasswordHistoryMessage","error_description":null}`, contentType: "application/json", status: 400},
		{name: "html", body: policy, contentType: "text/html", status: 400},
		{name: "forbidden", body: policy, contentType: "application/json", status: 403},
		{name: "server-error", body: policy, contentType: "application/json", status: 503},
		{name: "lost-password-ack", disconnect: true},
		{name: "after-password-write", partial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var writes, later atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					io.WriteString(w, `{"access_token":"fixture","expires_in":300}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/reset-password") {
					writes.Add(1)
					if test.disconnect {
						c, _, _ := w.(http.Hijacker).Hijack()
						c.Close()
						return
					}
					if test.partial {
						w.WriteHeader(204)
						return
					}
					w.Header().Set("Content-Type", test.contentType)
					w.WriteHeader(test.status)
					io.WriteString(w, test.body)
					return
				}
				later.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				io.WriteString(w, policy)
			}))
			defer server.Close()
			d, err := NewKeycloakDirectory(server.URL, server.URL+"/token", "fixture", "fixture", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			err = d.ActivateBootstrapAdmin(context.Background(), "subject", "admin", "S3cure-Quartz-71")
			if err == nil || BootstrapActivationUnchanged(err) != test.proof {
				t.Fatalf("proof=%t err=%v", BootstrapActivationUnchanged(err), err)
			}
			passwordAttempts, laterCalls := writes.Load(), later.Load()
			if passwordAttempts != 1 || (!test.partial && laterCalls != 0) {
				t.Fatalf("password attempts=%d later calls=%d", passwordAttempts, laterCalls)
			}
			if test.proof {
				if !errors.Is(err, ErrPasswordInvalid) {
					t.Fatal("policy classification lost")
				}
				if err = d.SetPassword(context.Background(), "subject", "S3cure-Quartz-71"); err == nil || BootstrapActivationUnchanged(err) {
					t.Fatal("ordinary SetPassword acquired bootstrap proof")
				}
			}
		})
	}
}
