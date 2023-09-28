{{/*
Common labels
*/}}
{{- define "memtierd.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "memtierd.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "memtierd.selectorLabels" -}}
app.kubernetes.io/name: nri-memtierd
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
