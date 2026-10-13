package f09fixture

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestF09PolicyIdentityPublicCoordinator(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual owned PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = newStageFixture(t, ctx, raw)
	dir := filepath.Join("..", "accountschema")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"test", "-race", "-json", "-count=1", "-mod=readonly", "-run", "^TestF09EntitlementPolicyMigrationIdentityPostgres$"}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			args = append(args, filepath.Join(dir, entry.Name()))
		}
	}
	args = append(args, filepath.Join(dir, "entitlement_policy_identity_integration_test.go"))
	child := exec.CommandContext(ctx, os.Getenv("PINVOU_PRIVATE_GO"), args...)
	child.Env = append(os.Environ(), "PINVOU_TEST_SCHEMA_DATABASE_URL="+raw)
	path := os.Getenv("PINVOU_F09_CHILD_STREAM")
	stream, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	child.Stdout = stream
	child.Stderr = stream
	runErr := child.Run()
	closeErr := stream.Close()
	if runErr != nil {
		t.Fatalf("finite policy identity child failed: %v", runErr)
	}
	if closeErr != nil {
		t.Fatal("close finite policy child stream")
	}
	stream, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	required := map[string]bool{}
	root := "TestF09EntitlementPolicyMigrationIdentityPostgres"
	for _, name := range []string{root, root + "/runtime-cannot-install-policy", root + "/renamed-original-runtime-cannot-install-policy", root + "/wrong-current-role-fails-closed", root + "/superuser-session-is-not-contract", root + "/qualified-original-public-up", root + "/unrelated-custom69-runtime-remains-generic"} {
		required[name] = false
	}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var e struct{ Action, Test string }
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			t.Fatal("invalid finite policy child event")
		}
		if e.Test == "" {
			continue
		}
		if e.Action == "fail" || e.Action == "skip" {
			t.Fatal("finite policy child failure or skipped case")
		}
		if e.Action == "pass" {
			if _, ok := required[e.Test]; ok {
				required[e.Test] = true
			}
		}
	}
	if scanner.Err() != nil {
		t.Fatal("read finite policy child stream")
	}
	for name, passed := range required {
		if !passed {
			t.Fatalf("finite policy child missing actual named PASS: %s", name)
		}
	}
}
