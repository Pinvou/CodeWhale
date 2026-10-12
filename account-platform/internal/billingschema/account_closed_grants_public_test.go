package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

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
	args := []string{"test", "-mod=readonly", "-tags=nomsgpack", "-count=1", "-json", "./internal/billing", "-run", "^TestClosedGrantPublishedOwnerEffects$"}
	if runtime.GOOS != "windows" {
		args = append(args, "-race")
	}
	child := exec.CommandContext(ctx, os.Getenv("F08_TEST_GO"), args...)
	child.Dir = "../.."
	child.Env = append(os.Environ(), "F08_BILLING_URL="+f.serviceURL(t, bc, "billing_service"))
	out, e := child.CombinedOutput()
	if err := os.WriteFile(os.Getenv("F08_TEST_CHILD_LOG"), out, 0600); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatal("actual owner child failed; log retained")
	}
}
