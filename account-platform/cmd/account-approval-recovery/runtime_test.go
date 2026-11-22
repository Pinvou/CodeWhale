package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type cancelledRecoveryPass struct{ entered chan struct{} }

func (r cancelledRecoveryPass) RunOnce(ctx context.Context, _ int) (account.ApprovalRecoveryPass, error) {
	close(r.entered)
	<-ctx.Done()
	return account.ApprovalRecoveryPass{}, ctx.Err()
}

func TestRecoveryShutdownCancelsActivePassAndDrainsBeforeReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, done := make(chan struct{}), make(chan struct{})
	var ready atomic.Bool
	go runRecoveryPasses(ctx, cancelledRecoveryPass{entered}, time.Minute, 200, &ready, done)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pass did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active pass did not drain on cancellation")
	}
	if ready.Load() {
		t.Fatal("cancelled pass reported readiness")
	}
}

func TestRecoveryConfigurationRequiresProjectedIdentityAndBoundedExactOrigins(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("fixture.header.signature"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKLOAD_IDENTITY_TOKEN_FILE", file)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "legacy-must-not-be-used")
	t.Setenv("APPROVAL_RECOVERY_DATABASE_URL", "postgres://fixture@localhost/fixture")
	t.Setenv("ACCOUNT_HTTP_URL", "http://account-service:8080")
	t.Setenv("BILLING_HTTP_URL", "https://billing.example")
	t.Setenv("APPROVAL_RECOVERY_BATCH_LIMIT", "")
	t.Setenv("APPROVAL_RECOVERY_INTERVAL", "")
	if _, err := loadRecoveryConfiguration(); err != nil {
		t.Fatal("valid internal origins rejected")
	}
	for _, test := range []struct{ key, value string }{
		{"WORKLOAD_IDENTITY_TOKEN_FILE", ""},
		{"WORKLOAD_IDENTITY_TOKEN_FILE", "relative/token"},
		{"APPROVAL_RECOVERY_DATABASE_URL", ""},
		{"ACCOUNT_HTTP_URL", "https://account.example?"},
		{"ACCOUNT_HTTP_URL", "https://:443"},
		{"ACCOUNT_HTTP_URL", "https://user:password@account.example"},
		{"ACCOUNT_HTTP_URL", "https://account.example/%2f"},
		{"ACCOUNT_HTTP_URL", "https://account.example/internal"},
		{"ACCOUNT_HTTP_URL", "ftp://account.example"},
		{"ACCOUNT_HTTP_URL", "http://account.example"},
		{"ACCOUNT_HTTP_URL", "http://billing-service:8080"},
		{"APPROVAL_RECOVERY_INTERVAL", "1s"},
		{"APPROVAL_RECOVERY_INTERVAL", "2h"},
		{"APPROVAL_RECOVERY_BATCH_LIMIT", "0"},
		{"APPROVAL_RECOVERY_BATCH_LIMIT", "201"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := loadRecoveryConfiguration(); err == nil {
				t.Fatal("unsafe or missing configuration accepted")
			}
		})
	}
	if err := os.WriteFile(file, []byte("legacy-static-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRecoveryConfiguration(); err == nil {
		t.Fatal("static token accepted")
	}
}

func TestRecoveryOutboundTransportDoesNotUseAmbientProxyOrRedirects(t *testing.T) {
	client, err := serviceauth.NewWorkloadHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify || client.Timeout <= 0 {
		t.Fatal("isolated verified bounded transport missing")
	}
	if client.CheckRedirect == nil || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect credential forwarding permitted")
	}
}
