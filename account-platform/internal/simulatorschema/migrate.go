// Package simulatorschema binds mapped order identity facts to their retained
// migration bytes, including the safe rollback boundary.
package simulatorschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// The constants are the canonical LF SQL, not a mutable checksum supplied by
// the loader. Down is validated even when no Up is performed by the CLI.
var retained = map[int64][2]string{
	5: {"c10369f3c1201aa37752d686645fac25c435b6e68daba401158f54090ae4e0bb", "cc69a8e25003a322227384a1117526bc060d325a87c98d482604f8b9400c5e5c"},
	6: {"a5629107cf1ba62324fb0fb9ea548037a291a327274ef35cdeafa69f4dc1e9dc", "fe153f59536c270d0e1e3f2bcf2956e9d33343ed731d31feb2dbf6209babde5f"},
}

func Ordered(input []migrations.Migration) ([]migrations.Migration, error) {
	seen := make(map[int64]bool, len(input))
	for _, migration := range input {
		if seen[migration.Version] {
			return nil, errors.New("duplicate simulator migration version")
		}
		seen[migration.Version] = true
		if expected, ok := retained[migration.Version]; ok {
			if migration.Checksum != expected[0] || checksum(migration.UpSQL) != expected[0] || checksum(migration.DownSQL) != expected[1] {
				return nil, errors.New("simulator identity migration is not the retained safe SQL")
			}
		}
	}
	for _, version := range []int64{1, 2, 3, 4, 5, 6} {
		if !seen[version] {
			return nil, errors.New("simulator identity migration prerequisites are missing")
		}
	}
	ordered := slices.Clone(input)
	slices.SortFunc(ordered, func(a, b migrations.Migration) int {
		if a.Version < b.Version {
			return -1
		}
		if a.Version > b.Version {
			return 1
		}
		return 0
	})
	return ordered, nil
}

func Up(ctx context.Context, connection *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	return migrations.Up(ctx, connection, ordered)
}

func checksum(sql string) string {
	canonical := strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}
