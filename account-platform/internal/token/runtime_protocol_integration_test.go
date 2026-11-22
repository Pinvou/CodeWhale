package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRuntimeProtocolPublicationPublicPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor := uuid.New()
	version, _, err := r.SyncCatalog(ctx, "CONTROLLED", "controlled-v1", []DiscoveredModel{{UpstreamModelRef: "published-model-alias", DisplayName: "deepseek-flash"}}, 8192, 1024, actor)
	if err != nil {
		t.Fatal(err)
	}
	_, models, err := r.ListCatalog(ctx)
	if err != nil || len(models) != 1 || models[0].RuntimeProtocol != "" {
		t.Fatalf("discovery must not infer runtime protocol: %v %+v", err, models)
	}
	id := models[0].ModelID
	service, _, closeAuth := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: actor, ContextType: "PERSONAL"})
	defer closeAuth()
	service.repository = r
	public := func(label string) CatalogModel {
		t.Helper()
		w := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer controlled-fixture")
		f04IdentityHandler(http.HandlerFunc(service.listCatalogModels)).ServeHTTP(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("public catalog: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Items []CatalogModel `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
			t.Fatalf("public catalog decode: %v %s", err, w.Body.String())
		}
		for _, secret := range []string{"controlled-v1", "provider_id", "upstream_model_ref", "adapter_version"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("private routing fact exposed: %s", secret)
			}
		}
		// The cross-language regression consumes these exact production HTTP
		// bytes, rather than hand-building the server side of the Rust fixture.
		t.Logf("RUNTIME_CATALOG_EVIDENCE %s %s", label, w.Body.String())
		return body.Items[0]
	}
	missing := public("missing")
	if missing.RuntimeProtocol != "" {
		t.Fatal("missing profile was guessed")
	}
	publish := func(name, profile string) CatalogModel {
		t.Helper()
		command := governedModelDraftCommand{ModelID: id, ExpectedCatalogVersion: version, DisplayName: name,
			ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 8192, MaximumOutputTokens: 1024,
			SupportedModalities: []string{"text"}, TokenizerVersion: "controlled-tokenizer", RuntimeProtocol: profile}
		raw, _ := json.Marshal(command)
		normalized, err := parseGovernedModelDraft(id.String(), raw)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if err = lockCatalogPublication(ctx, tx); err != nil {
			t.Fatal(err)
		}
		draft, err := saveCatalogDraftEffect(ctx, tx, id, normalized.catalogInput(), actor, time.Now().UTC())
		if err != nil || draft.RuntimeProtocol != profile {
			t.Fatalf("governed draft lost profile: %v %+v", err, draft)
		}
		item, err := publishCatalogDraftEffect(ctx, tx, id, version, draft.DraftVersion, "controlled protocol publication", actor, []GatewayProtocolCapability{{ProtocolCode: "CHAT", ProtocolVersion: 1}}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		version = item.CatalogVersion
		return item
	}
	t.Run("explicit-profile-and-display-only-rename", func(t *testing.T) {
		first := publish("deepseek-flash", "DEEPSEEK_V4_CHAT_V1")
		before := public("deepseek")
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := lockCatalogPublication(ctx, tx); err != nil {
			t.Fatal(err)
		}
		_, err = saveCatalogDraftEffect(ctx, tx, id, CatalogDraftInput{ExpectedCatalogVersion: version, DisplayName: "Display-only old client", ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 8192, MaximumOutputTokens: 1024, SupportedModalities: []string{"text"}, TokenizerVersion: "controlled-tokenizer"}, actor, time.Now().UTC())
		_ = tx.Rollback(context.Background())
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("omitted profile cleared an already configured protocol: %v", err)
		}
		var drafts int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM model_catalog_drafts WHERE model_id=$1`, id).Scan(&drafts); err != nil || drafts != 0 {
			t.Fatalf("rejected omission changed draft facts: count=%d err=%v", drafts, err)
		}
		renamed := publish("Renamed for people", "DEEPSEEK_V4_CHAT_V1")
		after := public("renamed")
		if first.ModelCapabilityVersionID != renamed.ModelCapabilityVersionID || before.RuntimeProtocol != "DEEPSEEK_V4_CHAT_V1" || after.RuntimeProtocol != before.RuntimeProtocol || after.ModelID != id {
			t.Fatal("display rename changed stable protocol capability")
		}
		plain := publish("deepseek-flash", "OPENAI_CHAT_V1")
		if plain.ModelCapabilityVersionID == first.ModelCapabilityVersionID || public("plain-with-deepseek-display").RuntimeProtocol != "OPENAI_CHAT_V1" {
			t.Fatal("explicit semantic change must create another immutable capability")
		}
		var oldProfile string
		if err := r.pool.QueryRow(ctx, `SELECT runtime_protocol FROM model_capability_versions WHERE id=$1`, first.ModelCapabilityVersionID).Scan(&oldProfile); err != nil || oldProfile != "DEEPSEEK_V4_CHAT_V1" {
			t.Fatalf("historical protocol changed: %q %v", oldProfile, err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE model_capability_versions SET runtime_protocol='OPENAI_CHAT_V1' WHERE id=$1`, first.ModelCapabilityVersionID); err == nil {
			t.Fatal("published capability mutation was permitted")
		}
	})
	t.Run("draft-parser-and-precheck-bind-profile", func(t *testing.T) {
		base := CatalogDraft{ModelID: id, TokenizerVersion: "same", RuntimeProtocol: "OPENAI_CHAT_V1"}
		changed := base
		changed.RuntimeProtocol = "DEEPSEEK_V4_CHAT_V1"
		if catalogDraftFingerprint(base) == catalogDraftFingerprint(changed) {
			t.Fatal("approval fingerprint omitted runtime protocol")
		}
		for _, invalid := range []string{"deepseek-flash", "UNKNOWN", "openai_chat_v1"} {
			input := CatalogDraftInput{DisplayName: "Name", ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 8192, MaximumOutputTokens: 1024, SupportedModalities: []string{"text"}, TokenizerVersion: "t", RuntimeProtocol: invalid}
			if validateCatalogDraft(&input) == nil {
				t.Fatalf("invalid protocol accepted: %q", invalid)
			}
		}
	})
	t.Run("rollback-refuses-to-erase-published-facts", func(t *testing.T) {
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "token_migrator"
		migration, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer migration.Close(context.Background())
		if _, err = migration.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
			t.Fatal(err)
		}
		down, err := os.ReadFile("../../migrations/token/000035_model_runtime_protocol.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = migration.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "rollback is forward-only") {
			t.Fatalf("unsafe rollback not refused: %v", err)
		}
		if public("after-refused-rollback").RuntimeProtocol != "OPENAI_CHAT_V1" {
			t.Fatal("refused rollback changed protocol")
		}
	})
}
