package billingschema

import (
	"context"
	_ "embed"
	"errors"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// This resource is the original published 053, not an instruction to record it
// on fresh databases. It contributes to expected history only when already
// installed with its exact original LF/CRLF checksum.
//go:embed retained/000053_token_account_wallet_provisioning_receipts.up.sql
var originalWalletReceiptSQL string

const originalWalletReceiptChecksum = "d442450f15b36db7c8d1b0bcee5a625c605f05f9380bb78f86b2274192f5f7f6"
const originalWalletReceiptCRLFChecksum = "db8e3366c543de2f1805b99df4023f723acc14cc753a58e00fb28c7b67359ba4"
const walletReceiptVersion int64 = 900023
const walletReceiptChecksum = "32d1f3c0f449d2423744a4991bd59b438dfbaaaf33709823a29b9965c1b5cfda"
const walletReceiptDownChecksum = "0d665405ae0c46461d72fc1c37a9b606cc9e2b5c9beeb6952f4b91d9c06422ce"

func validateWalletReceipts(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "token_account_wallet_receipts" || m.Checksum == walletReceiptChecksum) && m.Version != walletReceiptVersion {
			return errors.New("wallet receipt migration has a different frozen version")
		}
		if m.Version == walletReceiptVersion && (m.Name != "token_account_wallet_receipts" || m.Checksum != walletReceiptChecksum || sqlChecksum(m.UpSQL) != walletReceiptChecksum || sqlChecksum(m.DownSQL) != walletReceiptDownChecksum) {
			return errors.New("wallet receipts are not the registered forward-only migration")
		}
	}
	return nil
}

// VerifyApplied keeps the generic exact-set verifier strict. Only the known
// already-installed original 053 is an additional Billing history resource;
// no ledger row is inserted, deleted, renamed or rewritten here.
type walletReceiptHistoryQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Run before any durable migration, and again after the migration lock inside
// BeforeApply. A missing original table never excuses an unknown 053 ledger.
func knownOriginalWalletReceiptHistory(ctx context.Context, source walletReceiptHistoryQueryer) (bool, error) {
	if sqlChecksum(originalWalletReceiptSQL) != originalWalletReceiptChecksum {
		return false, errors.New("original wallet receipt history resource changed")
	}
	var ledger bool
	if err := source.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&ledger); err != nil {
		return false, err
	}
	if !ledger {
		return false, nil
	}
	var checksum string
	err := source.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=53`).Scan(&checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if checksum != originalWalletReceiptChecksum && checksum != originalWalletReceiptCRLFChecksum {
		return false, errors.New("installed wallet receipt 053 is not the original history")
	}
	return true, nil
}

func VerifyApplied(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) (migrations.Verification, error) {
	if err := validateWalletReceipts(loaded); err != nil {
		return migrations.Verification{}, err
	}
	original, err := knownOriginalWalletReceiptHistory(ctx, connection)
	if err != nil {
		return migrations.Verification{}, err
	}
	if !original {
		return migrations.VerifyApplied(ctx, connection, loaded)
	}
	expected := slices.Clone(loaded)
	for _, m := range expected {
		if m.Version == 53 {
			return migrations.Verification{}, errors.New("original wallet receipt history is already present in the package")
		}
	}
	expected = append(expected, migrations.Migration{Version: 53, Name: "token_account_wallet_provisioning_receipts", UpSQL: originalWalletReceiptSQL, Checksum: originalWalletReceiptChecksum})
	slices.SortFunc(expected, func(a, b migrations.Migration) int {
		if a.Version < b.Version {
			return -1
		}
		if a.Version > b.Version {
			return 1
		}
		return 0
	})
	return migrations.VerifyApplied(ctx, connection, expected)
}
