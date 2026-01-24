{{/*
Common labels
*/}}
{{- define "resource-annotator.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "resource-annotator.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "resource-annotator.selectorLabels" -}}
app.kubernetes.io/name: resource-annotator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
