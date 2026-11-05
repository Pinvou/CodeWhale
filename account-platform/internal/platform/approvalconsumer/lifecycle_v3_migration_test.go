package approvalconsumer

import (
	"os"
	"strings"
	"testing"
)

func TestLifecycleV3MigrationPreservesOtherFrozenMappings(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile("../../../migrations/account/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	oldRoutes := read("000077_usage_settlement_repair_consumer_route.up.sql")
	upgrade := read("900013_model_lifecycle_force_cancel_policy.up.sql")
	section := func(raw, begin, end string) string {
		start := strings.Index(raw, begin)
		if start < 0 {
			t.Fatal("missing function start")
		}
		stop := strings.Index(raw[start:], end)
		if stop < 0 {
			t.Fatal("missing function end")
		}
		return raw[start : start+stop]
	}
	begin := "CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_consumer_mapping_allows("
	end := "\nRESET ROLE;\nSET ROLE account_service;"
	want := section(oldRoutes, begin, end)
	got := section(upgrade, begin, end)
	if strings.Count(got, "p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version IN (2,3)") != 2 {
		t.Fatal("v3 mapping and route are not both explicit")
	}
	got = strings.ReplaceAll(got, "p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version IN (2,3)", "p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version=2")
	got = strings.ReplaceAll(got, "\n        AND (p_policy_version<>3 OR p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1')", "")
	if got != want {
		t.Fatal("consumer migration changed a historical mapping, serialization rule or grant")
	}
	oldRecovery := read("900009_approval_recovery_query_billing_commands.up.sql")
	begin = "CREATE OR REPLACE FUNCTION public.claim_approval_recovery_query("
	want = oldRecovery[strings.Index(oldRecovery, begin):]
	got = upgrade[strings.Index(upgrade, begin):]
	tuple := "\n        ('TOKEN_MODEL_LIFECYCLE_CHANGE',3,'token-command-executor','token-model-lifecycle-executor-v1'),"
	if strings.Count(got, tuple) != 1 {
		t.Fatal("v3 query-only recovery tuple missing or duplicated")
	}
	if strings.Replace(got, tuple, "", 1) != want {
		t.Fatal("recovery migration changed an existing lease, identity, grant or tuple")
	}
}
