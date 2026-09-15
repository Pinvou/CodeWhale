// event-resources is a bounded deployment operator, not a runtime service.
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(result)
}

var errArguments = errors.New("NATS_RESOURCE_ARGUMENTS_REJECTED")

func run(args []string, environment func(string) string, manage func(context.Context, string, events.NATSConfig, bool) error) (string, error) {
	flags := flag.NewFlagSet("event-resources", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	createOnly := flags.Bool("create-only", false, "create missing fixed resources")
	render := flags.Bool("render-authorization", false, "offline fixed public authorization output")
	renderManifest := flags.Bool("render-consumer-manifest", false, "offline fixed consumer expectation, not activation")
	consumerManifest := flags.String("consumer-manifest", "", "explicit consumer expectation artifact")
	consumerManifestDigest := flags.String("consumer-manifest-sha256", "", "independently trusted consumer artifact hash")
	public := flags.String("public-keys", "", "public identity artifact path")
	digest := flags.String("public-keys-sha256", "", "independently trusted public artifact hash")
	output := flags.String("output", "", "new authorization output path")
	overlap := flags.Bool("overlap", false, "explicit normal rotation overlap")
	previous := flags.String("previous-public-keys", "", "old public identity artifact path")
	previousDigest := flags.String("previous-public-keys-sha256", "", "trusted old public artifact hash")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "event-resources [-create-only] | -render-authorization -public-keys PATH -public-keys-sha256 SHA256 -output NEW_PATH [-overlap -previous-public-keys PATH -previous-public-keys-sha256 SHA256] [-consumer-manifest PATH -consumer-manifest-sha256 SHA256] | -render-consumer-manifest -public-keys PATH -public-keys-sha256 SHA256 -output NEW_PATH", nil
		}
		return "", errArguments
	}
	if flags.NArg() != 0 {
		return "", errArguments
	}
	if *renderManifest {
		if *render || *createOnly || *overlap || *previous != "" || *previousDigest != "" || *consumerManifest != "" || *consumerManifestDigest != "" || *public == "" || *digest == "" || *output == "" {
			return "", errArguments
		}
		if err := renderConsumerManifest(*public, *digest, *output); err != nil {
			return "", err
		}
		return "NATS_CONSUMER_EXPECTATION_RENDERED_NOT_ACTIVATED", nil
	}
	if *render {
		if *createOnly || *public == "" || *digest == "" || *output == "" || (*overlap && (*previous == "" || *previousDigest == "")) || (!*overlap && (*previous != "" || *previousDigest != "")) {
			return "", errArguments
		}
		if (*consumerManifest == "") != (*consumerManifestDigest == "") {
			return "", errArguments
		}
		if *consumerManifest != "" {
			return renderConsumerAuthorization(*public, *digest, *previous, *previousDigest, *consumerManifest, *consumerManifestDigest, *output)
		}
		// This branch must precede every workload environment, identity file and
		// connection access. Rendering only consumes independently hashed public data.
		if err := renderAuthorization(*public, *digest, *previous, *previousDigest, *output); err != nil {
			return "", err
		}
		return "NATS_PUBLIC_AUTHORIZATION_RENDERED", nil
	}
	if *public != "" || *digest != "" || *output != "" || *previous != "" || *previousDigest != "" || *overlap || *consumerManifest != "" || *consumerManifestDigest != "" {
		return "", errArguments
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := manage(ctx, environment("NATS_URL"), events.NATSConfig{Environment: "production", CAFile: environment("NATS_CA_FILE")}, *createOnly)
	if err != nil {
		return "", errors.New("NATS_RESOURCE_CHECK_OR_CREATION_FAILED")
	}
	return "NATS_FIXED_RESOURCES_VERIFIED", nil
}
