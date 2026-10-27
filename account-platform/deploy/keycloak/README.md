# F01 identity provider

This image builds the Java 21 SPI against Keycloak 26.7.0 and includes the
Pinvou password, SMS, registration and password-reset theme in Chinese, English
and Japanese. Identity records remain in Keycloak; Account decides whether the
identified user may log in. Realm roles do not grant Account platform permissions.

Build the provider with `mvn -B -C package` in `mobile-authenticator/`, or build
the supplied Dockerfile. The Java provisioning fixture test also requires the
current Account-produced `account-platform/test/fixtures/provisioning-password-login.json`.
`test/provision_mobile_auth_test.py` exercises the shell with an inert local
CLI double; it does not provision a real realm. Full deployment/E2E validation
is separate from these component tests.

## Platform administrator provisioning

`provision-platform-admin.sh` keeps the existing login-identity and realm-role
workflow; it does not grant Account business permissions. Supply its passwords
through the existing controlled environment, without shell tracing. The pinned
Keycloak 26.7.0 CLI supports `KC_CLI_PASSWORD` for both
[`config credentials`](https://github.com/keycloak/keycloak/blob/26.7.0/docs/documentation/server_admin/topics/admin-cli.adoc#authenticating)
and [`set-password`](https://github.com/keycloak/keycloak/blob/26.7.0/integration/client-cli/admin-cli/src/main/java/org/keycloak/client/admin/cli/commands/SetPasswordCmd.java).
The script supplies each password only to its corresponding invocation, omits
password arguments, and removes the original password variables from CLI child
environments. Sensitive-command errors use fixed diagnostics instead of raw CLI
output. Its temporary token configuration is created with mode 0600 and removed
on success, failure or a handled termination signal. This is not an assurance
against a privileged observer of the process environment or a real deployment
acceptance result.

The local CLI-double tests in `scripts/tests/test_personal_deployment_security.py`
exercise creation, reruns, secret transport and cleanup without connecting to
Keycloak or using real credentials.

## Offline production realm bootstrap bundle

From the repository root, `python3 scripts/account-platform-user-realm.py
--account-origin https://account.example.com` emits a secret-free JSON bundle
containing `realm`, `user_profile` and `provisioner_roles`. It does not connect
to Keycloak, import configuration, create users, assign administrator permissions,
read credentials or generate client secrets. Review it and extract only its
`realm` object for a separately authorized first import into the current pinned
Keycloak image with the Pinvou SPI/theme installed. The wrapper itself is not a
Keycloak realm-import document. Do not overwrite an existing realm with this
initial configuration; use the existing controlled provisioning workflow below
and verify its readback and real sessions.

`--account-origin` is the canonical public HTTPS **Portal** origin and supplies
only its `/auth/callback`. It does not set Keycloak `frontendUrl`. The current
Compose `keycloak-personal.conf.template`/Helm hostname configuration and exact
public user issuer remain authoritative, even when issuer and Portal share a
host. Keep issuer, JWKS, Account/Token verifiers, Desktop and Portal consistent;
this offline generator does not prove that deployment assembly.

Default client IDs match the current updater: `pinvou-desktop`, `pinvou-portal`,
`pinvou-account-provisioner`, optional `pinvou-portal-handoff` and optional
`pinvou-credentials`. The corresponding `--desktop-client`, `--portal-client`,
`--provisioner-client`, `--handoff-client`, `--credential-client` and
`--account-audience` options must exactly match deployed settings. The desktop
retains password/SMS direct grant and `offline_access`; this bundle adds no
desktop browser callback. Ordinary online Portal keeps authorization code plus
PKCE and its normal Keycloak session/refresh lifetime, with no `offline_access`.
`refreshTokenMaxReuse=0` means refresh-token rotation permits zero reuse; it does
not set Portal `refresh_expires_in` or an online session lifetime to zero.

Add `--handoff` only with the existing matching runtime
`PORTAL_SESSION_HANDOFF=true` configuration. It includes the separate confidential
access-only token-exchange client and Desktop audience, with refresh-requested
token exchange disabled. Add `--credential-origin` only when installing F04:
the current credential server permits exactly `https://credentials.pinvou.com`
or `https://credentials-uat.pinvou.com`, distinct from Portal. It receives only
the current `basic`, `roles`, `profile` scopes, its own audience and sole
`/oidc/callback`; ordinary Portal receives `basic`, `roles`, `profile`, `email`.
Omitting either option does not revoke previously installed clients or tokens.
The initial production realm also retains the original explicit identity baseline:
`registrationEmailAsUsername=false`, `loginWithEmailAllowed=false`,
`editUsernameAllowed=true`, `defaultSignatureAlgorithm=RS256`. These do not rely
on Keycloak import defaults. The local development realm has a different email
login flag and is not a production replacement; existing production realms are
not modified by this generator. Managed profile permissions still restrict
username edits to administrators.

No credential or handoff secret is emitted: establish each through the existing
private bootstrap and pass the matching secret files to the online updater.

The generated profile is read from the same bounded literal owned by
`provision-mobile-auth.sh`, including current identity-security/password-reset
fences. The bundle also carries the current realm hardening/audit baseline.
Provisioner role scope mappings are **not** real service-account role assignments:
after installation the existing updater resolves the actual service-account UUID
and assigns only `manage-users`, `view-users`, `query-users` on `realm-management`.
It creates no Account platform administrator; that uses the existing separate
secure Account bootstrap. Never import development users or reuse a workload
realm as the user realm. The current personal edge supports user realm `pinvou`;
a custom `--realm` requires a deployment that already supports that exact realm.

`npm --prefix account-platform run test:user-realm-tools` runs offline generator,
actual CLI and current-producer contract tests with Python 3. They do not import
a real Keycloak realm or validate actual browser login, refresh/logout, handoff,
SPI, issuer, secrets or administrator bootstrap. Record those separately before
production release; no deployment or scheduling is performed by this command.

## Realm and client provisioning

Import `pinvou-dev-realm.json` only for local development. It defines the
`pinvou-desktop-dev` client and the two SPI flows. Production must import its
own desktop client and set `PINVOU_OIDC_CLIENT_ID` to that exact client ID
(normally `pinvou-desktop`). Run `pinvou-provision-mobile-auth.sh` from the
image after Keycloak is available. The script creates or updates the flows,
profile attribute permissions, audience mappers and the clients below.

The script also applies and reads back the realm security baseline: brute-force
protection with temporary lockouts, a 600-second access-token lifetime, refresh
token rotation with zero reuse, and user/admin event retention for 30 days.
Admin event details remain disabled to avoid recording sensitive request bodies.
A failed readback stops provisioning; importing a development realm is not a
production hardening check. Existing sessions and real refresh/logout behavior
must still be verified before releasing the updated realm.

The pinned Keycloak 26.7 CLI reads the administrator password from
`KC_CLI_PASSWORD` only for the login invocation. The script supplies the
truststore password through `KC_CLI_TRUSTSTORE_PASSWORD` for each CLI invocation
that uses the configured private CA; the temporary CLI configuration stores
the truststore path without its password. These variables follow the upstream
[login](https://github.com/keycloak/keycloak/blob/26.7.0/integration/client-cli/admin-cli/src/main/java/org/keycloak/client/cli/common/BaseConfigCredentialsCmd.java)
and [TLS setup](https://github.com/keycloak/keycloak/blob/26.7.0/integration/client-cli/admin-cli/src/main/java/org/keycloak/client/cli/common/BaseAuthOptionsCmd.java)
contracts. Original password and client-secret variables are removed from CLI
child environments, shell tracing is disabled, and sensitive authentication or
truststore failures use fixed diagnostics. No password is passed in CLI arguments.
This protects argument and diagnostic exposure; it does not hide process
environments from privileged observers or constitute real realm acceptance.

| Variable | Meaning |
| --- | --- |
| `KEYCLOAK_REALM` | Realm to update, default `pinvou` |
| `KEYCLOAK_ADMIN_SERVER` | Administration endpoint, default container loopback |
| `KEYCLOAK_ADMIN_USERNAME_FILE`, `KEYCLOAK_ADMIN_PASSWORD_FILE` | Mounted provisioning administrator credentials |
| `KEYCLOAK_ADMIN_TRUSTSTORE`, `KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD_FILE` | Truststore and mounted password for a private CA administration endpoint |
| `KEYCLOAK_PROVISIONER_CLIENT_ID` | Account identity-adapter service client, default `pinvou-account-provisioner` |
| `KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE` | Mounted single-value client secret, 32–4096 printable bytes |
| `PINVOU_OIDC_CLIENT_ID` | Existing desktop client; it must differ from all server/Portal clients |
| `PINVOU_OIDC_AUDIENCE` | Existing Account JWT verifier audience, default desktop client ID |
| `PORTAL_OIDC_CLIENT_ID` | Public online client, default `pinvou-portal` |
| `PORTAL_PUBLIC_URL` | Required external Portal URL; the sole callback is this URL plus `/auth/callback` |
| `PORTAL_SESSION_HANDOFF` | `true` to provision the separate exchange client; default `false` |
| `PORTAL_HANDOFF_OIDC_CLIENT_ID` | Confidential exchange client, default `pinvou-portal-handoff` |
| `PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE` | Required mounted single-value secret when handoff is enabled, 32–4096 printable bytes |

The ordinary Portal uses authorization code and PKCE S256 with no direct grant,
implicit grant, service account, offline access or token exchange. The desktop
retains its approved password/SMS direct grant and offline refresh. The handoff
client uses confidential standard token exchange, has no browser redirect,
direct grant or offline/refresh exchange capability, and receives only an
access token. Its ID is added to the desktop token audience; both Portal token
types carry the Account audience. This follows the Keycloak
[standard token exchange](https://www.keycloak.org/securing-apps/token-exchange)
requester/audience contract. Changing the handoff flag does not revoke an
already-issued token; the Portal endpoint must use the matching runtime flag.

The script configures the identity-adapter client with only `manage-users`,
`view-users` and `query-users` on `realm-management`. It never creates a Pinvou
platform administrator or directly resets that administrator's password. Use
the separate Account secure bootstrap workflow for that purpose.

## SPI service connections

Configure `PINVOU_ACCOUNT_AUTH_URL` and `PINVOU_IDENTITY_AUTH_URL` as HTTP(S)
base URLs, `PINVOU_SMS_VERIFY_URL` as the complete internal SMS verification URL,
and `PINVOU_OIDC_ISSUER` as the same issuer trusted by Account. URLs cannot
contain user information, query strings or fragments. Mount the Keycloak
workload credential at `PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE` with the current
finite SMS-verification and login-check permissions. The local development
`PINVOU_INTERNAL_SERVICE_TOKEN` fallback is not a production workload credential.
No token value belongs in source, command output or logs.

The custom Java HTTP client verifies HTTPS using the JVM truststore and does
not follow redirects. For private CA connections configure the JVM truststore
through mounted files/JVM options; the SPI does not disable certificate or
hostname verification. Browser request headers cannot select a service token
or redirect its destination.

SMS and registration risk source is the Keycloak server's resolved client
connection address, never an arbitrary browser `X-Forwarded-For` or
`X-Pinvou-Client-Source`. Behind a proxy, use Keycloak's
[trusted proxy addresses](https://www.keycloak.org/server/reverseproxy) and an
explicit trusted proxy/header configuration. Without that deployment setup,
the observed address is the immediate peer. The SPA's device identifier remains
a separate bounded risk fact.

Password and registration prechecks, CAPTCHA creation, SMS delivery and
registration all carry that same resolved source and bounded device fact to
their service calls. A browser-supplied source header never replaces the
Keycloak connection address.

Only an incorrect, nonempty password for an identified, enabled and unblocked
user is attributed to Keycloak's password brute-force failure counter.
Unknown users retain the dummy-password verification and generic response;
disabled or blocked users, missing credentials, Account denial and service
failures do not count as incorrect passwords. A new password attempt clears
the previous attempted-user attribution. A new SMS, registration or reset
submission clears an authenticated user only when it matches the SPI's prior
bad-password marker; an unrelated authenticated user is preserved. Rendering
a mode switch also preserves the existing authentication context. The provider
identifies a bad-password user through the real
authentication context as well as the audit event, so the Keycloak failure
processor can attribute the attempt correctly.

The Maven regression suite exercises the Keycloak 26.7.0 result/failure-flow
chain with controlled storage and authority ports, plus real local HTTP
transport checks. Its matching RESTEasy dependency is test-scoped; these
component checks do not replace a deployed realm/identity-service acceptance.

The Portal service must use the same realm, online/handoff client IDs and
handoff secret file, together with `PORTAL_OIDC_ISSUER`, `PORTAL_OIDC_JWKS_URL`
and its authentication/token endpoint URLs. Never substitute the provisioner
client for either user client. Deployment configuration must connect these
producers and consumers; a component test does not prove that assembly.
