package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/nats-io/nkeys"
)

// This is controlled public receipt evidence, not a captured broker watermark
// or an activation claim. Field order follows the canonical receipt protocol.
type billingArtifactReceiptFixture struct {
	Schema                  string `json:"schema"`
	SchemaVersion           int    `json:"schema_version"`
	IdentitySHA256          string `json:"identity_sha256"`
	ManifestSHA256          string `json:"manifest_sha256"`
	Stream                  string `json:"stream"`
	Control                 string `json:"control"`
	CapturedMode            string `json:"captured_mode"`
	CapturedRelayState      string `json:"captured_relay_state"`
	ControlVersion          string `json:"control_version"`
	ApprovalEpoch           string `json:"approval_epoch"`
	AuthorizationRelayEpoch string `json:"authorization_relay_epoch"`
	CapturedAt              string `json:"captured_at"`
	HighWatermark           string `json:"high_watermark"`
	BillingStarts           []struct {
		Durable       string `json:"durable"`
		StartSequence string `json:"start_sequence"`
	} `json:"billing_starts"`
}

func billingArtifactDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func billingArtifactBootstrapFixture(t *testing.T, now time.Time) billingArtifactReceiptFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"approval-identities", "approval-manifest", "approval-receipt", "private"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
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
	identity, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys,
		"previous_v1_sha256": strings.Repeat("a", 64), "overlap_expires_at": now.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	identityHash := billingArtifactDigest(identity)
	manifest, err := events.ApprovalConsumerManifest(identityHash)
	if err != nil {
		t.Fatal(err)
	}
	receipt := billingArtifactReceiptFixture{Schema: "approval-consumer-cutover-receipt-v1", SchemaVersion: 1,
		IdentitySHA256: identityHash, ManifestSHA256: billingArtifactDigest(manifest), Stream: "ACCOUNT_PLATFORM", Control: "ADMIN_APPROVAL",
		CapturedMode: "QUIESCING", CapturedRelayState: "FENCED", ControlVersion: "3", ApprovalEpoch: "1", AuthorizationRelayEpoch: "2",
		CapturedAt: now.Format(time.RFC3339Nano), HighWatermark: "41"}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		receipt.BillingStarts = append(receipt.BillingStarts, struct {
			Durable       string `json:"durable"`
			StartSequence string `json:"start_sequence"`
		}{descriptor.Durable, "42"})
	}
	rawReceipt, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []struct {
		prefix, directory, filename string
		body                        []byte
	}{
		{"PUBLIC_KEYS", "approval-identities", "current.json", identity},
		{"MANIFEST", "approval-manifest", "manifest.json", manifest},
		{"RECEIPT", "approval-receipt", "receipt.json", rawReceipt},
	} {
		file := filepath.Join(root, artifact.directory, artifact.filename)
		if err := os.WriteFile(file, artifact.body, 0444); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BILLING_APPROVAL_CONSUMER_"+artifact.prefix+"_FILE", file)
		// Only the controlled test authority supplies these values; the runtime
		// bootstrap under test never derives its own trusted digest from a file.
		t.Setenv("BILLING_APPROVAL_CONSUMER_"+artifact.prefix+"_SHA256", billingArtifactDigest(artifact.body))
	}
	t.Setenv("NATS_NKEY_SEED_FILE", "")
	t.Setenv("NATS_CREDENTIALS_FILE", filepath.Join(root, "private", "billing-command-executor.creds"))
	return receipt
}

func TestBillingApprovalBootstrapLoadsCompleteRegistry(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	receipt := billingArtifactBootstrapFixture(t, now)
	if len(receipt.BillingStarts) != len(approvalconsumer.BillingCommands()) {
		t.Fatal("fixture did not carry the installed Billing descriptor set")
	}
	if binding, err := billingApprovalConsumerResources(now); err != nil || binding == nil {
		t.Fatal("real bootstrap rejected independently bound controlled evidence")
	}
	if binding, err := billingApprovalConsumerResources(now.Add(time.Minute)); err != nil || binding == nil {
		t.Fatal("subsequent bootstrap reload failed")
	}
}

func TestBillingApprovalBootstrapRejectsMissingInputs(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	for _, name := range billingApprovalConsumerArtifactEnvironment {
		t.Run(name, func(t *testing.T) {
			billingArtifactBootstrapFixture(t, now)
			t.Setenv(name, "")
			if _, err := billingApprovalConsumerResources(now); err == nil {
				t.Fatal("missing mandatory artifact input accepted")
			}
		})
	}
}

func TestBillingApprovalBootstrapRejectsRegistryAndArtifactDrift(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	for _, name := range []string{"missing-last-descriptor", "wrong-last-start", "wrong-last-durable", "wrong-identity-hash", "manifest-bytes", "receipt-bytes", "identity-directory-extra-file", "private-path-alias"} {
		t.Run(name, func(t *testing.T) {
			receipt := billingArtifactBootstrapFixture(t, now)
			file := os.Getenv("BILLING_APPROVAL_CONSUMER_RECEIPT_FILE")
			switch name {
			case "missing-last-descriptor":
				receipt.BillingStarts = receipt.BillingStarts[:len(receipt.BillingStarts)-1]
			case "wrong-last-start":
				q, _ := strconv.ParseUint(receipt.HighWatermark, 10, 64)
				receipt.BillingStarts[len(receipt.BillingStarts)-1].StartSequence = strconv.FormatUint(q+2, 10)
			case "wrong-last-durable":
				receipt.BillingStarts[len(receipt.BillingStarts)-1].Durable = approvalconsumer.TokenCommands()[0].Durable
			case "wrong-identity-hash":
				t.Setenv("BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_SHA256", strings.Repeat("0", 64))
			case "manifest-bytes", "receipt-bytes":
				if name == "manifest-bytes" {
					file = os.Getenv("BILLING_APPROVAL_CONSUMER_MANIFEST_FILE")
				}
				if err := os.Chmod(file, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("{}"), 0444); err != nil {
					t.Fatal(err)
				}
			case "identity-directory-extra-file":
				file = filepath.Join(filepath.Dir(os.Getenv("BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE")), "manifest.json")
				if err := os.WriteFile(file, []byte("{}"), 0444); err != nil {
					t.Fatal(err)
				}
			case "private-path-alias":
				t.Setenv("NATS_CREDENTIALS_FILE", filepath.Join(filepath.Dir(os.Getenv("BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE")), "private.creds"))
			}
			if strings.HasPrefix(name, "missing-last") || strings.HasPrefix(name, "wrong-last") {
				raw, err := json.Marshal(receipt)
				if err != nil || os.Chmod(file, 0600) != nil || os.WriteFile(file, raw, 0444) != nil {
					t.Fatal("controlled drift fixture failed")
				}
				// A test authority rebinds the changed receipt deliberately: the
				// real consumer must still reject registry/Q+1 semantic drift.
				t.Setenv("BILLING_APPROVAL_CONSUMER_RECEIPT_SHA256", billingArtifactDigest(raw))
			}
			if _, err := billingApprovalConsumerResources(now); err == nil {
				t.Fatal("untrusted or semantically drifting bootstrap evidence accepted")
			}
		})
	}
}

func TestBillingApprovalBootstrapRejectsProjectedIdentityDirectory(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	billingArtifactBootstrapFixture(t, now)
	file := os.Getenv("BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE")
	alias := filepath.Join(t.TempDir(), "projected")
	if err := os.Symlink(filepath.Dir(file), alias); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("Windows directory symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	t.Setenv("BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE", filepath.Join(alias, filepath.Base(file)))
	if _, err := billingApprovalConsumerResources(now); err == nil {
		t.Fatal("whole projected identity directory accepted")
	}
}
