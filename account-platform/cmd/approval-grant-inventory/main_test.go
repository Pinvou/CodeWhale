package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

func TestInventoryCommandRejectsUnsafeArguments(t *testing.T) {
	for _, args := range [][]string{{"--apply"}, {"--limit", "0"}, {"--limit", "201"}, {"--after-event", "bad"}, {"--after-durable", "only"}, {"extra"}} {
		var out bytes.Buffer
		called := false
		code := run(context.Background(), args, func(string) string { return "private-dsn" }, &out, func(context.Context, string, int, *account.UnstartedGrantCursor) (account.UnstartedGrantInventory, error) {
			called = true
			return account.UnstartedGrantInventory{}, nil
		})
		if code != 2 || called || !strings.Contains(out.String(), "INVALID_ARGUMENTS") {
			t.Fatal("invalid invocation accepted", args, out.String())
		}
	}
}

func TestInventoryCommandPreservesCursorAndRedactsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var out bytes.Buffer
		code := run(context.Background(), []string{"--limit", "1", "--after-event", "7b546120-67c8-43dd-a035-01d0458565f8", "--after-durable", "billing-wallet-adjustment-executor-p1-v1"}, func(key string) string {
			if key != "APPROVAL_GRANT_INVENTORY_DATABASE_URL" {
				t.Fatal("wrong config key")
			}
			return "private-dsn"
		}, &out, func(_ context.Context, dsn string, limit int, cursor *account.UnstartedGrantCursor) (account.UnstartedGrantInventory, error) {
			if dsn != "private-dsn" || limit != 1 || cursor == nil || cursor.Durable != "billing-wallet-adjustment-executor-p1-v1" {
				t.Fatal("cursor/config lost")
			}
			if fail {
				return account.UnstartedGrantInventory{}, errors.New("private-dsn secret dependency message")
			}
			return account.UnstartedGrantInventory{SchemaVersion: 1, Items: []account.UnstartedGrant{}}, nil
		})
		if fail && (code != 1 || !strings.Contains(out.String(), "INVENTORY_UNAVAILABLE")) || !fail && code != 0 || strings.Contains(out.String(), "private-dsn") || strings.Contains(out.String(), "secret") {
			t.Fatal("unsafe result", code, out.String())
		}
	}
}
