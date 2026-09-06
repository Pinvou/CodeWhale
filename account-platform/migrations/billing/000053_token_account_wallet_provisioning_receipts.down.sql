SET ROLE billing_service;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM token_account_wallet_provisioning_receipts) THEN
   RAISE EXCEPTION 'wallet provisioning receipt history exists; keep migration 000053' USING ERRCODE='55000';
 END IF;
END $$;
RESET ROLE;
SET ROLE billing_wallet_receipt_deployer;
DROP TRIGGER token_account_wallet_receipt_immutable ON token_account_wallet_provisioning_receipts;
DROP FUNCTION reject_token_account_wallet_receipt_mutation();
DROP TABLE token_account_wallet_provisioning_receipts;
SET ROLE billing_service;
