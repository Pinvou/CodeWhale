# Account Platform Helm Chart

This chart deploys Account Service, Billing Service, Token Gateway, Token Portal,
Keycloak, and Redis. NATS, PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

### Account governance expectations

Account retains its default models assembly, including base/staging installs with
optional governed workloads disabled. Its Token governance reader requires both
expected registry digests before startup. In the existing `global.runtimeSecret`,
supply `token-governance-policy-set-digest` and `token-governance-handler-set-digest`.
Only Account receives these two required Secret key references; there is no
literal fallback or new Secret resource. When ExternalSecret is enabled, add these
fields to the existing `externalSecrets.remoteKey` object: its existing dataFrom
extract synchronizes them into the same runtime Secret. Otherwise provision the
same keys out of band using the existing runtime Secret process before rollout.

Use the independently verified same-source release plan's
`candidate.policy_set_digest` / `candidate.handler_set_digest`, bound to the
Account/Token commit, images and build inputs. These are registry digests, not
random secrets or activation evidence. For a fresh local source calculation, see
the [temporary offline Go procedure](../../compose/README.md#account-治理摘要输入);
it calls the existing exported functions and does not install governance or
approve a release. PRE_ACTIVATION's empty installed digests are not expected
digests. Preserve default models, existing personal deployments and all runtime
checks; do not activate governance merely to obtain configuration. Rendering only
checks references, not live Secret synchronization, digest correspondence or Pod
readiness. Verify those through the existing release/site acceptance process.

### NATS prerequisite for every profile

Billing Service and the Account/Billing event relays always require provisioned
JetStream resources and explicit per-service authentication over TLS. This is
also true for base/staging installs with all optional governed workloads disabled.
The legacy bundled unauthenticated NATS cannot satisfy that runtime contract;
rendering now rejects it before installation instead of producing crashing Pods.
Do not weaken the runtime authentication or provisioning checks to install it.

In a site-specific values file, set `nats.enabled=false`, supply the approved
`dependencies.natsUrl` as a single `tls://<approved-host>:4222` URL without URL
credentials, path, query or fragment, and supply the HTTPS
`dependencies.natsMonitorUrl`. Set `external.natsCidrs` and
`external.natsMonitorCidrs` to the reviewed destinations for the existing
NetworkPolicies. Base/staging example values intentionally do not invent an
external cluster or credential; these operator inputs are required before a
successful render/install. Existing valid external TLS configurations continue
to work, including the dedicated production and governed-workload profiles.

Before rollout, provision the existing least-privilege per-service NATS credentials,
their matching server-side authentication, streams and durable consumers as
described in [the NATS provisioning runbook](../../runbooks/nats-resources.md).
Populate the existing runtime Secret credential keys and the Account approval
relay public-artifact ConfigMap; mounting client credentials alone does not
configure server authentication. Render and review the exact site values, then
perform the separately authorized TLS/authentication/resource and network
reachability checks. Rendering does not prove those external resources exist.

### Private NATS credential files

The unchanged NATS runtime reader rejects projected symlinks and group/world-readable
credential files or parent directories. Each NATS client uses a narrow
`prepare-nats-credentials` init mode in the same service image, running as UID/GID
65532 with fsGroup 65532. Only that init receives its one projected Secret key
(0440); it preserves and validates the complete multi-line JWT/NKey credential,
then atomically creates a real 0400 file in a 0700 private child directory of a
Memory `emptyDir`. The main container mounts only that memory output read-only;
it does not mount the original NATS Secret projection. No root container or
runtime permission/symlink exception is used.

The existing Secret keys stay `billing-command-executor.creds`,
`account-event-relay.creds`, `billing-event-relay.creds` and the independent
`account-approval-relay.creds`. With governed workloads enabled, the existing
`account-service.creds`, `token-gateway.creds` and
`token-command-executor.creds` keys use the same preparation. Each preparation
receives only its own key; Account's ordinary and approval channels retain
separate files and init steps. Runtime credential paths are now
`/run/pinvou/nats/private/<client>.creds`. The service image must support this
init command before chart rollout; an older image fails closed.

An init retry accepts an already published file only if it is private, valid and
byte-identical to the current projection; it never replaces a credential. Secret
rotation requires recreating the Pod and its memory volume. Rendering verifies
wiring only: confirm real cluster ownership/readability, server TLS/authentication,
ACLs and all existing resource-configuration inputs separately before rollout.
Credential preparation alone does not establish complete service readiness.

### Billing approval consumer public artifacts

Billing always loads all six `BILLING_APPROVAL_CONSUMER_*` inputs before opening
its broker connection, including base/staging installs with governed workloads off.
Set `billing.approvalConsumerArtifactsConfigMap` to the reviewed, existing public
ConfigMap in the release namespace (default name:
`pinvou-account-platform-billing-approval-artifacts`). It must contain exactly the
three referenced public JSON keys below. The chart neither creates these artifacts
nor initializes consumers, captures a watermark, signs a receipt or activates an
approval controller.

| Bootstrap input suffix | Fixed file / ConfigMap key | Independent key in `global.runtimeSecret` |
| --- | --- | --- |
| `PUBLIC_KEYS_FILE` / `PUBLIC_KEYS_SHA256` | `/run/pinvou/approval-identities/current.json` / `current-v2.json` | `billing-approval-consumer-public-keys-sha256` |
| `MANIFEST_FILE` / `MANIFEST_SHA256` | `/run/pinvou/approval-manifest/manifest.json` / `manifest.json` | `billing-approval-consumer-manifest-sha256` |
| `RECEIPT_FILE` / `RECEIPT_SHA256` | `/run/pinvou/approval-receipt/receipt.json` / `receipt.json` | `billing-approval-consumer-receipt-sha256` |

The prefix for every suffix in this table is `BILLING_APPROVAL_CONSUMER_`.
Each SHA must be an independently verified lowercase 64-character SHA-256 value
supplied by the deployment authority through the existing runtime Secret process.
There are no digest defaults in values and no startup self-hashing trust fallback.
Provision the Ready ConfigMap and all three required Secret keys before Billing
rollout. Missing keys prevent container startup; invalid digests, missing files or
content drift fail the unchanged bootstrap/readiness checks.

Follow [the NATS provisioning runbook](../../runbooks/nats-resources.md) with the
same fixed-source `approval-resources` operator. The manifest must be its complete
current registry output, and the separately captured, reviewed receipt must bind
all current `approvalconsumer.BillingCommands()` durables to their exact Q+1 start.
Do not use the runbook's historical three-consumer prose as the current inventory,
truncate the manifest/receipt, or regenerate evidence to disguise retained-resource
drift. Only the existing explicit operator procedure can establish live cutover
facts; a ConfigMap or successful Helm render does not prove resource readiness.

The three public files are 0444, read-only file `subPath` mounts in three separate
directories, isolated from private credentials. The identity directory contains
only `current.json`; a whole ConfigMap directory would expose projection symlinks
and unrelated files and is rejected by the reader. `subPath` snapshots do not
hot-reload. Rotate a matched artifact set and its independently controlled digest
keys in a controlled rollout that recreates Pods, then verify actual per-instance
readiness and broker/control state. Preserve prior cutover evidence and history.

Install External Secrets Operator and create the least-privilege referenced
`SecretStore` or `ClusterSecretStore` before install. Production values create
an `ExternalSecret` that owns the runtime Secret; non-production environments
may create that Secret out of band. It must contain separate
database URLs (`account-service-database-url`, `billing-service-database-url`,
`token-gateway-database-url`, and Keycloak database keys), the internal service
token, payment adapter URL/HMAC keys, the environment-specific external
secret-provider material, `token-default-provider-api-key`, and the New API
reconciliation credentials (`token-reconciliation-newapi-admin-token` and
`token-reconciliation-newapi-admin-user-id`). Mobile registration additionally
requires `registration-query-hmac-key-base64`, isolated from the Identity password
and SMS verification HMAC keys. Production authentication risk admission also
requires an independent `auth-risk-hmac-key-base64`. Controlled audit export
requires `audit-export-master-key-base64`, the standard Base64 encoding of 32
independent random bytes; keep it in the external secret store, separate from
registration, authentication, payment, and model keys. Account human-operation
delegations additionally require `human-delegation-signing-key-id` and an isolated
Ed25519 seed/private key in unpadded Base64URL as
`human-delegation-signing-key-base64url`; publish only the derived public key to
authorized consumers as `human-delegation-verification-keys-json` and overlap key IDs
during rotation. The Account-internal owner directory additionally requires an
independent 32-byte standard-Base64 `owner-candidate-cursor-hmac-key-base64`;
rotate it only after the 15-minute cursor TTL has drained, because old cursors
fail closed. The exact public OIDC token
path is routed through Identity Adapter and fails closed when its external Redis
or Keycloak dependency is unavailable. Never place those values in a
values file or rendered manifest.

Production-new deliberately excludes Payment Simulator and Mock New API. Use the
dedicated production values only after the real payment adapter, real New API,
and external KMS/Vault gates pass. The chart validation rejects local secret
providers, local issuers, disabled NetworkPolicy, and simulator callback URLs.
It also rejects production without an enabled ExternalSecret, configured TLS
Secret, and HTTPS public URLs. During key rotation, keep the previous key
decryptable, update `tokenGateway.secretKeyId`, and wait until every production Token
Gateway readiness probe succeeds before disabling the previous key. Production
rotation readiness requires zero old-key ciphertext. After disabling the old key, recheck `/health/ready` on every
Token Gateway instance and archive the scrubbed per-instance status, observation
time, and release identity with the release record. These checks do not replace
the separate external KMS/Vault, payment, and model-provider release evidence.
Registry names, provider hosts, and CIDRs in the production values file are
documentation examples; replace every one with the reviewed deployment values
before rendering a release manifest.

### User realm prerequisite

The chart starts Keycloak but does not import or create the `pinvou` user realm.
The three SQL migration hooks do not provision it, and
`keycloak.realmUpgradeEnabled` updates an **existing** realm and desktop client;
it is not a first-install bootstrap switch. Keycloak's management readiness
probe alone does not establish that application login or JWKS is available.

The realm updater accepts `oidc.audience` and `oidc.portalClientId` only as
nonempty ASCII letters, digits, dots, underscores and hyphens. The chart rejects
other characters during schema validation, before rendering resources. This is
the updater's existing alphabet, not the offline generator's full identifier
contract: that generator additionally requires an alphanumeric first character
and at most 64 characters. Follow each tool's applicable constraints.

Existing values outside the updater alphabet now fail rendering. Operators must
review their identity/client compatibility and plan any required configuration
transition separately before release; this chart does not migrate or rename
realm data. Do not bypass the schema or widen the updater to force installation.

For `production-new`, keep `keycloak.realmUpgradeEnabled=true` as required by
its schema. Complete this independently authorized bootstrap **before
`helm install`**; missing prerequisites or readback must stop installation.
The default `keycloak.enabled=true` bundled topology remains unchanged.

1. Bind the private bootstrap instance to the same existing PostgreSQL database
   and reviewed `images.keycloak.repository:tag` image as the release. Use this
   repository's SPI/theme image and `KC_DB=postgres`. Its `KC_DB_URL`,
   `KC_DB_USERNAME` and `KC_DB_PASSWORD` must match `global.runtimeSecret` keys
   `keycloak-database-url`, `keycloak-database-username` and
   `keycloak-database-password`. Obtain them from their existing controlled
   source; the chart-owned ExternalSecret may not exist before installation.
   Credentials remain in private configuration, not arguments or logs.
2. Only for an independently verified fresh database with no retained `pinvou`
   realm, review the
   [offline production realm bootstrap bundle](../../keycloak/README.md#offline-production-realm-bootstrap-bundle).
   Match its client IDs, audience and Portal origin to the release values;
   extract only its `realm` object to a controlled `pinvou-realm.json` file.
   With no other instance using that fresh database, mount it read-only at
   `/opt/keycloak/data/import/pinvou-realm.json` in the approved image. Apply the
   existing private first-bootstrap administrator, hostname and TLS settings,
   then run `/opt/keycloak/bin/kc.sh start --optimized --import-realm` in an
   independently controlled private instance, without exposing application
   traffic. The pinned
   [Keycloak startup import](https://github.com/keycloak/keycloak/blob/26.7.0/docs/guides/server/importExport.adoc)
   skips an existing realm; a skip is not evidence that its state is correct.
   The bundle wrapper and development realm are not production import documents.
3. On that private instance, complete the existing
   [realm/client provisioning and readback](../../keycloak/README.md#realm-and-client-provisioning)
   with the controlled administrator/client-secret files. Explicitly match
   desktop/Portal client IDs, audiences and origin to the release values; verify
   the `pinvou` realm, desktop client and provisioning readback. Stop only the
   temporary instance, retaining the same database and evidence. Only then
   install the release with the hook enabled. Its `post-install,post-upgrade`
   updater consumes runtime Secret keys `keycloak-realm-upgrade-admin-username`,
   `keycloak-realm-upgrade-admin-password` and `keycloak-provisioner-client-secret`
   from the corresponding controlled bootstrap inputs. It updates an existing
   realm and does not replace this pre-install import/readback.

Retained and external Keycloak realms keep their existing controlled update and
readback workflow: do not run a fresh import, recreate the database, overwrite
retained realms or import development users. No application traffic may be
exposed before the handoff and acceptance below are complete.

Before enabling application traffic, verify that the public discovery issuer
exactly matches `oidc.issuer`, the configured JWKS endpoint serves the intended
realm, and the deployed desktop/Portal clients, audiences and secret files match
the provisioning readback. Complete real login, refresh and logout acceptance;
neither a successful Helm render nor a healthy Keycloak process proves this.
Keep credentials in the existing controlled files and out of values, argv and
logs. TLS, workload identities and egress prerequisites below remain required.

### Built-in Kubernetes workload JWKS

The exact `workloadIdentity.jwksUrl=https://kubernetes.default.svc/openid/v1/jwks`
selects authenticated Kubernetes API key retrieval for Account, Identity, Billing
and Token Gateway. The configured `workloadIdentity.issuer` must match the actual
cluster ServiceAccount token issuer and signing keys. Changing this value does
not change Kubernetes TokenRequest's issuer. Independently verify the cluster's
configured issuer, projected tokens and serving certificate SAN/chain before
installation; do not substitute an unrelated Keycloak or external IdP issuer.

Each verifier receives the namespace's existing `kube-root-ca.crt` ConfigMap as
a read-only regular `subPath` CA file, isolated from public OIDC/payment roots.
Verify that its `ca.crt` actually trusts the API server certificate. CA replacement
requires a controlled Pod rollout. A separate read-only projected directory holds
a 600-second API credential; its projection deliberately omits `audience` so
Kubernetes chooses the API server's default API audience. The existing internal
business-audience token remains separate. The client reopens each current atomic
projection during key fetches, rejects missing/malformed/expired or mismatched
issuer/ServiceAccount credentials, and does not cache the API token. Local
credential failures reject the fetch without permanently disabling the verifier:
the same middleware retries with the current projection on a later request.
Local credential shape/time checks do not replace API-server signature and audience
authentication. Credentials exceeding the existing 15-minute bound are rejected.

The chart binds only these four ServiceAccounts to `GET /openid/v1/jwks` using
an exact non-resource ClusterRole/Binding. The dedicated client permits only this
fixed HTTPS URL and GET, disables ambient proxies, refuses redirects (including
discovery), and never sends Kubernetes credentials to an external JWKS URL.
It retains go-oidc's shared trusted-key cache; caller cancellation stops its wait,
while a shared fetch remains bounded by the existing ten-second client timeout.
A failed refresh cannot invent keys or grant business permissions.

With NetworkPolicy enabled, explicitly supply `external.workloadJwksCidrs` and
`workloadIdentity.kubernetesApiServerPorts` for the actual API/CNI DNAT path;
missing inputs fail rendering. Use reviewed bounded CIDRs and the precise TCP
port set, for example 443 or 6443 only when that is the site's actual route.
The chart does not infer API IPs, enable an all-address rule, or automatically
add 443 to a different selected port set. Verify runtime egress and RBAC; render
success is not a cluster connectivity or rotation acceptance result.

Other JWKS URLs retain the existing external/public-key endpoint contract and
receive neither the Kubernetes credential/CA mounts nor this RBAC binding. The
endpoint must still verify the actual projected token's issuer and signing keys;
a public URL alone does not prove that chain. Existing Compose transports are
unchanged. NATS/Redis private-CA support is not added by this workload-only change.
The Kubernetes projection/default-audience contract is documented in the
[official projected-volume guide](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken-projected-volumes).

### Identity Keycloak backchannel

When mobile authentication is enabled in a `production*` environment, Identity
requires direct HTTPS admin/token endpoints, a private client-secret file, and a
public CA file. The existing runtime checks remain mandatory. For bundled
Keycloak, supply `keycloak.tlsSecretName` (a Ready TLS Secret with `tls.crt` and
`tls.key`) and `keycloak.caConfigMap` (a Ready ConfigMap with `ca.crt`). The server
certificate must cover the exact rendered `<fullname>-keycloak` Service
DNS name and chain to that CA. This is separate from public ingress TLS. Identity
connects directly to that Service on 8443; public token requests retain the
Identity proxy. Existing ingress/backchannel HTTP consumers retain their routes;
the management interface explicitly retains HTTP on 9000 for the existing probes.

With external Keycloak (`keycloak.enabled=false`), also supply
`keycloak.adminApiUrl` and `keycloak.provisionerTokenUrl`. Use an HTTPS admin origin
and its same-origin `/realms/pinvou/protocol/openid-connect/token` endpoint,
without credentials, query, fragment or encoded path. These must reach Keycloak
directly, bypassing the public Identity token proxy; reusing the public proxy URL
would create a loop. The CA is still required. Review the actual endpoint IP/port
against the site's approved egress rules; configuring TLS does not grant egress.

The existing runtime Secret retains `keycloak-provisioner-client-secret`. A narrow
init mode in the same Identity image reads only that projected key and writes a
complete, owner-only 0400 real file into a Memory `emptyDir`, as UID/GID 65532.
The main container mounts that private volume read-only and receives only its
path. This avoids both Kubernetes projected symlinks and root-owned 0400 files
that the non-root runtime cannot read. The public CA uses a read-only file
`subPath`; no CA or secret bytes enter values, argv or logs. Secret/CA replacement
requires a controlled rollout that recreates Pods: the prepared file and CA
subPath are snapshots; restarting only the main container does not refresh them.
Prepare the capable Identity image before the chart rollout and reference it in
that rollout; older images do not implement the init mode. Provision TLS/CA resources
first, render and inspect both built-in/external forms, then perform the separately
authorized rollout and real Keycloak/issuer/connectivity acceptance. Rendering
does not prove Kubernetes ownership, certificate issuance or cluster reachability.

The existing `internal-and-approved-egress` policy selects ordinary release
pods, including `identity-service`, except pods marked as the New API data plane
or restricted Token control. Its TCP 443 destinations are the union of
`external.paymentProviderCidrs`, `external.secretProviderCidrs`,
`external.modelProviderCidrs`, and `external.telemetryCollectorCidrs`; these lists
do not isolate destinations by calling service. `external.oidcCidrs` is used by
the Portal policy and alone does not grant Identity an external Keycloak route.
Before enabling real SMS for Account Service or external Keycloak for Identity, review the rendered
selectors and ensure the endpoints' actual IPs and ports are covered by the
approved egress configuration. Same-release Keycloak uses the existing internal
pod route. An uncovered endpoint remains denied: do not disable NetworkPolicy,
use an all-address CIDR, or assume an HTTPS URL grants connectivity. If the site
cannot represent its destinations within its approved configuration, stop the
rollout for network review. Rendering is not a CNI or provider connectivity test.

`identity-service` also belongs to the dedicated `workload-verifiers` bucket,
which grants TCP 443 only to `external.workloadJwksCidrs` for its remote workload
key set. Keep those reviewed CIDRs distinct from ordinary approved-HTTPS lists;
the selector is scoped to the same release. Verify the complete rendered policy
union and real CNI reachability before rollout.

When `newApi.enabled=true` (the default), rendering also requires the New API TLS
Secret name, public CA ConfigMap name, and approved TLS image reference. Supply
`newApi.tlsSecretName`, `newApi.caConfigMap`, `images.newApiTLS.repository`, and
`images.newApiTLS.digest` explicitly. The ingress TLS Secret and native KMS CA
are different resources and do not satisfy these inputs. Build/review the
`newapi-tls` image and provision Ready resources as described in
[the New API TLS contract](README.newapi-status.md); use its actual immutable
SHA-256 digest, never the all-zero test fixture digest. Rendering cannot establish
that those resources or the image exist.

With reviewed operator inputs in the shell environment, validate the production
values and **actually render** them (do not print or supply private keys):

```sh
: "${NEWAPI_TLS_SECRET_NAME:?set the Ready TLS Secret name}"
: "${NEWAPI_CA_CONFIGMAP_NAME:?set the Ready public CA ConfigMap name}"
: "${NEWAPI_TLS_IMAGE_REPOSITORY:?set the approved image repository}"
: "${NEWAPI_TLS_IMAGE_DIGEST:?set the approved sha256 image digest}"
helm lint account-platform/deploy/helm/account-platform \
  -f account-platform/deploy/helm/account-platform/values-production-new.yaml \
  --set-string newApi.tlsSecretName="$NEWAPI_TLS_SECRET_NAME" \
  --set-string newApi.caConfigMap="$NEWAPI_CA_CONFIGMAP_NAME" \
  --set-string images.newApiTLS.repository="$NEWAPI_TLS_IMAGE_REPOSITORY" \
  --set-string images.newApiTLS.digest="$NEWAPI_TLS_IMAGE_DIGEST" \
  --set-string keycloak.tlsSecretName="${KEYCLOAK_TLS_SECRET_NAME:?set the Ready internal TLS Secret name}" \
  --set-string keycloak.caConfigMap="${KEYCLOAK_CA_CONFIGMAP_NAME:?set the Ready Keycloak public CA ConfigMap name}"
helm template pinvou account-platform/deploy/helm/account-platform \
  -f account-platform/deploy/helm/account-platform/values-production-new.yaml \
  --set-string newApi.tlsSecretName="$NEWAPI_TLS_SECRET_NAME" \
  --set-string newApi.caConfigMap="$NEWAPI_CA_CONFIGMAP_NAME" \
  --set-string images.newApiTLS.repository="$NEWAPI_TLS_IMAGE_REPOSITORY" \
  --set-string images.newApiTLS.digest="$NEWAPI_TLS_IMAGE_DIGEST" \
  --set-string keycloak.tlsSecretName="$KEYCLOAK_TLS_SECRET_NAME" \
  --set-string keycloak.caConfigMap="$KEYCLOAK_CA_CONFIGMAP_NAME" > rendered.yaml
```

The base, staging and production values intentionally omit these environment-owned
inputs and fail closed until supplied. `helm lint` alone is insufficient: it can
report a template `required`/`fail` diagnostic as INFO and still exit zero.
Require `helm template` to succeed and inspect its manifest before proceeding.

Migration hooks run before ordinary chart resources. Before the first install,
the deployment operator must provision the Secret named by `global.runtimeSecret`
through the existing external secret mechanism and wait until it is Ready in the
release namespace. The ordinary chart `ExternalSecret` is not a prerequisite hook
and cannot supply this Secret during that first pre-install phase. Before upgrades,
keep that existing Secret available while hooks run. Verify key presence without
printing values: the three `account-migration-database-url`,
`billing-migration-database-url`, and `token-migration-database-url` keys are required.
For production-new, also require `migration-backup-evidence-id`,
`migration-backup-completed-at`, and `migration-backup-sha256`; the migration binary
validates the backup age and checksum before connecting. When policy bootstrap is
enabled, also require `token-policy-database-url`, `token-policy-max-per-owner`,
`token-policy-expected-version`, `token-policy-idempotency-key`, and
`token-policy-reason`. Missing required Secret keys prevent a hook container from
starting; missing/invalid production backup values are rejected by the binary.
Rendering does not prove that an external Secret exists or is Ready.

The migration image contains `/service` built with `SERVICE=migrate`. Policy bootstrap
uses the separate `images.tokenPolicyBootstrap` image built with
`SERVICE=token-policy-bootstrap`; its default `/service` entrypoint receives
`--mode bootstrap`. Do not point this value at the migration-only image.

The dedicated `-migrations` ServiceAccount is created by a weight -40 prerequisite
hook and retained through the release; it is separate from the ordinary runtime
ServiceAccounts. Migration and bootstrap Pods use `global.imagePullSecrets` and
do not mount ServiceAccount tokens. Account, Billing, Token migrations retain
weights -30, -20, -10, followed by policy bootstrap -5. This preserves migration
before application rollout on upgrades; no runtime ServiceAccount is deleted.

Migration Jobs are pre-install/pre-upgrade hooks. Rollback does not delete PVCs;
database rollback remains an explicit, separately approved operation.

Token Account provisioning additionally requires separate secret keys
`account-migration-database-url`, `billing-migration-database-url`,
`token-migration-database-url`, and `token-policy-database-url`. The latter uses
the policy operator login; migration URLs use the domain migrator login, never
the runtime login. In particular, `account-migration-database-url` must identify
`account_migrator`, while the Account Job keeps `MIGRATION_ROLE=account_service`
and must never receive the `account_service` runtime credential. Provision
`account_approval_control_owner NOLOGIN NOINHERIT`, the low-privilege
`account_migrator LOGIN NOINHERIT`, their reviewed membership, and the temporary
Account-database `CREATE` grant before the Job runs; migration `000050` revokes
that database grant after creating the protected schema. Existing volumes need
this DBA preparation explicitly because changing a Secret or bootstrap script
does not alter existing roles. Policy bootstrap also requires `token-policy-max-per-owner`,
`token-policy-expected-version`, `token-policy-idempotency-key`, and
`token-policy-reason`; production has no implicit capacity default.
Each deployment login must have an independent credential, different from all
runtime and other deployment credentials. Never grant schema-owner membership
to runtime roles. The bootstrap command uses the policy operator URL; its
`--mode readiness` instead needs a runtime read-only URL with SELECT access,
and `--mode quiescence` needs the separately controlled inspection identity below.

Before the hooks, a DBA must assign the Token database and its `public` schema
to `token_schema_owner NOLOGIN`, and the Billing database/schema to
`billing_schema_owner NOLOGIN`. Only the corresponding migrator is a member of
its schema owner; runtime and policy-operator logins must not own the database or
schema or be members of those owner roles. Existing business-table ownership is
retained. For first installation, provision Token's `pgcrypto` extension as DBA;
grant runtime schema USAGE/CREATE only for the earlier migrations. Token 000029
and Billing 900003 runtime-ownership hardening revoke runtime/PUBLIC CREATE.
The current Billing migration chain installs wallet receipts through 900023;
its migration hooks reapply ownership hardening after each protected migration.
On existing volumes, perform this ownership preparation during the writer-fenced
maintenance window; changing the Compose init script or Secret alone does not
modify an existing database. Do not reopen old write-capable binaries afterward.

Before upgrading, close creation admission, drain pending/manual-review work,
and stop all old writer instances, including system/default-route creators.
Disable login for write-capable application and deployment roles and drain
their database sessions. Run the `SERVICE=token-policy-bootstrap` image with `/service --mode quiescence` with
an operations connection that can inspect `pg_stat_activity` and lock creation
tables. The command verifies the database role fence, including inherited table
privileges and SECURITY DEFINER entry points; an empty queue alone is insufficient.
Privileged DBA access remains an operational trust boundary. Keep the role fence
in place after the check, enabling only the migration/operator roles needed for
each controlled deployment step. This check is read-only and does not revoke
privileges or terminate sessions automatically.

Hook order is the complete current Billing migration chain (including 900023
wallet receipts), then the Token chain (including 000029), then explicit
policy bootstrap. New databases must not install or fabricate historical 000053;
existing original 053 receipts require the exact retained-history and ownership
checks described in [wallet receipts](../../../docs/token-wallet-receipts.md).
Migration 900023 is forward-only, even when its receipt table is empty.
Creation remains unavailable until APT-138 activation 000031
is ACTIVE and its polling worker is ready. Initial migration quiescence drains
the legacy creation queue; SAFE recovery preserves historical pending/manual
work instead of repeating that initial drain requirement. During rollback, fence
writers and evaluate migrations in reverse order; retained
receipt, reservation, command or policy history prevents destructive down.
Old binaries may resume only with read-only database identities, never with
the former unrestricted creation privileges.

## Governed Token workloads (APT-137, opt-in)

`tokenCredentialPortal.enabled`, `tokenCommandExecutor.enabled` and
`tokenGovernanceCheck.enabled` default to false. No additional lifecycle or
account-provisioning worker Deployment is created: those remain supervised by
the gateway. New workloads require NetworkPolicy and separate runtime Secret
references. The current gateway uses its release-scoped database Secret; the
credential portal uses a separate release Secret and the executor receives no
database URL. No ordinary Token Relay is deployed or restarted. Historical Token
Relay instances that shared an old gateway writer identity must remain in the
retired-writer inventory, NOLOGIN/session fence and old-process shutdown; removing
the current workload must not remove or revive those identities. Account and
Billing ordinary Relay identity and event behavior are unchanged.

The gateway release Secret contains `token-gateway-database-url` and
`token-gateway.creds`; the credential Secret contains `database-url`,
`secret-provider-url`, `secret-provider-token`; executor has only
`token-command-executor.creds`. Account runtime additionally needs
`account-service.creds` when governed workloads are enabled. Existing Billing and
Relay NATS Secret keys remain unchanged. All consumers/producers use the same
`dependencies.natsUrl`. Enabling either governed workload requires `nats.enabled=false`,
an external `tls://` URL and explicit shared `tokenCommandExecutor.natsCidrs`.
The unauthenticated development NATS is rejected; mounting a credentials file
does not establish server-side authentication. A narrow producer policy admits
the six existing broker clients to the same CIDRs, without adding them to the
new workloads' peer permissions. Issuing scoped credentials, proving the actual
server ACL/TLS identity and preserving retained JetStream history are environment
prerequisites, not consequences of rendering.

Credential OIDC authorization/token/JWKS endpoints must use the issuer's exact
HTTPS origin, without an explicit port. Never reuse the ordinary internal HTTP
Keycloak JWKS backchannel. UAT Portal/credential are the frozen pair
`https://account-uat.pinvou.com` / `https://credentials-uat.pinvou.com`;
Production uses `https://account.pinvou.com` /
`https://credentials.pinvou.com`. Dedicated OIDC client and audience,
actual HTTPS ingress, provider allowlists and dependency CIDRs must be supplied.
G4 provides [credential ingress safety templates](../../nginx/README.md);
rendering these workloads alone does not expose or validate the public origin.

Governance uses its own image built with the existing Dockerfile's
`SERVICE=token-governance`. The opt-in Job is suspended, has no Helm hook, no
retries and no apply arguments; it references a dedicated operations Secret with
`token-migrator-database-url` and `account-policy-database-url`. It also inherits
imagePullSecrets. Only an explicitly approved operation may unsuspend the
read-only check; a state transition must follow the now-delivered G3 operational procedure in
model-token-release.md section 6 and separate environment authorization, never
Helm install/upgrade hooks. Keep the digest-matched old control artifact for SAFE.

`values-model-token-test.yaml` is an intentionally non-deployable public rendering
fixture. It includes nonexistent New API TLS resource names, an invalid registry
and an all-zero image digest solely to exercise the actual templates. From the
repository root, render the remote provider fixture with:

```sh
helm template fixture account-platform/deploy/helm/account-platform \
  -f account-platform/deploy/helm/account-platform/values-model-token-test.yaml > fixture-rendered.yaml
```

For the native KMS fixture append
`-f account-platform/deploy/helm/account-platform/values-kms-native-test.yaml`
before the output redirect. Then run
`node scripts/account-platform-model-token-deployment-check.mjs --helm-render <output>`
from the repository root. The checker validates every selecting NetworkPolicy's
permits (including the control Job), not policy names alone; unbounded peers,
ports, all-address CIDRs and out-of-scope component/port pairs are rejected.
CIDR ownership and endpoint identity still require actual environment evidence.
To run both real rendering positive controls and the missing-input negative
controls with an installed Helm binary, use
`PINVOU_TEST_KMS_HELM=1 node --test account-platform/test/contract/kms-helm.test.mjs`.
Set `PINVOU_TEST_KMS_HELM_BINARY` to a reviewed local binary when it is not on PATH.
Without opt-in, the real rendering case is skipped, not passed. These commands
never install a release, issue certificates, or prove runtime readiness.
The checker validates structure, isolation and shared
event-cluster wiring, and always retains ENV_PENDING/NOT_DEPLOYED. For production
rendering use the matching production origin pair and explicit provider/Secret
references; do not weaken production image, HA or identity validation.

The real Helm/Compose cases remain opt-in for local runs. When selected, the
Account Platform Node contracts step in `.github/workflows/pr-check.yml` enables
both renderers with `PINVOU_TEST_KMS_HELM=1` and `PINVOU_TEST_KMS_COMPOSE=1`.
This wiring does not prove that a particular commit or job ran those cases:
check its input, logs and skip counts. Record the exact command, input and result
for an explicit run; local isolated rendering is not proof of a successful remote
Actions run or production rollout. See [release validation and environment boundaries](../../runbooks/model-token-release.md#9-g6-持续门禁与环境交接).

### Production on-call escalation chain

`values-production-new.yaml` intentionally leaves
`global.manualReviewOnCallUserIds` empty. Its existing schema requires at least
one unique UUID, so production rendering fails until operator values explicitly
provide the ordered stable user IDs of the real on-call escalation chain.
The schema validates configuration shape, not whether an Account user exists,
is active, or is currently on call; verify those facts before deployment.
Development defaults and runtime escalation/SLA behavior are unchanged.

For upgrades, include the current production values and the verified operator
override. Using only `helm --reuse-values` can retain earlier example IDs;
this default change does not repair saved release values or historical case
owners. If those values were used, inspect the deployment and use the existing
audited case-assignment process rather than automatically rewriting history.
