{{/*
Common labels
*/}}
{{- define "nri-plugin.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "nri-plugin.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "nri-plugin.selectorLabels" -}}
app.kubernetes.io/name: nri-resource-policy-topology-aware
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
