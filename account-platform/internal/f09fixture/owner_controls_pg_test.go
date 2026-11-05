package f09fixture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestF09UnexpiredReplayPublicCoordinator(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = newStageFixture(t, ctx, raw)
	dir := filepath.Join("..", "billing")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"test", "-race", "-json", "-count=1", "-mod=readonly", "-run", "^TestEntitlementCorrectionExecuteAndConsumerReadPostgresIntegration$"}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			args = append(args, filepath.Join(dir, e.Name()))
		}
	}
	args = append(args, filepath.Join(dir, "retained_financial_integration_test.go"), filepath.Join(dir, "finance_refund_reconcile_integration_test.go"), filepath.Join(dir, "entitlement_correction_execute_integration_test.go"))
	child := exec.CommandContext(ctx, os.Getenv("PINVOU_PRIVATE_GO"), args...)
	child.Env = append(os.Environ(), "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
	stream, err := os.Create(os.Getenv("PINVOU_F09_CHILD_STREAM"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	child.Stdout = stream
	child.Stderr = stream
	if err = child.Run(); err != nil {
		t.Fatalf("finite current unexpired/replay owner failed: %v", err)
	}
}
