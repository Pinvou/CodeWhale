package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalCommandPrecheckFreezesFullFinancialSnapshotAndDetectsDrift(t *testing.T) {
	modelID, formulaID, priceID := uuid.New(), uuid.New(), uuid.New()
	catalogVersion, priceVersion := int64(7), int64(1)
	fingerprint := strings.Repeat("a", 64)
	evidence := ImpactLocalEvidence{
		ResourceType: "MODEL", ResourceID: modelID.String(), CurrentStatus: "PUBLISHED", CurrentVersion: ImpactResourceVersion{CatalogVersion: &catalogVersion},
		PriceDimensions: []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}, ObservedAt: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		RouteSetFingerprint: fingerprint, ProvisioningSetFingerprint: fingerprint, InflightSetFingerprint: fingerprint,
	}
	pricing := func(revision int64, setFingerprint string) ImpactPriceCoverageResult {
		effective := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		return ImpactPriceCoverageResult{Source: "BILLING", AsOf: time.Date(2026, 9, 4, 0, 0, 1, 0, time.UTC), FinancialFactRevision: revision, PriceSetFingerprint: setFingerprint, Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effective}}}
	}
	request := ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), NormalizedCommand: json.RawMessage(`{"model_id":"` + modelID.String() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null}`)}
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, stubImpactPricing{result: pricing(11, strings.Repeat("b", 64))})
	first, err := service.approvalCommandPrecheck(context.Background(), request)
	if err != nil || !strings.Contains(string(first.CriticalResourceVersions), `"billing_price_version":11`) || !strings.Contains(string(first.FinancialSnapshot), `"price_set_fingerprint":"`+strings.Repeat("b", 64)+`"`) {
		t.Fatalf("first projection=%+v err=%v", first, err)
	}
	service.impactPricing = stubImpactPricing{result: pricing(12, strings.Repeat("c", 64))}
	second, err := service.approvalCommandPrecheck(context.Background(), request)
	if err != nil || string(first.FinancialSnapshot) == string(second.FinancialSnapshot) || first.PrecheckFingerprint == second.PrecheckFingerprint {
		t.Fatalf("financial drift was not detected first=%+v second=%+v err=%v", first, second, err)
	}
	if !strings.Contains(string(second.CriticalResourceVersions), `"billing_price_version":12`) {
		t.Fatalf("critical revision did not follow authoritative Billing revision: %s", second.CriticalResourceVersions)
	}
	var critical map[string]any
	var financial map[string]any
	if json.Unmarshal(second.CriticalResourceVersions, &critical) != nil || len(critical) != 13 || json.Unmarshal(second.FinancialSnapshot, &financial) != nil || len(financial) != 6 {
		t.Fatalf("approval projection is not the closed 13+6 field contract: critical=%s financial=%s", second.CriticalResourceVersions, second.FinancialSnapshot)
	}
}

func TestCredentialRevokeApprovalSnapshotUsesTokenOnlyClosedFacts(t *testing.T) {
	accountID, credentialID := uuid.New(), uuid.New()
	accountVersion, credentialVersion := int64(7), 3
	fingerprint := strings.Repeat("a", 64)
	evidence := ImpactLocalEvidence{
		ResourceType: "PROVIDER_CREDENTIAL", ResourceID: credentialID.String(), CurrentStatus: "ACTIVE",
		CurrentVersion: ImpactResourceVersion{AccountVersion: &accountVersion, CredentialVersion: &credentialVersion},
		ObservedAt:     time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), CredentialFingerprint: fingerprint,
		RouteSetFingerprint: fingerprint, ProvisioningSetFingerprint: fingerprint, InflightSetFingerprint: fingerprint,
	}
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil)
	request := ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":7,"credential_id":"` + credentialID.String() + `","credential_version":3}`)}
	projection, err := service.approvalCommandPrecheck(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var critical map[string]any
	var financial map[string]any
	if json.Unmarshal(projection.CriticalResourceVersions, &critical) != nil || len(critical) != 11 || json.Unmarshal(projection.FinancialSnapshot, &financial) != nil || len(financial) != 0 {
		t.Fatalf("credential projection is not closed 11+0: critical=%s financial=%s", projection.CriticalResourceVersions, projection.FinancialSnapshot)
	}
	if projection.BillingObservedAt != nil || critical["token_account_id"] != accountID.String() || critical["credential_id"] != credentialID.String() {
		t.Fatalf("credential projection leaked Billing or mismatched target: %+v", projection)
	}
	for name, body := range map[string]string{
		"account drift":    `{"token_account_id":"` + accountID.String() + `","expected_account_version":8,"credential_id":"` + credentialID.String() + `","credential_version":3}`,
		"credential drift": `{"token_account_id":"` + accountID.String() + `","expected_account_version":7,"credential_id":"` + credentialID.String() + `","credential_version":4}`,
		"extra":            `{"token_account_id":"` + accountID.String() + `","expected_account_version":7,"credential_id":"` + credentialID.String() + `","credential_version":3,"secret":"forbidden"}`,
	} {
		if _, err := service.approvalCommandPrecheck(context.Background(), ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: json.RawMessage(body)}); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestCommandPrecheckRequiresExactCatalogSyncAndModelPublishTargets(t *testing.T) {
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0)
	modelID := uuid.NewString()
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "catalog exact reaches authentication", body: `{"command_type":"TOKEN_MODEL_CATALOG_SYNC","target_resource_type":"MODEL_CATALOG","target_resource_id":"CURRENT","command":{}}`, want: http.StatusUnauthorized},
		{name: "catalog lowercase command", body: `{"command_type":"token_model_catalog_sync","target_resource_type":"MODEL_CATALOG","target_resource_id":"CURRENT","command":{}}`, want: http.StatusBadRequest},
		{name: "catalog padded target", body: `{"command_type":"TOKEN_MODEL_CATALOG_SYNC","target_resource_type":"MODEL_CATALOG","target_resource_id":" CURRENT ","command":{}}`, want: http.StatusBadRequest},
		{name: "catalog arbitrary target", body: `{"command_type":"TOKEN_MODEL_CATALOG_SYNC","target_resource_type":"MODEL_CATALOG","target_resource_id":"LATEST","command":{}}`, want: http.StatusBadRequest},
		{name: "catalog extra command field", body: `{"command_type":"TOKEN_MODEL_CATALOG_SYNC","target_resource_type":"MODEL_CATALOG","target_resource_id":"CURRENT","command":{"force":true}}`, want: http.StatusBadRequest},
		{name: "model exact reaches authentication", body: `{"command_type":"TOKEN_MODEL_PUBLISH","target_resource_type":"MODEL","target_resource_id":"` + modelID + `","command":{"model_id":"` + modelID + `"}}`, want: http.StatusUnauthorized},
		{name: "model mismatched target", body: `{"command_type":"TOKEN_MODEL_PUBLISH","target_resource_type":"MODEL","target_resource_id":"` + uuid.NewString() + `","command":{"model_id":"` + modelID + `"}}`, want: http.StatusBadRequest},
		{name: "model extra field", body: `{"command_type":"TOKEN_MODEL_PUBLISH","target_resource_type":"MODEL","target_resource_id":"` + modelID + `","command":{"model_id":"` + modelID + `","unexpected":1}}`, want: http.StatusBadRequest},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands:precheck", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			service.adminTokenCommandPrecheck(response, request)
			if response.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}

func TestCatalogRollbackApprovalSnapshotRequiresTargetToMatchSourceVersion(t *testing.T) {
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, stubImpactRepository{}, stubImpactPricing{})
	for _, target := range []string{"CURRENT", "08", "9"} {
		_, err := service.approvalCommandPrecheck(context.Background(), ApprovalSnapshotRequest{
			CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", TargetResourceType: "MODEL_CATALOG", TargetResourceID: target,
			NormalizedCommand: json.RawMessage(`{"source_catalog_version":8}`),
		})
		if err == nil {
			t.Fatalf("accepted rollback target %q that does not canonically identify source version 8", target)
		}
	}
}

func TestApprovalCommandPrecheckRejectsExtraOrMismatchedFields(t *testing.T) {
	modelID := uuid.New()
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, stubImpactRepository{}, stubImpactPricing{})
	for name, command := range map[string]string{
		"extra":            `{"model_id":"` + modelID.String() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null,"extra":1}`,
		"mismatched model": `{"model_id":"` + uuid.NewString() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null}`,
	} {
		_, err := service.approvalCommandPrecheck(context.Background(), ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), NormalizedCommand: json.RawMessage(command)})
		if err == nil {
			t.Fatalf("accepted %s command", name)
		}
	}
}
