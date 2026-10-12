package events

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

// ApprovalConsumerResourceFiles names independently trusted public artifacts.
// SHA values must come from deployment authority, never from hashing these
// untrusted files at startup. Call Load again during readiness checks: a loaded
// expectation is not evidence of current artifacts, control state or broker ACL.
type ApprovalConsumerResourceFiles struct {
	Identity       NATSAuthorizationArtifactFiles
	ManifestFile   string
	ManifestSHA256 string
	ReceiptFile    string
	ReceiptSHA256  string
}

type approvalConsumerReceiptStart struct {
	Durable       string `json:"durable"`
	StartSequence string `json:"start_sequence"`
}

type approvalConsumerCutoverReceipt struct {
	Schema                  string                         `json:"schema"`
	SchemaVersion           int                            `json:"schema_version"`
	IdentitySHA256          string                         `json:"identity_sha256"`
	ManifestSHA256          string                         `json:"manifest_sha256"`
	Stream                  string                         `json:"stream"`
	Control                 string                         `json:"control"`
	CapturedMode            string                         `json:"captured_mode"`
	CapturedRelayState      string                         `json:"captured_relay_state"`
	ControlVersion          string                         `json:"control_version"`
	ApprovalEpoch           string                         `json:"approval_epoch"`
	AuthorizationRelayEpoch string                         `json:"authorization_relay_epoch"`
	CapturedAt              string                         `json:"captured_at"`
	HighWatermark           string                         `json:"high_watermark"`
	BillingStarts           []approvalConsumerReceiptStart `json:"billing_starts"`
}

// VerifiedReceipt returns only independently hash-bound canonical public
// bytes, using the bounded reader again after the complete contract check.
func (files ApprovalConsumerResourceFiles) VerifiedReceipt(now time.Time) ([]byte, error) {
	if _, err := files.Load(now); err != nil {
		return nil, ErrNATSResources
	}
	raw, _, err := readNATSPublicArtifact(files.ReceiptFile, files.ReceiptSHA256)
	return raw, err
}

// Load only reads and verifies artifacts. GSA-38 owns receipt capture from the
// actual fenced stream and its independent trust projection. This function
// neither creates a receipt nor contacts NATS/Account nor grants execution.
func (files ApprovalConsumerResourceFiles) Load(now time.Time) (*ApprovalConsumerResourceBinding, error) {
	if files.Identity.Validate(now) != nil {
		return nil, ErrNATSResources
	}
	for _, path := range []string{files.ManifestFile, files.ReceiptFile} {
		if !isolatedNATSPublicArtifactPath(path, files.Identity.PrivateIdentityFiles) {
			return nil, ErrNATSResources
		}
	}
	manifest, manifestInfo, err := readNATSPublicArtifact(files.ManifestFile, files.ManifestSHA256)
	if err != nil {
		return nil, ErrNATSResources
	}
	expected, err := ApprovalConsumerManifest(files.Identity.CurrentSHA256)
	if err != nil || !bytes.Equal(manifest, expected) {
		return nil, ErrNATSResources
	}
	raw, receiptInfo, err := readNATSPublicArtifact(files.ReceiptFile, files.ReceiptSHA256)
	if err != nil || os.SameFile(manifestInfo, receiptInfo) {
		return nil, ErrNATSResources
	}
	var receipt approvalConsumerCutoverReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil {
		return nil, ErrNATSResources
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(raw, canonical) ||
		receipt.Schema != "approval-consumer-cutover-receipt-v1" || receipt.SchemaVersion != 1 ||
		receipt.IdentitySHA256 != files.Identity.CurrentSHA256 || receipt.ManifestSHA256 != files.ManifestSHA256 ||
		receipt.Stream != platformStream || receipt.Control != "ADMIN_APPROVAL" ||
		receipt.CapturedMode != "QUIESCING" || receipt.CapturedRelayState != "FENCED" {
		return nil, ErrNATSResources
	}
	for _, value := range []string{receipt.ControlVersion, receipt.ApprovalEpoch, receipt.AuthorizationRelayEpoch} {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
			return nil, ErrNATSResources
		}
	}
	captured, err := time.Parse(time.RFC3339Nano, receipt.CapturedAt)
	if err != nil || captured.Location() != time.UTC || captured.Format(time.RFC3339Nano) != receipt.CapturedAt || captured.After(now) {
		return nil, ErrNATSResources
	}
	q, err := strconv.ParseUint(receipt.HighWatermark, 10, 64)
	if err != nil || q == math.MaxUint64 || strconv.FormatUint(q, 10) != receipt.HighWatermark {
		return nil, ErrNATSResources
	}
	descriptors := approvalconsumer.BillingCommands()
	if len(receipt.BillingStarts) != len(descriptors) {
		return nil, ErrNATSResources
	}
	binding := &ApprovalConsumerResourceBinding{startSequences: make(map[string]uint64, len(descriptors))}
	for index, descriptor := range descriptors {
		entry := receipt.BillingStarts[index]
		if entry.Durable != descriptor.Durable || entry.StartSequence != strconv.FormatUint(q+1, 10) {
			return nil, ErrNATSResources
		}
		binding.startSequences[descriptor.Durable] = q + 1
	}
	return binding, nil
}
