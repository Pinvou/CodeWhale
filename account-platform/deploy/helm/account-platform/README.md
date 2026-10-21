# Account Platform Helm Chart

This chart deploys Account Service, Billing Service, Token Gateway, Token Portal,
Keycloak, Redis, and NATS. PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

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
and Billing 000053 revoke runtime/PUBLIC CREATE after assuming the schema owner.
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

Hook order is Billing migration 000053, Token migration 000029, then explicit
policy bootstrap. Creation remains unavailable until APT-138 activation 000031
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
references. The Token Relay uses the same release-scoped gateway database Secret
and must be stopped/fenced/restarted with that writer generation. Account and
Billing Relay identity and event behavior are unchanged.

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

The real Helm/Compose cases are opt-in and are not currently enabled by the
repository workflows. Default contract success therefore does not prove either
renderer ran. Record the exact command, input and result for an explicit run;
local isolated rendering is not proof of a successful remote Actions run or
production rollout. See [release validation and environment boundaries](../../runbooks/model-token-release.md#9-g6-持续门禁与环境交接).
