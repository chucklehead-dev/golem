{{- define "golem-gateway.name" -}}golem-gateway-{{ .Values.pool }}{{- end -}}
{{- define "golem-gateway.labels" -}}
app.kubernetes.io/name: golem-gateway
app.kubernetes.io/instance: {{ .Release.Name }}
golem.dev/pool: {{ .Values.pool }}
{{- end -}}
{{- define "golem-gateway.selector" -}}
app.kubernetes.io/name: golem-gateway
golem.dev/pool: {{ .Values.pool }}
{{- end -}}
{{/* seconds is a Go duration of the forms the values use (10m, 20s, 1h) in seconds. */}}
{{- define "golem-gateway.seconds" -}}
{{- $d := toString . -}}
{{- if hasSuffix "h" $d }}{{ mul (trimSuffix "h" $d | int) 3600 }}
{{- else if hasSuffix "m" $d }}{{ mul (trimSuffix "m" $d | int) 60 }}
{{- else }}{{ trimSuffix "s" $d | int }}{{ end -}}
{{- end -}}
