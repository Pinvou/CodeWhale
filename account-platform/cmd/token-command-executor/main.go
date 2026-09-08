package main

import (
	"context"
	"errors"
	"log"
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
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), "token-command-executor", events.NATSConfig{Environment: os.Getenv("APP_ENV"), CAFile: os.Getenv("NATS_CA_FILE")})
	if err != nil {
		log.Fatal("Token executor event connection is unavailable")
	}
	defer broker.Close()
	var stopped atomic.Bool
	go func() {
		if err := token.RunTokenApprovalConsumers(ctx, broker, reader, os.Getenv("TOKEN_GATEWAY_HTTP_URL"), os.Getenv("ACCOUNT_HTTP_URL"), credential); err != nil {
			stopped.Store(true)
			log.Print("TOKEN_APPROVAL_CONSUMER_STOPPED")
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
		log.Fatal("TOKEN_APPROVAL_CONSUMER_STOPPED")
	}
}
