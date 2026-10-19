package billingschema

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const (
	closedGrantCleanupReserve = 45 * time.Second
	closedGrantWaitDelay      = 5 * time.Second
	closedGrantExitGrace      = 15 * time.Second
	closedGrantMinChildBudget = 90 * time.Second
)

// Preserve the fixture's cleanup context even when Command.Wait consumes its
// entire delay after cancellation. The child timer leaves a separate exit gap.
func closedGrantChildBudget(now, parentDeadline time.Time, hasDeadline bool) (time.Time, time.Duration, error) {
	if !hasDeadline || parentDeadline.IsZero() {
		return time.Time{}, 0, errors.New("owner harness requires a finite Go test deadline")
	}
	commandDeadline := parentDeadline.Add(-closedGrantCleanupReserve - closedGrantWaitDelay)
	// The existing published-owner root alone may use up to 90 seconds.
	remaining := commandDeadline.Sub(now)
	if remaining < closedGrantMinChildBudget+closedGrantExitGrace {
		return time.Time{}, 0, errors.New("insufficient owner child budget after setup and cleanup reserves")
	}
	return commandDeadline, remaining - closedGrantExitGrace, nil
}

func TestClosedGrantPublicOwnerHarness(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("explicit isolated schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	bm, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	if e = Up(ctx, bc, bm); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, bc, bm); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, bc)
	// Setup keeps its existing limit; the eight complete owner roots use the
	// remaining enclosing test budget rather than sharing setup's 150 seconds.
	cancel()
	parentDeadline, hasDeadline := t.Deadline()
	commandDeadline, childTimeout, err := closedGrantChildBudget(time.Now(), parentDeadline, hasDeadline)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancelChild := context.WithDeadline(context.Background(), commandDeadline)
	defer cancelChild()
	args := []string{"test", "-mod=readonly", "-tags=nomsgpack", "-count=1", "-json", "./internal/billing", "-run", "^(TestClosedGrantPublishedOwnerEffects|TestAccountClosureIsolationPreservesPriorAuditFacts|TestAccountClosureIsolationScrubsContactWithoutFinancialMutation|TestAccountClosurePreservesUnknownForcedRefundEvidence|TestAccountClosureRequiresAuthoritativeChargebackTerminal|TestAccountClosureAssessmentIncludesMemberReservations|TestF08MembershipClosureReasonQuotaReplayPostgres|TestF08ClosedGrantConcurrentReceiptReplayPostgres)$"}
	args = append(args, "-timeout="+childTimeout.String())
	if runtime.GOOS != "windows" {
		args = append(args, "-race")
	}
	child := exec.CommandContext(childCtx, os.Getenv("F08_TEST_GO"), args...)
	child.WaitDelay = closedGrantWaitDelay
	child.Dir = "../.."
	child.Env = append(os.Environ(), "F08_BILLING_URL="+f.serviceURL(t, bc, "billing_service"))
	out, e := child.CombinedOutput()
	if err := os.WriteFile(os.Getenv("F08_TEST_CHILD_LOG"), out, 0600); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		if childCtx.Err() != nil {
			t.Fatal("actual owner child execution budget exhausted; log retained")
		}
		t.Fatal("actual owner child exited unsuccessfully; log retained")
	}
}
