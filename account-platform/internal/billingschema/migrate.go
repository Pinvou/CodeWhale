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

// F02 already owns 900004 for refund dispatch. Keep the draft pricing SQL bytes
// frozen while giving this F04 receipt its distinct forward identity.
const pricingReceiptVersion int64 = 900008
const pricingReceiptChecksum = "60cb8519502e377de242d858872366486083cdd9b501d80a60421d663c250b43"
const pricingReceiptDownChecksum = "49843bf03fc3120f13798ff4b4296344ca9dd12a73544e0a041009f28172af6e"

const expiredHoldRecoveryVersion int64 = 900009
const expiredHoldRecoveryChecksum = "9c926bdb0acbec53cbe3907b92d42430f3177fcd304528b4a4e0a1fe60ae47d5"
const expiredHoldRecoveryDownChecksum = "f309bf2ac39e67860593585e5db72236b1109b957e859521e839bf2053115fdd"

const meteringDeactivationVersion int64 = 900010
const meteringDeactivationChecksum = "c07c9f04924cb761d6473e614f6ae42e466234359c793b138b62140c8a21f4b8"
const meteringDeactivationDownChecksum = "9be8bea9e561d6ab85646a1fe3712ac264332cef41f7ac01d36e05131a40aee8"
const memberIdentityVersion int64 = 900011
const memberIdentityChecksum = "af4a63ed0592a0e60d5f4744176e15cc5013c3633c6f0f414f7580b7132b2aa8"
const memberIdentityDownChecksum = "051eae7b8cf15c7708fbfe88aa096c4d85a284e35465f0684a1ff25fe3af6925"

// Ordered installs the new forward-only fence immediately after original 006.
// The migration engine checks each applied version, so later delivery of lower
// numeric versions remains possible after the high-numbered fence is recorded.
// The same validation is required before DownOne to prevent rollback-file edits.
func Ordered(input []migrations.Migration) ([]migrations.Migration, error) {
	if err := validateReceivableActivationLock(input); err != nil {
		return nil, err
	}
	if err := validateLedgerActivationLock(input); err != nil {
		return nil, err
	}
	if err := validateEntitlementActivationLock(input); err != nil {
		return nil, err
	}
	if err := validateMemberFinancialReviewReason(input); err != nil {
		return nil, err
	}
	var fence *migrations.Migration
	var intent *migrations.Migration
	var pricingReceipt *migrations.Migration
	var expiredHoldRecovery *migrations.Migration
	var meteringDeactivation *migrations.Migration
	var memberIdentity *migrations.Migration
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
		if migration.Version == memberIdentityVersion {
			copy := migration
			memberIdentity = &copy
		}
		if migration.Version == pricingReceiptVersion {
			copy := migration
			pricingReceipt = &copy
		}
		if migration.Version == intentVersion {
			copy := migration
			intent = &copy
		}
		if migration.Version == expiredHoldRecoveryVersion {
			copy := migration
			expiredHoldRecovery = &copy
		}
		if migration.Version == meteringDeactivationVersion {
			copy := migration
			meteringDeactivation = &copy
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
	if seen[20] != (memberIdentity != nil) {
		return nil, errors.New("billing membership projection requires lifecycle schema and event identity fence together")
	}
	if memberIdentity != nil && (memberIdentity.Checksum != memberIdentityChecksum || sqlChecksum(memberIdentity.UpSQL) != memberIdentityChecksum || sqlChecksum(memberIdentity.DownSQL) != memberIdentityDownChecksum) {
		return nil, errors.New("billing member identity is not the frozen forward-only migration")
	}
	if seen[51] != (pricingReceipt != nil) {
		return nil, errors.New("billing pricing delivery requires immutable operation receipts")
	}
	if pricingReceipt != nil && (!seen[22] || pricingReceipt.Checksum != pricingReceiptChecksum || sqlChecksum(pricingReceipt.UpSQL) != pricingReceiptChecksum || sqlChecksum(pricingReceipt.DownSQL) != pricingReceiptDownChecksum) {
		return nil, errors.New("billing pricing receipt migration is not the frozen forward-only migration")
	}
	// The same F04 delivery installs recovery evidence and deactivation
	// receipts. Validate their frozen identities before any Up or Down action,
	// without changing the original execution order or requiring an earlier
	// package without this pricing delivery to include later migrations.
	if pricingReceipt != nil && (expiredHoldRecovery == nil || meteringDeactivation == nil) {
		return nil, errors.New("billing pricing delivery requires expired hold recovery and metering deactivation receipts")
	}
	for _, pin := range []struct {
		migration      *migrations.Migration
		name, up, down string
	}{
		{expiredHoldRecovery, "expired_hold_recovery", expiredHoldRecoveryChecksum, expiredHoldRecoveryDownChecksum},
		{meteringDeactivation, "metering_deactivation_receipts", meteringDeactivationChecksum, meteringDeactivationDownChecksum},
	} {
		if pin.migration != nil && (pin.migration.Name != pin.name || pin.migration.Checksum != pin.up || sqlChecksum(pin.migration.UpSQL) != pin.up || sqlChecksum(pin.migration.DownSQL) != pin.down) {
			return nil, errors.New("billing recovery and deactivation migrations are not the frozen forward-only identities")
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
	result := make([]migrations.Migration, 0, len(ordered))
	for _, migration := range ordered {
		if migration.Version == generationVersion || migration.Version == intentVersion || migration.Version == pricingReceiptVersion || migration.Version == memberIdentityVersion {
			continue
		}
		result = append(result, migration)
		if migration.Version == 6 {
			result = append(result, *fence)
		}
		if migration.Version == 20 {
			result = append(result, *memberIdentity)
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
	return migrations.UpWithHooks(ctx, connection, ordered, migrations.UpHooks{BeforeApply: prepareBillingMigration, AfterApply: finishBillingMigration, ApplySQL: applyBillingMigration, LedgerRole: "billing_schema_owner"})
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
