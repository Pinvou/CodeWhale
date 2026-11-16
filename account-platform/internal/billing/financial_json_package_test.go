package billing

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func stageFinancialJSONPackage(t *testing.T) (string, string) {
	t.Helper()
	source, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	staged := filepath.Join(root, "migrations", "billing")
	work := filepath.Join(root, "internal", "billing")
	if err = os.MkdirAll(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		versionText, _, valid := strings.Cut(e.Name(), "_")
		version, parseErr := strconv.ParseInt(versionText, 10, 64)
		if !valid || parseErr != nil || version >= 900042 {
			continue
		}
		body, x := os.ReadFile(filepath.Join(source, e.Name()))
		if x != nil {
			t.Fatal(x)
		}
		if x = os.WriteFile(filepath.Join(staged, e.Name()), body, 0600); x != nil {
			t.Fatal(x)
		}
	}
	t.Chdir(work)
	return source, staged
}
func addFinancialJSONGuard(t *testing.T, source, staged string) {
	t.Helper()
	for _, suffix := range []string{"up", "down"} {
		name := "900042_financial_json_object_contracts." + suffix + ".sql"
		body, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(staged, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
