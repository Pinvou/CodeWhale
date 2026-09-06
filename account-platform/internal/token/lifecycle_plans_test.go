package token

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeLifecyclePlanInputUsesAbsoluteInstantsAndFailsClosed(t *testing.T) {
	modelID := uuid.New()
	effective, _ := time.Parse(time.RFC3339, "2026-11-01T01:30:00-07:00")
	cutoff, _ := time.Parse(time.RFC3339, "2026-11-01T02:30:00-08:00")
	input := LifecyclePlanInput{ExpectedCatalogVersion: 3, TargetLifecycle: " deprecated ", EffectiveAt: effective, ExistingSessionCutoffAt: &cutoff, Reason: " reviewed absolute lifecycle instant "}
	normalized, err := normalizeLifecyclePlanInput(modelID, input)
	if err != nil || normalized.TargetLifecycle != "DEPRECATED" || normalized.EffectiveAt.Location() != time.UTC || normalized.ExistingSessionCutoffAt == nil || normalized.ExistingSessionCutoffAt.Location() != time.UTC || normalized.Reason != "reviewed absolute lifecycle instant" {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
	invalid := []LifecyclePlanInput{
		{ExpectedCatalogVersion: 3, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, ForceCancelInflight: true, Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "REMOVED", EffectiveAt: effective, Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "PUBLISHED", EffectiveAt: effective, ExistingSessionCutoffAt: &cutoff, Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "PUBLISHED", EffectiveAt: effective, Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, ReplacementModelID: func() *uuid.UUID { value := uuid.New(); return &value }(), Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "DEPRECATED", EffectiveAt: cutoff, ExistingSessionCutoffAt: &effective, Reason: input.Reason},
		{ExpectedCatalogVersion: 3, TargetLifecycle: "REMOVED", EffectiveAt: effective, ExistingSessionCutoffAt: &cutoff, ReplacementModelID: func() *uuid.UUID { value := uuid.New(); return &value }(), Reason: input.Reason},
	}
	for index, candidate := range invalid {
		if _, err := normalizeLifecyclePlanInput(modelID, candidate); err == nil {
			t.Fatalf("invalid case %d accepted", index)
		}
	}
}

func TestLifecyclePlanEnvelopeIsClosedAndVersionBound(t *testing.T) {
	modelID, planID := uuid.New(), uuid.New()
	effective := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	command := lifecyclePlanCommand{ModelID: modelID, LifecyclePlanID: planID, LifecyclePlanVersion: 2, TargetLifecycle: "DEPRECATED", EffectiveAt: effective}
	expected := lifecyclePlanCritical{ModelID: modelID, CatalogVersion: 4, CurrentLifecycle: "PUBLISHED", TargetLifecycle: "DEPRECATED", LifecyclePlanVersion: 2, EffectiveAt: effective, BillingPriceVersion: 5, RouteSetFingerprint: repeatFingerprint("1"), ProvisioningSetFingerprint: repeatFingerprint("2"), InflightRequestSetFingerprint: repeatFingerprint("3"), PolicyVersion: 2}
	envelope := TokenCommandEnvelope{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: mustMarshalJSON(t, command), ExpectedVersions: mustMarshalJSON(t, expected)}
	if _, _, err := parseLifecyclePlanEnvelope(envelope); err != nil {
		t.Fatal(err)
	}
	extra := map[string]any{}
	if json.Unmarshal(envelope.Command, &extra) != nil {
		t.Fatal("command did not decode")
	}
	extra["ignore_blocker"] = true
	envelope.Command = mustMarshalJSON(t, extra)
	if _, _, err := parseLifecyclePlanEnvelope(envelope); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("extra command field error=%v", err)
	}
	if worker := NewLifecyclePlanWorker(nil, nil, "worker"); func() error { _, err := worker.RunOnce(context.Background()); return err }() != ErrTokenCommandInvalid {
		t.Fatal("incomplete worker did not fail closed")
	}
}

func TestModelPublishCommandDiscriminatorIsClosed(t *testing.T) {
	modelID, planID := uuid.New(), uuid.New()
	effective := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	legacy, _ := json.Marshal(map[string]any{"model_id": modelID})
	parsed, err := parseModelPublishCommand(legacy)
	if err != nil || parsed.PublishMode != "IMMEDIATE" {
		t.Fatalf("legacy immediate=%+v err=%v", parsed, err)
	}
	scheduled, _ := json.Marshal(map[string]any{"model_id": modelID, "publish_mode": "SCHEDULED", "lifecycle_plan_id": planID, "lifecycle_plan_version": 2, "effective_at": effective})
	parsed, err = parseModelPublishCommand(scheduled)
	if err != nil || parsed.PublishMode != "SCHEDULED" || parsed.LifecyclePlanID == nil || *parsed.LifecyclePlanID != planID || parsed.EffectiveAt == nil || !parsed.EffectiveAt.Equal(effective) {
		t.Fatalf("scheduled=%+v err=%v", parsed, err)
	}
	invalid := []map[string]any{
		{"model_id": modelID, "publish_mode": "SCHEDULED", "lifecycle_plan_id": planID, "lifecycle_plan_version": 2},
		{"model_id": modelID, "publish_mode": "IMMEDIATE", "effective_at": effective},
		{"model_id": modelID, "publish_mode": "scheduled", "lifecycle_plan_id": planID, "lifecycle_plan_version": 2, "effective_at": effective},
		{"model_id": modelID, "publish_mode": "SCHEDULED", "lifecycle_plan_id": planID, "lifecycle_plan_version": 2, "effective_at": effective, "unexpected": true},
	}
	for index, value := range invalid {
		raw, _ := json.Marshal(value)
		if _, err := parseModelPublishCommand(raw); !errors.Is(err, ErrTokenCommandInvalid) {
			t.Fatalf("invalid publish command %d accepted: %v", index, err)
		}
	}
}

func TestScheduledModelPublishExpectedVersionsRequireGovernanceProofs(t *testing.T) {
	modelID := uuid.New()
	expected := scheduledModelPublishExpected{
		ModelID: modelID, ExpectedCatalogVersion: 4, ExpectedDraftVersion: 2,
		DraftFingerprint: repeatFingerprint("1"), ProtocolDefinitionID: uuid.New(), ProtocolVersion: 1,
		StableRouteCount: 1, StableRouteSetFingerprint: repeatFingerprint("2"), ProvisioningCount: 1,
		ReadyRouteCount: 1, ProvisioningSetFingerprint: repeatFingerprint("3"), FinancialFactRevision: 7,
		PriceSetFingerprint: repeatFingerprint("4"), PrecheckFingerprint: repeatFingerprint("5"), RequestFingerprint: repeatFingerprint("6"),
	}
	if _, err := parseModelPublishExpected(mustMarshalJSON(t, expected), modelID); err != nil {
		t.Fatal(err)
	}
	missingRequest := map[string]any{}
	if err := json.Unmarshal(mustMarshalJSON(t, expected), &missingRequest); err != nil {
		t.Fatal(err)
	}
	delete(missingRequest, "request_fingerprint")
	if _, err := parseModelPublishExpected(mustMarshalJSON(t, missingRequest), modelID); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("missing request fingerprint error=%v", err)
	}
	extra := map[string]any{}
	if err := json.Unmarshal(mustMarshalJSON(t, expected), &extra); err != nil {
		t.Fatal(err)
	}
	extra["unexpected"] = true
	if _, err := parseModelPublishExpected(mustMarshalJSON(t, extra), modelID); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("extra expected version error=%v", err)
	}
	authoritative := map[string]any{}
	if err := json.Unmarshal(mustMarshalJSON(t, expected), &authoritative); err != nil {
		t.Fatal(err)
	}
	delete(authoritative, "precheck_fingerprint")
	delete(authoritative, "request_fingerprint")
	if !tokenCommandExpectedVersionsMatch(mustMarshalJSON(t, expected), mustMarshalJSON(t, authoritative)) {
		t.Fatal("governed proof fields changed the authoritative domain comparison")
	}
	authoritative["expected_draft_version"] = float64(expected.ExpectedDraftVersion + 1)
	if tokenCommandExpectedVersionsMatch(mustMarshalJSON(t, expected), mustMarshalJSON(t, authoritative)) {
		t.Fatal("drifted domain evidence was accepted")
	}
}

func TestModelPublishLegacyCanonicalizesToExplicitImmediateAndBindsRequest(t *testing.T) {
	modelID := uuid.New()
	expected := scheduledModelPublishExpected{
		ModelID: modelID, ExpectedCatalogVersion: 4, ExpectedDraftVersion: 2,
		DraftFingerprint: repeatFingerprint("1"), ProtocolDefinitionID: uuid.New(), ProtocolVersion: 1,
		StableRouteCount: 1, StableRouteSetFingerprint: repeatFingerprint("2"), ProvisioningCount: 1,
		ReadyRouteCount: 1, ProvisioningSetFingerprint: repeatFingerprint("3"), FinancialFactRevision: 7,
		PriceSetFingerprint: repeatFingerprint("4"), PrecheckFingerprint: repeatFingerprint("5"),
	}
	expectedMap := map[string]any{}
	if err := json.Unmarshal(mustMarshalJSON(t, expected), &expectedMap); err != nil {
		t.Fatal(err)
	}
	explicit := bindModelPublishRequestFingerprint(t, TokenCommandEnvelope{
		CommandType: "TOKEN_MODEL_PUBLISH", Reason: "publish reviewed immediate draft", TargetResourceType: "MODEL", TargetResourceID: modelID.String(),
		PrecheckFingerprint: expected.PrecheckFingerprint, Command: mustMarshalJSON(t, modelPublishCommand{ModelID: modelID, PublishMode: "IMMEDIATE"}),
	}, expectedMap)
	legacy := explicit
	legacy.Command = mustMarshalJSON(t, map[string]any{"model_id": modelID})
	canonicalExplicit, explicitDigest, _, explicitFingerprint, err := normalizeTokenCommand(explicit, "model-publish-immediate-key", "")
	if err != nil {
		t.Fatal(err)
	}
	canonicalLegacy, legacyDigest, _, legacyFingerprint, err := normalizeTokenCommand(legacy, "model-publish-immediate-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalLegacy.Command) != string(canonicalExplicit.Command) || legacyDigest != explicitDigest || legacyFingerprint != explicitFingerprint {
		t.Fatalf("legacy=%s/%s/%s explicit=%s/%s/%s", canonicalLegacy.Command, legacyDigest, legacyFingerprint, canonicalExplicit.Command, explicitDigest, explicitFingerprint)
	}
	var tampered map[string]any
	if err := json.Unmarshal(explicit.ExpectedVersions, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["expected_draft_version"] = float64(3)
	legacy.ExpectedVersions = mustMarshalJSON(t, tampered)
	if _, _, _, _, err := normalizeTokenCommand(legacy, "model-publish-immediate-key", ""); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("tampered frozen facts error=%v", err)
	}
	uppercase := explicit
	if err := json.Unmarshal(explicit.ExpectedVersions, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["request_fingerprint"] = strings.ToUpper(tampered["request_fingerprint"].(string))
	uppercase.ExpectedVersions = mustMarshalJSON(t, tampered)
	if _, _, _, _, err := normalizeTokenCommand(uppercase, "model-publish-immediate-key", ""); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("uppercase request fingerprint error=%v", err)
	}
	reordered := explicit
	reordered.ExpectedVersions = json.RawMessage(`{"request_fingerprint":"` + expectedMap["request_fingerprint"].(string) + `","precheck_fingerprint":"` + expected.PrecheckFingerprint + `","price_set_fingerprint":"` + expected.PriceSetFingerprint + `","financial_fact_revision":7,"provisioning_set_fingerprint":"` + expected.ProvisioningSetFingerprint + `","ready_route_count":1,"provisioning_count":1,"stable_route_set_fingerprint":"` + expected.StableRouteSetFingerprint + `","stable_route_count":1,"protocol_version":1,"protocol_definition_id":"` + expected.ProtocolDefinitionID.String() + `","draft_fingerprint":"` + expected.DraftFingerprint + `","expected_draft_version":2,"expected_catalog_version":4,"model_id":"` + modelID.String() + `"}`)
	if _, _, _, _, err := normalizeTokenCommand(reordered, "model-publish-reordered", ""); err != nil {
		t.Fatalf("object field order changed canonical request: %v", err)
	}
	planID, planVersion := uuid.New(), int64(3)
	effectiveAt := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	scheduled := explicit
	scheduled.Command = mustMarshalJSON(t, modelPublishCommand{ModelID: modelID, PublishMode: "SCHEDULED", LifecyclePlanID: &planID, LifecyclePlanVersion: &planVersion, EffectiveAt: &effectiveAt})
	if err := json.Unmarshal(explicit.ExpectedVersions, &tampered); err != nil {
		t.Fatal(err)
	}
	scheduled = bindModelPublishRequestFingerprint(t, scheduled, tampered)
	var changedCommand map[string]any
	if err := json.Unmarshal(scheduled.Command, &changedCommand); err != nil {
		t.Fatal(err)
	}
	changedCommand["effective_at"] = effectiveAt.Add(time.Minute).Format(time.RFC3339Nano)
	scheduled.Command = mustMarshalJSON(t, changedCommand)
	if _, _, _, _, err := normalizeTokenCommand(scheduled, "model-publish-scheduled-key", ""); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("tampered scheduled command time error=%v", err)
	}
}

func TestModelPublishRequestFingerprintMatchesPortalFixture(t *testing.T) {
	modelID := uuid.MustParse("018f4f72-9b3a-7abc-8def-1234567890ab")
	expected := scheduledModelPublishExpected{
		ModelID: modelID, ExpectedCatalogVersion: 4, ExpectedDraftVersion: 2,
		DraftFingerprint: repeatFingerprint("1"), ProtocolDefinitionID: uuid.MustParse("018f4f72-9b3a-7abc-8def-1234567890ac"), ProtocolVersion: 1,
		StableRouteCount: 1, StableRouteSetFingerprint: repeatFingerprint("2"), ProvisioningCount: 1,
		ReadyRouteCount: 1, ProvisioningSetFingerprint: repeatFingerprint("3"), FinancialFactRevision: 7,
		PriceSetFingerprint: repeatFingerprint("4"), PrecheckFingerprint: repeatFingerprint("5"),
	}
	var facts map[string]any
	if err := json.Unmarshal(mustMarshalJSON(t, expected), &facts); err != nil {
		t.Fatal(err)
	}
	envelope := TokenCommandEnvelope{
		CommandType: "TOKEN_MODEL_PUBLISH", Reason: "publish reviewed immediate draft", TargetResourceType: "MODEL", TargetResourceID: modelID.String(),
		PrecheckFingerprint: expected.PrecheckFingerprint, Command: mustMarshalJSON(t, modelPublishCommand{ModelID: modelID, PublishMode: "IMMEDIATE"}),
	}
	const portalFingerprint = "2f4f93843cf8f35a065298fb6239977ccc2b8e9dacaac88c3a434ac0225f551b"
	if got := modelPublishRequestFingerprint(envelope, facts); got != portalFingerprint {
		t.Fatalf("Go fingerprint=%s Portal fixture=%s", got, portalFingerprint)
	}
	facts["request_fingerprint"] = portalFingerprint
	envelope.ExpectedVersions = mustMarshalJSON(t, facts)
	if _, _, _, _, err := normalizeTokenCommand(envelope, "portal-fingerprint-fixture", ""); err != nil {
		t.Fatalf("Portal fixture rejected: %v", err)
	}
	planID := uuid.MustParse("018f4f72-9b3a-7abc-8def-1234567890ad")
	effectiveAt := time.Date(2026, 9, 5, 12, 30, 0, int(500*time.Millisecond), time.UTC)
	scheduled := envelope
	scheduled.Reason = "schedule reviewed model publication"
	scheduled.Command = mustMarshalJSON(t, modelPublishCommand{ModelID: modelID, PublishMode: "SCHEDULED", LifecyclePlanID: &planID, LifecyclePlanVersion: func() *int64 { value := int64(3); return &value }(), EffectiveAt: &effectiveAt})
	canonicalCommand, err := parseModelPublishCommand(scheduled.Command)
	if err != nil {
		t.Fatal(err)
	}
	scheduled.Command = mustMarshalJSON(t, canonicalCommand)
	const scheduledPortalFingerprint = "859a6a99a84e58655a4fc89e139ee01eac31835f13e7783d5980150afdfd86e1"
	delete(facts, "request_fingerprint")
	if got := modelPublishRequestFingerprint(scheduled, facts); got != scheduledPortalFingerprint {
		t.Fatalf("scheduled Go fingerprint=%s Portal fixture=%s command=%s", got, scheduledPortalFingerprint, scheduled.Command)
	}
	facts["request_fingerprint"] = scheduledPortalFingerprint
	scheduled.ExpectedVersions = mustMarshalJSON(t, facts)
	canonical, _, _, _, err := normalizeTokenCommand(scheduled, "scheduled-portal-fingerprint-fixture", "")
	if err != nil || strings.Contains(string(canonical.Command), ".000Z") || !strings.Contains(string(canonical.Command), `"effective_at":"2026-09-05T12:30:00Z"`) {
		t.Fatalf("scheduled Portal fixture canonical=%s err=%v", canonical.Command, err)
	}
}

func TestDeterministicLifecyclePrecheckError(t *testing.T) {
	for _, err := range []error{ErrNotFound, ErrCatalogVersionConflict, ErrProtocolFormulaPrecheck, ErrTokenCommandInvalid} {
		if !deterministicLifecyclePrecheckError(err) {
			t.Fatalf("deterministic error %v was treated as retryable", err)
		}
	}
	if deterministicLifecyclePrecheckError(context.DeadlineExceeded) || deterministicLifecyclePrecheckError(errors.New("billing unavailable")) {
		t.Fatal("transient precheck failure was terminalized")
	}
}

func repeatFingerprint(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}

func bindModelPublishRequestFingerprint(t *testing.T, envelope TokenCommandEnvelope, expected map[string]any) TokenCommandEnvelope {
	t.Helper()
	var err error
	envelope, err = canonicalizeModelAccountEnvelope(envelope)
	if err != nil {
		t.Fatalf("canonical publication fixture: %v", err)
	}
	expected["precheck_fingerprint"] = envelope.PrecheckFingerprint
	expected["request_fingerprint"] = ""
	envelope.ExpectedVersions = mustMarshalJSON(t, expected)
	expected["request_fingerprint"] = modelPublishRequestFingerprint(envelope, expected)
	envelope.ExpectedVersions = mustMarshalJSON(t, expected)
	return envelope
}
