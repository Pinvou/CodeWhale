package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

func main() {
	eventID := flag.String("event-id", "", "dead-lettered event UUID")
	inspect := flag.Bool("inspect", false, "print immutable dead-letter metadata and digest without changing state")
	expectedDigest := flag.String("expected-digest", "", "SHA-256 digest returned by --inspect")
	idempotencyKey := flag.String("idempotency-key", "", "stable replay command idempotency key")
	reason := flag.String("reason", "", "auditable reason for replay")
	confirm := flag.Bool("confirm", false, "confirm the event should be requeued")
	flag.Parse()
	parsedID, err := uuid.Parse(strings.TrimSpace(*eventID))
	if err != nil {
		log.Fatal("--event-id must be a UUID")
	}
	ctx := context.Background()
	store, err := events.NewPostgresStore(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("EVENT_REPLAY_STORE_UNAVAILABLE")
	}
	defer store.Close()
	if *inspect {
		candidate, err := store.InspectDeadLetter(ctx, parsedID)
		if err != nil {
			log.Fatal("EVENT_REPLAY_INSPECT_FAILED")
		}
		if err := writeInspection(os.Stdout, candidate); err != nil {
			log.Fatal("EVENT_REPLAY_INSPECT_OUTPUT_FAILED")
		}
		return
	}
	if strings.TrimSpace(*reason) == "" || strings.TrimSpace(*expectedDigest) == "" || strings.TrimSpace(*idempotencyKey) == "" || !*confirm {
		log.Fatal("--expected-digest, --idempotency-key, --reason, and --confirm are required for replay")
	}
	ctx, err = serviceauth.ContextForJob(ctx, "event-replay", "EVENT_REPLAY", uuid.NewString())
	if err != nil {
		log.Fatal("EVENT_REPLAY_IDENTITY_INVALID")
	}
	result, err := store.ReplayDeadLetter(ctx, events.ReplayRequest{EventID: parsedID, ExpectedDigest: *expectedDigest, IdempotencyKey: *idempotencyKey, Reason: *reason})
	if err != nil {
		log.Fatal("EVENT_REPLAY_FAILED")
	}
	log.Printf("event %s replay request %s committed=%t", result.EventID, result.ReplayID, result.Replayed)
}

// writeInspection retains the complete immutable metadata and reports both
// JSON encoding and output failures instead of printing an empty success.
func writeInspection(output io.Writer, candidate events.DeadLetterCandidate) error {
	return json.NewEncoder(output).Encode(candidate)
}
