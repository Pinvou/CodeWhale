package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func main() {
	eventID := flag.String("event-id", "", "dead-lettered event UUID")
	reason := flag.String("reason", "", "auditable reason for replay")
	confirm := flag.Bool("confirm", false, "confirm the event should be requeued")
	flag.Parse()
	parsedID, err := uuid.Parse(strings.TrimSpace(*eventID))
	if err != nil || strings.TrimSpace(*reason) == "" || !*confirm {
		log.Fatal("--event-id, --reason, and --confirm are required")
	}
	ctx := context.Background()
	store, err := events.NewPostgresStore(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if err := store.Requeue(ctx, parsedID, *reason); err != nil {
		log.Fatal(err)
	}
	log.Printf("event %s requeued", parsedID)
}
