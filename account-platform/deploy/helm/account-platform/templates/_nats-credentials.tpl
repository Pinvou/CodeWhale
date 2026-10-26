{{/* The same non-root service image prepares only its allowlisted credential.
     Secret projection is init-only; the main container sees the memory copy. */}}
{{- define "pinvou.natsCredentialsInit" -}}
- name: prepare-nats-{{ .client }}
  image: "{{ .image.repository }}:{{ .image.tag }}"
  imagePullPolicy: {{ .image.pullPolicy }}
  command: ["/service"]
  args: ["prepare-nats-credentials", {{ .client | quote }}]
  securityContext: { runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } }
  resources: { requests: { cpu: 10m, memory: 16Mi }, limits: { cpu: 100m, memory: 32Mi } }
  volumeMounts:
    - { name: nats-source-{{ .client }}, mountPath: /var/run/bootstrap/pinvou/nats, readOnly: true }
    - { name: nats-private, mountPath: /run/pinvou/nats }
{{- end -}}

{{- define "pinvou.natsCredentialsSource" -}}
- name: nats-source-{{ .client }}
  secret:
    secretName: {{ .secret }}
    defaultMode: 288
    items: [{ key: {{ .key }}, path: {{ .client }}.creds }]
{{- end -}}
