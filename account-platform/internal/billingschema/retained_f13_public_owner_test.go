package billingschema

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateF13OwnerIntegration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL not configured; no PG validation claimed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	admin, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal("isolated connect")
	}
	// The URL must explicitly select an isolated superuser session with no
	// existing deployment actors; original UUID/cleanup guards remain below.
	var actors int
	var isolatedOwner bool
	e = admin.QueryRow(ctx, `SELECT current_user=session_user AND (SELECT rolsuper FROM pg_roles WHERE rolname=session_user),
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user)`).Scan(&isolatedOwner, &actors)
	_ = admin.Close(ctx)
	if e != nil || !isolatedOwner || actors != 0 {
		t.Fatal("explicit isolated owner and exclusive actor guard")
	}
	f := newStageFixture(t, ctx, raw)
	_ = f
	t.Run("original-owner-contract-child", func(t *testing.T) {
		dir := "../billing"
		args := []string{"test", "-mod=readonly", "-race", "-count=1", "-json", ".", "-run", "^(TestRefundExceptionRepairPostgresIntegration|TestRefundExceptionFrozenExecutionPostgres|TestRefundSuccessConflictEvidencePostgres)$"}
		cmd := exec.CommandContext(ctx, os.Getenv("F13_GO_BINARY"), args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
		out, e := cmd.CombinedOutput()
		dest := os.Getenv("F13_CHILD_LOG")
		if dest == "" {
			t.Fatal("child log destination required")
		}
		if er := os.WriteFile(dest, out, 0600); er != nil {
			t.Fatal(er)
		}
		_ = filepath.Base(dest)
		if e != nil {
			t.Fatal("original owner child failed; retained log", e)
		}
		pass, failed, skipped := 0, 0, 0
		original := make(map[string]bool)
		for _, line := range strings.Split(string(out), "\n") {
			var a struct{ Action, Test string }
			if json.Unmarshal([]byte(line), &a) == nil && a.Action == "pass" && a.Test != "" {
				pass++
				original[a.Test] = true
			}
			if a.Test != "" && a.Action == "fail" {
				failed++
			}
			if a.Test != "" && a.Action == "skip" {
				skipped++
			}
		}
		if pass == 0 || failed != 0 || skipped != 0 || !original["TestRefundExceptionRepairPostgresIntegration"] || !original["TestRefundExceptionFrozenExecutionPostgres"] || !original["TestRefundSuccessConflictEvidencePostgres"] {
			t.Fatal("zero original owner assertions")
		}
	})
}
