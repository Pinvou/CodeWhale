package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recoveryConfiguration struct {
	databaseURL, accountURL, billingURL, credential, caFile string
	limit                                                   int
	interval                                                time.Duration
}

func loadRecoveryConfiguration() (recoveryConfiguration, error) {
	cfg := recoveryConfiguration{databaseURL: strings.TrimSpace(os.Getenv("APPROVAL_RECOVERY_DATABASE_URL")), accountURL: strings.TrimSpace(os.Getenv("ACCOUNT_HTTP_URL")), billingURL: strings.TrimSpace(os.Getenv("BILLING_HTTP_URL")), credential: serviceauth.CredentialFromEnvironment(), caFile: strings.TrimSpace(os.Getenv("APPROVAL_RECOVERY_CA_FILE")), limit: 200, interval: time.Minute}
	if cfg.databaseURL == "" || !strings.HasPrefix(cfg.credential, "file:") || !filepath.IsAbs(strings.TrimPrefix(cfg.credential, "file:")) {
		return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
	}
	for index, raw := range []string{cfg.accountURL, cfg.billingURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
			return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
		}
		if u.Scheme == "http" && u.Hostname() != []string{"account-service", "billing-service"}[index] {
			return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
		}
	}
	request, _ := http.NewRequest(http.MethodGet, cfg.accountURL, nil)
	if serviceauth.SetAuthorization(request, cfg.credential) != nil || strings.Count(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), ".") != 2 {
		return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
	}
	if raw := strings.TrimSpace(os.Getenv("APPROVAL_RECOVERY_BATCH_LIMIT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
		}
		cfg.limit = n
	}
	if raw := strings.TrimSpace(os.Getenv("APPROVAL_RECOVERY_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Minute || d > time.Hour {
			return cfg, errors.New("RECOVERY_CONFIGURATION_INVALID")
		}
		cfg.interval = d
	}
	return cfg, nil
}

func verifyRecoveryActor(ctx context.Context, pool *pgxpool.Pool) error {
	return account.VerifyApprovalRecoveryActor(ctx, pool)
}

func connectRecovery(ctx context.Context, cfg recoveryConfiguration) (*pgxpool.Pool, *account.ApprovalRecoveryRuntime, error) {
	parsed, err := pgxpool.ParseConfig(cfg.databaseURL)
	if err != nil {
		return nil, nil, errors.New("RECOVERY_DATABASE_CONFIGURATION_INVALID")
	}
	parsed.MaxConns = 2
	parsed.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		return nil, nil, errors.New("RECOVERY_DATABASE_UNAVAILABLE")
	}
	if err = verifyRecoveryActor(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	leaser, err := account.NewRecoveryQueryLeaseStore(pool)
	if err != nil {
		pool.Close()
		return nil, nil, errors.New("RECOVERY_DATABASE_ACTOR_INVALID")
	}
	client, err := serviceauth.NewWorkloadHTTPClient(cfg.caFile)
	if err != nil {
		pool.Close()
		return nil, nil, errors.New("RECOVERY_TRUST_INVALID")
	}
	runtime, err := account.NewApprovalRecoveryRuntime(cfg.accountURL, cfg.billingURL, cfg.credential, leaser, client)
	if err != nil {
		pool.Close()
		return nil, nil, errors.New("RECOVERY_CLIENT_CONFIGURATION_INVALID")
	}
	return pool, runtime, nil
}
