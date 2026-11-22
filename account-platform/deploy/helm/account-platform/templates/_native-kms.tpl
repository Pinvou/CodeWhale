{{- define "pinvou.validateNativeKms" -}}
{{- if eq .Values.tokenGateway.secretProvider "vault-transit" -}}
{{- $k := .Values.nativeKms -}}
{{- $url := urlParse $k.endpoint -}}
{{- $port := "443" -}}{{- if hasSuffix ":8200" $url.host -}}{{- $port = "8200" -}}{{- end -}}
{{- if or (ne $url.scheme "https") (eq $url.host "") (ne $url.userinfo "") (ne $url.query "") (ne $url.fragment "") (not (has $url.path (list "" "/"))) (contains "?" $k.endpoint) (contains "#" $k.endpoint) (contains "%" $k.endpoint) (contains "\\" $k.endpoint) (ne (toString $k.port) $port) -}}{{ fail "Native KMS requires an exact HTTPS origin and matching port" }}{{- end -}}
{{- if and (contains ":" $url.host) (not (hasSuffix (printf ":%s" $port) $url.host)) -}}{{ fail "Native KMS port must be 443 or 8200" }}{{- end -}}
{{- if or (not .Values.networkPolicy.enabled) (eq (len $k.cidrs) 0) (eq $k.caConfigMap "") (eq $k.gatewayBootstrapSecret "") -}}{{ fail "Native KMS requires public CA, separate bootstrap and explicit network policy" }}{{- end -}}
{{- $forbidden := list .Values.global.runtimeSecret .Values.tokenGateway.runtimeSecret .Values.tokenCredentialPortal.runtimeSecret .Values.tokenCommandExecutor.runtimeSecret .Values.tokenGovernanceCheck.operationsSecret -}}
{{- if has $k.gatewayBootstrapSecret $forbidden -}}{{ fail "Native KMS bootstrap must not share application or operations Secrets" }}{{- end -}}
{{- if .Values.tokenCredentialPortal.enabled -}}
{{- if or (eq $k.credentialBootstrapSecret "") (eq $k.credentialBootstrapSecret $k.gatewayBootstrapSecret) (has $k.credentialBootstrapSecret $forbidden) -}}{{ fail "Native KMS credential bootstrap must be separate" }}{{- end -}}
{{- end -}}
{{- else -}}
{{- if or .Values.nativeKms.endpoint .Values.nativeKms.caConfigMap .Values.nativeKms.gatewayBootstrapSecret .Values.nativeKms.credentialBootstrapSecret (gt (len .Values.nativeKms.cidrs) 0) -}}{{ fail "Native KMS configuration requires vault-transit provider" }}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pinvou.nativeKmsEnv" -}}
- { name: TOKEN_SECRET_PROVIDER_URL, value: {{ .Values.nativeKms.endpoint | quote }} }
- { name: TOKEN_SECRET_PROVIDER_TOKEN_FILE, value: /var/run/secrets/pinvou/kms/token }
- { name: TOKEN_SECRET_PROVIDER_CA_FILE, value: /var/run/secrets/pinvou/kms-ca/ca.crt }
{{- end -}}

{{- define "pinvou.nativeKmsMounts" -}}
- { name: kms-token, mountPath: /var/run/secrets/pinvou/kms, readOnly: true }
- { name: kms-ca, mountPath: /var/run/secrets/pinvou/kms-ca, readOnly: true }
{{- end -}}

{{- define "pinvou.nativeKmsVolumes" -}}
- { name: kms-token, emptyDir: { medium: Memory, sizeLimit: 1Mi } }
- { name: kms-agent-tmp, emptyDir: { medium: Memory, sizeLimit: 16Mi } }
- name: kms-bootstrap
  secret:
    secretName: {{ if eq .component "token-gateway" }}{{ .root.Values.nativeKms.gatewayBootstrapSecret }}{{ else }}{{ .root.Values.nativeKms.credentialBootstrapSecret }}{{ end }}
    defaultMode: 288
    items: [{ key: role-id, path: role-id }, { key: secret-id, path: secret-id }]
- name: kms-ca
  configMap: { name: {{ .root.Values.nativeKms.caConfigMap }}, items: [{ key: ca.crt, path: ca.crt }] }
- name: kms-agent-config
  configMap: { name: {{ include "pinvou.fullname" .root }}-kms-agent, items: [{ key: agent.hcl, path: agent.hcl }] }
{{- end -}}

{{- define "pinvou.nativeKmsAgent" -}}
- name: kms-agent
  image: quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641
  imagePullPolicy: IfNotPresent
  command: ["/usr/bin/bao"]
  args: ["agent", "-config=/bao/config/agent.hcl"]
  securityContext: { runAsUser: 65532, runAsGroup: 65532, runAsNonRoot: true, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } }
  resources: { requests: { cpu: 25m, memory: 32Mi }, limits: { cpu: 500m, memory: 128Mi } }
  volumeMounts:
    - { name: kms-bootstrap, mountPath: /bao/bootstrap, readOnly: true }
    - { name: kms-ca, mountPath: /bao/trust, readOnly: true }
    - { name: kms-agent-config, mountPath: /bao/config, readOnly: true }
    - { name: kms-token, mountPath: /bao/token }
    - { name: kms-agent-tmp, mountPath: /tmp }
{{- end -}}
