{{- define "nekromancer.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
{{- define "nekromancer.labels" -}}
app.kubernetes.io/name: nekromancer
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
