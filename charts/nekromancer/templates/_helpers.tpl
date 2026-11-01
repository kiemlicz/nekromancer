{{- define "nekromancer.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
{{- define "nekromancer.labels" -}}
app.kubernetes.io/name: nekromancer
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{/* Args: root, component. */}}
{{- define "nekromancer.serviceAccountName" -}}
{{- $sa := (index .root.Values .component).serviceAccount }}
{{- if $sa.create }}
{{- $sa.name | default (printf "%s-%s" .root.Release.Name .component) }}
{{- else }}
{{- $sa.name | default "default" }}
{{- end }}
{{- end }}
{{/* Pod labels. The ones selectors rely on win over podLabels. Args: root, component. */}}
{{- define "nekromancer.podLabels" -}}
{{- $labels := include "nekromancer.labels" .root | fromYaml }}
{{- $_ := set $labels "app.kubernetes.io/component" .component }}
{{- toYaml (merge $labels (index .root.Values .component).podLabels) }}
{{- end }}
{{/*
Renders an affinity with a guard in every required node term, as terms are ORed. Values can narrow placement, never widen it.
Args: affinity, field (matchExpressions or matchFields), guard (list of requirements).
*/}}
{{- define "nekromancer.affinity" -}}
{{- $affinity := deepCopy (.affinity | default dict) }}
{{- $nodeAffinity := $affinity.nodeAffinity | default dict }}
{{- $required := $nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution | default dict }}
{{- $terms := list }}
{{- range $required.nodeSelectorTerms | default (list dict) }}
{{- $terms = append $terms (set . $.field (concat (get . $.field | default list) $.guard)) }}
{{- end }}
{{- $_ := set $required "nodeSelectorTerms" $terms }}
{{- $_ = set $nodeAffinity "requiredDuringSchedulingIgnoredDuringExecution" $required }}
{{- $_ = set $affinity "nodeAffinity" $nodeAffinity }}
{{- toYaml $affinity }}
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
