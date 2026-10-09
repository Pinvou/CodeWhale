package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestBackgroundPanicRemainsFatalWithoutExposingItsValue(t *testing.T) {
	const marker = "private-background-panic"
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	defer func() {
		if value := recover(); value != "BACKGROUND_TASK_PANIC" {
			t.Fatalf("panic=%v", value)
		}
		if !strings.Contains(output.String(), "BACKGROUND_TASK_PANIC") || strings.Contains(output.String(), marker) {
			t.Fatalf("unsafe or absent background diagnostic: %s", output.String())
		}
	}()
	RunBackground(context.Background(), "fixture", func(context.Context) { panic(marker) })
}
