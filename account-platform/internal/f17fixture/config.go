// Package f17fixture admits only explicitly owned temporary test instances.
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

// OwnedInstance supplies the identity chosen by a fixture's caller. Prefix is a
// fixed fixture-specific directory prefix, never a value read from the database.
// Callers must still probe the actual server identity and empty-instance state.
type OwnedInstance struct {
	User, Port, Directory, Prefix string
}

func OwnedConfigurationAllowed(config *pgx.ConnConfig, port, directory string) bool {
	return (OwnedInstance{User: OwnedActor, Port: port, Directory: directory, Prefix: "pinvou-f17-audit-"}).ConfigurationAllowed(config)
}

func (instance OwnedInstance) ConfigurationAllowed(config *pgx.ConnConfig) bool {
	n, err := strconv.ParseUint(instance.Port, 10, 16)
	if err != nil || n < 1024 || strconv.FormatUint(n, 10) != instance.Port || instance.User == "" || config == nil || config.Host != "127.0.0.1" || config.Port != uint16(n) || config.User != instance.User || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 || len(config.RuntimeParams) != 0 {
		return false
	}
	return instance.directoryAllowed()
}

func OwnedDirectoryAllowed(directory string) bool {
	return (OwnedInstance{Directory: directory, Prefix: "pinvou-f17-audit-"}).directoryAllowed()
}

func (instance OwnedInstance) directoryAllowed() bool {
	directory, prefix := instance.Directory, instance.Prefix
	if prefix == "" || strings.ContainsAny(prefix, `/\`) {
		return false
	}
	clean := filepath.Clean(directory)
	if !filepath.IsAbs(directory) || filepath.Base(clean) != "data" {
		return false
	}
	parent := filepath.Dir(clean)
	if filepath.Clean(filepath.Dir(parent)) != filepath.Clean(os.TempDir()) {
		return false
	}
	name := filepath.Base(parent)
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	id := strings.TrimPrefix(name, prefix)
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func DirectoryMatches(actual, expected string) bool {
	return (OwnedInstance{Directory: expected, Prefix: "pinvou-f17-audit-"}).DirectoryMatches(actual)
}

func (instance OwnedInstance) DirectoryMatches(actual string) bool {
	return instance.directoryAllowed() && filepath.Clean(filepath.FromSlash(actual)) == filepath.Clean(instance.Directory)
}

// CIConfigurationAllowed binds the client port to the verified Actions service,
// independently of PostgreSQL's internal port (5432).
func CIConfigurationAllowed(config *pgx.ConnConfig, port string, githubActions bool) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return githubActions && err == nil && n >= 1024 && strconv.FormatUint(n, 10) == port && config != nil && config.Host == "127.0.0.1" && config.Port == uint16(n) && config.User == "postgres" && config.Password == "" && config.Database == "postgres" && config.TLSConfig == nil && len(config.Fallbacks) == 0 && len(config.RuntimeParams) == 0
}
