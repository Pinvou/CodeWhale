package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

type catalogPreviewRepository struct {
	CatalogRepository
	version      int64
	models       []CatalogModel
	syncCalls    int
	synchronized []DiscoveredModel
}

func (repository catalogPreviewRepository) ListCatalog(context.Context) (int64, []CatalogModel, error) {
	return repository.version, repository.models, nil
}

func (repository *catalogPreviewRepository) SyncCatalog(_ context.Context, _, _ string, models []DiscoveredModel, _, _ int64, _ uuid.UUID) (int64, bool, error) {
	repository.syncCalls++
	repository.synchronized = append([]DiscoveredModel(nil), models...)
	return repository.version + 1, true, nil
}

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

func TestCatalogSyncPreviewIsProviderScopedAndSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret-value" {
			t.Fatalf("unexpected discovery request path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"available-again"},{"id":"new-model"},{"id":"new-model"}]}`))
	}))
	defer server.Close()
	availableAgainID, removedID := uuid.New(), uuid.New()
	repository := &catalogPreviewRepository{version: 9, models: []CatalogModel{
		{ModelID: availableAgainID, ProviderID: "PROVIDER", UpstreamModelRef: "available-again", DisplayName: "Available Again", LifecycleStatus: "PUBLISHED", SourceAvailable: false},
		{ModelID: removedID, ProviderID: "PROVIDER", UpstreamModelRef: "removed-model", DisplayName: "Removed Model", LifecycleStatus: "PUBLISHED", SourceAvailable: true},
		{ModelID: uuid.New(), ProviderID: "OTHER", UpstreamModelRef: "other-provider-model", DisplayName: "Other Provider", LifecycleStatus: "PUBLISHED", SourceAvailable: true},
	}}
	synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider", server.URL, []byte("secret-value"), 128000, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := synchronizer.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if preview.CatalogVersion != 9 || preview.Added != 1 || preview.Missing != 1 || preview.Difference == nil || preview.Difference.ModifiedCount != 1 || preview.Difference.TotalCount != 3 || preview.Difference.Truncated || len(preview.Difference.Items) != 3 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if preview.Difference.Items[0].ChangeType != "ADDED" || preview.Difference.Items[0].ModelID != nil || preview.Difference.Items[1].ModelID == nil || *preview.Difference.Items[1].ModelID != availableAgainID || preview.Difference.Items[2].ModelID == nil || *preview.Difference.Items[2].ModelID != removedID {
		t.Fatalf("unexpected sanitized fixed-order items: %+v", preview.Difference.Items)
	}
	for _, item := range preview.Difference.Items {
		if item.DisplayName == "Other Provider" {
			t.Fatal("another provider leaked into the scoped difference")
		}
	}
}

func TestCatalogSyncConsumesOnlyTheExactlyMatchedSecondObservation(t *testing.T) {
	discoveries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		discoveries++
		if discoveries == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"}]}`))
	}))
	defer server.Close()
	repository := &catalogPreviewRepository{version: 2}
	synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider", server.URL, []byte("secret-value"), 128000, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := synchronizer.Sync(context.Background()); !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("changed provider observation must fail closed, got %v", err)
	}
	if discoveries != 2 || repository.syncCalls != 0 {
		t.Fatalf("drift must use exactly one write-before re-observation and persist nothing: discoveries=%d writes=%d", discoveries, repository.syncCalls)
	}
}

func TestCatalogObservedVersionBindsEveryConsumedProviderField(t *testing.T) {
	serverPayload := `{"data":[{"id":"model-b"},{"id":"model-a"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(serverPayload)) }))
	defer server.Close()
	repository := &catalogPreviewRepository{version: 2}
	synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider-a", server.URL, []byte("secret-value"), 128000, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := synchronizer.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := synchronizer.SyncObserved(context.Background(), preview.AdapterObservedVersion); err != nil {
		t.Fatal(err)
	}
	if repository.syncCalls != 1 || len(repository.synchronized) != 2 || repository.synchronized[0].UpstreamModelRef != "model-a" {
		t.Fatalf("matched normalized observation was not consumed exactly: %+v", repository.synchronized)
	}
	otherProvider, err := NewCatalogSynchronizer(&catalogPreviewRepository{version: 2}, NewHTTPUpstreamClient(), "provider-b", server.URL, []byte("secret-value"), 128000, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	otherPreview, err := otherProvider.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if otherPreview.AdapterObservedVersion == preview.AdapterObservedVersion {
		t.Fatal("provider identity must be part of the authoritative observation")
	}
	ordered, orderedVersion, err := normalizeCatalogObservation("PROVIDER-A", "adapter-v1", []DiscoveredModel{{UpstreamModelRef: "b", DisplayName: "B"}, {UpstreamModelRef: "a", DisplayName: "A"}})
	if err != nil || len(ordered) != 2 || ordered[0].UpstreamModelRef != "a" {
		t.Fatalf("observation was not normalized: models=%+v err=%v", ordered, err)
	}
	_, reorderedVersion, err := normalizeCatalogObservation("PROVIDER-A", "adapter-v1", []DiscoveredModel{{UpstreamModelRef: "a", DisplayName: "A"}, {UpstreamModelRef: "b", DisplayName: "B"}})
	if err != nil || reorderedVersion != orderedVersion {
		t.Fatal("provider result order must not change the observation")
	}
	_, renamedVersion, err := normalizeCatalogObservation("PROVIDER-A", "adapter-v1", []DiscoveredModel{{UpstreamModelRef: "a", DisplayName: "renamed"}, {UpstreamModelRef: "b", DisplayName: "B"}})
	if err != nil || renamedVersion == orderedVersion {
		t.Fatal("a consumed display-name change must invalidate the observation")
	}
	if _, _, err := normalizeCatalogObservation("PROVIDER-A", "adapter-v1", []DiscoveredModel{{UpstreamModelRef: "a", DisplayName: "A"}, {UpstreamModelRef: "a", DisplayName: "different"}}); !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("ambiguous duplicate provider facts must fail closed: %v", err)
	}
}
