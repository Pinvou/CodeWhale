package account

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalBackfillDiagnosticIsClosedAndRedacted(t *testing.T) {
	untrusted := "private-password"
	id := uuid.New()
	err := newApprovalAuthorizationBackfillDiagnostic(approvalAuthorizationBackfillCandidate{EventID: id, Payload: json.RawMessage(`{"secret":"private-password"}`), SidecarSerializationVersion: &untrusted}, untrusted)
	var diagnostic *approvalAuthorizationBackfillDiagnostic
	if !errors.As(err, &diagnostic) || !errors.Is(err, ErrApprovalBackfillUnavailable) {
		t.Fatal("lost stable classification")
	}
	report, ok := ApprovalAuthorizationBlockerReportFrom(fmt.Errorf("private wrapper: %w", err))
	if !ok {
		t.Fatal("missing verified report")
	}
	encoded, encodeErr := json.Marshal(report)
	if encodeErr != nil || report.EventID != id || report.Reason != "UNKNOWN" || report.SchemaVersion != 1 || report.SerializationVersion != "UNKNOWN" || len(report.PayloadDigestPrefix) != 12 || strings.Contains(string(encoded), untrusted) || strings.Contains(err.Error(), untrusted) {
		t.Fatal("unsafe diagnostic")
	}
	if _, err := hex.DecodeString(report.PayloadDigestPrefix); err != nil || report.PayloadDigestPrefix != strings.ToLower(report.PayloadDigestPrefix) {
		t.Fatal("digest prefix is not lowercase hex")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 5 {
		t.Fatal("report is not closed")
	}
}

func TestApprovalBackfillDiagnosticRejectsZeroValues(t *testing.T) {
	for _, err := range []error{nil, errors.New("private"), &approvalAuthorizationBackfillDiagnostic{}} {
		if _, ok := ApprovalAuthorizationBlockerReportFrom(err); ok {
			t.Fatal("invalid diagnostic was exposed")
		}
	}
}

func TestApprovalBackfillDiagnosticPreservesClosedReasons(t *testing.T) {
	for _, reason := range []string{"INVALID_EVENT_IDENTITY", "LEGACY_RECONSTRUCTION_FAILED", "LEGACY_CLOSED_DECODE_FAILED", "MISSING_IMMUTABLE_EVIDENCE", "SIDECAR_BINDING_MISMATCH", "DELIVERY_EVIDENCE_MISMATCH", "SEMANTIC_EVIDENCE_MISMATCH", "UNKNOWN_PROVENANCE", "ROW_WRITE_REJECTED", "ROW_WRITE_OUTCOME_UNKNOWN"} {
		err := newApprovalAuthorizationBackfillDiagnostic(approvalAuthorizationBackfillCandidate{EventID: uuid.New()}, reason)
		report, ok := ApprovalAuthorizationBlockerReportFrom(err)
		if !ok || report.Reason != reason {
			t.Fatalf("lost reason %s", reason)
		}
	}
}
