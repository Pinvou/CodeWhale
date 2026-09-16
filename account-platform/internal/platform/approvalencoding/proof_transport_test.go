package approvalencoding

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProofTransportClosedGoldenAndBinding(t *testing.T) {
	fixture := readGolden(t)
	requestID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	input := frozenLegacyInputs()[0].input
	artifacts, err := ConvertLegacyProofInput(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := EncodeProofRequest(requestID, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(request, []byte(`"proof_schema_version":"1"`)) || bytes.Contains(request, []byte(`"normalized_command":{"secret"`)) {
		t.Fatalf("invalid proof request: %s", request)
	}
	responseRaw := []byte(fixture.ProofResponseVectors[0].ResponseJSON)
	response, digest, err := DecodeProofResponse(responseRaw)
	if err != nil || digest != fixture.ProofResponseVectors[0].ProofDigest || !ProofResponseBound(response, requestID, artifacts) ||
		!ProofResponseFresh(response, response.IssuedAt.Add(5*time.Second)) || !ProofTransportWithinBudget(request, responseRaw) {
		t.Fatalf("golden proof response invalid: %+v digest=%q err=%v", response, digest, err)
	}
	changed := artifacts
	changed.CommandDigest = strings.Repeat("a", 64)
	if _, err := EncodeProofRequest(requestID, changed); !errors.Is(err, ErrInvalidDocument) || ProofResponseBound(response, requestID, changed) {
		t.Fatal("mismatched command digest was accepted")
	}
	if ProofResponseFresh(response, response.IssuedAt.Add(31*time.Second)) || ProofResponseFresh(response, response.IssuedAt.Add(-31*time.Second)) {
		t.Fatal("proof clock skew exceeded thirty seconds")
	}
	if ProofTransportWithinBudget(bytes.Repeat([]byte("x"), MaximumDocumentBytes-4), []byte("12345")) {
		t.Fatal("aggregate 64KiB limit was exceeded")
	}
}

func TestProofTransportRejectsMalformedResponse(t *testing.T) {
	valid := readGolden(t).ProofResponseVectors[0].ResponseJSON
	for _, raw := range []string{
		strings.Replace(valid, `"verdict":"ALLOW"`, `"verdict":"ALLOW","verdict":"DENY"`, 1),
		strings.Replace(valid, `"verdict":"ALLOW"`, `"verdict":"ALLOW","secret":"leak"`, 1),
		strings.Replace(valid, `"policy_version":1`, `"policy_version":1.0`, 1),
		strings.Replace(valid, `"reason_code":"PROOF_VALID"`, `"reason_code":"OTHER"`, 1),
		strings.Replace(valid, `"expires_at":"2026-09-15T01:05:05.678Z"`, `"expires_at":"2026-09-15T01:05:06.678Z"`, 1),
		valid + `{}`,
	} {
		if _, _, err := DecodeProofResponse([]byte(raw)); err == nil {
			t.Fatalf("malformed proof accepted: %s", raw)
		}
	}
}
