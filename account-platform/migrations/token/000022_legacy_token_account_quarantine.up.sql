CREATE TABLE legacy_token_account_quarantines (
    token_account_id UUID PRIMARY KEY REFERENCES token_accounts(id),
    owner_type TEXT NOT NULL,
    owner_id UUID NOT NULL,
    prior_status TEXT NOT NULL,
    credential_count INTEGER NOT NULL CHECK (credential_count >= 0),
    route_count INTEGER NOT NULL CHECK (route_count >= 0),
    quarantine_reason TEXT NOT NULL,
    quarantined_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE legacy_token_credential_quarantines (
    credential_id UUID PRIMARY KEY REFERENCES provider_credentials(id),
    token_account_id UUID NOT NULL REFERENCES legacy_token_account_quarantines(token_account_id),
    prior_status TEXT NOT NULL
);

CREATE TABLE legacy_token_route_quarantines (
    route_id UUID PRIMARY KEY REFERENCES routing_policies(id),
    token_account_id UUID NOT NULL REFERENCES legacy_token_account_quarantines(token_account_id),
    prior_health_status TEXT NOT NULL
);

INSERT INTO legacy_token_account_quarantines(
    token_account_id, owner_type, owner_id, prior_status,
    credential_count, route_count, quarantine_reason
)
SELECT account.id, account.owner_type, account.owner_id, account.status,
       (SELECT count(*) FROM provider_credentials credential WHERE credential.token_account_id=account.id),
       (SELECT count(*) FROM routing_policies route WHERE route.token_account_id=account.id),
       'ORDINARY_TOKEN_MANAGEMENT_RETIRED'
FROM token_accounts account
WHERE account.management_mode='USER';

INSERT INTO legacy_token_credential_quarantines(credential_id, token_account_id, prior_status)
SELECT credential.id, credential.token_account_id, credential.status
FROM provider_credentials credential
JOIN legacy_token_account_quarantines quarantine ON quarantine.token_account_id=credential.token_account_id;

INSERT INTO legacy_token_route_quarantines(route_id, token_account_id, prior_health_status)
SELECT route.id, route.token_account_id, route.health_status
FROM routing_policies route
JOIN legacy_token_account_quarantines quarantine ON quarantine.token_account_id=route.token_account_id;

UPDATE provider_credentials credential
SET status='RETIRED', retired_at=COALESCE(retired_at, clock_timestamp())
FROM legacy_token_account_quarantines quarantine
WHERE quarantine.token_account_id=credential.token_account_id
  AND credential.status='ACTIVE';

UPDATE routing_policies route
SET health_status='UNHEALTHY', updated_at=clock_timestamp()
FROM legacy_token_account_quarantines quarantine
WHERE quarantine.token_account_id=route.token_account_id;

UPDATE token_accounts account
SET status='FROZEN', updated_at=clock_timestamp()
FROM legacy_token_account_quarantines quarantine
WHERE quarantine.token_account_id=account.id;
