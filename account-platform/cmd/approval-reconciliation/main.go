// approval-reconciliation is an operator-owned, bounded one-shot process.
// It is deliberately absent from Account Service startup and deployment
// manifests; production cutover remains a separate, explicitly authorized act.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
)

const applyConfirmation = "RECONCILE_APPROVALS"

type commandConfig struct {
	DatabaseURL string
	BillingURL  string
	TokenURL    string
	Credential  string
	Limit       int
	Apply       bool
}

type reconciliationAction func(context.Context, commandConfig) (account.ApprovalReconciliationSummary, error)

type commandResult struct {
	Status  string                                 `json:"status"`
	Reason  string                                 `json:"reason"`
	Summary *account.ApprovalReconciliationSummary `json:"summary,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, executeReconciliation))
}

func run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, action reconciliationAction) int {
	emit := func(result commandResult, code int) int {
		if json.NewEncoder(output).Encode(result) != nil {
			return 1
		}
		return code
	}
	flags := flag.NewFlagSet("approval-reconciliation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	limit := flags.Int("limit", 100, "maximum requests in this bounded run (1..200)")
	deadline := flags.Duration("deadline", 5*time.Minute, "maximum run duration (1s..30m)")
	apply := flags.Bool("apply", false, "apply eligible invalidations")
	confirm := flags.String("confirm", "", "must equal RECONCILE_APPROVALS when applying")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 200 ||
		*deadline < time.Second || *deadline > 30*time.Minute || (*apply && *confirm != applyConfirmation) || (!*apply && *confirm != "") {
		return emit(commandResult{Status: "failed", Reason: "INVALID_ARGUMENTS"}, 2)
	}
	credential := strings.TrimSpace(getenv("INTERNAL_SERVICE_TOKEN"))
	if tokenFile := strings.TrimSpace(getenv("WORKLOAD_IDENTITY_TOKEN_FILE")); tokenFile != "" {
		if !filepath.IsAbs(tokenFile) {
			return emit(commandResult{Status: "failed", Reason: "RECONCILIATION_CONFIGURATION_REQUIRED"}, 2)
		}
		credential = "file:" + tokenFile
	}
	config := commandConfig{
		DatabaseURL: strings.TrimSpace(getenv("APPROVAL_RECONCILIATION_DATABASE_URL")),
		BillingURL:  strings.TrimSpace(getenv("BILLING_HTTP_URL")),
		TokenURL:    strings.TrimSpace(getenv("TOKEN_GATEWAY_HTTP_URL")),
		Credential:  credential,
		Limit:       *limit,
		Apply:       *apply,
	}
	if config.DatabaseURL == "" || config.BillingURL == "" || config.TokenURL == "" || strings.TrimSpace(config.Credential) == "" {
		return emit(commandResult{Status: "failed", Reason: "RECONCILIATION_CONFIGURATION_REQUIRED"}, 2)
	}
	bounded, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()
	summary, err := action(bounded, config)
	if err != nil {
		reason := "APPROVAL_RECONCILIATION_UNAVAILABLE"
		if errors.Is(err, context.Canceled) {
			reason = "CANCELED"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "DEADLINE_EXCEEDED"
		} else if errors.Is(err, account.ErrApprovalReconciliationControlChanged) {
			reason = "CONTROL_CHANGED"
		} else if errors.Is(err, account.ErrApprovalReconciliationBlocked) {
			reason = "RECONCILIATION_BLOCKED"
		}
		return emit(commandResult{Status: "failed", Reason: reason}, 1)
	}
	if !summary.CompleteWithoutBlocker {
		return emit(commandResult{Status: "blocked", Reason: "RECONCILIATION_BLOCKED", Summary: &summary}, 1)
	}
	if !*apply {
		return emit(commandResult{Status: "planned", Reason: "DRY_RUN_ONLY", Summary: &summary}, 0)
	}
	return emit(commandResult{Status: "complete", Reason: "APPROVALS_RECONCILED", Summary: &summary}, 0)
}

func executeReconciliation(ctx context.Context, config commandConfig) (account.ApprovalReconciliationSummary, error) {
	repository, err := account.NewPostgresRepository(ctx, config.DatabaseURL)
	if err != nil {
		return account.ApprovalReconciliationSummary{}, err
	}
	defer repository.Close()
	effects, err := approvaleffect.NewClient(config.BillingURL, config.TokenURL, config.Credential)
	if err != nil {
		return account.ApprovalReconciliationSummary{}, err
	}
	return account.RunApprovalReconciliation(ctx, repository, effects, config.Limit, config.Apply)
}
