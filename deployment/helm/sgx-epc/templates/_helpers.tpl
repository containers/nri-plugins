{{/*
Common labels
*/}}
{{- define "sgx-epc.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "sgx-epc.selectorLabels" . }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "sgx-epc.selectorLabels" -}}
app.kubernetes.io/name: nri-sgx-epc
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
