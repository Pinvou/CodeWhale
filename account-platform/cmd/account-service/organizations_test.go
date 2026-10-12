package main

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"testing"
)

func TestOrganizationLifecycleUsesProductionTokenEndpoint(t *testing.T) {
	t.Setenv("BILLING_HTTP_URL", "http://billing-service:8080")
	t.Setenv("TOKEN_HTTP_URL", "http://token-gateway:8080")
	t.Setenv("TOKEN_GATEWAY_HTTP_URL", "")
	if err := configureOrganizationLifecycle(new(account.Service), "controlled-workload"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_HTTP_URL", "")
	t.Setenv("TOKEN_GATEWAY_HTTP_URL", "http://obsolete-alias:8080")
	if err := configureOrganizationLifecycle(new(account.Service), "controlled-workload"); err == nil || err.Error() != "ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID" {
		t.Fatalf("canonical endpoint omission: %v", err)
	}
}
