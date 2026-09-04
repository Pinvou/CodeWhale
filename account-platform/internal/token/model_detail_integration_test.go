package token

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAdminModelDetailPostgresIntegration is a release gate. CI supplies an
// isolated PINVOU_TEST_TOKEN_DATABASE_URL and must reject a skipped result.
func TestAdminModelDetailPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	var protocolID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' ORDER BY version DESC LIMIT 1`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	modelID, capabilityID, catalogID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	previousCapabilityID, previousCatalogID := uuid.New(), uuid.New()
	now := time.Now().UTC().Add(-time.Minute)
	var previousCatalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'previous model detail integration',$2,$3,$3) RETURNING version`, previousCatalogID, actorID, now.Add(-time.Minute)).Scan(&previousCatalogVersion); err != nil {
		t.Fatal(err)
	}
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'model detail integration',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'private-provider','private-adapter','private-upstream',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($5,$1,1,$6,128000,8192,'["TEXT"]','v1',$3,$4),
      ($8,$1,2,$6,256000,16384,'["TEXT"]','v2',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($9,$1,'Detail model legacy','detail model legacy','safe legacy description','{}','PUBLISHED',$5,$6,true),
      ($7,$1,'Detail model','detail model','safe description','{}','PUBLISHED',$8,$6,true);`, modelID, digest, actorID, now, previousCapabilityID, protocolID, catalogVersion, capabilityID, previousCatalogVersion); err != nil {
		t.Fatal(err)
	}
	detail, err := repository.adminModelDetail(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Model.ModelID != modelID || detail.Protocol.ID != protocolID || detail.Protocol.SchemaFieldCount != 4 || detail.AsOf.IsZero() {
		t.Fatalf("unexpected detail projection: %+v", detail)
	}
	page, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: actorID, AuthorizationVersion: 1, Resource: "model-versions", Scope: "model:" + modelID.String(), Filters: map[string]string{}, Sort: adminModelVersionsSort, Limit: 25}, modelVersionSnapshotSource(modelID))
	if err != nil {
		t.Fatal(err)
	}
	history, err := repository.adminModelVersionsByIDs(ctx, modelID, page.ResourceIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].CatalogVersion != previousCatalogVersion || history[0].SchemaFieldCount != 4 || history[0].CapabilityVersion != 1 {
		t.Fatalf("unexpected immutable model history: %+v", history)
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(payload)
	historyPayload, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	serialized += string(historyPayload)
	for _, private := range []string{"private-provider", "private-adapter", "private-upstream", "request_schema", "expression", "safe legacy description"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("private field leaked in model detail: %s", private)
		}
	}
	if _, err = repository.adminModelDetail(ctx, uuid.New()); err != ErrNotFound {
		t.Fatalf("missing model error=%v", err)
	}
}
