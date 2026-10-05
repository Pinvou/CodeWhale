# Account Platform Helm Chart

This chart deploys Account Service, Billing Service, Token Gateway, Token Portal,
Keycloak, Redis, and NATS. PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

Create the referenced runtime Secret before install. It must contain separate
database URLs (`account-service-database-url`, `billing-service-database-url`,
`token-gateway-database-url`, and Keycloak database keys), the internal service
token, payment adapter URL/HMAC keys, the environment-specific external
secret-provider material, `token-default-provider-api-key`, and the New API
reconciliation credentials (`token-reconciliation-newapi-admin-token` and
`token-reconciliation-newapi-admin-user-id`). Never place those values in a
values file.

New API's private entry point is HTTPS on port 8443. Build the project-owned
`newapi-tls` image using the existing Dockerfile with `SERVICE=newapi-tls` and
set `images.newApiTls`. The same-pod TLS front end forwards only the exact
chat-completions, model-list and reconciliation-log routes to loopback HTTP
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
