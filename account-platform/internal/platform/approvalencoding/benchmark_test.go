package approvalencoding

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Use the same frozen semantic vectors as the cross-language tests. Padding is
// legal JSON transport whitespace, not an invented field or business payload.
func benchmarkProofVector(b *testing.B) (string, int64, []byte) {
	b.Helper()
	body, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		b.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Command string `json:"command_type"`
			Policy  int64  `json:"policy_version"`
			Proof   string `json:"proof_json"`
		} `json:"proof_vectors"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil || len(fixture.Vectors) == 0 {
		b.Fatalf("invalid benchmark fixture: %v", err)
	}
	largest := fixture.Vectors[0]
	for _, vector := range fixture.Vectors[1:] {
		if len(vector.Proof) > len(largest.Proof) {
			largest = vector
		}
	}
	if len(largest.Proof) > MaximumDocumentBytes {
		b.Fatal("semantic fixture exceeds protocol limit")
	}
	raw := []byte(largest.Proof + strings.Repeat(" ", MaximumDocumentBytes-len(largest.Proof)))
	return largest.Command, largest.Policy, raw
}

func BenchmarkProofMaximumTransport(b *testing.B) {
	command, policy, raw := benchmarkProofVector(b)
	if _, err := DecodeProofInput(command, policy, raw); err != nil {
		b.Fatalf("maximum valid transport rejected: %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := DecodeProofInput(command, policy, raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProofMalformedMaximumTransport(b *testing.B) {
	command, policy, raw := benchmarkProofVector(b)
	// A trailing non-whitespace byte forces a complete bounded scan and rejection.
	raw[len(raw)-1] = '!'
	if _, err := DecodeProofInput(command, policy, raw); err == nil {
		b.Fatal("malformed maximum transport accepted")
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := DecodeProofInput(command, policy, raw); err == nil {
			b.Fatal("malformed maximum transport accepted")
		}
	}
}
