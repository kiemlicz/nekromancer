#!/usr/bin/env bash
# Checks which hosts the privileged node-agent lands on.
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

only_atx=$(render <<'YAML'
config:
  hosts:
  - {name: atx1, wake: pikvm, shutdown: pikvm, pikvmURL: "https://a"}
YAML
)
[ -z "$(agent <<<"$only_atx")" ] || fail "agent rendered with no os shutdown host"
echo PASS
