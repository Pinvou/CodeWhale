package billingschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const walletReceiptHardeningChecksum = "b53440b288fab93fd126bc85f76a4574ac926386431f9bab56c8666294ea5216"
const walletReceiptHardeningCRLFChecksum = "f0e5f7db0233b9eb1c8d69385d8395b5f0111a45b2dc42b579e40f0cead160d7"
const billingTruncateGuardBody = "\nBEGIN RAISE EXCEPTION 'billing tables cannot be truncated' USING ERRCODE='55000'; END "

// Check before every pending migration: ordinary AfterApply hardening replaces
// a trigger with this name, so waiting until 900023 would erase its provenance.
func protectExistingWalletReceiptHardening(ctx context.Context, tx pgx.Tx) error {
	owner, err := originalWalletReceiptHardeningOwner(ctx, tx)
	if err != nil || owner == "" {
		return err
	}
	if _, err = tx.Exec(ctx, `SET ROLE billing_schema_owner; LOCK TABLE public.token_account_wallet_provisioning_receipts IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	if _, err = verifiedWalletReceiptTruncateGuard(ctx, tx); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SET ROLE billing_service`)
	return err
}

// Original 053 may already belong to the schema owner after storage recovery.
// Hardening then adds its standard TRUNCATE trigger, while frozen 900023 checks
// the original single-trigger shape. Borrow only this exact installed guard
// inside 900023's transaction; AfterApply re-hardens before the ledger write.
func prepareWalletReceiptValidation(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != walletReceiptVersion {
		return nil
	}
	if err := validateWalletReceipts([]migrations.Migration{m}); err != nil {
		return err
	}
	selected, err := schemaOwnedOriginalWalletReceipt(ctx, tx)
	if err != nil || !selected {
		return err
	}
	if _, err = tx.Exec(ctx, `SET ROLE billing_schema_owner; LOCK TABLE public.token_account_wallet_provisioning_receipts IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	present, err := verifiedWalletReceiptTruncateGuard(ctx, tx)
	if err != nil {
		return err
	}
	if present {
		if _, err = tx.Exec(ctx, `DROP TRIGGER billing_no_truncate ON public.token_account_wallet_provisioning_receipts`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SET ROLE billing_service`)
	return err
}

func verifyWalletReceiptValidationRestored(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != walletReceiptVersion {
		return nil
	}
	selected, err := schemaOwnedOriginalWalletReceipt(ctx, tx)
	if err != nil || !selected {
		return err
	}
	present, err := verifiedWalletReceiptTruncateGuard(ctx, tx)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("wallet receipt hardening guard was not restored")
	}
	return nil
}

func schemaOwnedOriginalWalletReceipt(ctx context.Context, tx pgx.Tx) (bool, error) {
	owner, err := originalWalletReceiptHardeningOwner(ctx, tx)
	return owner == "billing_schema_owner", err
}

func originalWalletReceiptHardeningOwner(ctx context.Context, tx pgx.Tx) (string, error) {
	original, err := knownOriginalWalletReceiptHistory(ctx, tx)
	if err != nil || !original {
		return "", err
	}
	var owner string
	err = tx.QueryRow(ctx, `SELECT coalesce((SELECT pg_get_userbyid(c.relowner) FROM pg_catalog.pg_class c
 WHERE c.oid=to_regclass('public.token_account_wallet_provisioning_receipts')
 AND c.relkind='r' AND pg_get_userbyid(c.relowner) IN ('billing_service','billing_schema_owner')),'')`).Scan(&owner)
	return owner, err
}

func verifiedWalletReceiptTruncateGuard(ctx context.Context, tx pgx.Tx) (bool, error) {
	var present, valid, installed, functionValid, originalValid bool
	var count int
	err := tx.QueryRow(ctx, `WITH reviewed_function AS (
 SELECT p.oid FROM pg_catalog.pg_proc p
 JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 JOIN pg_catalog.pg_language l ON l.oid=p.prolang
 WHERE n.nspname='public' AND p.proname='reject_billing_truncate'
 AND p.prokind='f' AND p.pronargs=0 AND p.prorettype='trigger'::regtype
 AND NOT p.prosecdef AND pg_get_userbyid(p.proowner)='billing_schema_owner'
 AND l.lanname='plpgsql' AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
 AND replace(replace(p.prosrc,E'\r\n',E'\n'),E'\r',E'\n')=$1
 AND EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900003 AND checksum IN ($2,$3))
) SELECT
 (SELECT count(*) FROM pg_catalog.pg_trigger WHERE tgrelid='public.token_account_wallet_provisioning_receipts'::regclass AND NOT tgisinternal),
 EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='public.token_account_wallet_provisioning_receipts'::regclass AND tgname='billing_no_truncate' AND NOT tgisinternal),
 EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t
 WHERE t.tgrelid='public.token_account_wallet_provisioning_receipts'::regclass
 AND t.tgname='billing_no_truncate' AND t.tgtype=34 AND t.tgenabled='O'
 AND NOT t.tgisinternal AND t.tgconstraint=0 AND t.tgparentid=0
 AND t.tgnargs=0 AND octet_length(t.tgargs)=0 AND t.tgqual IS NULL
 AND t.tgfoid IN (SELECT oid FROM reviewed_function)),
 EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900003),
 EXISTS(SELECT 1 FROM reviewed_function),
 EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t
 JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid
 JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 JOIN pg_catalog.pg_language l ON l.oid=p.prolang
 JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid
 WHERE t.tgrelid='public.token_account_wallet_provisioning_receipts'::regclass
 AND t.tgname='token_account_wallet_receipt_immutable' AND t.tgtype=27 AND t.tgenabled='O'
 AND NOT t.tgisinternal AND t.tgconstraint=0 AND t.tgparentid=0
 AND t.tgnargs=0 AND octet_length(t.tgargs)=0 AND t.tgqual IS NULL
 AND n.nspname='public' AND p.proname='reject_token_account_wallet_receipt_mutation'
 AND p.proowner=c.relowner AND p.prokind='f' AND p.pronargs=0 AND p.prorettype='trigger'::regtype
 AND NOT p.prosecdef AND l.lanname='plpgsql' AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
 AND md5(replace(replace(p.prosrc,E'\r\n',E'\n'),E'\r',E'\n'))='2b37a23317572e5d4c07c4dc688a04ad')`, billingTruncateGuardBody, walletReceiptHardeningChecksum, walletReceiptHardeningCRLFChecksum).Scan(&count, &present, &valid, &installed, &functionValid, &originalValid)
	if err != nil {
		return false, err
	}
	if (!present && count != 1) || !originalValid {
		return false, errors.New("wallet receipt shape does not match the original contract: unreviewed hardening guard")
	}
	if (present && (!valid || count != 2)) || (installed && !functionValid) {
		return false, errors.New("wallet receipt has an unreviewed hardening guard")
	}
	return present, nil
}
