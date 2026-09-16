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
	reencoded, err := EncodeProofResponse(response)
	if err != nil || !bytes.Equal(reencoded, responseRaw) {
		t.Fatalf("owner response differs from frozen proof: %v", err)
	}
	response.DomainRevision = ""
	if _, err := EncodeProofResponse(response); err == nil {
		t.Fatal("owner encoded an unbound domain revision")
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

func TestProofRequestSixClosedVariants(t *testing.T) {
	id := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	for _, vector := range frozenLegacyInputs() {
		t.Run(vector.name, func(t *testing.T) {
			artifacts, err := ConvertLegacyProofInput(vector.input)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeProofRequest(id, artifacts)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeProofRequest(encoded)
			if err != nil || decoded.ApprovalRequestID != id || !bytes.Equal(decoded.Artifacts.ProofInputJSON, artifacts.ProofInputJSON) ||
				decoded.Artifacts.CommandDigest != artifacts.CommandDigest || decoded.Artifacts.SnapshotDigest != artifacts.SnapshotDigest {
				t.Fatalf("closed proof request changed: %v", err)
			}
			legacy, err := LegacyProofInputFromArtifacts(decoded.Artifacts)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := ConvertLegacyProofInput(legacy)
			if err != nil || roundTrip.CommandDigest != artifacts.CommandDigest || roundTrip.SnapshotDigest != artifacts.SnapshotDigest {
				t.Fatalf("owner-domain read adapter changed proof facts: %v", err)
			}
			for _, invalid := range [][]byte{
				bytes.Replace(encoded, []byte(`"command_digest":`), []byte(`"command_digest":"wrong","command_digest":`), 1),
				bytes.Replace(encoded, []byte(`"command_digest":`), []byte(`"secret":"untrusted","command_digest":`), 1),
				bytes.Replace(encoded, []byte(artifacts.CommandDigest), []byte(strings.Repeat("a", 64)), 1),
				append(append([]byte{}, encoded...), []byte(`{}`)...),
			} {
				if _, err := DecodeProofRequest(invalid); err == nil {
					t.Fatal("accepted malformed owner-domain request")
				}
			}
		})
	}
}

func TestLegacyLifecycleOffsetNormalizesToSameFrozenProof(t *testing.T) {
	for _, vector := range frozenLegacyInputs() {
		if vector.input.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
			continue
		}
		original, err := ConvertLegacyProofInput(vector.input)
		if err != nil {
			t.Fatal(err)
		}
		offset := vector.input
		offset.NormalizedCommand = bytes.ReplaceAll(offset.NormalizedCommand, []byte("2026-09-15T01:02:03Z"), []byte("2026-09-15T09:02:03+08:00"))
		offset.FinancialSnapshot = bytes.ReplaceAll(offset.FinancialSnapshot, []byte("2026-09-15T01:02:03Z"), []byte("2026-09-15T09:02:03+08:00"))
		converted, err := ConvertLegacyProofInput(offset)
		if err != nil || converted.CommandDigest != original.CommandDigest || converted.SnapshotDigest != original.SnapshotDigest {
			t.Fatalf("equivalent timezone changed proof: %v", err)
		}
		offset.NormalizedCommand = bytes.ReplaceAll(offset.NormalizedCommand, []byte("2026-09-15T09:02:03+08:00"), []byte("2026-09-15T09:02:03+99:00"))
		if _, err := ConvertLegacyProofInput(offset); err == nil {
			t.Fatal("invalid timezone offset accepted")
		}
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
