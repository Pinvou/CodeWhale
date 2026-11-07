package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

func TestOrganizationLifecycleStartupErrorCategories(t *testing.T) {
	for _, item := range []struct {
		name, profile, billing, token, want string
	}{
		{"profile", "invalid-private-profile", "http://billing.test", "http://token.test", "ACCOUNT_ORGANIZATION_PROFILE_INVALID"},
		{"billing", "models", "invalid-private-billing", "http://token.test", "ACCOUNT_ORGANIZATION_BILLING_AUTHORITY_INVALID"},
		{"token", "models", "http://billing.test", "invalid-private-token", "ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID"},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv("ACCOUNT_PLATFORM_PROFILE", item.profile)
			t.Setenv("BILLING_HTTP_URL", item.billing)
			t.Setenv("TOKEN_HTTP_URL", item.token)
			err := configureOrganizationLifecycle(account.NewService(nil, nil), "controlled-workload")
			if err == nil || organizationLifecycleErrorCode(err) != item.want || organizationLifecycleErrorCode(fmt.Errorf("private-detail: %w", err)) != item.want {
				t.Fatal("startup did not retain its bounded error category")
			}
		})
	}
}

func TestOrganizationLifecycleStartupErrorRedaction(t *testing.T) {
	if code := organizationLifecycleErrorCode(errOrganizationWallet); code != "ACCOUNT_WALLET_AUTHORITY_INVALID" {
		t.Fatal("earlier wallet-authority guard lost its bounded category")
	}
	for _, err := range []error{nil, errors.New("private-detail"), errors.New("ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID"), fmt.Errorf("private-detail: %w", errors.New("unknown-provider-error"))} {
		if code := organizationLifecycleErrorCode(err); code != "ACCOUNT_ORGANIZATION_AUTHORITY_INVALID" || strings.Contains(code, "private-detail") {
			t.Fatal("unknown startup error escaped the bounded fallback")
		}
	}
}
