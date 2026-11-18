package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nkeys"
)

func TestF04OperatorInvalidProfileRejectsBeforeCredentials(t *testing.T) {
	for _, action := range []string{"status", "initialize-consumers", "activate", "resume"} {
		t.Run(action, func(t *testing.T) {
			_, err := run([]string{"-expected-version", "1", "-action", action}, func(key string) string {
				if key != "ACCOUNT_PLATFORM_PROFILE" {
					t.Fatalf("invalid profile reached runtime configuration %s", key)
				}
				return "model-typo"
			})
			if err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
}

func TestF04PersonalTokenResourcesRejectBeforeCredentials(t *testing.T) {
	for _, profile := range []string{"personal", "invalid"} {
		for _, action := range []string{"initialize", "check"} {
			t.Run(profile+"/"+action, func(t *testing.T) {
				_, err := runTokenResources([]string{"token-consumers", action}, func(key string) string {
					if key != "ACCOUNT_PLATFORM_PROFILE" {
						t.Fatalf("unavailable model operation reached runtime configuration %s", key)
					}
					return profile
				})
				if err == nil || err.Error() != "TOKEN_RESOURCES_REQUIRE_MODELS_PROFILE" {
					t.Fatal("unavailable Token resource operation accepted")
				}
			})
		}
	}
}

func TestF04OfflineRendererPreservesArtifactSuperset(t *testing.T) {
	for _, profile := range []string{"personal", "models", ""} {
		t.Run("profile="+profile, func(t *testing.T) {
			keys := map[string]string{}
			for _, role := range []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
				pair, err := nkeys.CreateUser()
				if err != nil {
					t.Fatal(err)
				}
				key, err := pair.PublicKey()
				pair.Wipe()
				if err != nil {
					t.Fatal(err)
				}
				keys[role] = key
			}
			identityDirectory, resourcesDirectory := privateResourceTestDir(t), privateResourceTestDir(t)
			raw, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys, "previous_v1_sha256": hex.EncodeToString(make([]byte, 32)), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
			if err != nil {
				t.Fatal(err)
			}
			identityFile := filepath.Join(identityDirectory, "current.json")
			if err := os.WriteFile(identityFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			values := map[string]string{"ACCOUNT_PLATFORM_PROFILE": profile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE": identityFile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256": hex.EncodeToString(digest[:]), "APPROVAL_RESOURCE_MANIFEST_FILE": filepath.Join(resourcesDirectory, "manifest.json"), "APPROVAL_RESOURCE_CONFIGURATION_FILE": filepath.Join(resourcesDirectory, "authorization.conf")}
			if _, err := renderPublicResources(func(key string) string { return values[key] }); err != nil {
				t.Fatal(err)
			}
			manifest, err := os.ReadFile(values["APPROVAL_RESOURCE_MANIFEST_FILE"])
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Consumers []struct {
					Durable string `json:"durable"`
				} `json:"consumers"`
			}
			if json.Unmarshal(manifest, &parsed) != nil || len(parsed.Consumers) != 14 {
				t.Fatal("fixed offline expectation changed with deployment profile")
			}
			expectedDurables := []string{
				"billing-usage-settlement-repair-executor-p1-v1",
				"billing-reconciliation-financial-repair-executor-p1-v1",
				"billing-chargeback-financial-repair-executor-p1-v1",
				"billing-refund-exception-repair-executor-p1-v1",
				"billing-payment-financial-repair-executor-p1-v1",
				"billing-entitlement-component-adjustment-executor-p2-v1",
				"billing-ledger-repair-executor-p1-v1",
				"billing-wallet-adjustment-executor-p1-v1",
				"billing-payment-provider-resume-executor-p2-v1",
				"billing-payment-product-resume-executor-p1-v1",
				"billing-receivable-write-off-executor-p1-v1",
				"token-model-lifecycle-executor-v1",
				"token-catalog-rollback-executor-v1",
				"token-credential-revoke-executor-v1",
			}
			for i, durable := range expectedDurables {
				if parsed.Consumers[i].Durable != durable {
					t.Fatal("fixed phase consumer identity changed")
				}
			}
			for i, descriptor := range approvalconsumer.All() {
				if parsed.Consumers[i].Durable != descriptor.Durable {
					t.Fatal("offline consumer identity changed")
				}
			}
			configuration, err := os.ReadFile(values["APPROVAL_RESOURCE_CONFIGURATION_FILE"])
			if err != nil {
				t.Fatal(err)
			}
			var rendered struct {
				Authorization struct {
					Users []struct {
						NKey string `json:"nkey"`
					} `json:"users"`
				} `json:"authorization"`
			}
			if json.Unmarshal(configuration, &rendered) != nil {
				t.Fatal("public authorization configuration invalid")
			}
			found := 0
			for _, user := range rendered.Authorization.Users {
				if user.NKey == keys[approvalconsumer.TokenWorkload] {
					found++
				}
			}
			if found != 1 {
				t.Fatal("fixed opt-in Token public identity changed")
			}
		})
	}
}

func TestF04InvalidRendererProfileRejectsBeforeArtifacts(t *testing.T) {
	_, err := renderPublicResources(func(key string) string {
		if key != "ACCOUNT_PLATFORM_PROFILE" {
			t.Fatalf("invalid profile read public artifacts %s", key)
		}
		return "invalid"
	})
	if err == nil {
		t.Fatal("invalid renderer profile accepted")
	}
}

func TestTokenEventRendererRejectsUnavailableProfileBeforeArtifacts(t *testing.T) {
	for _, profile := range []string{"personal", "invalid"} {
		_, err := renderResourcesSelection(func(key string) string {
			if key != "ACCOUNT_PLATFORM_PROFILE" {
				t.Fatal("unavailable Token publisher read artifacts", key)
			}
			return profile
		}, false, false, true)
		if err == nil {
			t.Fatal("unavailable Token publisher accepted")
		}
	}
}

func TestTokenEventOfflineRendererUsesDedicatedIdentity(t *testing.T) {
	keys := map[string]string{}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		keys[role], err = pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	identityDirectory := privateResourceTestDir(t)
	raw, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys, "previous_v1_sha256": hex.EncodeToString(make([]byte, 32)), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(identityDirectory, "current.json")
	if err := os.WriteFile(identityFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	for _, kind := range []string{"fresh", "existing-broker", "refund-upgrade"} {
		t.Run(kind, func(t *testing.T) {
			resources := privateResourceTestDir(t)
			values := map[string]string{"ACCOUNT_PLATFORM_PROFILE": "models", "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE": identityFile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256": hex.EncodeToString(digest[:]), "APPROVAL_RESOURCE_MANIFEST_FILE": filepath.Join(resources, "manifest.json"), "APPROVAL_RESOURCE_CONFIGURATION_FILE": filepath.Join(resources, "authorization.conf")}
			result, err := renderResourcesSelection(func(key string) string { return values[key] }, kind == "existing-broker", kind == "refund-upgrade", true)
			if err != nil {
				t.Fatal(err)
			}
			configuration, err := os.ReadFile(values["APPROVAL_RESOURCE_CONFIGURATION_FILE"])
			if err != nil {
				t.Fatal(err)
			}
			actualHash := sha256.Sum256(configuration)
			if result["configuration_sha256"] != hex.EncodeToString(actualHash[:]) {
				t.Fatal("written configuration is not the returned digest")
			}
			var parsed struct {
				Authorization struct {
					Users []struct{ NKey string } `json:"users"`
				} `json:"authorization"`
			}
			if json.Unmarshal(configuration, &parsed) != nil {
				t.Fatal("invalid public configuration")
			}
			publishers := 0
			for _, user := range parsed.Authorization.Users {
				if user.NKey == keys["token-outbox-relay"] {
					publishers++
				}
			}
			if publishers != 1 {
				t.Fatal("dedicated Token publisher is not mounted exactly once")
			}
		})
	}
}
