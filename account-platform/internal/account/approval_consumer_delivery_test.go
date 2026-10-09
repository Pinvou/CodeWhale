package account

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalAuthorizationBindingValidationIsClosed(t *testing.T) {
	valid := ApprovalAuthorizationBinding{
		EventID:                uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Durable:                "billing-wallet-adjustment-executor-p1-v1",
		Workload:               "billing-command-executor",
		ApprovalRequestID:      uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		ApprovedRequestVersion: 3,
		ApprovalEpoch:          1,
		SerializationVersion:   "LEGACY_TYPED_V1",
		Provenance:             "FROZEN_AT_APPROVAL",
		EventDigest:            strings.Repeat("a", 64),
		DigestKind:             "DELIVERY_BYTES",
		CommandType:            "WALLET_ADJUSTMENT",
		PolicyVersion:          1,
		ExecutorIdentity:       "billing-command-executor",
	}
	if !validApprovalAuthorizationBinding(valid) {
		t.Fatal("valid closed binding was rejected")
	}

	tests := map[string]func(*ApprovalAuthorizationBinding){
		"nil event":             func(value *ApprovalAuthorizationBinding) { value.EventID = uuid.Nil },
		"padded durable":        func(value *ApprovalAuthorizationBinding) { value.Durable += " " },
		"unknown workload":      func(value *ApprovalAuthorizationBinding) { value.Workload = "other" },
		"nil request":           func(value *ApprovalAuthorizationBinding) { value.ApprovalRequestID = uuid.Nil },
		"nonpositive version":   func(value *ApprovalAuthorizationBinding) { value.ApprovedRequestVersion = 0 },
		"nonpositive epoch":     func(value *ApprovalAuthorizationBinding) { value.ApprovalEpoch = 0 },
		"uppercase digest":      func(value *ApprovalAuthorizationBinding) { value.EventDigest = strings.Repeat("A", 64) },
		"short digest":          func(value *ApprovalAuthorizationBinding) { value.EventDigest = strings.Repeat("a", 63) },
		"unknown serialization": func(value *ApprovalAuthorizationBinding) { value.SerializationVersion = "UNKNOWN" },
		"unknown provenance":    func(value *ApprovalAuthorizationBinding) { value.Provenance = "UNKNOWN" },
		"wrong digest kind":     func(value *ApprovalAuthorizationBinding) { value.DigestKind = "LEGACY_SEMANTIC" },
		"control command":       func(value *ApprovalAuthorizationBinding) { value.CommandType = "BAD\nCOMMAND" },
		"wrong executor":        func(value *ApprovalAuthorizationBinding) { value.ExecutorIdentity = "other" },
		"nonpositive policy":    func(value *ApprovalAuthorizationBinding) { value.PolicyVersion = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if validApprovalAuthorizationBinding(candidate) {
				t.Fatal("invalid binding was accepted")
			}
		})
	}

	legacy := valid
	legacy.Provenance = "RECONSTRUCTED_AFTER_PUBLISH"
	legacy.DigestKind = "LEGACY_SEMANTIC"
	if !validApprovalAuthorizationBinding(legacy) {
		t.Fatal("legacy semantic binding was rejected")
	}
	legacy.SerializationVersion = "APPROVAL_RFC8785_DECIMAL_V1"
	if validApprovalAuthorizationBinding(legacy) {
		t.Fatal("proof wire was accepted with legacy semantic provenance")
	}
}

func TestApprovalAuthorizationDLQIdentityValidation(t *testing.T) {
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	durable := "billing-wallet-adjustment-executor-p1-v1"
	messageID := eventID.String() + ":" + durable + ":dead-letter"
	if !boundedApprovalConsumerString(messageID, 240) {
		t.Fatal("stable DLQ message identity was rejected")
	}
	for _, candidate := range []string{"", messageID + " ", messageID + "\n", strings.Repeat("a", 241)} {
		if boundedApprovalConsumerString(candidate, 240) {
			t.Fatalf("invalid DLQ identity %q was accepted", candidate)
		}
	}
}
