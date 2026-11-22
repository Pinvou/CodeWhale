package token

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestForceCancelLifecycleIntentIsPlanAndVersionBound(t *testing.T) {
	model, replacement := uuid.New(), uuid.New()
	effective := time.Now().UTC().Add(10 * time.Minute)
	input := LifecyclePlanInput{ExpectedCatalogVersion: 1, TargetLifecycle: "REMOVED", EffectiveAt: effective, ExistingSessionCutoffAt: &effective, ReplacementModelID: &replacement, ForceCancelInflight: true, Reason: "reviewed removal with cancellation"}
	if _, err := normalizeLifecyclePlanInput(model, input); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"PUBLISHED", "DEPRECATED"} {
		bad := input
		bad.TargetLifecycle = target
		if _, err := normalizeLifecyclePlanInput(model, bad); err == nil {
			t.Fatal("force accepted outside REMOVED")
		}
	}
	plan := LifecyclePlan{ID: uuid.New(), ModelID: model, PlanVersion: 1, TargetLifecycle: "REMOVED", EffectiveAt: effective, ForceCancelInflight: true}
	if lifecyclePlanFlagMatches(plan, nil) {
		t.Fatal("v2 false intent matched a true plan")
	}
	command := lifecyclePlanCommandFromPlan(plan)
	if command.ForceCancelInflight == nil || !*command.ForceCancelInflight {
		t.Fatal("new plan intent lost")
	}
	flagFalse, flagTrue := false, true
	for _, tc := range []struct {
		command, critical *bool
		version           int64
		target            string
		want              bool
	}{
		{nil, nil, 2, "REMOVED", true}, {&flagTrue, &flagTrue, 3, "REMOVED", true}, {&flagFalse, &flagFalse, 3, "DEPRECATED", true},
		{nil, nil, 3, "REMOVED", false}, {&flagTrue, &flagTrue, 2, "REMOVED", false}, {&flagTrue, &flagFalse, 3, "REMOVED", false}, {&flagTrue, &flagTrue, 3, "DEPRECATED", false},
	} {
		if got := lifecycleProtocolFlagsMatch(tc.command, tc.critical, tc.version, tc.target); got != tc.want {
			t.Fatal("protocol flag mismatch")
		}
	}
	plan.ForceCancelInflight = false
	plan.CriticalResourceVersions = json.RawMessage(`{"policy_version":2}`)
	old := lifecyclePlanCommandFromPlan(plan)
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	if _, present := object["force_cancel_inflight"]; present || !lifecyclePlanFlagMatches(plan, nil) {
		t.Fatal("historical false command bytes acquired a flag")
	}
}
