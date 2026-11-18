package account

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type terminalRecoveryFunction func(context.Context, int) (int, error)

func (f terminalRecoveryFunction) RecoverApprovalDeliveryTerminals(ctx context.Context, limit int) (int, error) {
	return f(ctx, limit)
}

type terminalLogFailure struct{}

func (terminalLogFailure) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestApprovalDeliveryTerminalWorkerFailureAndCancellation(t *testing.T) {
	for _, scenario := range []string{"retry-next-tick", "parent-cancel", "independent-error-during-cancel", "logger-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			var logs bytes.Buffer
			var output io.Writer = &logs
			if scenario == "logger-unavailable" {
				output = terminalLogFailure{}
			}
			previous := slog.Default()
			slog.SetDefault(observability.NewLogger(output, observability.Config{Service: "account-service"}))
			t.Cleanup(func() { slog.SetDefault(previous) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			repo := terminalRecoveryFunction(func(pass context.Context, limit int) (int, error) {
				calls++
				deadline, ok := pass.Deadline()
				if !ok || time.Until(deadline) > 10*time.Second || limit != 100 {
					t.Fatal("unbounded worker pass")
				}
				if scenario == "parent-cancel" {
					cancel()
					return 0, pass.Err()
				}
				if scenario == "independent-error-during-cancel" {
					cancel()
					return 0, errors.New("private database payload")
				}
				if calls == 1 {
					return 0, errors.New("private database payload")
				}
				cancel()
				return 1, nil
			})
			NewApprovalDeliveryTerminalRecoveryWorker(repo, time.Millisecond).Run(ctx)
			want := 2
			if scenario == "parent-cancel" || scenario == "independent-error-during-cancel" {
				want = 1
			}
			if calls != want {
				t.Fatal("worker did not stop/retry correctly", calls, want)
			}
			if strings.Contains(logs.String(), "private database payload") {
				t.Fatal("raw error leaked")
			}
			if scenario == "parent-cancel" {
				if logs.Len() != 0 {
					t.Fatal("normal cancellation logged as failure")
				}
			} else if scenario != "logger-unavailable" && !strings.Contains(logs.String(), `"event":"approval_delivery_terminal_recovery_failed"`) {
				t.Fatal("safe failure event lost", logs.String())
			}
		})
	}
}
