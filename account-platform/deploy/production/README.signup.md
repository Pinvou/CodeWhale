# F07 signup and registration gift

This overlay extends the existing verified personal, finance, model and invocation
Compose deployment; use the same Compose `-f` apply flow and append
`deploy/production/compose.signup.yaml`. Account/Billing, their public migrations,
and `TOKEN_PORTAL_IMAGE` must use the same reviewed F07 source. Build the Portal
with the existing `npm run build:account-portal` command so its canonical bundle
includes the three-language signup policy form and its exact GET/PUT allowlist.
It does not publish another listener or
change the HTTPS/OIDC/PKCE edge, projected identity directories, CA or isolation.

Keep `REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64` in the existing private
Account env file: strict padded standard Base64 of exactly 32 bytes, key id
`registration-bonus-v1`. Do not log, render into tracked files or rotate this key
as an ordinary credential: permanent first-registration eligibility is bound to
its verifier. Existing registration-query and SMS HMAC keys, Aliyun or generated
HTTP SMS provider settings remain the current Account private config. SMS HTTP
must use HTTPS in production. The existing Identity password handle/username
policy and Keycloak SMS authenticator are used; no additional login mode is added.

Supply full verified current incoming arrays through `ACCOUNT_WORKLOAD_RULES_JSON`
and `BILLING_WORKLOAD_RULES_JSON`, preserving every currently installed exact rule.
Within the Account array add only `internal.registration-bonus-eligibility.read`
to the existing verified Billing subject/service `billing-service`. Within Billing
add only `internal.billing.registration-bonus.provision` to the existing verified
Account subject/service `account-service`. The existing Account wallet.ensure
permission stays required for receipt owner verification. No wildcard permission,
new subject, consumer role, closure/reclaim permission or private value is needed.
The arrays are actual `WORKLOAD_IDENTITY_RULES_JSON` inputs in this overlay; a
missing projected identity or omitted permission fails closed in production.

Account configures the fingerprint and prepares permanent historical eligibility
before accepting registration traffic. BLOCKED historical readiness remains visible
and prevents bonus policy enablement; unavailable preparation fails startup.
Upgrade in this order: stop registration writers and wallet workers; install the
complete Account/Billing F07 migrations using their existing migration identities;
provide the permanent fingerprint key and exact incoming workload rules; start
Account to finish its eligibility backfill; then start Billing and the existing
wallet workers before restoring registration traffic. Original Account 087 retains
its registered checksum; 900004 is delivered with it and validates required job
facts. No applied migration or historical eligibility row is rewritten.
Missing or invalid key material prevents a mobile-enabled Account from starting.
A reused key id with different key bytes also fails closed. Restore the original
private key from the operator's approved secret backup; do not generate a new key,
rename the key id, erase eligibility, or enable the policy to bypass this check.
If historical anonymized registrations make readiness BLOCKED, new registrations
still consume their permanent eligibility and receive ordinary wallet provisioning
without a gift. Policy activation remains unavailable until the historical issue
is resolved with trustworthy evidence; this deployment supplies no manual override.
Billing queries Account readiness using its fresh projected credential. The bonus
policy starts disabled. GET/PUT `/admin/v1/registration-bonus-policy` require the
current read/manage permission; PUT also requires existing recent authentication,
AuditOnly policy, expected revision, reason and an immutable actor-bound key.

Registration freezes eligibility and its fingerprint, not a Billing policy
snapshot. The first successful Billing provisioning transaction uses the active
policy at that time and freezes its revision and outcome in the claim/receipt.
A wallet-worker delay can therefore make a later policy apply to a registration
that has not yet received its first receipt. Replays retain the original receipt
even after a policy change. Disabled and zero-amount outcomes also permanently
consume that registration's eligibility; enabling or increasing the policy later
does not grant the same fingerprint a second opportunity.

Existing public SMS/captcha/register flows freeze the operation, username/mobile,
challenge, password-policy version and eligibility fingerprint before provisioning.
`X-Registration-Query-Token` is scoped to one operation and is read-only: querying
does not consume SMS, grant a gift or repeat Identity creation. Pending responses
remain PROVISIONING/recovery states. The lease-fenced wallet worker confirms exact
Billing gift receipt plus PERSONAL wallet owner; unknown ACK stays retryable with
the same operation. Duplicate retries do not issue another gift. Ordinary Bootstrap
uses NOT_APPLICABLE and receives no registration gift. Success clears temporary job
fingerprint data, while the permanent eligibility/claim/receipt facts remain.
