// event-resources is a bounded deployment operator, not a runtime service.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func main() {
	createOnly := flag.Bool("create-only", false, "create missing fixed resources; existing resources are read-only")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "NATS_RESOURCE_ARGUMENTS_REJECTED")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := events.ManageNATSResources(ctx, os.Getenv("NATS_URL"), events.NATSConfig{Environment: "production", CAFile: os.Getenv("NATS_CA_FILE")}, *createOnly)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NATS_RESOURCE_CHECK_OR_CREATION_FAILED")
		os.Exit(1)
	}
	fmt.Println("NATS_FIXED_RESOURCES_VERIFIED")
}
