{{- define "pinvou.name" -}}pinvou-account-platform{{- end -}}
{{- define "pinvou.fullname" -}}{{ .Release.Name }}-account-platform{{- end -}}
{{- define "pinvou.labels" -}}
app.kubernetes.io/name: {{ include "pinvou.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}
{{- define "pinvou.selector" -}}
app.kubernetes.io/name: {{ include "pinvou.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "pinvou.redisUrl" -}}{{ default (printf "redis://%s-redis:6379/0" (include "pinvou.fullname" .)) .Values.dependencies.redisUrl }}{{- end -}}
{{- define "pinvou.newApiRedisUrl" -}}{{ default (printf "redis://%s-redis:6379/1" (include "pinvou.fullname" .)) .Values.dependencies.newApiRedisUrl }}{{- end -}}
{{- define "pinvou.natsUrl" -}}{{ default (printf "nats://%s-nats:4222" (include "pinvou.fullname" .)) .Values.dependencies.natsUrl }}{{- end -}}
{{- define "pinvou.natsMonitorUrl" -}}{{ default (printf "http://%s-nats:8222/healthz" (include "pinvou.fullname" .)) .Values.dependencies.natsMonitorUrl }}{{- end -}}
{{- define "pinvou.availabilityPodSpec" -}}
{{- if .root.Values.availability.enabled }}
terminationGracePeriodSeconds: {{ .root.Values.availability.terminationGracePeriodSeconds }}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: {{ .root.Values.availability.topologyKey | quote }}
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        {{- include "pinvou.selector" .root | nindent 8 }}
        app.kubernetes.io/component: {{ .component }}
  - maxSkew: 1
    topologyKey: {{ .root.Values.availability.hostnameTopologyKey | quote }}
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "pinvou.selector" .root | nindent 8 }}
        app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end -}}
{{- define "pinvou.validate" -}}
{{- if eq .Values.global.environment "production-new" -}}
{{- if or (not .Values.networkPolicy.enabled) (not .Values.ingress.enabled) -}}{{ fail "production-new requires NetworkPolicy and ingress" }}{{- end -}}
{{- if eq .Values.ingress.tlsSecretName "" -}}{{ fail "production-new requires an ingress TLS secret" }}{{- end -}}
{{- if not .Values.externalSecrets.enabled -}}{{ fail "production-new requires an external KMS/Vault-backed runtime secret" }}{{- end -}}
{{- if or (eq .Values.externalSecrets.secretStoreName "") (eq .Values.externalSecrets.remoteKey "") -}}{{ fail "production-new requires an external secret store and remote key" }}{{- end -}}
{{- if or (eq .Values.tokenGateway.secretProvider "") (eq .Values.tokenGateway.secretProvider "local") -}}{{ fail "production-new requires an external Token secret provider" }}{{- end -}}
{{- if or (contains "localhost" .Values.oidc.issuer) (contains "127.0.0.1" .Values.oidc.issuer) -}}{{ fail "production-new OIDC issuer must not be local" }}{{- end -}}
{{- if or (contains "simulator" .Values.public.paymentCallbackUrl) (contains "mock" .Values.public.paymentCallbackUrl) -}}{{ fail "production-new must not reference a payment simulator" }}{{- end -}}
{{- if and .Values.newApi.enabled (eq .Values.images.newApi.digest "") -}}{{ fail "production-new New API image must be pinned by digest" }}{{- end -}}
{{- if and .Values.newApi.enabled (not (has .Values.newApi.serviceName .Values.tokenGateway.providerAllowedHosts)) -}}{{ fail "Token Gateway provider allowlist must include the private New API service name" }}{{- end -}}
{{- if not .Values.availability.enabled -}}{{ fail "production-new requires availability scheduling and disruption protection" }}{{- end -}}
{{- if or .Values.redis.enabled .Values.nats.enabled -}}{{ fail "production-new requires external high-availability Redis and NATS" }}{{- end -}}
{{- if or (not (hasPrefix "rediss://" .Values.dependencies.redisUrl)) (not (hasPrefix "rediss://" .Values.dependencies.newApiRedisUrl)) -}}{{ fail "production-new Redis dependencies must use TLS" }}{{- end -}}
{{- if not (hasPrefix "tls://" .Values.dependencies.natsUrl) -}}{{ fail "production-new NATS dependency must use TLS" }}{{- end -}}
{{- if not (hasPrefix "https://" .Values.dependencies.natsMonitorUrl) -}}{{ fail "production-new NATS monitor dependency must use HTTPS" }}{{- end -}}
{{- if or (lt (int .Values.replicas.accountService) 2) (lt (int .Values.replicas.billingService) 2) (lt (int .Values.replicas.tokenGateway) 2) (lt (int .Values.replicas.tokenPortal) 2) (lt (int .Values.replicas.eventRelay) 2) (lt (int .Values.keycloak.replicas) 2) (lt (int .Values.newApi.replicas) 2) -}}{{ fail "production-new requires at least two replicas for horizontally safe workloads" }}{{- end -}}
{{- if or (eq (len .Values.external.postgresCidrs) 0) (eq (len .Values.external.paymentProviderCidrs) 0) (eq (len .Values.external.secretProviderCidrs) 0) (eq (len .Values.external.modelProviderCidrs) 0) (eq (len .Values.external.timeSourceCidrs) 0) -}}{{ fail "production-new requires explicit external dependency CIDRs" }}{{- end -}}
{{- end -}}
{{- if and (not .Values.keycloak.enabled) (or (eq .Values.oidc.jwksUrl "") (eq .Values.oidc.tokenUrl "")) -}}{{ fail "external Keycloak requires explicit OIDC JWKS and token backchannel URLs" }}{{- end -}}
{{- end -}}
