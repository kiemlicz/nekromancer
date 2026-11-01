#!/usr/bin/env bash
# Checks RBAC, pod placement and that the node-agent stays unprivileged.
set -euo pipefail

fail() { echo "FAIL: $*"; exit 1; }
render() { helm template t charts/nekromancer -f - ; }
agent() { yq 'select(.kind == "DaemonSet")' ; }
controller() { yq 'select(.kind == "Deployment")' ; }
# Prints a field of a rendered pod spec on one line.
field() { yq -o=json -I=0 ".spec.template.spec$1" ; }
TERMS=.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms
expect() { [ "$2" = "$3" ] || fail "$1: got $2, want $3"; }

mixed=$(render <<'YAML'
config:
  hosts:
  - {name: atx1, wake: pikvm, shutdown: pikvm, pikvmURL: "https://a"}
  - {name: wol1, wake: mikrotik, mac: "AA:BB:CC:DD:EE:01", interface: b, address: 10.0.0.1}
  - {name: wol2, wake: mikrotik, shutdown: os, mac: "AA:BB:CC:DD:EE:02", interface: b, address: 10.0.0.2}
YAML
)
names=$(agent <<<"$mixed" | yq -r '.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchFields[0].values | join(",")')
[ "$names" = wol1,wol2 ] || fail "agent targets '$names'"
[ "$(agent <<<"$mixed" | yq '.spec.template.spec.hostPID // false')" = false ] || fail "agent shares the host PID namespace"
[ "$(agent <<<"$mixed" | yq '.spec.template.spec.containers[0].securityContext.privileged // false')" = false ] || fail "agent is privileged"
[ "$(agent <<<"$mixed" | yq '.spec.template.spec.volumes[0].hostPath.path')" = /run/dbus/system_bus_socket ] || fail "agent has no system bus"

only_atx=$(render <<'YAML'
config:
  hosts:
  - {name: atx1, wake: pikvm, shutdown: pikvm, pikvmURL: "https://a"}
YAML
)
[ -z "$(agent <<<"$only_atx")" ] || fail "agent rendered with no os shutdown host"

# Leader election records core events in the Lease namespace.
core_events='.rules[] | select(.resources[] == "events" and .apiGroups[] == "") | .verbs'
expect "leader election core events" "$(yq -o=json -I=0 "select(.kind == \"Role\") | $core_events" <<<"$mixed")" '["create","patch"]'
expect "cluster-wide core events" "$(yq -o=json -I=0 "select(.kind == \"ClusterRole\") | $core_events" <<<"$mixed")" ""

# Defaults
expect "agent nodeSelector" "$(agent <<<"$mixed" | field .nodeSelector)" '{"kubernetes.io/arch":"amd64","nekromancer.kiemlicz.github.io/managed":"true"}'
expect "agent tolerations" "$(agent <<<"$mixed" | field .tolerations)" '[{"operator":"Exists"}]'
expect "controller nodeSelector" "$(controller <<<"$mixed" | field .nodeSelector)" '{"kubernetes.io/arch":"amd64"}'
expect "controller tolerations" "$(controller <<<"$mixed" | field .tolerations)" null
OFF_MANAGED='{"matchExpressions":[{"key":"nekromancer.kiemlicz.github.io/managed","operator":"DoesNotExist"}]}'
expect "controller guard" "$(controller <<<"$mixed" | field "$TERMS")" "[$OFF_MANAGED]"

# Overrides narrow placement, the guards stay in every required term.
custom=$(render <<'YAML'
controller:
  nodeSelector: {kubernetes.io/arch: arm64}
  tolerations: [{key: infra, operator: Exists}]
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions: [{key: zone, operator: In, values: [a]}]
        - matchExpressions: [{key: zone, operator: In, values: [b]}]
agent:
  nodeSelector: {kubernetes.io/arch: arm64}
  tolerations: [{key: gpu, operator: Exists}]
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions: [{key: zone, operator: In, values: [a]}]
        - matchFields: [{key: metadata.name, operator: In, values: [other]}]
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 1
        preference:
          matchExpressions: [{key: ssd, operator: Exists}]
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 1
        podAffinityTerm:
          topologyKey: kubernetes.io/hostname
          labelSelector: {matchLabels: {a: b}}
config:
  hosts:
  - {name: wol1, wake: mikrotik, mac: "AA:BB:CC:DD:EE:01", interface: b, address: 10.0.0.1}
YAML
)
expect "agent nodeSelector override" "$(agent <<<"$custom" | field .nodeSelector)" '{"kubernetes.io/arch":"arm64","nekromancer.kiemlicz.github.io/managed":"true"}'
expect "agent tolerations override" "$(agent <<<"$custom" | field .tolerations)" '[{"key":"gpu","operator":"Exists"}]'
expect "controller nodeSelector override" "$(controller <<<"$custom" | field .nodeSelector)" '{"kubernetes.io/arch":"arm64"}'
expect "controller tolerations override" "$(controller <<<"$custom" | field .tolerations)" '[{"key":"infra","operator":"Exists"}]'
GUARD='{"key":"metadata.name","operator":"In","values":["wol1"]}'
expect "agent guard" "$(agent <<<"$custom" | field "$TERMS")" \
  "[{\"matchExpressions\":[{\"key\":\"zone\",\"operator\":\"In\",\"values\":[\"a\"]}],\"matchFields\":[$GUARD]},{\"matchFields\":[{\"key\":\"metadata.name\",\"operator\":\"In\",\"values\":[\"other\"]},$GUARD]}]"
expect "agent preferred kept" "$(agent <<<"$custom" | field .affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight)" 1
expect "agent pod anti-affinity kept" "$(agent <<<"$custom" | field .affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight)" 1
M='{"key":"nekromancer.kiemlicz.github.io/managed","operator":"DoesNotExist"}'
expect "controller guard override" "$(controller <<<"$custom" | field "$TERMS")" \
  "[{\"matchExpressions\":[{\"key\":\"zone\",\"operator\":\"In\",\"values\":[\"a\"]},$M]},{\"matchExpressions\":[{\"key\":\"zone\",\"operator\":\"In\",\"values\":[\"b\"]},$M]}]"

# Defaults that used to be hardcoded
expect "replicas" "$(controller <<<"$mixed" | yq .spec.replicas)" 1
expect "controller pod securityContext" "$(controller <<<"$mixed" | field .securityContext)" \
  '{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"},"sysctls":[{"name":"net.ipv4.ping_group_range","value":"65532 65532"}]}'
expect "controller securityContext" "$(controller <<<"$mixed" | field '.containers[0].securityContext')" \
  '{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}'
expect "agent securityContext" "$(agent <<<"$mixed" | field '.containers[0].securityContext')" \
  '{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsUser":0}'
expect "liveness" "$(controller <<<"$mixed" | field '.containers[0].livenessProbe')" '{"httpGet":{"path":"/healthz","port":"health"}}'
expect "readiness" "$(controller <<<"$mixed" | field '.containers[0].readinessProbe')" '{"httpGet":{"path":"/readyz","port":"health"}}'
expect "controller account" "$(controller <<<"$mixed" | field .serviceAccountName)" '"t-controller"'
expect "agent account" "$(agent <<<"$mixed" | field .serviceAccountName)" '"t-agent"'

tuned=$(render <<'YAML'
imagePullSecrets: [{name: regcred}]
controller:
  replicas: 2
  strategy: {type: Recreate}
  serviceAccount: {name: ctl}
  podAnnotations: {a: "1"}
  podLabels: {team: infra, app.kubernetes.io/component: hijack}
  podSecurityContext: {runAsUser: 1000}
  securityContext: {privileged: false}
  resources: {requests: {cpu: 10m}}
  priorityClassName: high
  livenessProbe: {httpGet: {path: /healthz, port: health}, periodSeconds: 7}
  readinessProbe: {httpGet: {path: /readyz, port: health}, periodSeconds: 8}
agent:
  updateStrategy: {type: OnDelete}
  serviceAccount: {name: agt}
  podAnnotations: {b: "2"}
  podLabels: {team: infra}
  podSecurityContext: {fsGroup: 1}
  securityContext: {runAsUser: 1}
  resources: {limits: {memory: 32Mi}}
  priorityClassName: low
config:
  hosts:
  - {name: wol1, wake: mikrotik, mac: "AA:BB:CC:DD:EE:01", interface: b, address: 10.0.0.1}
YAML
)
expect "replicas override" "$(controller <<<"$tuned" | yq .spec.replicas)" 2
expect "strategy" "$(controller <<<"$tuned" | yq -o=json -I=0 .spec.strategy)" '{"type":"Recreate"}'
expect "updateStrategy" "$(agent <<<"$tuned" | yq -o=json -I=0 .spec.updateStrategy)" '{"type":"OnDelete"}'
for d in controller agent; do
  expect "$d pull secrets" "$($d <<<"$tuned" | field .imagePullSecrets)" '[{"name":"regcred"}]'
  expect "$d pod label" "$($d <<<"$tuned" | yq '.spec.template.metadata.labels.team')" infra
  expect "$d selector matches" "$($d <<<"$tuned" | yq '.spec.selector.matchLabels | sort_keys(.) | to_entries | map(.key + "=" + .value) | join(",")')" \
    "app.kubernetes.io/component=$d,app.kubernetes.io/instance=t,app.kubernetes.io/name=nekromancer"
  expect "$d selector labels kept" "$($d <<<"$tuned" | yq '.spec.template.metadata.labels["app.kubernetes.io/component"]')" "$d"
done
expect "controller config checksum kept" "$(controller <<<"$tuned" | yq '.spec.template.metadata.annotations | has("checksum/config")')" true
expect "controller annotation" "$(controller <<<"$tuned" | yq '.spec.template.metadata.annotations.a')" 1
expect "agent annotation" "$(agent <<<"$tuned" | yq '.spec.template.metadata.annotations.b')" 2
expect "controller pod securityContext override" "$(controller <<<"$tuned" | field .securityContext.runAsUser)" 1000
expect "controller securityContext override" "$(controller <<<"$tuned" | field '.containers[0].securityContext.privileged')" false
expect "agent pod securityContext override" "$(agent <<<"$tuned" | field .securityContext)" '{"fsGroup":1}'
expect "agent securityContext override" "$(agent <<<"$tuned" | field '.containers[0].securityContext.runAsUser')" 1
expect "controller resources" "$(controller <<<"$tuned" | field '.containers[0].resources')" '{"requests":{"cpu":"10m"}}'
expect "agent resources" "$(agent <<<"$tuned" | field '.containers[0].resources')" '{"limits":{"memory":"32Mi"}}'
expect "controller priority" "$(controller <<<"$tuned" | field .priorityClassName)" '"high"'
expect "agent priority" "$(agent <<<"$tuned" | field .priorityClassName)" '"low"'
expect "liveness override" "$(controller <<<"$tuned" | field '.containers[0].livenessProbe.periodSeconds')" 7
expect "readiness override" "$(controller <<<"$tuned" | field '.containers[0].readinessProbe.periodSeconds')" 8

# A floating image tag needs pulling on every start.
floating=$(render <<'YAML'
image: {repository: ghcr.io/o/nekromancer, tag: main, pullPolicy: Always}
config:
  hosts:
  - {name: wol1, wake: mikrotik, mac: "AA:BB:CC:DD:EE:01", interface: b, address: 10.0.0.1}
YAML
)
for d in controller agent; do
  expect "$d floating image" "$($d <<<"$floating" | field '.containers[0].image')" '"ghcr.io/o/nekromancer:main"'
  expect "$d pull policy" "$($d <<<"$floating" | field '.containers[0].imagePullPolicy')" '"Always"'
done

# ServiceAccount names reach pods and bindings.
expect "controller account override" "$(controller <<<"$tuned" | field .serviceAccountName)" '"ctl"'
expect "agent account override" "$(agent <<<"$tuned" | field .serviceAccountName)" '"agt"'
expect "accounts created" "$(yq -N 'select(.kind == "ServiceAccount") | .metadata.name' <<<"$tuned" | sort | paste -sd,)" agt,ctl
expect "binding subjects" "$(yq -N 'select(.kind == "ClusterRoleBinding" or .kind == "RoleBinding") | .subjects[0].name' <<<"$tuned" | sort | paste -sd,)" agt,ctl,ctl

external=$(render <<'YAML'
rbac: {create: false}
controller: {serviceAccount: {create: false, name: ctl}}
agent: {serviceAccount: {create: false}}
config:
  hosts:
  - {name: wol1, wake: mikrotik, mac: "AA:BB:CC:DD:EE:01", interface: b, address: 10.0.0.1}
YAML
)
expect "no rbac objects" "$(yq -N 'select(.kind | test("Role")) | .kind' <<<"$external")" ""
expect "no accounts" "$(yq -N 'select(.kind == "ServiceAccount") | .kind' <<<"$external")" ""
expect "existing controller account" "$(controller <<<"$external" | field .serviceAccountName)" '"ctl"'
expect "default agent account" "$(agent <<<"$external" | field .serviceAccountName)" '"default"'
echo PASS
