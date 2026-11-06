package billingschema

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise psql itself: a printed warning with exit status zero is not a
// deployment failure. These invalid targets must stop before provisioning.
func TestBillingRoleBootstrapRejectsInvalidTarget(t *testing.T) {
	dsn := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	psql := os.Getenv("PINVOU_TEST_PSQL")
	if psql == "" {
		var err error
		psql, err = exec.LookPath("psql")
		if err != nil {
			t.Fatal("PostgreSQL fixture requires psql")
		}
	}
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Do not report connection arguments or captured diagnostics: the
		// fixture DSN may contain credentials.
		cmd := exec.CommandContext(ctx, psql, append([]string{"-X", "--dbname", dsn}, args...)...)
		// Keep fixture connections independent of the Windows console code
		// page and optional server encoding-conversion libraries.
		cmd.Env = append(os.Environ(), "PGCLIENTENCODING=UTF8")
		return cmd.Run()
	}
	if err := run("-Atc", "SELECT 1"); err != nil {
		var status *exec.ExitError
		if errors.As(err, &status) {
			t.Fatalf("PostgreSQL fixture preflight failed (exit %d)", status.ExitCode())
		}
		t.Fatalf("PostgreSQL fixture process could not run (%T)", err)
	}
	script := filepath.Join("..", "..", "deploy", "postgres", "billing-roles.sql")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing", nil},
		{"different", []string{"-v", "billing_database=controlled_nonexistent_billing_target"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "-f", script)
			err := run(args...)
			var status *exec.ExitError
			if !errors.As(err, &status) || status.ExitCode() != 3 {
				t.Fatal("invalid target must produce psql script-error exit status 3")
			}
		})
	}
}
