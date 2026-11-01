{{- define "nekromancer.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
{{- define "nekromancer.labels" -}}
app.kubernetes.io/name: nekromancer
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{/* Names of the hosts that shut down through the node-agent, as a JSON list. */}}
{{- define "nekromancer.osHosts" -}}
{{- $out := list }}
{{- range .Values.config.hosts }}
{{- if ne (default "os" .shutdown) "pikvm" }}
{{- $out = append $out .name }}
{{- end }}
{{- end }}
{{- toJson $out }}
{{- end }}
