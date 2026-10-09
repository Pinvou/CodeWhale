package main

import (
	"errors"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

// Both financial and invocation authorities must confirm the projection.
// No organization route may treat an omitted domain as an activation receipt.
func configureOrganizationLifecycle(service *account.Service, credential string) error {
	billing, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("BILLING_HTTP_URL"), credential, "BILLING")
	if err != nil {
		return errors.New("ACCOUNT_ORGANIZATION_BILLING_AUTHORITY_INVALID")
	}
	token, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("TOKEN_HTTP_URL"), credential, "TOKEN")
	if err != nil {
		return errors.New("ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID")
	}
	service.WithOrganizationLifecycle(billing, token).
		WithOrganizationUnitLifecycle(billing, token).
		WithOrganizationMemberLifecycle(billing)
	return nil
}
