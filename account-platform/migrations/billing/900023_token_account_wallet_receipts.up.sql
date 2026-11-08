-- Install the original Token command receipt contract without rewriting any
-- deployed numeric migration. A genuine original 053 is an explicit compatible
-- predecessor, never a reason to replace historical receipts or their ledger.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=session_user) THEN
  RAISE EXCEPTION 'wallet receipts require the deployment migrator';
 END IF;
END $$;
SET ROLE billing_schema_owner;
CREATE TEMP TABLE pinvou_expected_token_wallet_receipt_900023 (
    command_reference UUID PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK(owner_type IN ('PERSONAL','ORGANIZATION','ORG_UNIT')),
    owner_id UUID NOT NULL,
    request_digest CHAR(64) NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK(status IN ('PENDING','SUCCEEDED','FAILED','UNKNOWN')),
    wallet_id UUID,
    stable_error_code TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 1 CHECK(attempt_count>0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    completed_at TIMESTAMPTZ,
    CHECK(
      (status='PENDING' AND wallet_id IS NULL AND stable_error_code IS NULL AND completed_at IS NULL) OR
      (status='SUCCEEDED' AND wallet_id IS NOT NULL AND stable_error_code IS NULL AND completed_at IS NOT NULL) OR
      (status IN ('FAILED','UNKNOWN') AND wallet_id IS NULL AND stable_error_code IS NOT NULL AND completed_at IS NOT NULL)
    ),
    CHECK(stable_error_code IS NULL OR (stable_error_code ~ '^[A-Z][A-Z0-9_]{0,119}$' AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'))
) ON COMMIT DROP;

DO $$
DECLARE target regclass := to_regclass('public.token_account_wallet_provisioning_receipts');
 expected regclass := 'pg_temp.pinvou_expected_token_wallet_receipt_900023'::regclass;
 expected_columns jsonb; actual_columns jsonb; expected_checks text[]; actual_checks text[];
 owner_name text;
BEGIN
 IF target IS NOT NULL THEN
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=53 AND checksum IN ('d442450f15b36db7c8d1b0bcee5a625c605f05f9380bb78f86b2274192f5f7f6','db8e3366c543de2f1805b99df4023f723acc14cc753a58e00fb28c7b67359ba4')) THEN
   RAISE EXCEPTION 'existing wallet receipt table requires the exact original 053 history';
  END IF;
  SELECT pg_get_userbyid(relowner) INTO owner_name FROM pg_class WHERE oid=target AND relkind='r';
  IF owner_name IS NULL OR owner_name NOT IN ('billing_wallet_receipt_deployer','billing_schema_owner')
   OR pg_has_role('billing_service',owner_name,'MEMBER') THEN
   RAISE EXCEPTION 'legacy wallet receipt ownership is not isolated from runtime';
  END IF;
  SELECT jsonb_agg(jsonb_build_array(attname,atttypid,atttypmod,attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),'')) ORDER BY attnum)
   INTO expected_columns FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
   WHERE a.attrelid=expected AND a.attnum>0 AND NOT a.attisdropped;
  SELECT jsonb_agg(jsonb_build_array(attname,atttypid,atttypmod,attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),'')) ORDER BY attnum)
   INTO actual_columns FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
   WHERE a.attrelid=target AND a.attnum>0 AND NOT a.attisdropped;
  SELECT array_agg(pg_get_constraintdef(oid) ORDER BY contype,pg_get_constraintdef(oid)) INTO expected_checks
   FROM pg_constraint WHERE conrelid=expected;
  SELECT array_agg(pg_get_constraintdef(oid) ORDER BY contype,pg_get_constraintdef(oid)) INTO actual_checks
   FROM pg_constraint WHERE conrelid=target AND contype<>'f';
  IF actual_columns IS DISTINCT FROM expected_columns OR actual_checks IS DISTINCT FROM expected_checks
   OR (SELECT count(*) FROM pg_constraint WHERE conrelid=target AND contype='f')<>1
   OR NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=target AND contype='f' AND conkey=ARRAY[6]::smallint[]
    AND confrelid='public.wallets'::regclass AND confkey=ARRAY[1]::smallint[] AND convalidated
    AND NOT condeferrable AND confupdtype='a' AND confdeltype='a' AND confmatchtype='s') THEN
   RAISE EXCEPTION 'legacy wallet receipt shape does not match the immutable original contract';
  END IF;
  IF (SELECT count(*) FROM pg_trigger WHERE tgrelid=target AND NOT tgisinternal)<>1
   OR NOT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
    WHERE t.tgrelid=target AND t.tgname='token_account_wallet_receipt_immutable' AND t.tgtype=27 AND t.tgenabled='O'
     AND n.nspname='public' AND p.proname='reject_token_account_wallet_receipt_mutation' AND pg_get_userbyid(p.proowner)=owner_name AND NOT p.prosecdef
     AND p.pronargs=0 AND p.prorettype='trigger'::regtype AND p.proconfig=ARRAY['search_path=pg_catalog']
     AND md5(replace(replace(p.prosrc,E'\r\n',E'\n'),E'\r',E'\n'))='2b37a23317572e5d4c07c4dc688a04ad') THEN
   RAISE EXCEPTION 'legacy wallet receipt immutable guard does not match the original contract';
  END IF;
  IF NOT has_table_privilege('billing_service',target,'SELECT') OR NOT has_table_privilege('billing_service',target,'INSERT')
   OR has_table_privilege('billing_service',target,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
   OR EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) a WHERE c.oid=target AND a.grantee=0) THEN
   RAISE EXCEPTION 'legacy wallet receipt runtime privileges are not append-only';
  END IF;
 ELSE
  CREATE TABLE public.token_account_wallet_provisioning_receipts
   (LIKE pg_temp.pinvou_expected_token_wallet_receipt_900023 INCLUDING ALL);
  ALTER TABLE public.token_account_wallet_provisioning_receipts
   ADD FOREIGN KEY(wallet_id) REFERENCES public.wallets(id);
  CREATE FUNCTION public.reject_token_account_wallet_receipt_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$
BEGIN RAISE EXCEPTION 'wallet provisioning receipt is immutable' USING ERRCODE='55000'; END $guard$;
  CREATE TRIGGER token_account_wallet_receipt_immutable BEFORE UPDATE OR DELETE ON public.token_account_wallet_provisioning_receipts
   FOR EACH ROW EXECUTE FUNCTION public.reject_token_account_wallet_receipt_mutation();
  REVOKE ALL ON public.token_account_wallet_provisioning_receipts FROM PUBLIC,billing_service;
  GRANT SELECT,INSERT ON public.token_account_wallet_provisioning_receipts TO billing_service;
 END IF;
END $$;
SET ROLE billing_service;
