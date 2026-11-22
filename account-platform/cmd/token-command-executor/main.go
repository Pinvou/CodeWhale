package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if handled, err := events.PrepareNATSCredentials(os.Args[1:], "token-command-executor"); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if err := httpserver.ConfigureProcessLogging("token-command-executor"); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	handlerDigest, err := token.TokenCommandHandlerDigest()
	if err != nil {
		log.Fatal(err)
	}
	policyDigest, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		log.Fatal(err)
	}
	credential := serviceauth.CredentialFromEnvironment()
	reader, err := tokengovernance.NewClient(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), credential, policyDigest, handlerDigest)
	if err != nil {
		log.Fatal("Token executor activation configuration is invalid")
	}
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), "token-command-executor", tokenExecutorNATSConfig())
	if err != nil {
		log.Fatal("Token executor event connection is unavailable")
	}
	defer broker.Close()
	var stopped atomic.Bool
	go func() {
		if err := token.RunTokenApprovalConsumers(ctx, broker, reader, os.Getenv("TOKEN_GATEWAY_HTTP_URL"), os.Getenv("ACCOUNT_HTTP_URL"), credential); err != nil {
			stopped.Store(true)
			slog.Error("TOKEN_APPROVAL_CONSUMER_STOPPED", "error_code", "CONSUMER_STOPPED")
			cancel()
		}
	}()
	ready := func(ctx context.Context) error {
		if stopped.Load() {
			return errors.New("Token approval consumers are not running")
		}
		if err := broker.Ping(ctx); err != nil {
			return err
		}
		_, err := reader.Snapshot(ctx)
		return err
	}
	if err := httpserver.RunWithApplicationContext(ctx, "token-command-executor", http.NotFoundHandler(), ready); err != nil {
		log.Fatal(err)
	}
	if stopped.Load() {
		exitStoppedTokenApprovalConsumer()
	}
}

func exitStoppedTokenApprovalConsumer() {
	slog.Error("TOKEN_APPROVAL_CONSUMER_STOPPED", "error_code", "CONSUMER_STOPPED")
	os.Exit(1)
}

// The privileged consumer uses only pre-provisioned resources and the explicit
// identity files projected to this process, including in development fixtures.
func tokenExecutorNATSConfig() events.NATSConfig {
	return events.NATSConfig{
		Environment: os.Getenv("APP_ENV"), ResourceMode: "provisioned",
		CAFile: os.Getenv("NATS_CA_FILE"), SeedFile: os.Getenv("NATS_NKEY_SEED_FILE"),
		CredentialsFile: os.Getenv("NATS_CREDENTIALS_FILE"), RequireExplicitAuthentication: true,
	}
}
