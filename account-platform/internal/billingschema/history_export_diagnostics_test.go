package billingschema

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// CI may report a closed failure code, never the private ErrorRecord, stack,
// process output, arguments or connection environment retained by the CLI.
type historyExportDiagnostic struct {
	Stage, Status string
	Codes         []string
	Files         int
	Truncated     bool
}

func historyExportDiagnosticFor(stage, directory string, commandErr error) historyExportDiagnostic {
	d := historyExportDiagnostic{Status: "success"}
	switch stage {
	case "private-fixture", "export", "verify", "archive", "rejected-input", "race", "race-verify", "recovery":
		d.Stage = stage
	default:
		d.Stage = "unknown"
	}
	if commandErr != nil {
		d.Status = "command-error"
		var exit *exec.ExitError
		if errors.As(commandErr, &exit) {
			d.Status = "exit-" + strconv.Itoa(exit.ExitCode())
		} else if errors.Is(commandErr, context.Canceled) || errors.Is(commandErr, context.DeadlineExceeded) {
			d.Status = "context-ended"
		}
	}
	item, err := os.Lstat(directory)
	if err != nil || !item.IsDir() || item.Mode()&os.ModeSymlink != 0 {
		d.Codes = []string{"unavailable"}
		return d
	}
	dir, err := os.Open(directory)
	if err != nil {
		d.Codes = []string{"unavailable"}
		return d
	}
	defer dir.Close()
	entries, _ := dir.ReadDir(257)
	if len(entries) > 256 {
		entries = entries[:256]
		d.Truncated = true
	}
	codes := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".failure-") || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		if d.Files == 8 {
			d.Truncated = true
			break
		}
		d.Files++
		code := "unknown"
		if entry.Type().IsRegular() {
			file, e := os.Open(filepath.Join(directory, entry.Name()))
			if e == nil {
				// Only the first complete line, at most 128 bytes, is eligible.
				data, readErr := io.ReadAll(io.LimitReader(file, 129))
				file.Close()
				end := bytes.IndexByte(data, '\n')
				if end < 0 && len(data) <= 128 {
					end = len(data)
				}
				if readErr == nil && end >= 0 && end <= 128 {
					candidate := strings.TrimSuffix(string(data[:end]), "\r")
					switch candidate {
					case "HISTORY_ARCHIVE_REJECTED", "HISTORY_ARCHIVE_TABLES_REJECTED", "HISTORY_BUDGET_EXHAUSTED",
						"HISTORY_CHECKSUM_MISMATCH", "HISTORY_DESTINATION_EXISTS", "HISTORY_DIRECTORY_NOT_PRIVATE",
						"HISTORY_DIRECTORY_OWNER_REJECTED", "HISTORY_DIRECTORY_REJECTED", "HISTORY_LEDGER_CHANGED",
						"HISTORY_MANIFEST_REJECTED", "HISTORY_MANIFEST_TABLES_REJECTED", "HISTORY_METADATA_REJECTED",
						"HISTORY_METADATA_TOO_LARGE", "HISTORY_PARENT_REJECTED", "HISTORY_PG_COMMAND_FAILED",
						"HISTORY_PG_ENVIRONMENT_REQUIRED", "HISTORY_PLATFORM_UNSUPPORTED", "HISTORY_PROCESS_RETIREMENT_UNKNOWN",
						"HISTORY_PROCESS_START_FAILED", "HISTORY_PROCESS_STREAM_FAILED", "HISTORY_SOURCE_CHANGED", "HISTORY_TOOL_REJECTED":
						code = candidate
					}
				}
			}
		}
		codes[code] = true
	}
	for code := range codes {
		d.Codes = append(d.Codes, code)
	}
	if len(d.Codes) == 0 {
		d.Codes = []string{"unavailable"}
	}
	sort.Strings(d.Codes)
	return d
}

func TestHistoryExportDiagnosticSummary(t *testing.T) {
	const private = "PRIVATE_SENTINEL_NOT_FOR_CI"
	for _, tc := range []struct{ name, content, want string }{
		{"known-with-private-stack", "HISTORY_PG_COMMAND_FAILED\r\n" + private, "HISTORY_PG_COMMAND_FAILED"},
		{"unknown-message", private, "unknown"},
		{"embedded-code", private + " HISTORY_PG_COMMAND_FAILED", "unknown"},
		{"code-plus-private", "HISTORY_PG_COMMAND_FAILED " + private, "unknown"},
		{"unlisted-code", "HISTORY_" + private, "unknown"},
		{"oversized-line", strings.Repeat("x", 129) + "\nHISTORY_PG_COMMAND_FAILED", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".failure-fixture.txt"), []byte(tc.content), 0600); err != nil {
				t.Fatal("cannot prepare owned diagnostic fixture")
			}
			d := historyExportDiagnosticFor("export", dir, errors.New(private))
			if d.Stage != "export" || d.Status != "command-error" || len(d.Codes) != 1 || d.Codes[0] != tc.want || d.Files != 1 || strings.Contains(fmt.Sprint(d), private) {
				t.Fatal("diagnostic escaped its closed vocabulary")
			}
		})
	}
	t.Run("missing-and-nonregular", func(t *testing.T) {
		dir := t.TempDir()
		d := historyExportDiagnosticFor(private, filepath.Join(dir, "missing"), nil)
		if d.Stage != "unknown" || len(d.Codes) != 1 || d.Codes[0] != "unavailable" {
			t.Fatal("missing diagnostic must use a fixed unavailable state")
		}
		if err := os.Mkdir(filepath.Join(dir, ".failure-directory.txt"), 0700); err != nil {
			t.Fatal("cannot prepare owned nonregular fixture")
		}
		d = historyExportDiagnosticFor("verify", dir, nil)
		if d.Files != 1 || len(d.Codes) != 1 || d.Codes[0] != "unknown" {
			t.Fatal("nonregular diagnostic must not be read")
		}
	})
	t.Run("bounded-files", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < 9; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".failure-%d.txt", i)), []byte("HISTORY_BUDGET_EXHAUSTED\n"+private), 0600); err != nil {
				t.Fatal("cannot prepare owned bounded fixture")
			}
		}
		d := historyExportDiagnosticFor("recovery", dir, nil)
		if d.Files != 8 || !d.Truncated || len(d.Codes) != 1 || d.Codes[0] != "HISTORY_BUDGET_EXHAUSTED" || strings.Contains(fmt.Sprint(d), private) {
			t.Fatal("diagnostic file bound or vocabulary was lost")
		}
	})
}
