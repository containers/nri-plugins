{{/*
Common labels
*/}}
{{- define "balloons-plugin.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "balloons-plugin.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "balloons-plugin.selectorLabels" -}}
app.kubernetes.io/name: nri-resource-policy-balloons
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
