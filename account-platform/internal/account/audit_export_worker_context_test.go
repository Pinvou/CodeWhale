package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The closed pool forbids a real connection; a cancelled supervisor must still
// stop the worker instead of waiting for another five-second polling interval.
func TestAuditExportWorkerStopsWithApplicationContext(t *testing.T) {
	config, err := pgxpool.ParseConfig("postgres://controlled@127.0.0.1:1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal("controlled configuration")
	}
	config.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("network forbidden") }
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal("controlled pool")
	}
	pool.Close()
	service := &AuditExportService{repository: &PostgresRepository{pool: pool}, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled worker did not stop")
	}
}
