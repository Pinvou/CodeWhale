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
)

// Preserve the fixture's cleanup context even when Command.Wait consumes its
// entire delay after cancellation. The child timer leaves a separate exit gap.
func closedGrantChildBudget(now, parentDeadline time.Time, hasDeadline bool, minimum time.Duration) (time.Time, time.Duration, error) {
	if !hasDeadline || parentDeadline.IsZero() {
		return time.Time{}, 0, errors.New("owner harness requires a finite Go test deadline")
	}
	commandDeadline := parentDeadline.Add(-closedGrantCleanupReserve - closedGrantWaitDelay)
	remaining := commandDeadline.Sub(now)
	if minimum <= 0 || remaining < minimum+closedGrantExitGrace {
		return time.Time{}, 0, errors.New("insufficient owner child budget after setup and cleanup reserves")
	}
	return commandDeadline, remaining - closedGrantExitGrace, nil
}

func TestClosedGrantPublicOwnerHarness(t *testing.T) {
	// ~107s in the failed CI run, including the published-owner and concurrency
	// roots. The minimum admits this complete group, not one 90s owner root.
	runClosedGrantPublicOwnerHarness(t, 150*time.Second, "^(TestClosedGrantPublishedOwnerEffects|TestF08ClosedGrantConcurrentReceiptReplayPostgres|TestAccountClosureIsolationScrubsContactWithoutFinancialMutation|TestAccountClosureIsolationPreservesPriorAuditFacts)$")
}

func TestClosedGrantPublicFinancialHarness(t *testing.T) {
	// The four real financial-blocker roots took ~190s together in CI.
	runClosedGrantPublicOwnerHarness(t, 240*time.Second, "^(TestAccountClosurePreservesUnknownForcedRefundEvidence|TestAccountClosureRequiresAuthoritativeChargebackTerminal|TestF08ClosureBlocksPendingFinancialAnomalyPostgres|TestF08ReclaimBlocksPendingFinancialAnomalyPostgres)$")
}

func TestClosedGrantPublicMembershipHarness(t *testing.T) {
	// Assessment alone took ~96s; membership previously started with only 9s
	// left. Both now receive a separate full public harness invocation.
	runClosedGrantPublicOwnerHarness(t, 180*time.Second, "^(TestAccountClosureAssessmentIncludesMemberReservations|TestF08MembershipClosureReasonQuotaReplayPostgres)$")
}

func runClosedGrantPublicOwnerHarness(t *testing.T, minimum time.Duration, selection string) {
	t.Helper()
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
	// Every wrapper invocation selects one fixed public test above. Each group
	// owns fresh setup and cleanup and retains the enclosing eight-minute cap.
	cancel()
	parentDeadline, hasDeadline := t.Deadline()
	commandDeadline, childTimeout, err := closedGrantChildBudget(time.Now(), parentDeadline, hasDeadline, minimum)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancelChild := context.WithDeadline(context.Background(), commandDeadline)
	defer cancelChild()
	args := []string{"test", "-mod=readonly", "-tags=nomsgpack", "-count=1", "-json", "./internal/billing", "-run", selection}
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
