package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type recoveryAction func(context.Context, string, billing.ExpiredHoldProofClient, int) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, executeRecovery))
}

// No release is performed locally: the existing worker records intent and asks
// Token for the trusted dispatch fence before accepting its exact proof.
// TOKEN_HOLD_EXPIRY_URL must be HTTPS; WORKLOAD_IDENTITY_TOKEN_FILE (or the
// existing credential input) must supply the approved short-lived Billing
// identity with internal.token.hold-expiry.prove. Local job context does not
// grant remote authority. WORKLOAD_IDENTITY_CA_FILE uses the existing trust reader.
func run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, action recoveryAction) int {
	flags := flag.NewFlagSet("hold-recovery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	confirm := flags.Bool("confirm", false, "confirm trusted recovery of expired holds")
	limit := flags.Int("limit", 100, "maximum expired candidates in this pass (1..1000)")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !*confirm || *limit < 1 || *limit > 1000 {
		fmt.Fprintln(output, "HOLD_RECOVERY_ARGUMENTS_INVALID")
		return 2
	}
	credential := strings.TrimSpace(getenv("INTERNAL_SERVICE_TOKEN"))
	if file := strings.TrimSpace(getenv("WORKLOAD_IDENTITY_TOKEN_FILE")); file != "" {
		credential = "file:" + file
	}
	client, err := serviceauth.NewWorkloadHTTPClient(getenv("WORKLOAD_IDENTITY_CA_FILE"))
	if err != nil {
		fmt.Fprintln(output, "HOLD_RECOVERY_TRUST_INVALID")
		return 1
	}
	proof, err := billing.NewHTTPExpiredHoldProofClient(strings.TrimSpace(getenv("TOKEN_HOLD_EXPIRY_URL")), credential, client)
	if err != nil || strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintln(output, "HOLD_RECOVERY_CONFIGURATION_INVALID")
		return 1
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	bounded, err = serviceauth.ContextForJob(bounded, "hold-recovery", "EXPIRED_HOLD_RECOVERY", uuid.NewString())
	if err != nil || action == nil {
		fmt.Fprintln(output, "HOLD_RECOVERY_IDENTITY_INVALID")
		return 1
	}
	if err = action(bounded, getenv("DATABASE_URL"), proof, *limit); err != nil {
		fmt.Fprintln(output, "HOLD_RECOVERY_FAILED")
		return 1
	}
	fmt.Fprintln(output, "expired_hold_recovery_pass_completed")
	return 0
}

func executeRecovery(ctx context.Context, databaseURL string, proof billing.ExpiredHoldProofClient, limit int) error {
	repository, err := billing.NewPostgresRepository(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer repository.Close()
	return billing.NewExpiredHoldRecoveryWorker(repository, proof, time.Minute).RunOnceLimit(ctx, limit)
}
