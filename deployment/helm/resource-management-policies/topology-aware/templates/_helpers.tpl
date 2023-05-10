{{/*
Common labels
*/}}
{{- define "topology-aware-plugin.labels" -}}
app: nri-resource-policy-topology-aware
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
