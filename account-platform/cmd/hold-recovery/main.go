package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
)

func main() {
	confirm := flag.Bool("confirm", false, "confirm release of expired ACTIVE holds")
	limit := flag.Int("limit", 100, "maximum expired holds to release in this pass")
	flag.Parse()
	if !*confirm || *limit < 1 || *limit > 1000 {
		log.Fatal("--confirm and --limit between 1 and 1000 are required")
	}
	ctx := context.Background()
	repository, err := billing.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("HOLD_RECOVERY_STORE_UNAVAILABLE")
	}
	defer repository.Close()
	released, err := repository.ExpireHolds(ctx, *limit)
	if err != nil {
		log.Fatal("HOLD_RECOVERY_FAILED")
	}
	log.Printf("released_expired_holds=%d", released)
}
