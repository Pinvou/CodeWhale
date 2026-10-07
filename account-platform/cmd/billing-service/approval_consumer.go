package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

// billingApprovalConsumerArtifactEnvironment names the six independent inputs
// of the dedicated Billing authorization consumer bootstrap. One verified
// receipt set covers the three installed financial command durables. Only
// the three digests are site-supplied: every artifact path is fixed by the
// deployment template and the public-artifact directories are read-only mounts.
var billingApprovalConsumerArtifactEnvironment = []string{
	"BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE",
	"BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_SHA256",
	"BILLING_APPROVAL_CONSUMER_MANIFEST_FILE",
	"BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256",
	"BILLING_APPROVAL_CONSUMER_RECEIPT_FILE",
	"BILLING_APPROVAL_CONSUMER_RECEIPT_SHA256",
}

// billingApprovalConsumerResources loads the closed, verified Q+1 resource
// expectation for every dedicated Billing durable. Every input is required: a
// half-configured deployment must not start a Billing authorization consumer
// from an unverified resource expectation, and the returned binding can only be
// produced by the package's own manifest/receipt reader.
func billingApprovalConsumerResources(now time.Time) (*events.ApprovalConsumerResourceBinding, error) {
	values := make(map[string]string, len(billingApprovalConsumerArtifactEnvironment))
	for _, name := range billingApprovalConsumerArtifactEnvironment {
		values[name] = strings.TrimSpace(os.Getenv(name))
		if values[name] == "" {
			return nil, errors.New("billing approval consumer artifacts are incomplete")
		}
	}
	files := events.ApprovalConsumerResourceFiles{
		Identity: events.NATSAuthorizationArtifactFiles{
			CurrentFile:   values["BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE"],
			CurrentSHA256: values["BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_SHA256"],
			PrivateIdentityFiles: []string{
				strings.TrimSpace(os.Getenv("NATS_NKEY_SEED_FILE")),
				strings.TrimSpace(os.Getenv("NATS_CREDENTIALS_FILE")),
			},
		},
		ManifestFile:   values["BILLING_APPROVAL_CONSUMER_MANIFEST_FILE"],
		ManifestSHA256: values["BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256"],
		ReceiptFile:    values["BILLING_APPROVAL_CONSUMER_RECEIPT_FILE"],
		ReceiptSHA256:  values["BILLING_APPROVAL_CONSUMER_RECEIPT_SHA256"],
	}
	binding, err := files.Load(now)
	if err != nil {
		return nil, err
	}
	return binding, nil
}
