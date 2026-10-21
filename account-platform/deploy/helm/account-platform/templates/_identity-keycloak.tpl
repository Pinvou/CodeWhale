{{- define "pinvou.identityKeycloakOrigin" -}}
{{- if .Values.keycloak.enabled -}}
{{- if .Values.keycloak.tlsSecretName -}}
{{- printf "https://%s-keycloak:8443" (include "pinvou.fullname" .) -}}
{{- else -}}
{{- printf "http://%s-keycloak:8080" (include "pinvou.fullname" .) -}}
{{- end -}}
{{- else -}}{{- .Values.keycloak.adminApiUrl -}}{{- end -}}
{{- end -}}

{{- define "pinvou.identityKeycloakTokenURL" -}}
{{- if .Values.keycloak.enabled -}}
{{- printf "%s/realms/pinvou/protocol/openid-connect/token" (include "pinvou.identityKeycloakOrigin" .) -}}
{{- else -}}{{- .Values.keycloak.provisionerTokenUrl -}}{{- end -}}
{{- end -}}

{{- define "pinvou.identityKeycloakFiles" -}}
{{- if and .Values.sms.enabled (or (hasPrefix "production" (lower (trim .Values.global.environment))) .Values.keycloak.caConfigMap) -}}true{{- end -}}
{{- end -}}

{{- define "pinvou.validateIdentityKeycloak" -}}
{{- if .Values.sms.enabled -}}
{{- $production := hasPrefix "production" (lower (trim .Values.global.environment)) -}}
{{- if and (not .Values.keycloak.enabled) (or (eq .Values.keycloak.adminApiUrl "") (eq .Values.keycloak.provisionerTokenUrl "")) -}}
{{- fail "Identity external Keycloak requires explicit direct adminApiUrl and provisionerTokenUrl" -}}
{{- end -}}
{{- if and $production (or (eq .Values.keycloak.caConfigMap "") (and .Values.keycloak.enabled (eq .Values.keycloak.tlsSecretName ""))) -}}
{{- fail "Identity production Keycloak requires a public CA and an internal TLS Secret or direct HTTPS backchannel" -}}
{{- end -}}
{{- if or $production .Values.keycloak.caConfigMap -}}
{{- $adminRaw := include "pinvou.identityKeycloakOrigin" . -}}
{{- $tokenRaw := include "pinvou.identityKeycloakTokenURL" . -}}
{{- $admin := urlParse $adminRaw -}}{{- $token := urlParse $tokenRaw -}}
{{- if or (ne $admin.scheme "https") (eq $admin.host "") (ne $token.scheme "https") (ne $admin.host $token.host) (not (has $admin.path (list "" "/"))) (ne $token.path "/realms/pinvou/protocol/openid-connect/token") -}}
{{- fail "Identity Keycloak requires same-origin HTTPS admin and realm token endpoints" -}}
{{- end -}}
{{- range $endpoint := list $adminRaw $tokenRaw -}}
{{- $url := urlParse $endpoint -}}
{{- if or $url.userinfo $url.query $url.fragment (contains "?" $endpoint) (contains "#" $endpoint) (contains "%" $endpoint) (contains "\\" $endpoint) -}}
{{- fail "Identity Keycloak endpoints must not contain credentials, query, fragment or escaped paths" -}}
{{- end -}}
{{- end -}}
{{- $tokenAuthority := trimSuffix ":443" (lower $token.host) -}}
{{- $issuerAuthority := trimSuffix ":443" (lower (urlParse .Values.oidc.issuer).host) -}}
{{- $ingressAuthority := trimSuffix ":443" (lower .Values.ingress.keycloakHost) -}}
{{- if and (not .Values.keycloak.enabled) .Values.ingress.enabled (has $tokenAuthority (list $issuerAuthority $ingressAuthority)) -}}
{{- fail "Identity external Keycloak token backchannel must bypass the public Identity token proxy" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
