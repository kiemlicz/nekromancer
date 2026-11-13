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
Args: affinity, field (matchExpressions or matchFields), guard (list of requirements) or guards (list of guards, ORed:
each required term is repeated once per guard; matchFields metadata.name takes a single value, so one guard per host).
*/}}
{{- define "nekromancer.affinity" -}}
{{- $affinity := deepCopy (.affinity | default dict) }}
{{- $nodeAffinity := $affinity.nodeAffinity | default dict }}
{{- $required := $nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution | default dict }}
{{- $guards := .guards | default (list .guard) }}
{{- $terms := list }}
{{- range $term := $required.nodeSelectorTerms | default (list dict) }}
{{- range $guard := $guards }}
{{- $t := deepCopy $term }}
{{- $terms = append $terms (set $t $.field (concat (get $t $.field | default list) $guard)) }}
{{- end }}
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
