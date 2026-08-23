package token

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCatalogDraftNormalizationAndVersionBoundaries(t *testing.T) {
	input := CatalogDraftInput{DisplayName: "  DeepSeek V4  ", Aliases: []string{" DS-V4 ", "ds-v4"}, ProtocolCode: "chat", ProtocolVersion: 1, ContextLength: 128000, MaximumOutputTokens: 4096, SupportedModalities: []string{" Text ", "text"}, TokenizerVersion: " tokenizer-v1 "}
	if err := validateCatalogDraft(&input); err != nil {
		t.Fatal(err)
	}
	if input.DisplayName != "DeepSeek V4" || input.ProtocolCode != "CHAT" || len(input.Aliases) != 1 || input.Aliases[0] != "ds-v4" || len(input.SupportedModalities) != 1 || input.TokenizerVersion != "tokenizer-v1" {
		t.Fatalf("catalog draft was not normalized deterministically: %#v", input)
	}
	item := CatalogModel{ContextLength: 128000, MaximumOutputTokens: 4096}
	if item.ValidateRequestLimits(1000, 4096) != nil {
		t.Fatal("valid request was rejected")
	}
	if item.ValidateRequestLimits(127000, 4096) == nil || item.ValidateRequestLimits(1, 4097) == nil {
		t.Fatal("capability boundaries must reject oversized requests")
	}
}

func TestCatalogSnapshotsKeepHistoricalDisplayAndLifecycleFacts(t *testing.T) {
	effective := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	item := CatalogModel{ModelID: uuid.MustParse("00000000-0000-7000-8000-000000000042"), CatalogVersion: 9, DisplayName: "Historical name", LifecycleStatus: "DEPRECATED", DeprecationEffectiveAt: &effective, ProtocolDefinitionID: uuid.MustParse("00000000-0000-7000-8200-000000000000"), ProtocolCode: "CHAT", ProtocolVersion: 1, ModelCapabilityVersionID: uuid.MustParse("00000000-0000-7000-8200-000000000042"), CapabilityVersion: 3, ContextLength: 64000, MaximumOutputTokens: 2048, SupportedModalities: []string{"text"}, TokenizerVersion: "tok-v3"}
	snapshot := item.Snapshot()
	for _, key := range []string{"model_id", "catalog_version", "display_name", "lifecycle_status", "protocol_definition_id", "model_capability_version_id", "context_length"} {
		if snapshot[key] == nil {
			t.Fatalf("historical snapshot omitted %s", key)
		}
	}
}

func TestProtocolCapabilityMustMatchCatalogVersion(t *testing.T) {
	capabilities := []GatewayProtocolCapability{{ProtocolCode: "CHAT", ProtocolVersion: 1}}
	if !protocolSupported(capabilities, "CHAT", 1) || protocolSupported(capabilities, "CHAT", 2) || protocolSupported(capabilities, "EMBEDDING", 1) {
		t.Fatal("catalog protocol validation must require an exact declared version")
	}
}
