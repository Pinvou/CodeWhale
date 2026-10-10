// approval-authorization-backfill is an operator-owned, one-shot process.
// It must never be started by Account Service or receive its database role.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

type backfillAction func(context.Context, string, int) (account.ApprovalAuthorizationBackfillReadiness, error)

type commandResult struct {
	Blocker             *account.ApprovalAuthorizationBlockerReport `json:"blocker,omitempty"`
	Status              string                                      `json:"status"`
	Reason              string                                      `json:"reason"`
	AuthorizationCount  int64                                       `json:"authorization_count,omitempty"`
	MissingEpochCount   int64                                       `json:"missing_epoch_count,omitempty"`
	MissingSidecarCount int64                                       `json:"missing_sidecar_count,omitempty"`
	InvalidSidecarCount int64                                       `json:"invalid_sidecar_count,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, executeBackfill))
}

func run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, action backfillAction) int {
	emit := func(result commandResult, code int) int {
		if json.NewEncoder(output).Encode(result) != nil {
			return 1
		}
		return code
	}
	flags := flag.NewFlagSet("approval-authorization-backfill", flag.ContinueOnError)
	// flag errors can include caller-supplied text. Emit only fixed reasons.
	flags.SetOutput(io.Discard)
	batch := flags.Int("batch", 100, "batch size (1..200)")
	deadline := flags.Duration("deadline", 5*time.Minute, "maximum run duration (1s..30m)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *batch < 1 || *batch > 200 || *deadline < time.Second || *deadline > 30*time.Minute {
		return emit(commandResult{Status: "failed", Reason: "INVALID_ARGUMENTS"}, 2)
	}
	dsn := strings.TrimSpace(getenv("APPROVAL_BACKFILL_DATABASE_URL"))
	if dsn == "" {
		return emit(commandResult{Status: "failed", Reason: "BACKFILL_CREDENTIAL_REQUIRED"}, 2)
	}
	bounded, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()
	readiness, err := action(bounded, dsn, *batch)
	if err != nil {
		reason := "APPROVAL_BACKFILL_UNAVAILABLE"
		if errors.Is(err, context.Canceled) {
			reason = "CANCELED"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "DEADLINE_EXCEEDED"
		}
		result := commandResult{Status: "failed", Reason: reason}
		if report, ok := account.ApprovalAuthorizationBlockerReportFrom(err); ok {
			result.Blocker = &report
		}
		return emit(result, 1)
	}
	result := commandResult{Status: "complete", Reason: "HIGH_WATER_BACKFILL_COMPLETE", AuthorizationCount: readiness.AuthorizationCount, MissingEpochCount: readiness.MissingEpochCount, MissingSidecarCount: readiness.MissingSidecarCount, InvalidSidecarCount: readiness.InvalidSidecarCount}
	if readiness.MissingEpochCount != 0 || readiness.MissingSidecarCount != 0 || readiness.InvalidSidecarCount != 0 {
		result.Status, result.Reason = "blocked", "AUTHORIZATION_VALIDATION_REQUIRED"
		return emit(result, 1)
	}
	// This reports a bounded backfill only, never permission to activate proof.
	return emit(result, 0)
}

func executeBackfill(ctx context.Context, dsn string, batch int) (account.ApprovalAuthorizationBackfillReadiness, error) {
	pool, err := account.NewApprovalBackfillPool(ctx, dsn)
	if err != nil {
		return account.ApprovalAuthorizationBackfillReadiness{}, err
	}
	defer pool.Close()
	return account.BackfillApprovalAuthorizationEvents(ctx, pool, batch)
}
