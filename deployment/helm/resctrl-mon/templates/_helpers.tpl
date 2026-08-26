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
app.kubernetes.io/name: nri-resctrl-mon
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Prometheus metrics port extracted from telemetry.prometheus.listenAddress.
Matches the trailing numeric port so IPv6 addresses (e.g. "[::]:9200") and
host:port / :port forms all resolve correctly; falls back to 9100.
*/}}
{{- define "nri-resctrl-mon.metricsPort" -}}
{{- regexFind "[0-9]+$" .Values.telemetry.prometheus.listenAddress | default "9100" -}}
{{- end -}}

