package f09fixture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Named Go file arguments bypass build constraints. Ask the same child
// toolchain to select production sources using its actual GOOS/GOARCH/GOFLAGS,
// then retain the coordinator's original explicit test-file contract.
func stageProductionSources(t *testing.T, ctx context.Context, directory string) []string {
	t.Helper()
	command := exec.CommandContext(ctx, os.Getenv("PINVOU_PRIVATE_GO"), "list", "-json", ".")
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatal("select platform production sources with the declared Go toolchain", err)
	}
	var selected struct {
		GoFiles, CgoFiles []string
		Incomplete        bool
	}
	if err := json.Unmarshal(output, &selected); err != nil || selected.Incomplete {
		t.Fatal("invalid or incomplete Go source selection", err)
	}
	names := append(selected.GoFiles, selected.CgoFiles...)
	if len(names) == 0 {
		t.Fatal("no selected production sources")
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names))
	for _, name := range names {
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			t.Fatal("unexpected production source path")
		}
		paths = append(paths, filepath.Join(directory, name))
	}
	return paths
}

func TestStageProductionSourcesRespectPlatformAndTags(t *testing.T) {
	directory := t.TempDir()
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	t.Setenv("PINVOU_PRIVATE_GO", filepath.Join(runtime.GOROOT(), "bin", goName))
	t.Setenv("GOOS", runtime.GOOS)
	t.Setenv("GOARCH", runtime.GOARCH)
	t.Setenv("GOFLAGS", "-tags=pinvou_source_probe -mod=readonly")
	opposite := "linux"
	if runtime.GOOS == "linux" {
		opposite = "windows"
	}
	for name, content := range map[string]string{
		"go.mod":                           "module example.test/source-probe\n\ngo 1.26.0\n",
		"shared.go":                        "package probe\n",
		"selected_" + runtime.GOOS + ".go": "package probe\n",
		"rejected_" + opposite + ".go":     "package probe\n",
		"tagged.go":                        "//go:build pinvou_source_probe\n\npackage probe\n",
		"rejected_tag.go":                  "//go:build !pinvou_source_probe\n\npackage probe\n",
		"ignored_test.go":                  "package probe\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	selected := stageProductionSources(t, t.Context(), directory)
	var names []string
	for _, path := range selected {
		names = append(names, filepath.Base(path))
	}
	want := []string{"selected_" + runtime.GOOS + ".go", "shared.go", "tagged.go"}
	sort.Strings(want)
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatal("source selection ignored platform, tags or test-file boundary", names)
	}
}
