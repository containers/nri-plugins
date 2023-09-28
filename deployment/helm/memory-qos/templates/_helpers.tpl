{{/*
Common labels
*/}}
{{- define "memory-qos.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "memory-qos.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "memory-qos.selectorLabels" -}}
app.kubernetes.io/name: nri-memory-qos
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
