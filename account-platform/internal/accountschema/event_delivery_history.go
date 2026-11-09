package accountschema

import (
	"context"
	_ "embed"
	"errors"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Retain the actual published SQL; never rewrite an installed 003 ledger row.
//
//go:embed retained/000003_event_delivery.up.sql
var originalEventDeliverySQL string

const originalEventDeliveryChecksum = "e75bccd8da580606c7d4800f19267aa930ea08c97cc1b3b58a2d7806d6cea90b"
const originalEventDeliveryCRLFChecksum = "6a1fe50d4c5c3868f703a220608d8e353bc5afa03b32dbada37cb2826f877f32"
const currentEventDeliveryChecksum = "8cb68b837be92d1e86982d6fe8b24ecbd3f39ee324e0abe84663b07107de685a"
const outboxGenerationVersion int64 = 900012
const outboxGenerationChecksum = "d69bc590c056a3d99ad86e206323bd0f288b5297fe0b2c08883ab1ee68f0bd74"
const outboxGenerationDownChecksum = "2ac62f0bc954f92cedc0edf04f700f10ebbce76dc800163b649dccc594ab2a75"

func validateOutboxGeneration(input []migrations.Migration) error {
	for _, m := range input {
		if m.Version == outboxGenerationVersion && (m.Name != "outbox_generation_upgrade" || m.Checksum != outboxGenerationChecksum || closureReplaySQLChecksum(m.UpSQL) != outboxGenerationChecksum || closureReplaySQLChecksum(m.DownSQL) != outboxGenerationDownChecksum) {
			return errors.New("Account outbox generation upgrade is not the frozen SQL")
		}
	}
	return nil
}

// The caller holds the shared migration lock during Up. Read-only verification
// uses the same exact legacy identity without changing the packaged SQL.
func expectedEventDeliveryHistory(ctx context.Context, c *pgx.Conn, input []migrations.Migration) ([]migrations.Migration, error) {
	if closureReplaySQLChecksum(originalEventDeliverySQL) != originalEventDeliveryChecksum {
		return nil, errors.New("original Account event delivery resource changed")
	}
	if err := validateOutboxGeneration(input); err != nil {
		return nil, err
	}
	var exists bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return input, nil
	}
	var name, checksum string
	err := c.QueryRow(ctx, `SELECT name,checksum FROM public.schema_migrations WHERE version=3`).Scan(&name, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return input, nil
	}
	if err != nil {
		return nil, err
	}
	if name != "event_delivery" {
		return nil, errors.New("Account event delivery history has an unknown name")
	}
	if checksum != originalEventDeliveryChecksum && checksum != originalEventDeliveryCRLFChecksum {
		return input, nil // The shared verifier still rejects every other SQL hash.
	}
	result := slices.Clone(input)
	bridge, replaced := false, false
	for i, m := range result {
		if m.Version == outboxGenerationVersion {
			bridge = true
		}
		if m.Version == 3 {
			if m.Name != "event_delivery" || m.Checksum != currentEventDeliveryChecksum || closureReplaySQLChecksum(m.UpSQL) != currentEventDeliveryChecksum {
				return nil, errors.New("Account event delivery candidate is not the known SQL")
			}
			result[i].UpSQL = originalEventDeliverySQL
			result[i].Checksum = originalEventDeliveryChecksum
			replaced = true
		}
	}
	if !bridge || !replaced {
		return nil, errors.New("original Account event delivery requires its forward generation upgrade")
	}
	return result, nil
}

func VerifyApplied(ctx context.Context, c *pgx.Conn, input []migrations.Migration) (migrations.Verification, error) {
	if err := validateOrganizationEvidenceGuard(input); err != nil {
		return migrations.Verification{}, err
	}
	expected, err := expectedEventDeliveryHistory(ctx, c, input)
	if err != nil {
		return migrations.Verification{}, err
	}
	return migrations.VerifyApplied(ctx, c, expected)
}
