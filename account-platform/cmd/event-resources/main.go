// event-resources manages only the installed shared stream, not a consumer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func main() {
	result, err := run(os.Args[1:], os.Getenv, events.ManageNATSResources)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NATS_RESOURCE_CHECK_OR_CREATION_FAILED")
		os.Exit(1)
	}
	fmt.Println(result)
}

func run(args []string, environment func(string) string, manage func(context.Context, string, events.NATSConfig, bool) error) (string, error) {
	flags := flag.NewFlagSet("event-resources", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	createOnly := flags.Bool("create-only", false, "create the missing fixed stream without modifying existing resources")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return "", errors.New("NATS_RESOURCE_ARGUMENTS_REJECTED")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := manage(ctx, environment("NATS_URL"), events.NATSConfig{Environment: "production", ResourceMode: "provisioned", CAFile: environment("NATS_CA_FILE"), SeedFile: environment("NATS_NKEY_SEED_FILE"), CredentialsFile: environment("NATS_CREDENTIALS_FILE"), RequireExplicitAuthentication: true}, *createOnly)
	if err != nil {
		return "", errors.New("NATS_RESOURCE_CHECK_OR_CREATION_FAILED")
	}
	return "NATS_FIXED_STREAM_VERIFIED", nil
}
