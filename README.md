# Nekromancer

A Kubernetes controller that powers bare-metal nodes off when they sit idle and wakes them when a pod needs them.
It pairs with KEDA scale-to-zero: KEDA empties the node, Nekromancer reaps it, and the next scale-up raises it again.
See [how it works](docs/how-it-works.md) for sequence diagrams.

## Lifecycle

| Trigger | What happens |
|---|---|
| Node has no workload pods for the idle timeout | `idle-since` stamped, then `power-state=off`, then the shutdown below |
| `kubectl annotate node <node> nekromancer.kiemlicz.github.io/power-state=off` | Cordon, drain, power off, verify offline, delete the Node |
| Physical power button, or any other host-side shutdown | Nothing. The Node stays `NotReady`, and the host can still be woken |
| Pending, `Unschedulable` pod whose selector matches an inventory host | Host woken, kubelet registers or updates its Node, scheduler binds the pod |

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
- A Node is only deleted after a requested shutdown, once the host is confirmed down. A probe error counts as online.
- A `NotReady` Node is never deleted or shut down: a host that was switched off and one that is cut off look the same.
- Power off waits for the drain.
  PodDisruptionBudgets are honoured, so a blocked eviction retries indefinitely rather than forcing anything.
- The chart keeps the controller off managed nodes, so it can't power off its own host.
- The chart only schedules the node-agent on inventory hosts with `shutdown: os`, so a PiKVM shutdown host never gets a second shutdown.

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
  pikvm:
    insecureSkipVerify: true    # for the stock self-signed cert, or set caFile
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
    pikvmURL: https://pikvm.lan
    # pikvmPort: mini2          # PiKVM Switch port, default is the host name. "" for a PiKVM without a Switch
```

```bash
helm install nekromancer oci://ghcr.io/<owner>/charts/nekromancer --version 1.2.3 -n nekromancer --create-namespace -f values.yaml
```

Other settings, with defaults: `wakeCooldown: 5m`, `powerOffRetry: 2m`, and `pikvm.insecureSkipVerify` / `pikvm.caFile`.

`controller` and `agent` each take scheduling, security context, resources, pod metadata and `serviceAccount` settings;
`values.yaml` documents every property and its default. The image is built for amd64 and arm64.
The chart adds its safety rule to every required node affinity term, so `affinity` can only narrow where a pod runs.
With `rbac.create: false` you grant the roles yourself, and with `serviceAccount.create: false` the account must already exist.

To run the controller on the control plane, for example Raspberry Pis that are never powered off:

```yaml
controller:
  nodeSelector:
    node-role.kubernetes.io/control-plane: "true"   # k3s sets "true", kubeadm sets ""
  tolerations:
  - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
```

TLS options apply to all PiKVMs. The stock PiKVM certificate is self-signed for `localhost` and has no SANs, so `caFile` can't validate it:
use `insecureSkipVerify: true`, or install a certificate that names the host. Without verification, the PiKVM password is sent to whoever answers at that address.

The RouterOS user needs the `rest-api` policy and rights to run `/tool/wol`.

### Providers

Each host picks how it wakes and how it shuts down separately.

| `wake` | Call |
|---|---|
| `mikrotik` | `POST /rest/tool/wol` |
| `pikvm` | `POST /api/switch/atx/power?port=<index>&action=on`, skipped if the power LED is already on |

| `shutdown` | How |
|---|---|
| `os` (default) | The node-agent calls logind `PowerOff` over the host's system bus, a graceful OS shutdown like `shutdown -h now`. Needs systemd |
| `pikvm` | ATX soft press, `POST /api/switch/atx/power?port=<index>&action=off`, skipped if the power LED is already off |

A host using PiKVM for either is verified offline by its power LED. Any other host needs `address`, and is verified with 3 pings, 1s timeout each.

### How a node maps to its hardware

The Node name is looked up in `hosts[].name` with an exact match, and that entry's own `pikvmURL`, MAC and address are used.
A PiKVM reports nothing about the machine wired to it, so the pointer is yours to set, in one of two ways:

| Setup | Inventory | Calls |
|---|---|---|
| PiKVM Switch, one or several machines | `pikvmURL`, plus `pikvmPort` if the port name differs from the host name | `/api/switch`, `/api/switch/atx/power?port=<index>` |
| One machine per PiKVM, no Switch | `pikvmURL` and `pikvmPort: ""` | `/api/atx`, `/api/atx/power` |

### Setting up PiKVM Switch ports

List the ports first, as a stock install has no names:

```bash
curl -sk -H 'X-KVMD-User: admin' -H 'X-KVMD-Passwd: <password>' https://pikvm.lan/api/switch
```

`result.model.ports` holds one entry per port, and `result.atx.leds.power` the power LEDs in the same order.
Ports with a machine attached normally show `true` in `result.video.links` and `result.usb.links`.

Name each port in the PiKVM's port settings, or through the API. `pikvmPort` defaults to the host name, so using the Node name needs no extra config.
The API's `port` is the zero-based index, so the port with `id` 1 is `port=0`:

```bash
curl -sk -X POST -H 'X-KVMD-User: admin' -H 'X-KVMD-Passwd: <password>' \
  'https://pikvm.lan/api/switch/set_port_params?port=0&name=mini1'
```

Read `/api/switch` again to confirm the full name was stored.
PiKVM's docs only mention letters and numbers in names, so a Node name with hyphens is worth checking.
Then point each host at its port, with `pikvm.insecureSkipVerify: true` if the PiKVM still has its stock certificate:

```yaml
hosts:
- {name: mini1, wake: pikvm, shutdown: pikvm, pikvmURL: https://pikvm.lan}             # port named mini1
- {name: mini2, wake: pikvm, pikvmURL: https://pikvm.lan, pikvmPort: rack-2}            # port named rack-2
```

The port is looked up by exact name on every call, so moving it to another number is harmless.
If the name matches no port or several, nothing is pressed and the host doesn't count as offline.
Hosts sharing a `pikvmURL` must all resolve to different ports, or config validation fails.

### Check the wiring before relying on it

Nothing checks the physical wiring, so a wrongly named port or swapped URL presses the other machine's button.
Its LED then answers for the wrong machine too, which can delete the Node of a host that's still running.

Do these checks by hand when adding a host, with or without Switch ports:

1. **Power LED:** with the host running, its LED must read `true`.
   An LED that stays `false` usually means the wire isn't connected, so don't use PiKVM for that host.
   A requested shutdown would look finished at once and delete the Node of a running host,
   and a wake would press the button of a machine that's already on, which can shut it down.
   A `false` LED with live video and USB links is fine only if the machine really is off, on standby power.
2. **Right machine:** trigger one real shutdown on a host that's safe to take down, then confirm that this machine went down and its LED turned off.

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
- **One host at a time:** if any matching host already has a Ready Node, nothing else is woken, even if that host is full.
  This prevents a wake and reap loop for pods that a second machine wouldn't help.
  With several matching hosts, the least recently tried is woken after `wakeCooldown`.
- **Cancelling:** removing `power-state` before the host goes down stops the shutdown if the agent hasn't acted yet,
  but the node stays cordoned. Run `kubectl uncordon` yourself.
- **Hosts switched off by hand:** the pods stay on the `NotReady` Node until Kubernetes evicts them, 5 minutes by default.
  Only then does a replacement go pending and wake the host, and pods of a StatefulSet wait for an admin.
- **`power-state=off` on a `NotReady` Node:** the shutdown waits for Ready, and that host isn't woken for pending pods.
  If the host was off, it shuts down again after its next manual boot. To cancel, remove the annotation and run `kubectl uncordon`.
- **False offline:** if a host blocks ICMP during a requested shutdown, it looks offline as soon as the request is sent, so its Node is deleted.
  If the shutdown then failed, the host keeps running without a Node. Allow ICMP from the controller to each host, or use PiKVM.
- **Bare pods:** a manual shutdown evicts them too, and nothing recreates them.

## Development

```bash
make test   # unit tests plus a lifecycle test against a real API server (envtest)
make lint
make e2e    # real image and chart on a throwaway kind cluster, needs Docker
```

`make e2e` uses its own kubeconfig and never touches your current context.
`docker stop` stands in for a host powering off, and a stub plays RouterOS and the PiKVM.
kind nodes have no system bus, so the node-agent's real power off is only covered by `internal/agent/logind_test.go`,
which runs against a private bus with a fake logind.
