package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/nats-io/nkeys"
)

func TestInvalidOperatorActionDoesNotReadCredentials(t *testing.T) {
	for _, args := range [][]string{{}, {"-expected-version", "0"}, {"-expected-version", "1", "-action", "drain"}, {"-expected-version", "1", "unexpected"}, {"-expected-version", "1", "-unknown", "secret"}} {
		if _, err := run(args, func(string) string { t.Fatal("invalid command read runtime credentials"); return "" }); err == nil {
			t.Fatal("invalid operation accepted")
		}
	}
}

func TestPublicReceiptIsBoundedCreateOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	if writePublicReceipt(path, []byte(`{"current":true}`)) != nil {
		t.Fatal("new receipt failed")
	}
	if writePublicReceipt(path, []byte(`{"replacement":true}`)) == nil {
		t.Fatal("existing receipt overwritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != `{"current":true}` {
		t.Fatal("prior evidence changed")
	}
	for _, raw := range [][]byte{nil, []byte(`{} {}`), make([]byte, 256<<10+1)} {
		if writePublicReceipt(filepath.Join(t.TempDir(), "rejected.json"), raw) == nil {
			t.Fatal("unbounded or incomplete receipt accepted")
		}
	}
}

func TestOfflineResourceRendererAndIsolation(t *testing.T) {
	identityDir, resourceDir, privateDir := t.TempDir(), t.TempDir(), t.TempDir()
	keys := map[string]string{}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal("controlled key generation failed")
		}
		keys[role], err = pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal("controlled public key failed")
		}
	}
	raw, _ := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys, "previous_v1_sha256": hex.EncodeToString(make([]byte, 32)), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	current := filepath.Join(identityDir, "current.json")
	if os.WriteFile(current, raw, 0600) != nil {
		t.Fatal("controlled identity write failed")
	}
	digest := sha256.Sum256(raw)
	values := map[string]string{"APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE": current, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256": hex.EncodeToString(digest[:]), "APPROVAL_RESOURCE_MANIFEST_FILE": filepath.Join(resourceDir, "manifest.json"), "APPROVAL_RESOURCE_CONFIGURATION_FILE": filepath.Join(resourceDir, "authorization.conf"), "NATS_NKEY_SEED_FILE": filepath.Join(privateDir, "operator.seed")}
	environment := func(k string) string { return values[k] }
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: current, CurrentSHA256: values["APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"], PrivateIdentityFiles: []string{values["NATS_NKEY_SEED_FILE"]}}
	for _, bad := range []string{filepath.Join(identityDir, "receipt.json"), filepath.Join(privateDir, "receipt.json"), "relative.json"} {
		if validateNewResourcePath(bad, identity) == nil {
			t.Fatal("resource entered identity/private custody")
		}
	}
	result, err := renderPublicResources(environment)
	if err != nil || identity.Validate(time.Now().UTC()) != nil {
		t.Fatal("offline renderer corrupted identity boundary")
	}
	for key, path := range map[string]string{"manifest_sha256": values["APPROVAL_RESOURCE_MANIFEST_FILE"], "configuration_sha256": values["APPROVAL_RESOURCE_CONFIGURATION_FILE"]} {
		bytes, err := os.ReadFile(path)
		sum := sha256.Sum256(bytes)
		if err != nil || result[key] != hex.EncodeToString(sum[:]) {
			t.Fatal("renderer output digest mismatch")
		}
	}
	if _, err = renderPublicResources(environment); err == nil {
		t.Fatal("renderer overwrote existing public resources")
	}
}
