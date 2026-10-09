-- First-delivery metadata for unchanged three-scope authorization migration.
-- No future route, command executor or effective capability is installed here.
-- Account's compiled delivered-permission boundary still denies future codes.
DO $phase$
BEGIN
 IF EXISTS(SELECT 1 FROM schema_migrations WHERE version=34) THEN
  IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=34 AND checksum IN ('658324df04b4e19fb98531c192f409fb68b82f5d0f027fe6b65527ebddca8637','7e4ea64626e271cb51b138b945812380e2b0ee4c2ab58300408e67baf66ab5d0'))
     OR to_regprocedure('increment_authorization_scope_version(uuid,text,uuid)') IS NULL
     OR NOT EXISTS(SELECT 1 FROM roles WHERE code='PERSONAL_USER' AND scope='PERSONAL') THEN
   RAISE EXCEPTION 'existing three-scope authority is not the retained migration' USING ERRCODE='55000';
  END IF;
  RETURN;
 END IF;
 IF EXISTS(
  SELECT 1 FROM permissions p
  JOIN (VALUES
   ('platform.account-closures.read','Read redacted account closure state and blockers.'),
   ('platform.account-closures.repair','Retry a failed account closure stage without bypassing blockers.'),
   ('platform.billing.payment-controls.resume.request','Request version-frozen payment Provider recovery.'),
   ('platform.billing.payment-controls.resume.approve','Independently approve payment Provider recovery.')
  ) expected(code,description) ON expected.code=p.code
  WHERE p.scope<>'PLATFORM' OR p.description<>expected.description
 ) THEN
  RAISE EXCEPTION 'first-delivery permission metadata conflicts with retained facts' USING ERRCODE='55000';
 END IF;
 INSERT INTO permissions(code,scope,description) VALUES
 ('platform.account-closures.read','PLATFORM','Read redacted account closure state and blockers.'),
 ('platform.account-closures.repair','PLATFORM','Retry a failed account closure stage without bypassing blockers.'),
 ('platform.billing.payment-controls.resume.request','PLATFORM','Request version-frozen payment Provider recovery.'),
 ('platform.billing.payment-controls.resume.approve','PLATFORM','Independently approve payment Provider recovery.')
 ON CONFLICT(code) DO NOTHING;
END;
$phase$;
