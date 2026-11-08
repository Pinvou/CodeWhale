package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/jackc/pgx/v5"
)

// Execute the actual closed Node wrapper and the separately built real CLI.
// Dedicated fixture guards in the caller prohibit non-local/shared databases.
func runGovernanceExpansionWrapper(t *testing.T, ctx context.Context, tokenConfig, accountConfig *pgx.ConnConfig, request token.GovernanceTransitionRequest, apply bool) {
	t.Helper()
	directory := os.Getenv("PINVOU_TEST_RELEASE_CLI_DIRECTORY")
	if !filepath.IsAbs(directory) {
		t.Fatal("same-candidate governance CLI directory is required")
	}
	binary := filepath.Join(directory, "token-governance")
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal("governance CLI artifact unavailable")
	}
	sum := sha256.Sum256(body)
	operation := "governance-active-check"
	if apply {
		operation = "governance-activate"
	}
	input := map[string]any{"schema_version": 1, "operation": operation, "approval_reference": "managed-fixture-v3",
		"executable": binary, "executable_sha256": hex.EncodeToString(sum[:]), "expected_database": tokenConfig.Database,
		"expected_account_database": accountConfig.Database,
		"options": map[string]any{"expected_state": request.ExpectedState, "expected_epoch": request.ExpectedEpoch,
			"request_id": request.RequestID.String(), "operator": request.OperatorID, "reason": request.Reason,
			"runtime_roles": request.ApprovedRuntimeRoles, "policy_upgrade": request.PolicyUpgrade}}
	encoded, _ := json.Marshal(input)
	file := filepath.Join(t.TempDir(), "operation.json")
	if err := os.WriteFile(file, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../../scripts/account-platform-model-token-operations.mjs")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "node", script, "--request", file, "--execute")
	dsn := func(config *pgx.ConnConfig) string {
		u, err := url.Parse(os.Getenv("PINVOU_TEST_MANAGED_POSTGRES_URL"))
		if err != nil {
			t.Fatal("fixture connection cannot be serialized")
		}
		u.User, u.Path = url.User(config.User), "/"+config.Database
		return u.String()
	}
	command.Env = append(os.Environ(), "TOKEN_MIGRATOR_DATABASE_URL="+dsn(tokenConfig), "ACCOUNT_POLICY_DATABASE_URL="+dsn(accountConfig))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual governance wrapper/CLI failed: %s", output)
	}
	var decoded map[string]any
	if json.Unmarshal(output, &decoded) != nil || decoded["decision"] != "OPERATION_COMPLETED" || decoded["environment_status"] != "ENV_PENDING" || decoded["deployment_status"] != "NOT_DEPLOYED" {
		t.Fatal("wrapper receipt did not preserve its limited operation-completed semantics")
	}
}
