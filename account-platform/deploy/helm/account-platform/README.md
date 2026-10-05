# Account Platform Helm Chart

This chart deploys Account Service, Identity Adapter, Billing Service, Token Gateway, Token Portal,
Keycloak, Redis, and NATS. PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

Create the referenced runtime Secret before install. It must contain separate
database URLs (`account-service-database-url`, `billing-service-database-url`,
`token-gateway-database-url`, and Keycloak database keys), the internal service
token, payment adapter URL/HMAC keys, the environment-specific external
secret-provider material, `token-default-provider-api-key`, and the New API
reconciliation credentials (`token-reconciliation-newapi-admin-token` and
`token-reconciliation-newapi-admin-user-id`). Mobile registration additionally
requires `registration-query-hmac-key-base64`, isolated from the Identity password
and SMS verification HMAC keys, and `identity-password-hmac-key-base64` for the
Identity workload. Identity password references are process-local, so the schema
requires exactly one `replicas.identityService`; a restart invalidates pending
references. Unknown remote creation must still recover by its exact marker rather
than treating reference expiry as proof that no identity exists. Never place those values in a
values file.

Portal also reads the existing internal service token, solely to authenticate its
server-derived client source to Account. Account ignores an unproved source
header and uses the peer address. The proof is never returned to the browser or
forwarded to Billing, Token Gateway, or arbitrary upstreams. Device identifiers
are rate-limit correlation fields and do not replace user/mobile authorization.

New API's private entry point is HTTPS on port 8443. Build the project-owned
`newapi-tls` image using the existing Dockerfile with `SERVICE=newapi-tls` and
set `images.newApiTls`. The same-pod TLS front end forwards only the exact
chat-completions, model-list, reconciliation-log and exact `GET /api/status` routes to loopback HTTP
8080; no New API ingress or cross-pod HTTP service is exposed. Create the
referenced server TLS Secret (`tls.crt`, `tls.key`) and a separate CA Secret
(`ca.crt`) before installation. Its certificate DNS SAN must match
`newApi.serviceName`. Only the front end mounts the private key; Gateway mounts
the CA and keeps system roots through `SSL_CERT_DIR`. Provider/reconciliation
URLs must both equal `https://<serviceName>:8443`. Missing certificate Secrets
prevent startup and incorrect CA/SAN/expiry fails TLS verification. Rotate
certificates by restarting the front end and Gateway; do not disable validation.

Production-new deliberately excludes Payment Simulator and Mock New API. Use the
dedicated production values only after the real payment adapter, real New API,
and external KMS/Vault gates pass. Production-new validation rejects local
secret providers and disabled NetworkPolicy. HTTPS OIDC endpoint consistency
and the exact HTTPS callback ingress are also checked; validation does not
prove the configured issuer, merchant, or provider is a real production service.
Registry names, provider hosts, and CIDRs in the production values file are
documentation examples; replace every one with the reviewed deployment values
before rendering a release manifest.

Staging and production use explicit HTTPS `oidc.jwksUrl` and `oidc.tokenUrl`
under `oidc.issuer`; enabling bundled Keycloak does not replace these with HTTP.
Configure its public TLS ingress and trusted certificate, and replace
`external.oidcCidrs` with the approved public endpoint CIDRs. NetworkPolicy also
permits HTTPS to the explicitly selected ingress controller. Only development
and test may use bundled Keycloak's internal HTTP endpoints. A mismatched issuer
or missing HTTPS endpoint stops rendering; this does not relax service checks.

The Billing ingress exposes only the exact `/internal/payment/notifications`
path on `ingress.billingHost`. Configure `public.paymentCallbackUrl` for that
HTTPS host and path, include the host in the TLS certificate, and ensure the
payment adapter can reach it. The handler accepts POST and verifies the channel
header, HMAC, timestamp window, merchant/order facts, and notification idempotency.
Wallet, hold, pricing, reconciliation, and admin routes have no ingress path.

The Identity ingress on `ingress.identityHost` exposes only the exact
`/v1/registration/password-prechecks` path. Include this host in the configured
TLS certificate. Its internal identity creation, profile, password reset and
administration paths are never exposed by ingress.

```sh
helm lint deploy/helm/account-platform \
  -f deploy/helm/account-platform/values-production-new.yaml
helm template pinvou deploy/helm/account-platform \
  -f deploy/helm/account-platform/values-production-new.yaml > rendered.yaml
```

Migration Jobs are pre-install/pre-upgrade hooks. Rollback does not delete PVCs;
database rollback remains an explicit, separately approved operation.
Redis and NATS use their explicit non-root UID/GID and matching PVC `fsGroup`;
the selected storage driver must support volume ownership or provide these
permissions before startup. Bundled Keycloak requires the reviewed realm/client
configuration to be provisioned separately; installing workloads alone is not
an authentication or production-release acceptance.

Mobile registration is opt-in (`mobileAuth.enabled`). Use the project Keycloak
SPI image built from `deploy/keycloak/Dockerfile`, set `images.keycloak`, then run
its `provision-mobile-auth.sh` through the controlled administrator channel.
Provisioning must finish with the three service-account roles read back and the
managed, administrator-only phone and opaque registration attributes present.
The bundled Keycloak calls the Account service's authenticated POST identity
and SMS-verification routes; it never selects the first realm user sharing a
phone. Disabling a business account does not itself revoke a realm admin role.

The neutral `http` SMS adapter accepts authenticated `/send` and `/verify`
JSON requests. Configure `mobileAuth.smsEndpoint`, the Keycloak administrator
and token URLs, `sms-http-service-token` and
`keycloak-provisioner-client-secret` in the referenced runtime Secret. The
optional Aliyun adapter uses its three Secret keys and reviewed template names.
`aliyun` and `aliyun-managed` both select vendor-managed verification. Explicit
`http-generated` uses the send-only endpoint with `sms-http-bearer-token` and a
separate `sms-verification-hmac-key-base64` Secret; it does not reuse the managed
HTTP service Token or require Aliyun templates. Existing `http` configurations
retain their send/verify semantics.
Outside development/test these credential-bearing endpoints require HTTPS and
`external.smsProviderCidrs`; only Account receives the added port-443 egress.

Commerce is optional. Community registration and wallet activation do not wait
for an enterprise service; unconfigured Mapping jobs stay pending without an
active worker. Selecting `billing.paymentProvider=h3c_mall`
requires `commerce.enabled`, a reviewed HTTPS `commerce.baseUrl`, its runtime
Secret credentials and `external.commerceProviderCidrs`. Only Account and Billing
receive those additional port-443 destinations. Tentative PLAN prices remain
disabled outside the development simulator unless explicitly approved through
`billing.allowTentativePrices`; rendering does not constitute supplier, merchant,
certificate or production-price acceptance. The four original LEGACY organization
offers remain available through the development simulator only; personal sales
of retired LEGACY offers are not reopened.

`account.commerceProviderId` and `account.commerceAdapterVersion` freeze the
provider identity of each Mapping and job; the existing `commerce.baseUrl` remains
the single adapter URL configuration. Wallet jobs snapshot
`account.walletProvisioningPolicyVersion` and `account.walletProvisioningSlaMinutes`.

If a remote registration creation was accepted but its response or local
completion failed, retry the same username and phone with a new verified SMS.
Only the exact opaque Account registration marker may recover that identity.
`password_preserved=true` means its first submitted password remains in effect;
recovery does not reset a password, adopt another realm user or remove a retained
identity. Deleted identities continue to reserve their realm usernames.
