package tokenschema

import (
	"context"
	_ "embed"
	"errors"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Already-installed audit history is retained, never installed on a fresh DB.
//
//go:embed retained/000018_event_replay_audit.up.sql
var originalEventReplaySQL string

const originalEventReplayChecksum = "733b1f380b9f4cc848c28fabefa4621e42db67466bcb2fba9b4fbbcf4d677a61"
const originalEventReplayCRLFChecksum = "2382724355e100869433043f45f910868b4bb73a7e1d90eda8af8c4e224120c1"

// ExpectedHistory adds only the known, already-installed 018 to an expected
// package. It is also used by the release planner's partial-history verifier.
// Unknown names/checksums and missing audit guards fail before durable writes.
func ExpectedHistory(ctx context.Context, c *pgx.Conn, input []migrations.Migration) ([]migrations.Migration, error) {
	if sqlChecksum(originalEventReplaySQL) != originalEventReplayChecksum {
		return nil, errors.New("original Token event replay resource changed")
	}
	var exists bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return input, nil
	}
	var name, checksum string
	err := c.QueryRow(ctx, `SELECT name,checksum FROM public.schema_migrations WHERE version=18`).Scan(&name, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return input, nil
	}
	if err != nil {
		return nil, err
	}
	if name != "event_replay_audit" || (checksum != originalEventReplayChecksum && checksum != originalEventReplayCRLFChecksum) {
		return nil, errors.New("installed Token event replay 018 is not the original history")
	}
	// The original body only raises a constant exception: it performs no name
	// lookup. Accept its original, canonical and observed RDS restore search paths
	// without accepting arbitrary function settings or changing the audit body.
	var body string
	err = c.QueryRow(ctx, `SELECT COALESCE((
 SELECT p.prosrc FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 JOIN pg_language l ON l.oid=p.prolang
 WHERE t.tgrelid=to_regclass('public.event_replay_requests') AND NOT t.tgisinternal
 AND t.tgname='event_replay_requests_immutable' AND t.tgtype=27 AND t.tgenabled IN ('O','A')
 AND t.tgqual IS NULL AND t.tgnargs=0 AND t.tgattr=''::int2vector
 AND n.nspname='public' AND p.proname='reject_event_replay_audit_mutation' AND NOT p.prosecdef
 AND p.pronargs=0 AND p.prorettype='trigger'::regtype AND l.lanname='plpgsql'
 AND (p.proconfig IS NULL OR p.proconfig=ARRAY['search_path=pg_catalog']
      OR p.proconfig=ARRAY['search_path=public, public, pg_catalog'])
 ),'')`).Scan(&body)
	if err != nil {
		return nil, err
	}
	if sqlChecksum(body) != sqlChecksum("\nBEGIN\n    RAISE EXCEPTION 'event replay audit records are immutable';\nEND ") {
		return nil, errors.New("original Token event replay audit guard is missing or changed")
	}
	result := slices.Clone(input)
	if slices.ContainsFunc(result, func(m migrations.Migration) bool { return m.Version == 18 }) {
		return nil, errors.New("retained Token event replay history is already in the package")
	}
	result = append(result, migrations.Migration{Version: 18, Name: name, UpSQL: originalEventReplaySQL, Checksum: originalEventReplayChecksum})
	slices.SortFunc(result, func(a, b migrations.Migration) int {
		if a.Version < b.Version {
			return -1
		}
		if a.Version > b.Version {
			return 1
		}
		return 0
	})
	return result, nil
}

func VerifyApplied(ctx context.Context, c *pgx.Conn, input []migrations.Migration) (migrations.Verification, error) {
	ordered, err := Ordered(input)
	if err != nil {
		return migrations.Verification{}, err
	}
	expected, err := ExpectedHistory(ctx, c, ordered)
	if err != nil {
		return migrations.Verification{}, err
	}
	verification, err := migrations.VerifyApplied(ctx, c, expected)
	if err != nil {
		return migrations.Verification{}, err
	}
	rows, err := c.Query(ctx, `SELECT version,name FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		return migrations.Verification{}, err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var version int64
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return migrations.Verification{}, err
		}
		if i >= len(expected) || version != expected[i].Version || name != expected[i].Name {
			return migrations.Verification{}, errors.New("Token database is outside the installed management stage")
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return migrations.Verification{}, err
	}
	if i != len(expected) {
		return migrations.Verification{}, errors.New("Token database management history is incomplete")
	}
	return verification, nil
}
