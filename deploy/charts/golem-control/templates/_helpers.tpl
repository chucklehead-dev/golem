{{- define "golem-control.labels" -}}
app.kubernetes.io/name: golem-control
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "golem-control.selector" -}}
app.kubernetes.io/name: golem-control
{{- end -}}
{{/* gitSyncArgs are git-sync's flags, the init container's and the sidecar's. */}}
{{- define "golem-control.gitSyncArgs" -}}
- --repo={{ .Values.config.repo }}
- --ref={{ .Values.config.ref }}
- --root=/config
- --link=current
- --period={{ .Values.config.period }}
- --ssh-key-file=/etc/git-secret/ssh
- --ssh-known-hosts-file=/etc/git-secret/known_hosts
{{- end -}}
