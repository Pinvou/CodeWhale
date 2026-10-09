package main

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"os"
)

func configuredAccountClosureClients(credential string) (*account.HTTPAccountClosureClient, *account.HTTPAccountClosureClient, error) {
	billing, err := account.NewHTTPAccountClosureClient(os.Getenv("BILLING_HTTP_URL"), credential, "BILLING")
	if err != nil {
		return nil, nil, err
	}
	token, err := account.NewHTTPAccountClosureClient(os.Getenv("TOKEN_HTTP_URL"), credential, "TOKEN")
	if err != nil {
		return nil, nil, err
	}
	return billing, token, nil
}
