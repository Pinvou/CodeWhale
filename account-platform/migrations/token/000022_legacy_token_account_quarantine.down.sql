UPDATE token_accounts account
SET status=quarantine.prior_status, updated_at=clock_timestamp()
FROM legacy_token_account_quarantines quarantine
WHERE quarantine.token_account_id=account.id;

UPDATE provider_credentials credential
SET status=quarantine.prior_status,
    retired_at=CASE WHEN quarantine.prior_status='ACTIVE' THEN NULL ELSE credential.retired_at END
FROM legacy_token_credential_quarantines quarantine
WHERE quarantine.credential_id=credential.id;

UPDATE routing_policies route
SET health_status=quarantine.prior_health_status, updated_at=clock_timestamp()
FROM legacy_token_route_quarantines quarantine
WHERE quarantine.route_id=route.id;

DROP TABLE legacy_token_route_quarantines;
DROP TABLE legacy_token_credential_quarantines;
DROP TABLE legacy_token_account_quarantines;
