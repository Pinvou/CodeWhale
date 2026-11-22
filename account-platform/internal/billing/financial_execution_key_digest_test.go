package billing

import "testing"

func TestLegacyFinancialExecutionKeyDigestV1HistoricalWire(t *testing.T) {
	// Literal preimages and digests preserve the historical contract independently
	// of the implementation's marshaling code, including HTML escaping and UTF-8.
	cases := []struct {
		name, key, preimage, digest string
	}{
		{"trim", "  approval:legacy-0001 \t\n", `{"command_type":"LEDGER_REPAIR","execution_key":"approval:legacy-0001"}`, "f546660307c2429b22bd84384f559bef74a6c24b32ce11d29dbf30f2a350d306"},
		{"unicode", "\u3000审批:旧key\u00a0", `{"command_type":"LEDGER_REPAIR","execution_key":"审批:旧key"}`, "c9f3e5a83fdca702c3bef06ec8e8313ff00bda60085e2b24974b4b583c3a073e"},
		{"escaping", "x<>&\"\\\n\t\x00z", `{"command_type":"LEDGER_REPAIR","execution_key":"x\u003c\u003e\u0026\"\\\n\t\u0000z"}`, "8ab1a909f8179020f873b11d26ae78094259419a214bc133a7b589d49412d136"},
		{"empty", " \t\n", `{"command_type":"LEDGER_REPAIR","execution_key":""}`, "392e6b4aca0500b5acd658eef350bfb9066a08b49f248f769073cd4fc42f4524"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := digestHex([]byte(tc.preimage)); got != tc.digest {
				t.Fatalf("historical literal preimage digest=%s want=%s", got, tc.digest)
			}
			if got := legacyFinancialExecutionKeyDigestV1(tc.key); got != tc.digest {
				t.Fatalf("legacy digest=%s want=%s", got, tc.digest)
			}
			if got := ledgerRepairExecutionKeyDigest(tc.key); got != tc.digest {
				t.Fatalf("Ledger compatibility wrapper digest=%s want=%s", got, tc.digest)
			}
		})
	}
}
