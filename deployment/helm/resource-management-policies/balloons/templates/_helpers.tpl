{{/*
Common labels
*/}}
{{- define "balloons-plugin.labels" -}}
app: nri-resource-policy-balloons
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
