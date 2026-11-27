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
app.kubernetes.io/name: nri-dra-driver-cxl
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The spec.config of a DeviceClass from the Helm value deviceClassConfig,
indented for the spec of the class and preceded by a newline: an opaque
MemoryConfig entry of the class's driver, or nothing when the value has no
entry for the class.
Parameters:
- "root": the top context
- "class": the name of the class
- "driver": the DRA driver of the class
*/}}
{{- define "dra-driver-cxl.classConfig" -}}
{{- $config := get (.root.Values.deviceClassConfig | default dict) .class -}}
{{- if $config -}}
{{- $entry := printf "config:\n- opaque:\n    driver: %s\n    parameters:\n      apiVersion: cxl.nri.io/v1alpha1\n      kind: MemoryConfig\n%s" .driver (toYaml $config | indent 6) -}}
{{- $entry | nindent 2 -}}
{{- end -}}
{{- end -}}
