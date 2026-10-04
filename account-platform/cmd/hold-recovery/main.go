package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
)

func main() {
	confirm := flag.Bool("confirm", false, "confirm moving expired ACTIVE holds to REVIEW without releasing funds")
	limit := flag.Int("limit", 100, "maximum expired holds to mark REVIEW in this pass")
	flag.Parse()
	if !*confirm || *limit < 1 || *limit > 1000 {
		log.Fatal("--confirm and --limit between 1 and 1000 are required")
	}
	ctx := context.Background()
	repository, err := billing.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	reviewed, err := repository.ExpireHolds(ctx, *limit)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("expired_holds_marked_review=%d", reviewed)
}
