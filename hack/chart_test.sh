#!/usr/bin/env bash
# Checks where the node-agent lands and that it stays unprivileged.
set -euo pipefail

fail() { echo "FAIL: $*"; exit 1; }
render() { helm template t charts/nekromancer -f - ; }
agent() { yq 'select(.kind == "DaemonSet")' ; }

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
echo PASS
