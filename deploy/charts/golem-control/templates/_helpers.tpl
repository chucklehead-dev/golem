{{- define "golem-control.labels" -}}
app.kubernetes.io/name: golem-control
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "golem-control.selector" -}}
app.kubernetes.io/name: golem-control
{{- end -}}
