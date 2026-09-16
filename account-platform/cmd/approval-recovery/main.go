// approval-recovery is a bounded, query-only one-shot investigator. It is not
// deployed or scheduled by default and cannot retry or complete an execution.
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
	"github.com/jackc/pgx/v5"
)

type commandConfig struct {
	DatabaseURL, AccountURL, BillingURL, TokenURL, Credential string
	Limit                                                     int
}

type commandSummary struct {
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	Candidates     int    `json:"candidates,omitempty"`
	Observed       int    `json:"observed,omitempty"`
	Unavailable    int    `json:"unavailable,omitempty"`
	MoreCandidates bool   `json:"more_candidates,omitempty"`
}

type investigateAction func(context.Context, commandConfig) (commandSummary, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, investigate))
}

func run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, action investigateAction) int {
	emit := func(summary commandSummary, code int) int {
		if json.NewEncoder(output).Encode(summary) != nil {
			return 1
		}
		return code
	}
	flags := flag.NewFlagSet("approval-recovery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	limit := flags.Int("limit", 25, "maximum candidates (1..200)")
	deadline := flags.Duration("deadline", 30*time.Second, "maximum investigation duration (1s..30s)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 200 ||
		*deadline < time.Second || *deadline > 30*time.Second {
		return emit(commandSummary{Status: "failed", Reason: "INVALID_ARGUMENTS"}, 2)
	}
	credential := strings.TrimSpace(getenv("APPROVAL_RECOVERY_SERVICE_TOKEN"))
	if tokenFile := strings.TrimSpace(getenv("WORKLOAD_IDENTITY_TOKEN_FILE")); tokenFile != "" {
		if !filepath.IsAbs(tokenFile) {
			return emit(commandSummary{Status: "failed", Reason: "CONFIGURATION_REQUIRED"}, 2)
		}
		credential = "file:" + tokenFile
	}
	config := commandConfig{
		DatabaseURL: strings.TrimSpace(getenv("APPROVAL_RECOVERY_DATABASE_URL")),
		AccountURL:  strings.TrimSpace(getenv("ACCOUNT_HTTP_URL")),
		BillingURL:  strings.TrimSpace(getenv("BILLING_HTTP_URL")),
		TokenURL:    strings.TrimSpace(getenv("TOKEN_GATEWAY_HTTP_URL")),
		Credential:  credential, Limit: *limit,
	}
	if config.DatabaseURL == "" || config.AccountURL == "" || config.BillingURL == "" || config.TokenURL == "" || config.Credential == "" {
		return emit(commandSummary{Status: "failed", Reason: "CONFIGURATION_REQUIRED"}, 2)
	}
	bounded, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()
	summary, err := action(bounded, config)
	if err != nil {
		reason := "INVESTIGATION_UNAVAILABLE"
		if errors.Is(err, context.Canceled) {
			reason = "CANCELED"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "DEADLINE_EXCEEDED"
		}
		return emit(commandSummary{Status: "failed", Reason: reason}, 1)
	}
	return emit(summary, 0)
}

func investigate(ctx context.Context, config commandConfig) (commandSummary, error) {
	connection, err := pgx.Connect(ctx, config.DatabaseURL)
	if err != nil {
		return commandSummary{}, err
	}
	defer connection.Close(context.Background())
	var role string
	if err := connection.QueryRow(ctx, `SELECT session_user`).Scan(&role); err != nil || role != "account_approval_recovery" {
		return commandSummary{}, account.ErrApprovalRecoveryPlanInvalid
	}
	leaser, err := account.NewRecoveryQueryLeaseStore(connection)
	if err != nil {
		return commandSummary{}, err
	}
	planner, err := account.NewApprovalRecoveryPlanClient(config.AccountURL, config.Credential)
	if err != nil {
		return commandSummary{}, err
	}
	effects, err := approvaleffect.NewClient(config.BillingURL, config.TokenURL, config.Credential)
	if err != nil {
		return commandSummary{}, err
	}
	batch, err := account.InspectProofRecoveryLeased(ctx, planner, leaser, effects, config.Limit)
	if err != nil {
		return commandSummary{}, err
	}
	summary := commandSummary{Status: "query_only", Reason: "NO_TERMINAL_AUTHORITY", Candidates: len(batch.Plan.Candidates), MoreCandidates: batch.Plan.MoreCandidates}
	for _, inspection := range batch.Inspections {
		if inspection.EffectObservation == "UNAVAILABLE" {
			summary.Unavailable++
		} else {
			summary.Observed++
		}
	}
	return summary, nil
}
