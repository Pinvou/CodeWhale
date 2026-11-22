package main

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"time"
)

// This explicit resource operation has no Account control mutation and does
// not overwrite the independently trusted Billing Q+1 cutover receipt.
func runTokenResources(args []string, environment func(string) string) ([]byte, error) {
	if len(args) != 2 || args[0] != "token-consumers" || (args[1] != "initialize" && args[1] != "check") {
		return nil, errors.New("TOKEN_RESOURCE_ARGUMENTS_INVALID")
	}
	profile, err := config.ParseAccountPlatformProfile(environment("ACCOUNT_PLATFORM_PROFILE"))
	if err != nil || profile != config.AccountPlatformProfileModels {
		return nil, errors.New("TOKEN_RESOURCES_REQUIRE_MODELS_PROFILE")
	}
	cfg := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", RequireExplicitAuthentication: true, CAFile: environment("NATS_CA_FILE"), SeedFile: environment("NATS_NKEY_SEED_FILE"), CredentialsFile: environment("NATS_CREDENTIALS_FILE")}
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"), PreviousFile: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_SHA256"), PrivateIdentityFiles: []string{cfg.SeedFile, cfg.CredentialsFile}}
	output := environment("TOKEN_CONSUMER_RESOURCE_RECEIPT_FILE")
	if validateNewResourcePath(output, identity) != nil {
		return nil, errors.New("TOKEN_RESOURCE_RECEIPT_PATH_INVALID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	raw, err := events.ProvisionTokenApprovalConsumers(ctx, environment("NATS_URL"), cfg, identity, environment("APPROVAL_RESOURCE_MANIFEST_FILE"), environment("APPROVAL_RESOURCE_MANIFEST_SHA256"), args[1] == "initialize")
	if err != nil {
		return nil, err
	}
	if writePublicReceipt(output, raw) != nil {
		return nil, errors.New("TOKEN_RESOURCE_RECEIPT_WRITE_FAILED")
	}
	// The caller emits only the public receipt, never environment or credentials.
	return raw, nil
}
