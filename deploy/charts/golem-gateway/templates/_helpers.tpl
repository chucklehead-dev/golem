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
{{/* telemetry is the gateway's -telemetry: the pod's collector on loopback, or the remote one. */}}
{{- define "golem-gateway.telemetry" -}}
{{- if .Values.collector.enabled }}127.0.0.1:4318{{ else }}{{ .Values.telemetry }}{{ end -}}
{{- end -}}
{{/* collectorConfigMap is the ConfigMap holding the pod collector's config. */}}
{{- define "golem-gateway.collectorConfigMap" -}}
{{- .Values.collector.configMap | default (printf "%s-collector" (include "golem-gateway.name" .)) -}}
{{- end -}}
