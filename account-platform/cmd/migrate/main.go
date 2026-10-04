package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

func main() {
	directory := flag.String("dir", os.Getenv("MIGRATIONS_DIR"), "directory containing SQL migrations")
	direction := flag.String("direction", "up", "migration direction: up or down")
	flag.Parse()
	if *directory == "" {
		log.Fatal("migration directory is required through --dir or MIGRATIONS_DIR")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	loaded, err := migrations.Load(*directory)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer connection.Close(context.Background())
	defer migrations.Unlock(context.Background(), connection)

	switch *direction {
	case "up":
		err = migrations.Up(ctx, connection, loaded)
	case "down":
		err = migrations.DownOne(ctx, connection, loaded)
	default:
		err = fmt.Errorf("unsupported migration direction %q", *direction)
	}
	if err != nil {
		log.Fatal(err)
	}
}
