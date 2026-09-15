package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/nats-io/nkeys"
)

func TestConsumerRenderIsExplicitOfflineAndHashBound(t *testing.T) {
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("fixture permissions")
	}
	previous, previousHash := publicFixture(t, dir, "v1.json")
	raw, _ := os.ReadFile(previous)
	var old struct {
		PublicKeys map[string]string `json:"public_keys"`
	}
	if json.Unmarshal(raw, &old) != nil {
		t.Fatal("fixture identity")
	}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay"} {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		old.PublicKeys[role], err = pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": old.PublicKeys,
		"previous_v1_sha256": previousHash, "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, "v2.json")
	if os.WriteFile(current, raw, 0600) != nil {
		t.Fatal("fixture write")
	}
	digest := sha256.Sum256(raw)
	identityHash := hex.EncodeToString(digest[:])
	manifest := filepath.Join(dir, "consumers.json")
	environment := func(string) string { t.Fatal("offline environment accessed"); return "" }
	manage := func(context.Context, string, events.NATSConfig, bool) error {
		t.Fatal("offline manager invoked")
		return errArguments
	}
	generate := []string{"-render-consumer-manifest", "-public-keys", current, "-public-keys-sha256", identityHash, "-output", manifest}
	if result, err := run(generate, environment, manage); err != nil || result != "NATS_CONSUMER_EXPECTATION_RENDERED_NOT_ACTIVATED" {
		t.Fatalf("manifest: %s %v", result, err)
	}
	rawManifest, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(rawManifest)
	manifestHash := hex.EncodeToString(manifestDigest[:])
	output := filepath.Join(dir, "authorization.json")
	render := []string{"-render-authorization", "-public-keys", current, "-public-keys-sha256", identityHash, "-consumer-manifest", manifest, "-consumer-manifest-sha256", manifestHash, "-output", output}
	result, err := run(render, environment, manage)
	if err != nil || !strings.Contains(result, "identity_sha256="+identityHash) || !strings.Contains(result, "manifest_sha256="+manifestHash) {
		t.Fatalf("render: %s %v", result, err)
	}
	policy, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	policyHash := sha256.Sum256(policy)
	if !strings.Contains(result, "configuration_sha256="+hex.EncodeToString(policyHash[:])) {
		t.Fatal("output hash not bound")
	}
	if _, err := run(render, environment, manage); err == nil {
		t.Fatal("output overwritten")
	}
	for _, args := range [][]string{
		{"-consumer-manifest", manifest},
		{"-consumer-manifest-sha256", manifestHash},
		append(append([]string{}, generate...), "-render-authorization"),
		append(append([]string{}, generate...), "-create-only"),
		{"-render-authorization", "-public-keys", current, "-public-keys-sha256", identityHash, "-consumer-manifest", manifest, "-output", filepath.Join(dir, "bad.json")},
		{"-render-authorization", "-public-keys", current, "-public-keys-sha256", identityHash, "-consumer-manifest", manifest, "-consumer-manifest-sha256", strings.Repeat("0", 64), "-output", filepath.Join(dir, "bad.json")},
	} {
		if _, err := run(args, environment, manage); err == nil {
			t.Fatal("unsafe arguments accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Fatal("failed preflight created file")
	}
}
