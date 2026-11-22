# HTTP admission source and callback budgets

The common HTTP limiter defaults to `RATE_LIMIT_RPS=100` and
`RATE_LIMIT_BURST=200`. Ordinary requests share a bucket per resolved source.
GET/HEAD health probes retain their exemption. This is per-process admission;
it does not replace authentication, business permissions or payment risk limits.

`HTTP_TRUSTED_PROXY_IPS` is an optional comma-separated list of exact immediate
proxy IP addresses. The default empty list uses the TCP peer. Invalid configured
addresses, hostnames, CIDRs, zones or empty list elements fail configuration load
without echoing the input. IPv4-mapped IPv6 and IPv6 spellings are canonicalized.
Only a listed immediate peer may supply exactly one IP address in
`X-Pinvou-RateLimit-Source`. Missing, invalid, comma-separated or duplicate header
values fall back to that peer. X-Forwarded-For, X-Real-IP and business source
headers cannot select the HTTP bucket. A trusted proxy must overwrite the new
header on every request, including requests with a client-supplied header.

Portal propagates the common limiter's resolved source only on its existing
allowlisted Account/Billing/Token API calls, including handoff identity checks
and the user-triggered system-status Account bootstrap and Token reconciliation
reads. The receiving service must trust Portal's actual immediate peer as well
as its edge peer. OIDC exchanges,
third-party probes and background requests do not receive the hint. Existing
path-specific business source signing, session, CSRF and authorization rules
continue independently.

Billing classifies the two POST callback routes through a narrow admission
policy. It verifies configured Basic credentials without reading the form body
or minting a token, or verifies the bearer signature, claims and configured
provider before selecting a stable configured client principal. Raw JWTs and
unverified client IDs cannot create authenticated buckets. Verified notification
and token-issuance traffic have separate 200/s, burst-200 buckets per
principal/source. Invalid callback traffic has its own ordinary-sized source
bucket and cannot spend the verified notification budget. The existing handler
still performs all credential, scope, body, provider and persistence checks.
Callback source accounting uses the same resolved source; retained notification
idempotency remain in force. Account's separate anonymous-mobile GLOBAL 300
business rule and SMS send/verify rules are unchanged. Admission
rates are configuration/algorithm guarantees, not a measured end-to-end 200/s
capacity result. NFR-41 performance and the remaining parts of issue #1618 need
their own acceptance evidence.

## Deployment

Development Compose exposes per-service
`ACCOUNT_SERVICE_HTTP_TRUSTED_PROXY_IPS`, `IDENTITY_SERVICE_HTTP_TRUSTED_PROXY_IPS`,
`BILLING_SERVICE_HTTP_TRUSTED_PROXY_IPS`, `TOKEN_GATEWAY_HTTP_TRUSTED_PROXY_IPS` and
`TOKEN_PORTAL_HTTP_TRUSTED_PROXY_IPS`; all default empty for direct connections.
Set a value only after verifying the immediate proxy and its overwrite behavior.

Production personal Compose pins edge and Portal peer addresses. Its NGINX
template overwrites `X-Pinvou-RateLimit-Source` with `$remote_addr` at server scope;
the current proxy locations inherit it. Account/Billing/Token trust the exact
edge and Portal peers; Identity and Portal trust the exact edge peer. The
existing network restrictions and fixed peer assignments must match those
values. An upstream load balancer requires a separately verified real-IP setup;
never copy arbitrary incoming XFF into this header.

Helm uses `security.httpTrustedProxyIPs` keyed by `account-service`,
`identity-service`, `billing-service`, `token-gateway`, and `token-portal`.
Each list defaults empty. Rendered application environments contain
`HTTP_TRUSTED_PROXY_IPS`, but this alone does not configure the ingress writer.
The current chart requires class `nginx` for its ingress-nginx affinity contract.
Use a dedicated ingress-nginx controller that exclusively owns this class and
serves only this platform's namespace. If the class/controller is shared, do not
change its controller-wide configuration; provision an isolated boundary first.
The application chart deliberately does not modify any external controller.

For that dedicated controller only, its actual `--configmap` ConfigMap must set
`proxy-set-headers: <controller-namespace>/pinvou-admission-headers`. Provision
that referenced ConfigMap in the controller namespace with:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: pinvou-admission-headers
data:
  X-Pinvou-RateLimit-Source: "$remote_addr"
```

`proxy-set-headers` is a controller ConfigMap key, not an Ingress annotation.
Include the ConfigMap namespace in the actual manifest. Verify that the
controller reads it and that its generated NGINX configuration contains exactly
the intended `proxy_set_header X-Pinvou-RateLimit-Source "$remote_addr";` in every
applicable upstream location. Header-ConfigMap changes do not trigger a reload;
perform a controlled rolling restart of this dedicated controller and recheck
the generated configuration. See the upstream
[custom-header configuration and reload caveat](https://kubernetes.github.io/ingress-nginx/examples/customization/custom-headers/).

Pin the actual immediate ingress and Portal egress addresses in the appropriate
service lists, accounting for any verified SNAT. Do not use pod/network CIDRs or
guess peer identities. On peer changes, reconcile the exact lists and reload the
affected app processes before cutover. Test two client addresses behind the
same edge, then direct spoofing, missing/duplicate headers and untrusted peers;
only the overwritten trusted path may isolate client buckets. Restrict backend
reachability to the intended peers. Empty/default trust is a safe peer fallback,
not evidence that a deployed site has client isolation. Helm/Compose rendering
proves configuration wiring only; controller reload, real proxy traffic and
load capacity require site acceptance before claiming deployment readiness.
