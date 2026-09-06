package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Artifacts must be built externally from the candidate being verified. This
// test never builds code, embeds host paths, or substitutes an injected runner.
type releaseCLIArtifacts struct {
	directory string
	node      string
	wrapper   string
}

func newReleaseCLIArtifacts(t *testing.T) *releaseCLIArtifacts {
	t.Helper()
	directory := os.Getenv("PINVOU_TEST_RELEASE_CLI_DIRECTORY")
	if directory == "" {
		t.Skip("PINVOU_TEST_RELEASE_CLI_DIRECTORY with three original CLI artifacts is required")
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("CLI artifact directory must be absolute")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for the actual operations wrapper")
	}
	wrapper, err := filepath.Abs("../../../scripts/account-platform-model-token-operations.mjs")
	if err != nil {
		t.Fatal(err)
	}
	return &releaseCLIArtifacts{directory: directory, node: node, wrapper: wrapper}
}

func releaseTestDSN(config *pgx.ConnConfig, database, user string) string {
	address := &url.URL{Scheme: "postgresql", User: url.User(user), Host: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), Path: "/" + database}
	address.RawQuery = "sslmode=disable"
	return address.String() // Dedicated loopback trust fixture, not HBA/Secret proof.
}

func releaseTestHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func (a *releaseCLIArtifacts) invoke(t *testing.T, ctx context.Context, operation, database string, accountDatabase any, options map[string]any, environment map[string]string, execute bool, wantError string) map[string]any {
	t.Helper()
	name := "token-governance"
	if strings.HasPrefix(operation, "cap-") {
		name = "token-policy-bootstrap"
	} else if operation == "migrate-up" {
		name = "migrate"
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(a.directory, name)
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal("required original CLI artifact unavailable")
	}
	request := map[string]any{"schema_version": 1, "operation": operation, "approval_reference": "isolated-test-approval", "executable": executable, "executable_sha256": releaseTestHash(contents), "expected_database": database, "expected_account_database": accountDatabase, "options": options}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestFile := filepath.Join(t.TempDir(), "operation.json")
	if err = os.WriteFile(requestFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{a.wrapper, "--request", requestFile}
	if execute {
		args = append(args, "--execute")
	}
	command := exec.CommandContext(ctx, a.node, args...)
	// Never inherit a developer's database/KMS/identity environment.
	for _, key := range []string{"SystemRoot", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	var output, stderr bytes.Buffer
	command.Stdout, command.Stderr = &output, &stderr
	err = command.Run()
	if wantError != "" {
		if err == nil || strings.TrimSpace(stderr.String()) != wantError || output.Len() != 0 {
			t.Fatalf("%s did not return the expected fixed wrapper failure", operation)
		}
		return nil
	}
	if err != nil {
		// Only the wrapper's closed code can be shown; never raw child output.
		code := strings.TrimSpace(stderr.String())
		if !regexp.MustCompile(`^MODEL_TOKEN_OPERATION_[A-Z_]+$`).MatchString(code) {
			code = "UNSAFE_OR_MISSING_DIAGNOSTIC"
		}
		t.Fatalf("actual %s wrapper failed: %s", operation, code)
	}
	var evidence map[string]any
	if stderr.Len() != 0 || json.Unmarshal(output.Bytes(), &evidence) != nil {
		t.Fatal("wrapper evidence was not a safe JSON object")
	}
	decision := "PLAN_VALID"
	if execute {
		decision = "OPERATION_COMPLETED"
		if value, ok := evidence["output_sha256"].(string); !ok || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(value) {
			t.Fatal("real child output hash missing")
		}
	}
	if evidence["decision"] != decision || evidence["environment_status"] != "ENV_PENDING" || evidence["deployment_status"] != "NOT_DEPLOYED" {
		t.Fatal("wrapper exaggerated environment or deployment completion")
	}
	return evidence
}

func (a *releaseCLIArtifacts) migrate(t *testing.T, ctx context.Context, database, dsn string) {
	t.Helper()
	directory, err := filepath.Abs("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		if regexp.MustCompile(`^\d+_[a-z0-9_]+\.(up|down)\.sql$`).MatchString(file.Name()) {
			names = append(names, file.Name())
		}
	}
	sort.Strings(names)
	entries := make([][2]string, 0, len(names))
	for _, name := range names {
		contents, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, [2]string{name, releaseTestHash(contents)})
	}
	manifest, _ := json.Marshal(entries)
	// This is a disposable-fixture input record, explicitly NOT a restorable
	// production backup or proof that the environment's restore gate passed.
	options := map[string]any{"domain": "token", "directory": directory, "manifest_sha256": releaseTestHash(manifest), "backup_reference": "isolated-fixture-input-record", "backup_completed_at": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "backup_sha256": releaseTestHash([]byte("disposable-fixture-not-production-backup"))}
	a.invoke(t, ctx, "migrate-up", database, nil, options, map[string]string{"TOKEN_MIGRATOR_DATABASE_URL": dsn}, true, "")
}

func (a *releaseCLIArtifacts) cap(t *testing.T, ctx context.Context, control *pgx.Conn, database, operatorDSN, readDSN string) {
	t.Helper()
	facts := func() string {
		t.Helper()
		var result string
		if err := control.QueryRow(ctx, `SELECT jsonb_build_object('revisions',(SELECT jsonb_agg(r ORDER BY policy_version) FROM public.token_account_owner_cap_policy_revisions r),'active',(SELECT jsonb_agg(p ORDER BY singleton_key) FROM public.token_account_owner_cap_policy_active p),'outbox',(SELECT jsonb_agg(e ORDER BY event_id) FROM public.outbox_events e))::text`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := facts()
	a.invoke(t, ctx, "cap-readiness", database, nil, map[string]any{}, map[string]string{"TOKEN_POLICY_DATABASE_URL": readDSN}, true, "MODEL_TOKEN_OPERATION_CHECK_FAILED")
	options := map[string]any{"max_per_owner": 19, "expected_version": 0, "idempotency_key": "isolated-cap-bootstrap-key", "reason": "isolated wrapper integration"}
	a.invoke(t, ctx, "cap-bootstrap", database, nil, options, nil, false, "")
	if facts() != before {
		t.Fatal("readiness failure or default plan changed policy facts")
	}
	first := a.invoke(t, ctx, "cap-bootstrap", database, nil, options, map[string]string{"TOKEN_POLICY_DATABASE_URL": operatorDSN}, true, "")
	var exact bool
	if err := control.QueryRow(ctx, `SELECT (SELECT count(*)=1 AND bool_and(policy_version=1 AND max_per_owner=19 AND changed_by='token_policy_operator') FROM public.token_account_owner_cap_policy_revisions) AND (SELECT active_policy_version=1 AND pointer_version=1 FROM public.token_account_owner_cap_policy_active WHERE singleton_key) AND (SELECT count(*)=1 FROM public.outbox_events WHERE event_type='token.account-owner-cap-policy.changed.v1')`).Scan(&exact); err != nil || !exact {
		t.Fatal("actual operator bootstrap did not produce one exact revision/pointer/event")
	}
	after := facts()
	a.invoke(t, ctx, "cap-readiness", database, nil, map[string]any{}, map[string]string{"TOKEN_POLICY_DATABASE_URL": readDSN}, true, "")
	replay := a.invoke(t, ctx, "cap-bootstrap", database, nil, options, map[string]string{"TOKEN_POLICY_DATABASE_URL": operatorDSN}, true, "")
	if facts() != after || replay["output_sha256"] != first["output_sha256"] {
		t.Fatal("same-key actual CLI replay changed frozen result, policy or event")
	}
	options["max_per_owner"] = 20
	a.invoke(t, ctx, "cap-bootstrap", database, nil, options, map[string]string{"TOKEN_POLICY_DATABASE_URL": operatorDSN}, true, "MODEL_TOKEN_OPERATION_OUTCOME_UNCONFIRMED_QUERY_ORIGINAL")
	if facts() != after {
		t.Fatal("conflicting-key child failure changed policy facts")
	}
}

func (a *releaseCLIArtifacts) governance(t *testing.T, ctx context.Context, control *pgx.Conn, database, accountDatabase, tokenDSN, accountDSN string, requestID uuid.UUID) {
	t.Helper()
	facts := func() string {
		t.Helper()
		var result string
		if err := control.QueryRow(ctx, `SELECT jsonb_build_object('activation',(SELECT jsonb_agg(a) FROM public.token_command_governance_activation a),'transitions',(SELECT jsonb_agg(r ORDER BY epoch) FROM public.token_command_governance_transitions r))::text`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := facts()
	a.invoke(t, ctx, "governance-check", database, accountDatabase, map[string]any{}, map[string]string{"TOKEN_MIGRATOR_DATABASE_URL": tokenDSN, "ACCOUNT_POLICY_DATABASE_URL": accountDSN}, true, "")
	first := a.invoke(t, ctx, "governance-result", database, nil, map[string]any{"request_id": requestID.String()}, map[string]string{"TOKEN_MIGRATOR_DATABASE_URL": tokenDSN}, true, "")
	replay := a.invoke(t, ctx, "governance-result", database, nil, map[string]any{"request_id": requestID.String()}, map[string]string{"TOKEN_MIGRATOR_DATABASE_URL": tokenDSN}, true, "")
	if first["output_sha256"] != replay["output_sha256"] || facts() != before {
		t.Fatal("original request-id lookup returned changing frozen output")
	}
}
