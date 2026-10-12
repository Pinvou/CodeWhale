package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var input token.GovernanceTransitionRequest
	var requestID, roles string
	flag.BoolVar(&input.Apply, "apply", false, "commit the explicitly selected transition; omitted means read-only check")
	flag.StringVar(&input.TargetState, "to", "", "ACTIVE or SAFE_ROLLBACK; omitted checks current installation")
	flag.StringVar(&input.ExpectedState, "expected-state", "", "exact current persistent state")
	flag.Int64Var(&input.ExpectedEpoch, "expected-epoch", -1, "exact current persistent epoch")
	flag.StringVar(&input.OperatorID, "operator", "", "auditable operations identity")
	flag.StringVar(&input.Reason, "reason", "", "auditable reason, without credentials or private data")
	flag.StringVar(&requestID, "request-id", "", "stable UUID for this operations attempt")
	flag.StringVar(&roles, "runtime-roles", "", "sorted comma-separated new release NOLOGIN writer roles")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if requestID != "" {
		parsed, err := uuid.Parse(requestID)
		if err != nil {
			return fmt.Errorf("request-id must be a UUID")
		}
		input.RequestID = parsed
	}
	if roles != "" {
		input.ApprovedRuntimeRoles = strings.Split(roles, ",")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(name string) (*pgxpool.Pool, error) {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
		config, err := pgxpool.ParseConfig(os.Getenv(name))
		if err != nil {
			return nil, fmt.Errorf("%s is invalid", name)
		}
		config.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("%s connection is unavailable", name)
		}
		return pool, nil
	}
	tokenPool, err := open("TOKEN_MIGRATOR_DATABASE_URL")
	if err != nil {
		return err
	}
	defer tokenPool.Close()
	var accountPool *pgxpool.Pool
	if input.TargetState != "SAFE_ROLLBACK" && !(input.TargetState == "" && !input.Apply && input.RequestID != uuid.Nil) {
		accountPool, err = open("ACCOUNT_POLICY_DATABASE_URL")
		if err != nil {
			return err
		}
		defer accountPool.Close()
	}
	snapshot, err := token.CheckOrTransitionTokenGovernance(ctx, tokenPool, accountPool, input)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Applied   bool                          `json:"applied"`
		RequestID uuid.UUID                     `json:"request_id"`
		State     token.TokenGovernanceSnapshot `json:"activation"`
	}{input.Apply, input.RequestID, snapshot})
}
