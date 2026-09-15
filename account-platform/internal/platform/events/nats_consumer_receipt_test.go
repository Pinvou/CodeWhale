package events

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

func consumerReceiptFixture(t *testing.T, now time.Time, q uint64) (ApprovalConsumerResourceFiles, approvalConsumerCutoverReceipt) {
	t.Helper()
	root := t.TempDir()
	publicDir, resourceDir := filepath.Join(root, "identity"), filepath.Join(root, "resources")
	for _, dir := range []string{publicDir, resourceDir} {
		if os.Mkdir(dir, 0700) != nil {
			t.Fatal("fixture directory")
		}
	}
	current, _, _, _ := natsV2Fixture(t, now, time.Hour)
	path, digest := writeNATSPublicArtifactFixture(t, publicDir, "current.json", current)
	manifest, err := ApprovalConsumerManifest(digest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, manifestHash := writeNATSPublicArtifactFixture(t, resourceDir, "manifest.json", manifest)
	receipt := approvalConsumerCutoverReceipt{Schema: "approval-consumer-cutover-receipt-v1", SchemaVersion: 1,
		IdentitySHA256: digest, ManifestSHA256: manifestHash, Stream: platformStream, Control: "ADMIN_APPROVAL",
		CapturedMode: "QUIESCING", CapturedRelayState: "FENCED", ControlVersion: "3", ApprovalEpoch: "1", AuthorizationRelayEpoch: "2",
		CapturedAt: now.Format(time.RFC3339Nano), HighWatermark: strconv.FormatUint(q, 10)}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		receipt.BillingStarts = append(receipt.BillingStarts, approvalConsumerReceiptStart{Durable: descriptor.Durable, StartSequence: strconv.FormatUint(q+1, 10)})
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath, receiptHash := writeNATSPublicArtifactFixture(t, resourceDir, "receipt.json", raw)
	return ApprovalConsumerResourceFiles{Identity: NATSAuthorizationArtifactFiles{CurrentFile: path, CurrentSHA256: digest},
		ManifestFile: manifestPath, ManifestSHA256: manifestHash, ReceiptFile: receiptPath, ReceiptSHA256: receiptHash}, receipt
}

func replaceConsumerReceiptFixture(t *testing.T, files *ApprovalConsumerResourceFiles, receipt approvalConsumerCutoverReceipt) {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil || os.WriteFile(files.ReceiptFile, raw, 0600) != nil {
		t.Fatal("fixture receipt")
	}
	files.ReceiptSHA256 = consumerManifestHash(raw)
}

func TestApprovalConsumerReceiptExactIntegersAndImmutableBinding(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 123, time.UTC)
	for _, q := range []uint64{0, 1, 9007199254740993, math.MaxUint64 - 1} {
		t.Run(strconv.FormatUint(q, 10), func(t *testing.T) {
			files, _ := consumerReceiptFixture(t, now, q)
			binding, err := files.Load(now)
			if err != nil {
				t.Fatal(err)
			}
			for _, descriptor := range approvalconsumer.BillingCommands() {
				if binding.startSequences[descriptor.Durable] != q+1 {
					t.Fatal("Q+1 precision lost")
				}
			}
			binding.startSequences[approvalconsumer.BillingCommands()[0].Durable] = 0
			again, err := files.Load(now)
			if err != nil || again.startSequences[approvalconsumer.BillingCommands()[0].Durable] != q+1 {
				t.Fatal("loaded binding shared caller state")
			}
		})
	}
}

func TestApprovalConsumerReceiptRejectsClosedFieldDrift(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	mutations := map[string]func(*approvalConsumerCutoverReceipt){
		"schema":         func(r *approvalConsumerCutoverReceipt) { r.Schema = "other" },
		"version":        func(r *approvalConsumerCutoverReceipt) { r.SchemaVersion = 2 },
		"identity":       func(r *approvalConsumerCutoverReceipt) { r.IdentitySHA256 = strings.Repeat("0", 64) },
		"manifest":       func(r *approvalConsumerCutoverReceipt) { r.ManifestSHA256 = strings.Repeat("0", 64) },
		"stream":         func(r *approvalConsumerCutoverReceipt) { r.Stream = "OTHER" },
		"control":        func(r *approvalConsumerCutoverReceipt) { r.Control = "OTHER" },
		"mode":           func(r *approvalConsumerCutoverReceipt) { r.CapturedMode = "PROOF_REQUIRED" },
		"relay":          func(r *approvalConsumerCutoverReceipt) { r.CapturedRelayState = "RUNNING" },
		"zero-version":   func(r *approvalConsumerCutoverReceipt) { r.ControlVersion = "0" },
		"null-epoch":     func(r *approvalConsumerCutoverReceipt) { r.ApprovalEpoch = "" },
		"overflow-epoch": func(r *approvalConsumerCutoverReceipt) { r.AuthorizationRelayEpoch = "9223372036854775808" },
		"leading-zero":   func(r *approvalConsumerCutoverReceipt) { r.ControlVersion = "03" },
		"future": func(r *approvalConsumerCutoverReceipt) {
			r.CapturedAt = now.Add(time.Nanosecond).Format(time.RFC3339Nano)
		},
		"offset":            func(r *approvalConsumerCutoverReceipt) { r.CapturedAt = "2026-09-15T08:00:00+00:00" },
		"negative-q":        func(r *approvalConsumerCutoverReceipt) { r.HighWatermark = "-1" },
		"overflow-q":        func(r *approvalConsumerCutoverReceipt) { r.HighWatermark = "18446744073709551615" },
		"padded-q":          func(r *approvalConsumerCutoverReceipt) { r.HighWatermark = "01" },
		"wrong-start":       func(r *approvalConsumerCutoverReceipt) { r.BillingStarts[0].StartSequence = "3" },
		"missing-durable":   func(r *approvalConsumerCutoverReceipt) { r.BillingStarts = r.BillingStarts[:2] },
		"duplicate-durable": func(r *approvalConsumerCutoverReceipt) { r.BillingStarts[1] = r.BillingStarts[0] },
		"extra-durable":     func(r *approvalConsumerCutoverReceipt) { r.BillingStarts = append(r.BillingStarts, r.BillingStarts[0]) },
		"token-durable": func(r *approvalConsumerCutoverReceipt) {
			r.BillingStarts[0].Durable = approvalconsumer.TokenCommands()[0].Durable
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			files, receipt := consumerReceiptFixture(t, now, 1)
			mutate(&receipt)
			replaceConsumerReceiptFixture(t, &files, receipt)
			if _, err := files.Load(now); err == nil {
				t.Fatal("invalid receipt accepted even with test-controlled trusted digest")
			}
		})
	}
}

func TestApprovalConsumerReceiptRejectsArtifactDrift(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, name := range []string{"missing-hash", "wrong-hash", "changed-bytes", "duplicate-key", "extra-field", "whitespace", "manifest-drift", "identity-drift"} {
		t.Run(name, func(t *testing.T) {
			files, receipt := consumerReceiptFixture(t, now, 1)
			raw, _ := json.Marshal(receipt)
			switch name {
			case "missing-hash":
				files.ReceiptSHA256 = ""
			case "wrong-hash":
				files.ReceiptSHA256 = strings.Repeat("0", 64)
			case "changed-bytes":
				if os.WriteFile(files.ReceiptFile, []byte("{}"), 0600) != nil {
					t.Fatal("fixture")
				}
			case "duplicate-key", "extra-field", "whitespace":
				if name == "whitespace" {
					raw = append(raw, '\n')
				} else {
					field := `,"schema_version":1}`
					if name == "extra-field" {
						field = `,"extra":true}`
					}
					raw = append(raw[:len(raw)-1], []byte(field)...)
				}
				if os.WriteFile(files.ReceiptFile, raw, 0600) != nil {
					t.Fatal("fixture")
				}
				files.ReceiptSHA256 = consumerManifestHash(raw)
			case "manifest-drift":
				files.ManifestSHA256 = strings.Repeat("0", 64)
			case "identity-drift":
				files.Identity.CurrentSHA256 = strings.Repeat("0", 64)
			}
			if _, err := files.Load(now); err == nil {
				t.Fatal("untrusted or noncanonical artifact accepted")
			}
		})
	}
}
