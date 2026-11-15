// approval-grant-inventory is a bounded operator-owned read-only investigation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/google/uuid"
)

type inventoryAction func(context.Context, string, int, *account.UnstartedGrantCursor) (account.UnstartedGrantInventory, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, inspect))
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer, action inventoryAction) int {
	fail := func(reason string, code int) int {
		if json.NewEncoder(out).Encode(map[string]string{"status": "failed", "reason": reason}) != nil {
			return 1
		}
		return code
	}
	flags := flag.NewFlagSet("approval-grant-inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	limit := flags.Int("limit", 25, "maximum observations (1..200)")
	event := flags.String("after-event", "", "event_id from the previous next cursor")
	durable := flags.String("after-durable", "", "durable from the same next cursor")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 200 || (*event == "") != (*durable == "") {
		return fail("INVALID_ARGUMENTS", 2)
	}
	var after *account.UnstartedGrantCursor
	if *event != "" {
		id, err := uuid.Parse(*event)
		if err != nil || id == uuid.Nil || len(*durable) > 120 || strings.TrimSpace(*durable) != *durable || strings.ContainsAny(*durable, "\r\n\t") {
			return fail("INVALID_ARGUMENTS", 2)
		}
		after = &account.UnstartedGrantCursor{EventID: id, Durable: *durable}
	}
	dsn := strings.TrimSpace(getenv("APPROVAL_GRANT_INVENTORY_DATABASE_URL"))
	if dsn == "" {
		return fail("CONFIGURATION_REQUIRED", 2)
	}
	result, err := action(ctx, dsn, *limit, after)
	if err != nil {
		return fail("INVENTORY_UNAVAILABLE", 1)
	}
	if json.NewEncoder(out).Encode(result) != nil {
		return 1
	}
	return 0
}

func inspect(ctx context.Context, dsn string, limit int, after *account.UnstartedGrantCursor) (account.UnstartedGrantInventory, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	repo, err := account.NewPostgresRepository(bounded, dsn)
	if err != nil {
		return account.UnstartedGrantInventory{}, err
	}
	defer repo.Close()
	return repo.InspectUnstartedApprovalGrants(bounded, limit, after)
}
