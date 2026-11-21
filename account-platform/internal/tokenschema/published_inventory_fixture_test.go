package tokenschema

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Run the shipped inventory entry point against the real installed predecessor,
// not Up directly: its max-version prefix check caught the original gap bug.
func assertPublishedTokenInventory(t *testing.T, ctx context.Context, admin, database *pgx.ConnConfig, version int64) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "token-release-prepare.exe")
	build := exec.CommandContext(ctx, "go", "build", "-tags=nomsgpack", "-mod=readonly", "-o", binary, "../../cmd/token-release-prepare")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public release inventory: %v\n%s", err, output)
	}
	u := url.URL{Scheme: "postgresql", User: url.User(admin.User), Host: net.JoinHostPort(database.Host, strconv.Itoa(int(database.Port))), Path: "/" + database.Database, RawQuery: "sslmode=disable"}
	if admin.Password != "" {
		u.User = url.UserPassword(admin.User, admin.Password)
	}
	cmd := exec.CommandContext(ctx, binary, "--operation", "inventory", "--expected-database", database.Database, "--migrations", "../../migrations/token")
	cmd.Env = append(os.Environ(), "TOKEN_RELEASE_DBA_DATABASE_URL="+u.String())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("published predecessor inventory failed: %v\n%s", err, output)
	}
	var evidence struct {
		MigrationVersion int64  `json:"migration_version"`
		Applied          bool   `json:"applied"`
		Decision         string `json:"decision"`
	}
	if err = json.Unmarshal(output, &evidence); err != nil || evidence.MigrationVersion != version || evidence.Applied || evidence.Decision != "CHECKED" {
		t.Fatal("inventory did not preserve the exact published boundary", err)
	}
}
