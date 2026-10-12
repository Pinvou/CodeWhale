package billingschema

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateF11PublicOwnerIntegration(t *testing.T) {
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
	var dd, user string
	var port, actors int
	e = admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_setting('port')::int,(SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'pinvou_wave_test')`).Scan(&dd, &user, &port, &actors)
	admin.Close(ctx)
	if e != nil || port != 55504 || user != "pinvou_wave_test" || actors != 0 || strings.ReplaceAll(dd, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" {
		t.Fatal("owner/port/datadir/absence guard")
	}
	f := newStageFixture(t, ctx, raw)
	t.Run("public-account-original072-policy", func(t *testing.T) {
		c := f.database(t, ctx, "account", "account_service")
		all, e := migrations.Load("../../migrations/account")
		if e != nil {
			t.Fatal(e)
		}
		if e = accountschema.Up(ctx, c, all); e != nil {
			t.Fatal("public Account Up", e)
		}
		if _, e = migrations.VerifyApplied(ctx, c, all); e != nil {
			t.Fatal(e)
		}
		if digest, err := approvalpolicy.VerifyPolicySet(ctx, c); err != nil || len(digest) != 64 {
			t.Fatal("actual full current 25 policies differ", err)
		}

		migrations.Unlock(ctx, c)
		var n int
		if e = c.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE command_type='RECEIVABLE_WRITE_OFF' AND policy_version=1 AND snapshot_schema_version=1`).Scan(&n); e != nil || n != 1 {
			t.Fatal("original policy", n, e)
		}
	})
	t.Run("original-owner-contract-child", func(t *testing.T) {
		dir := "../billing"
		args := []string{"test", "-mod=readonly", "-race", "-count=1", "-json", ".", "-run", "^(TestReceivableWriteOffPostgresIntegration|TestPrivateF11OwnerFrozenFailureAndRollback)$"}
		cmd := exec.CommandContext(ctx, os.Getenv("F11_GO_BINARY"), args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
		out, e := cmd.CombinedOutput()
		dest := os.Getenv("F11_CHILD_LOG")
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
		if pass == 0 || failed != 0 || skipped != 0 || !original["TestReceivableWriteOffPostgresIntegration"] || !original["TestPrivateF11OwnerFrozenFailureAndRollback"] {
			t.Fatalf("original owner assertions incomplete: pass=%d fail=%d skip=%d", pass, failed, skipped)
		}
	})
}
