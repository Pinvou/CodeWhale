// Package f17fixture admits only explicitly owned temporary audit-test instances.
// It grants no authority: callers still verify the server identity, directory,
// absent actors and databases, and exclusive sessions before any fixture write.
package f17fixture

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const OwnedProfile = "local-owned"
const OwnedActor = "pinvou_audit_fixture"

func OwnedConfigurationAllowed(config *pgx.ConnConfig, port, directory string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n < 1024 || strconv.FormatUint(n, 10) != port || config == nil || config.Host != "127.0.0.1" || config.Port != uint16(n) || config.User != OwnedActor || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 {
		return false
	}
	return OwnedDirectoryAllowed(directory)
}

func OwnedDirectoryAllowed(directory string) bool {
	clean := filepath.Clean(directory)
	if !filepath.IsAbs(directory) || filepath.Base(clean) != "data" {
		return false
	}
	parent := filepath.Dir(clean)
	if filepath.Clean(filepath.Dir(parent)) != filepath.Clean(os.TempDir()) {
		return false
	}
	name := filepath.Base(parent)
	const prefix = "pinvou-f17-audit-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	id := strings.TrimPrefix(name, prefix)
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func DirectoryMatches(actual, expected string) bool {
	return OwnedDirectoryAllowed(expected) && filepath.Clean(filepath.FromSlash(actual)) == filepath.Clean(expected)
}
