package billing

import (
	"context"
	"strings"
	"testing"
	"time"
)

const packageExpiryWorkloadBudget = 5 * time.Minute

// Log only the exact expiry-index root using fixed phase labels and no business data.
func logPackageExpiryPhase(t *testing.T, ctx context.Context, phase string) {
	t.Helper()
	const root = "TestPackageExpiryCrossWalletIndexPostgres"
	if t.Name() != root && !strings.HasPrefix(t.Name(), root+"/") {
		return
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Logf("PACKAGE_EXPIRY_PHASE phase=%s deadline=absent", phase)
		return
	}
	remaining := time.Until(deadline)
	state := "live"
	if ctx.Err() == context.DeadlineExceeded {
		state = "deadline"
	} else if ctx.Err() == context.Canceled {
		state = "cancelled"
	}
	t.Logf("PACKAGE_EXPIRY_PHASE phase=%s elapsed_ms=%d remaining_ms=%d state=%s", phase, (packageExpiryWorkloadBudget - remaining).Milliseconds(), remaining.Milliseconds(), state)
}
