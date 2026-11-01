#!/usr/bin/env bash
# Runs the real image, chart and RBAC on a throwaway kind cluster.
# `docker stop` stands in for a host powering off, a stub plays RouterOS and the PiKVM.
# The node-agent is scheduled but cannot start: kind nodes have no system bus.
set -euo pipefail

CLUSTER=nekromancer-e2e
HOST=$CLUSTER-worker2
ATX_HOST=$CLUSTER-worker3
KIND="go run sigs.k8s.io/kind@v0.33.0"
TMP=$(mktemp -d)
export KUBECONFIG=$TMP/kubeconfig
MANAGED=nekromancer.kiemlicz.github.io

cleanup() {
  rc=$?
  if [ $rc -ne 0 ]; then
    kubectl get nodes,pods -A -o wide || true
    kubectl -n nekromancer logs deploy/nekromancer-controller --tail=40 || true
  fi
  $KIND delete cluster --name $CLUSTER >/dev/null 2>&1 || true
  rm -rf "$TMP"
  exit $rc
}
trap cleanup EXIT

step() { echo "=== $*"; }
wait_for() { # description, command
  local deadline=$((SECONDS + 240))
  until eval "$2" >/dev/null 2>&1; do
    [ $SECONDS -lt $deadline ] || { echo "TIMEOUT: $1"; return 1; }
    sleep 2
  done
  echo "ok: $1"
}
annotation() { kubectl get node "$HOST" -o json | jq -r ".metadata.annotations[\"$MANAGED/$1\"] // empty"; }

step "cluster"
cat >"$TMP/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      kubeletExtraArgs:
      - name: node-labels
        value: "$MANAGED/managed=true,workload=plex"
- role: worker
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      kubeletExtraArgs:
      - name: node-labels
        value: "$MANAGED/managed=true,workload=atx"
YAML
$KIND create cluster --name $CLUSTER --config "$TMP/kind.yaml" --wait 180s
docker build -q -t nekromancer:e2e .
$KIND load docker-image nekromancer:e2e --name $CLUSTER
IP=$(kubectl get node "$HOST" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

step "fake RouterOS and PiKVM"
kubectl create namespace nekromancer
kubectl -n nekromancer create secret generic nekromancer-credentials \
  --from-literal=MIKROTIK_USERNAME=e2e --from-literal=MIKROTIK_PASSWORD=e2e
kubectl -n nekromancer apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: fake-routeros, labels: {app: fake-routeros}}
spec:
  # On a managed node it would keep that host busy, or die with it.
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - {key: nekromancer.kiemlicz.github.io/managed, operator: DoesNotExist}
  containers:
  - name: stub
    image: python:3.13-alpine
    command: [python, -u, -c]
    args:
    - |
      import json
      from http.server import BaseHTTPRequestHandler, HTTPServer
      led = {'power': True}
      class H(BaseHTTPRequestHandler):
          def reply(self, body):
              self.send_response(200); self.end_headers(); self.wfile.write(body)
          def do_GET(self):
              self.reply(json.dumps({'ok': True, 'result': {'enabled': True, 'leds': led}}).encode())
          def do_POST(self):
              body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
              print(self.path, body.decode(), flush=True)
              if self.path == '/api/atx/power?action=off':
                  led['power'] = False
              self.reply(b'[]')
      HTTPServer(('', 8080), H).serve_forever()
---
apiVersion: v1
kind: Service
metadata: {name: fake-routeros}
spec:
  selector: {app: fake-routeros}
  ports: [{port: 80, targetPort: 8080}]
YAML
kubectl -n nekromancer wait --for=condition=Ready pod/fake-routeros --timeout=180s

step "workload, running before the idle timer can start"
kubectl create deployment plex --image=registry.k8s.io/pause:3.10
kubectl patch deployment plex --type=merge -p '{"spec":{"template":{"spec":{"nodeSelector":{"workload":"plex"}}}}}'
kubectl rollout status deployment/plex --timeout=180s

step "install"
cat >"$TMP/values.yaml" <<YAML
image: {repository: nekromancer, tag: e2e, pullPolicy: Never}
config:
  defaultIdleTimeout: 15s
  notReadyGrace: 10s
  wakeCooldown: 30s
  powerOffRetry: 1h
  mikrotik: {url: "http://fake-routeros.nekromancer.svc"}
  hosts:
  - name: $HOST
    labels: {workload: plex}
    wake: mikrotik
    mac: "AA:BB:CC:DD:EE:01"
    interface: bridge
    address: "$IP"
  - name: $ATX_HOST
    labels: {workload: atx}
    wake: mikrotik
    shutdown: pikvm
    mac: "AA:BB:CC:DD:EE:02"
    interface: bridge
    pikvmURL: "http://fake-routeros.nekromancer.svc"
YAML
helm install nekromancer charts/nekromancer -n nekromancer -f "$TMP/values.yaml"
kubectl -n nekromancer rollout status deploy/nekromancer-controller --timeout=180s
controller_node=$(kubectl -n nekromancer get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].spec.nodeName}')
case "$controller_node" in "$HOST"|"$ATX_HOST") echo "controller landed on a managed node"; exit 1;; esac
wait_for "agent scheduled" '[ "$(kubectl -n nekromancer get ds nekromancer-agent -o jsonpath={.status.desiredNumberScheduled})" = 1 ]'
wait_for "agent pod created" '[ -n "$(kubectl -n nekromancer get pod -l app.kubernetes.io/component=agent -o name)" ]'
agents=$(kubectl -n nekromancer get pod -l app.kubernetes.io/component=agent -o jsonpath='{.items[*].spec.nodeName}')
[ "$agents" = "$HOST" ] || { echo "agent runs on: $agents"; exit 1; }
echo "ok: agent only on the os shutdown host"
for sa in controller agent; do
  kubectl auth can-i list nodes --as="system:serviceaccount:nekromancer:nekromancer-$sa" | grep -q yes
done
if kubectl auth can-i delete nodes --as=system:serviceaccount:nekromancer:nekromancer-agent 2>/dev/null | grep -q yes; then
  echo "agent can delete nodes"; exit 1
fi

step "workload keeps the node awake"
sleep 25
[ -z "$(annotation power-state)" ] || { echo "reaped a busy node"; exit 1; }
echo "ok: busy node left alone"

step "pikvm shutdown"
wait_for "idle pikvm host pressed" 'kubectl -n nekromancer logs fake-routeros | grep -q "/api/atx/power?action=off"'
wait_for "pikvm host's node deleted on LED off" '! kubectl get node "$ATX_HOST"'
docker stop "$ATX_HOST" >/dev/null

step "use case 1: idle shutdown"
kubectl scale deployment plex --replicas=0
wait_for "idle-since stamped" '[ -n "$(annotation idle-since)" ]'
wait_for "power-state=off after timeout" '[ "$(annotation power-state)" = off ]'
wait_for "shutdown requested" '[ -n "$(annotation shutdown-requested)" ]'
[ "$(kubectl get node "$HOST" -o jsonpath='{.spec.unschedulable}')" = true ]
echo "ok: cordoned"
sleep 10
kubectl get node "$HOST" >/dev/null || { echo "node deleted while host was answering pings"; exit 1; }
echo "ok: node kept while host answers ping"
docker stop "$HOST" >/dev/null
wait_for "node deleted after host went silent" '! kubectl get node "$HOST"'

step "use case 4: resurrection"
kubectl scale deployment plex --replicas=1
wait_for "wake call received" 'kubectl -n nekromancer logs fake-routeros | grep -q "/rest/tool/wol.*AA:BB:CC:DD:EE:01"'
docker start "$HOST" >/dev/null
wait_for "node re-registered" 'kubectl get node "$HOST"'
kubectl rollout status deployment/plex --timeout=240s
echo "ok: pod running on resurrected node"

step "use case 2: annotation-driven shutdown"
kubectl annotate node "$HOST" "$MANAGED/power-state=off"
wait_for "workload evicted, pod pending again" '[ "$(kubectl get pods -l app=plex --field-selector=status.phase=Running -o name | wc -l)" = 0 ]'
wait_for "shutdown requested" '[ -n "$(annotation shutdown-requested)" ]'
docker stop "$HOST" >/dev/null
wait_for "node deleted" '! kubectl get node "$HOST"'

step "use case 3: host goes down on its own"
docker start "$HOST" >/dev/null
wait_for "node re-registered" 'kubectl get node "$HOST"'
kubectl rollout status deployment/plex --timeout=240s
docker stop "$HOST" >/dev/null
wait_for "node deleted without any annotation" '! kubectl get node "$HOST"'

step "PASS"
