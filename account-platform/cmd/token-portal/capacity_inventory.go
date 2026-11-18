package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

var errCapacityInventoryArguments = errors.New("invalid inventory command arguments")

// An explicit one-shot command in the shipped Portal binary; no IdP secret or
// HTTP service startup is needed. Its default is read-only inspection.
func runCapacityInventory(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("token-portal capacity-inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apply := flags.Bool("apply", false, "build the shared metadata inventory")
	timeout := flags.Duration("timeout", 5*time.Minute, "bounded execution budget")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, e := fmt.Fprintln(output, "token-portal capacity-inventory [--apply] [--timeout=5m]\nUses REDIS_URL and optional REDIS_CA_FILE; default is read-only inspection.")
			return e
		}
		return errCapacityInventoryArguments
	}
	if flags.NArg() != 0 || *timeout <= 0 || *timeout > 30*time.Minute {
		return errCapacityInventoryArguments
	}
	store, err := portal.NewRedisStoreWithCA(os.Getenv("REDIS_URL"), os.Getenv("REDIS_CA_FILE"))
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *apply {
		if err = store.InventoryTransientCapacity(ctx); err != nil {
			return err
		}
	}
	state, err := store.InspectTransientCapacity(ctx)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(output).Encode(state); err != nil {
		return err
	}
	if !state.FlowReady || !state.HandoffReady {
		return portal.ErrCapacityInventoryRequired
	}
	return nil
}
