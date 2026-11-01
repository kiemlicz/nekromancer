# Nekromancer

A Kubernetes controller that powers bare-metal nodes off when they sit idle and wakes them when a pod needs them.
It pairs with KEDA scale-to-zero: KEDA empties the node, Nekromancer reaps it, and the next scale-up raises it again.
See [how it works](docs/how-it-works.md) for sequence diagrams.

## Lifecycle

| Trigger | What happens |
|---|---|
| Node has no workload pods for the idle timeout | `idle-since` stamped, then `power-state=off`, then the shutdown below |
| `kubectl annotate node <node> nekromancer.kiemlicz.github.io/power-state=off` | Cordon, drain, power off, verify offline, delete the Node |
| Physical power button, or any other host-side shutdown | Node goes `NotReady`, host verified offline, Node deleted |
| Pending, `Unschedulable` pod whose selector matches an inventory host | Host woken, kubelet registers a fresh Node, scheduler binds the pod |

## Labels and annotations

All use the `nekromancer.kiemlicz.github.io/` prefix.

| Key | Kind | Set by | Meaning |
|---|---|---|---|
| `managed: "true"` | label | kubelet `--node-labels` | Opts the node in |
| `idle-timeout: "10m"` | label | kubelet `--node-labels` | Per-node idle timeout override |
| `power-state: "off"` | annotation | admin or controller | Shut this node down now |
| `idle-since: <RFC3339>` | annotation | controller | Idle timer, survives restarts |
| `shutdown-requested: <RFC3339>` | annotation | controller | Drain finished, tells the node-agent to power off |

Labels must be set through kubelet flags, because the Node object is deleted on every power off.

## Safety rules

- Nodes without `managed: "true"`, and anything labelled `node-role.kubernetes.io/control-plane`, are never touched.
  The node-agent checks this independently.
- A managed node missing from the inventory is ignored and gets a warning event.
  Without an inventory entry it can't be powered off or verified.
- A Node is only deleted after the host is confirmed down. A probe error counts as online.
- Power off waits for the drain.
  PodDisruptionBudgets are honoured, so a blocked eviction retries indefinitely rather than forcing anything.
- The chart keeps the controller off managed nodes, so it can't power off its own host.
- The node-agent is privileged and shares the host's PID namespace, because it has to run `shutdown` on the host.
  The chart only schedules it on inventory hosts with `shutdown: os`, so PiKVM shutdown hosts never run it.

## Install

Create the credentials Secret out of band, encrypted in your GitOps repo. Never commit it in plain text.
It holds `MIKROTIK_USERNAME`, `MIKROTIK_PASSWORD`, `PIKVM_USERNAME` and `PIKVM_PASSWORD`;
only the pair for the providers you use is needed. There is one credential pair per provider type.

```yaml
# values.yaml
credentialsSecret: nekromancer-credentials
config:
  defaultIdleTimeout: 10m
  mikrotik:
    url: https://192.168.1.1
    caFile: ""                  # or insecureSkipVerify: true for a self-signed cert
  hosts:
  - name: mini1                 # must equal the Node name
    labels: {workload: plex}    # must mirror the kubelet --node-labels
    wake: mikrotik
    mac: "AA:BB:CC:DD:EE:01"
    interface: bridge
    address: 192.168.1.50       # IPv4, pinged to verify the host is down
  - name: mini2
    labels: {workload: nas}
    wake: pikvm
    shutdown: pikvm             # default is os
    pikvmURL: https://pikvm-mini2.lan
```

```bash
helm install nekromancer oci://ghcr.io/<owner>/charts/nekromancer --version 1.2.3 -n nekromancer --create-namespace -f values.yaml
```

Other settings, with defaults: `notReadyGrace: 1m`, `wakeCooldown: 5m`, `powerOffRetry: 2m`, and `pikvm.insecureSkipVerify` / `pikvm.caFile`.

The RouterOS user needs the `rest-api` policy and rights to run `/tool/wol`.

### Providers

Each host picks how it wakes and how it shuts down separately.

| `wake` | Call |
|---|---|
| `mikrotik` | `POST /rest/tool/wol` |
| `pikvm` | `POST /api/atx/power?action=on` |

| `shutdown` | How |
|---|---|
| `os` (default) | The node-agent runs `shutdown -h now` on the host |
| `pikvm` | ATX soft press, `POST /api/atx/power?action=off`, skipped if the power LED is already off |

A host using PiKVM for either is verified offline by its power LED. Any other host needs `address`, and is verified with 3 pings, 1s timeout each.

### How a node maps to its hardware

The Node name is looked up in `hosts[].name` with an exact match, and that entry's own `pikvmURL`, MAC and address are used.
The ATX API has no target host, so one URL means one machine, and two hosts sharing a `pikvmURL` fail config validation.

Nothing checks that a PiKVM is really wired to the host named in its entry, so a swapped URL presses the other machine's button.
Its LED then answers for the wrong machine too, which can delete the Node of a host that's still running.
Check each mapping by hand when adding a host.

## Images and chart

`.github/workflows/image.yaml` pushes the image to `ghcr.io/<owner>/nekromancer`, then the chart to `oci://ghcr.io/<owner>/charts/nekromancer`.
The chart job only runs if the image built, and the packaged chart defaults to the image built from the same commit.

| Git ref | Image tags | Chart version | Chart `appVersion` |
|---|---|---|---|
| `main` branch | `main`, `sha-<short>` | `<Chart.yaml version>-main-<short>` | `sha-<short>` |
| `v1.2.3` tag | `1.2.3`, `sha-<short>`, `latest` | `1.2.3` | `1.2.3` |

To build locally, run `make image IMAGE=<registry>/nekromancer:dev`.

## Behaviour worth knowing

- **Waking:** pods with no `nodeSelector` or required node affinity never wake anything,
  because being unschedulable isn't a placement problem for them.
- **One host at a time:** if any matching host already has a live Node, nothing else is woken, even if that host is full.
  This prevents a wake and reap loop for pods that a second machine wouldn't help.
  With several matching hosts, the least recently tried is woken after `wakeCooldown`.
- **Cancelling:** removing `power-state` before the host goes down stops the shutdown if the agent hasn't acted yet,
  but the node stays cordoned. Run `kubectl uncordon` yourself.
- **False offline:** if a host blocks ICMP or is cut off from the controller while `NotReady`, it looks offline and its Node gets deleted.
  A kubelet generally doesn't re-register until restarted, and WoL won't help a running host.
  Allow ICMP from the controller to each host, or use PiKVM.
- **Bare pods:** a manual shutdown evicts them too, and nothing recreates them.

## Development

```bash
make test   # unit tests plus a lifecycle test against a real API server (envtest)
make lint
make e2e    # real image and chart on a throwaway kind cluster, needs Docker
```

`make e2e` uses its own kubeconfig and never touches your current context.
kind nodes have no `shutdown` binary, so a stub records that the node-agent ran it inside the host's namespaces, and `docker stop` stands in for the power going off.
A real `shutdown -h now`, a real PiKVM and a real RouterOS are not exercised.
