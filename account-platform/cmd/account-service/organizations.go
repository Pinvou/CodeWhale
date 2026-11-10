package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

var (
	errOrganizationProfile = errors.New("ACCOUNT_ORGANIZATION_PROFILE_INVALID")
	errOrganizationBilling = errors.New("ACCOUNT_ORGANIZATION_BILLING_AUTHORITY_INVALID")
	errOrganizationToken   = errors.New("ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID")
	errOrganizationWallet  = errors.New("ACCOUNT_WALLET_AUTHORITY_INVALID")
)

// Emit only the finite configuration categories owned by this startup step.
// Unknown errors must not expose provider URLs, credentials or wrapped details.
func organizationLifecycleErrorCode(err error) string {
	for _, category := range []error{errOrganizationProfile, errOrganizationBilling, errOrganizationToken, errOrganizationWallet} {
		if errors.Is(err, category) {
			return category.Error()
		}
	}
	return "ACCOUNT_ORGANIZATION_AUTHORITY_INVALID"
}

func logOrganizationStartupFailure(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "application event", "component", "organization-startup", "error_code", organizationLifecycleErrorCode(err))
}

// Both financial and invocation authorities must confirm the projection.
// No organization route may treat an omitted domain as an activation receipt.
func configureOrganizationLifecycle(service *account.Service, credential string) error {
	profile, err := config.ParseAccountPlatformProfile(os.Getenv("ACCOUNT_PLATFORM_PROFILE"))
	if err != nil {
		return errOrganizationProfile
	}
	if profile == config.AccountPlatformProfilePersonal {
		service.WithOrganizationsEnabled(false)
		return nil
	}

	billing, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("BILLING_HTTP_URL"), credential, "BILLING")
	if err != nil {
		return errOrganizationBilling
	}
	token, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("TOKEN_HTTP_URL"), credential, "TOKEN")
	if err != nil {
		return errOrganizationToken
	}
	service.WithOrganizationsEnabled(true).WithOrganizationLifecycle(billing, token).
		WithOrganizationUnitLifecycle(billing, token).
		WithOrganizationMemberLifecycle(billing)
	return nil
}
