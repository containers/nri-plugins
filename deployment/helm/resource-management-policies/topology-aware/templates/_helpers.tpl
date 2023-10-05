{{/*
Common labels
*/}}
{{- define "topology-aware-plugin.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "topology-aware-plugin.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "topology-aware-plugin.selectorLabels" -}}
app.kubernetes.io/name: nri-resource-policy-topology-aware
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
