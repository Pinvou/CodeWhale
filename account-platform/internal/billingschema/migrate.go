// Package billingschema retains numeric SQL history while installing the relay
// generation fence before any payment producer starts using the shared outbox.
package billingschema

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

const generationVersion int64 = 900001
const generationChecksum = "7e36e103ad25620dd7a882792fcd0d942f6208038aeaa966eb44b367cea27f05"
const generationDownChecksum = "d1895793b89e87a43e9e6b75aeafcab19d12b15676929ea864a16860128516d8"

const intentVersion int64 = 900002
const intentChecksum = "68df09f526088b981c992d0c9912cab8f4ac2c6f5567fdb6351cfdc2b905d2a2"
const intentDownChecksum = "14f58e7592a5733d75ff248ace7d5fffbe289b0c728de2a20d3377e54fe91044"

const pricingReceiptVersion int64 = 900004
const pricingReceiptChecksum = "60cb8519502e377de242d858872366486083cdd9b501d80a60421d663c250b43"
const pricingReceiptDownChecksum = "49843bf03fc3120f13798ff4b4296344ca9dd12a73544e0a041009f28172af6e"

// Ordered installs the new forward-only fence immediately after original 006.
// The migration engine checks each applied version, so later delivery of lower
// numeric versions remains possible after the high-numbered fence is recorded.
// The same validation is required before DownOne to prevent rollback-file edits.
func Ordered(input []migrations.Migration) ([]migrations.Migration, error) {
	var fence *migrations.Migration
	var intent *migrations.Migration
	var pricingReceipt *migrations.Migration
	seen := make(map[int64]bool, len(input))
	for _, migration := range input {
		if seen[migration.Version] {
			return nil, errors.New("duplicate billing migration version")
		}
		seen[migration.Version] = true
		if migration.Version == generationVersion {
			copy := migration
			fence = &copy
		}
		if migration.Version == pricingReceiptVersion {
			copy := migration
			pricingReceipt = &copy
		}
		if migration.Version == intentVersion {
			copy := migration
			intent = &copy
		}
	}
	if fence == nil || !seen[1] || !seen[6] {
		return nil, errors.New("billing relay fence requires foundation and original event delivery migrations")
	}
	if fence.Checksum != generationChecksum || sqlChecksum(fence.UpSQL) != generationChecksum || sqlChecksum(fence.DownSQL) != generationDownChecksum {
		return nil, errors.New("billing relay fence is not the frozen forward-only migration")
	}
	if seen[29] != (intent != nil) {
		return nil, errors.New("billing anomaly intents require their original anomaly schema in the same delivery")
	}
	if intent != nil && (intent.Checksum != intentChecksum || sqlChecksum(intent.UpSQL) != intentChecksum || sqlChecksum(intent.DownSQL) != intentDownChecksum) {
		return nil, errors.New("billing anomaly intents are not the frozen forward-only migration")
	}
	if seen[51] != (pricingReceipt != nil) {
		return nil, errors.New("billing pricing delivery requires immutable operation receipts")
	}
	if pricingReceipt != nil && (!seen[22] || pricingReceipt.Checksum != pricingReceiptChecksum || sqlChecksum(pricingReceipt.UpSQL) != pricingReceiptChecksum || sqlChecksum(pricingReceipt.DownSQL) != pricingReceiptDownChecksum) {
		return nil, errors.New("billing pricing receipt migration is not the frozen forward-only migration")
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
	result := make([]migrations.Migration, 0, len(ordered))
	for _, migration := range ordered {
		if migration.Version == generationVersion || migration.Version == intentVersion || migration.Version == pricingReceiptVersion {
			continue
		}
		result = append(result, migration)
		if migration.Version == 6 {
			result = append(result, *fence)
		}
		if migration.Version == 51 {
			result = append(result, *pricingReceipt)
		}
		if migration.Version == 29 {
			result = append(result, *intent)
		}
	}
	return result, nil
}

func Up(ctx context.Context, connection *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	return migrations.UpWithHooks(ctx, connection, ordered, migrations.UpHooks{BeforeApply: protectLegacyRefundMigration})
}

// The original target migration cannot preserve a pending partial refund's
// decision. Lock before checking so an older writer cannot add one concurrently.
// The lock, guard, original DDL and ledger write share the migration transaction.
func protectLegacyRefundMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if migration.Version != 27 {
		return nil
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE refunds IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM refunds WHERE status NOT IN ('SUCCEEDED','FAILED'))`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errors.New("legacy refunds must be completed and reconciled with the previous version before targeted-refund migration")
	}
	return nil
}

func sqlChecksum(sql string) string {
	canonical := strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}
