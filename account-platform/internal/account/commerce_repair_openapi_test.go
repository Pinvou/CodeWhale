package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func commerceRepairValidateDTO(t *testing.T, name string, value any, want bool) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("serialize actual operator DTO")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", filepath.Join("..", "..", "test", "fixtures", "commerce-repair-validator.mjs"), name)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exited *exec.ExitError
	valid := err == nil && stdout.String() == "VALID\n" && stderr.Len() == 0
	invalid := errors.As(err, &exited) && exited.ExitCode() == 1 && stdout.Len() == 0 && stderr.String() == "INVALID_COMMERCE_REPAIR_WIRE\n"
	if ctx.Err() != nil || want && !valid || !want && !invalid {
		t.Fatal("operator contract exact process protocol failed (payload suppressed)")
	}
}

func TestCommerceRepairOpenAPIActualDTO(t *testing.T) {
	now := time.Now().UTC()
	caseID, jobID, mappingID, subjectID, proposerID, reviewerID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := CommerceRepairCase{ID: caseID, JobID: jobID, MappingID: mappingID, MappingVersion: 1, SubjectType: "USER", SubjectID: subjectID, SubjectLifecycleVersion: 1, ProviderID: "owned-commerce", AdapterVersion: "v1", TaskCategory: "CUSTOMER_PROVISIONING", Attempts: 12, Version: 1, Status: "OPEN", CreatedAt: now}
	in := CommerceRepairObservationInput{ExpectedVersion: 1, ObservationID: uuid.New(), Outcome: "SUCCEEDED", EvidenceReference: "controlled-record-0001", EvidenceSHA256: strings.Repeat("b", 64), AllAttemptsEnded: true, ExternalReferenceRedacted: "***1234"}
	o := CommerceRepairObservation{ID: in.ObservationID, CaseID: caseID, CaseVersion: 2, Outcome: in.Outcome, EvidenceReference: in.EvidenceReference, EvidenceSHA256: in.EvidenceSHA256, AllAttemptsEnded: true, ExternalReferenceRedacted: in.ExternalReferenceRedacted, ProposerUserID: proposerID, ProposerBindingID: uuid.New(), ProposerAuthorizationVersion: 3, CreatedAt: now}
	t.Run("actual-open-case", func(t *testing.T) { commerceRepairValidateDTO(t, "Case", c, true) })
	t.Run("actual-observation-with-private-authority-hidden", func(t *testing.T) { commerceRepairValidateDTO(t, "Observation", o, true) })
	t.Run("actual-input-and-confirmation", func(t *testing.T) {
		commerceRepairValidateDTO(t, "ObservationInput", in, true)
		commerceRepairValidateDTO(t, "Confirmation", CommerceRepairConfirmation{ExpectedVersion: 2, ObservationID: o.ID}, true)
	})
	t.Run("actual-queue", func(t *testing.T) {
		commerceRepairValidateDTO(t, "Queue", map[string]any{"items": []CommerceRepairCase{c}}, true)
	})
	t.Run("actual-closed-case-with-late-evidence", func(t *testing.T) {
		closed := c
		closed.Status = "RESOLVED"
		closed.Version = 3
		resolution := "SUCCEEDED_CURRENT"
		closed.Resolution = &resolution
		closed.ApprovedObservationID = &o.ID
		closed.ReviewerUserID = &reviewerID
		closed.ResolvedAt = &now
		commerceRepairValidateDTO(t, "Detail", CommerceRepairDetail{Case: closed, Observations: []CommerceRepairObservation{o}, Callbacks: []CommerceRepairCallback{{ID: uuid.New(), Sequence: 1, CaseID: caseID, Attempts: 1, Kind: "PROVIDER_ERROR", AfterCompletion: true, RecordedAt: now}}}, true)
	})
	t.Run("unfinished-and-wrong-outcome-reference-rejected", func(t *testing.T) {
		bad := in
		bad.AllAttemptsEnded = false
		commerceRepairValidateDTO(t, "ObservationInput", bad, false)
		bad = in
		bad.Outcome = "ENDED_NO_EFFECT"
		commerceRepairValidateDTO(t, "ObservationInput", bad, false)
	})
}
