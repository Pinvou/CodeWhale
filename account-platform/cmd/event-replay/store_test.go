package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestReplayStoreSelectsExplicitAccountCapability(t *testing.T) {
	for _, user := range []string{"account_service", "billing_service", "token_service"} {
		t.Run(user, func(t *testing.T) {
			accountCalls, genericCalls := 0, 0
			want := errors.New("constructor result")
			account := func(context.Context, string) (replayStore, error) {
				accountCalls++
				return nil, want
			}
			generic := func(context.Context, string) (replayStore, error) {
				genericCalls++
				return nil, want
			}
			_, err := selectReplayStore(context.Background(), "postgres://"+user+"@127.0.0.1/fixture?sslmode=disable", account, generic)
			if !errors.Is(err, want) || (user == "account_service" && (accountCalls != 1 || genericCalls != 0)) ||
				(user != "account_service" && (accountCalls != 0 || genericCalls != 1)) {
				t.Fatal("configured runtime selected a different replay protocol")
			}
		})
	}
}

func TestReplayAccountCapabilityNeverFallsBack(t *testing.T) {
	for _, code := range []string{"42501", "42883"} {
		t.Run(code, func(t *testing.T) {
			want := &pgconn.PgError{Code: code}
			account := func(context.Context, string) (replayStore, error) { return nil, want }
			generic := func(context.Context, string) (replayStore, error) {
				t.Fatal("Account capability failure fell back to generic UPDATE")
				return nil, nil
			}
			if _, err := selectReplayStore(context.Background(), "user=account_service host=127.0.0.1 dbname=fixture sslmode=disable", account, generic); !errors.Is(err, want) {
				t.Fatal("Account constructor error was replaced")
			}
		})
	}
}
