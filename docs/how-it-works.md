# How it works

Nekromancer keeps all shutdown progress in Node annotations, so a controller restart resumes wherever it stopped.
The controller's only in-memory state is the wake cooldown; losing it costs at most one redundant wake.

## Components

```mermaid
flowchart LR
    subgraph cp["Unmanaged node"]
        NR["Node reconciler"]
        WR["Wake reconciler"]
    end
    API[("Kubernetes API")]
    subgraph host["Managed host"]
        AG["node-agent DaemonSet, shutdown: os hosts only"]
        OS["Host OS"]
        KL["kubelet"]
    end
    MT["MikroTik router"]
    PK["PiKVM"]

    NR <-->|"watch nodes and pods, patch, evict, delete"| API
    WR <-->|"watch pending pods, read nodes"| API
    AG <-->|"watch own Node"| API
    KL <-->|"register Node, run pods"| API
    AG -->|"shutdown -h now, through nsenter"| OS
    WR -->|"WoL request"| MT
    MT -.->|"magic packet"| host
    WR -->|"ATX power on"| PK
    NR -->|"ATX soft press on shutdown: pikvm, read power LED"| PK
    PK -.->|"ATX header"| host
    NR -.->|"ICMP probe, hosts without a PiKVM"| host
```

## Node state

The Node reconciler picks one branch on every reconcile, based only on the current Node.

```mermaid
stateDiagram-v2
    direction TB
    [*] --> Busy: Node registers with workload pods
    [*] --> Idle: Node registers with no workload pods
    Busy --> Idle: last workload pod gone, idle-since stamped
    Idle --> Busy: workload pod arrives, idle-since removed
    Idle --> Draining: idle timeout passed, power-state set to off
    Busy --> Draining: admin sets power-state to off
    Idle --> Draining: admin sets power-state to off
    Draining --> PoweringOff: drained, shutdown-requested stamped
    PoweringOff --> PoweringOff: still online after powerOffRetry, request again
    PoweringOff --> Deleted: host confirmed offline
    Draining --> Deleted: NotReady and confirmed offline
    Busy --> NotReady: Ready condition lost
    Idle --> NotReady: Ready condition lost
    NotReady --> Busy: Ready again, workload pods present
    NotReady --> Idle: Ready again, no workload pods
    NotReady --> Draining: admin sets power-state to off
    NotReady --> Deleted: grace passed and confirmed offline
    Deleted --> [*]
```

Nodes without `managed: "true"`, control plane nodes, and nodes missing from the inventory never enter this state machine.

## Use case 1: idle detection

Pod events on a node trigger a reconcile of that node. A timer covers the case where nothing else changes.

```mermaid
sequenceDiagram
    autonumber
    participant KEDA
    participant API as Kubernetes API
    participant NR as Node reconciler

    KEDA->>API: scale Deployment to 0
    API-->>NR: pod deleted on mini1
    NR->>API: list pods on mini1
    API-->>NR: only DaemonSet pods, or none
    NR->>API: patch idle-since=now
    Note over NR: requeue after the idle timeout

    alt workload returns before the timeout
        API-->>NR: pod bound to mini1
        NR->>API: remove idle-since
    else timeout passes
        NR->>API: list pods on mini1
        API-->>NR: still no workload pods
        NR->>API: patch power-state=off
        Note over NR: continues as the shutdown below
    end
```

## Use case 2: shutdown

Manual shutdown enters here directly through `kubectl annotate`, and idle shutdown enters here through the patch above.
The Node is kept until the host is confirmed down.
Each host uses exactly one shutdown method, picked in the inventory.

```mermaid
sequenceDiagram
    autonumber
    actor Admin
    participant API as Kubernetes API
    participant NR as Node reconciler
    participant AG as node-agent
    participant Host as Host OS
    participant PK as PiKVM

    Admin->>API: annotate power-state=off
    API-->>NR: Node updated
    NR->>API: cordon

    loop every 5s until no workload pods remain
        NR->>API: evict each workload pod
        Note over NR,API: a refused eviction, such as a PDB, is retried
    end

    NR->>API: patch shutdown-requested=now

    alt shutdown is os
        API-->>AG: own Node updated
        AG->>AG: check managed, not control plane, power-state=off
        AG->>Host: shutdown -h now, through nsenter
    else shutdown is pikvm
        Note over AG: not scheduled on this host
        NR->>PK: GET /api/atx
        PK-->>NR: power LED state
        opt LED is on
            NR->>PK: POST /api/atx/power?action=off
            PK->>Host: ATX soft press
        end
    end
    Host->>Host: graceful shutdown

    loop every 5s until confirmed offline
        alt host uses a PiKVM
            NR->>PK: read power LED
        else no PiKVM
            NR->>Host: up to 3 ICMP echoes
        end
        opt online for longer than powerOffRetry
            NR->>API: rewrite shutdown-requested, warning event
            Note over NR,PK: the agent sees a new timestamp, or the PiKVM gets another press
        end
    end

    NR->>API: delete Node, UID precondition
```

A probe error counts as online, so uncertainty never deletes a Node.

## Use case 3: physical power button

Nekromancer needs no help from the host here. It sees the Node lose Ready and checks the host itself.

```mermaid
sequenceDiagram
    autonumber
    actor Person
    participant Host as Host OS
    participant CM as Node lifecycle controller
    participant API as Kubernetes API
    participant NR as Node reconciler

    Person->>Host: press power button
    Host->>Host: graceful shutdown
    Note over Host,CM: kubelet stops reporting
    CM->>API: set Ready=Unknown
    API-->>NR: Node updated
    Note over NR: wait for the rest of notReadyGrace

    loop every 30s until confirmed offline
        NR->>Host: probe, ICMP or PiKVM power LED
    end

    NR->>API: delete Node, UID precondition
```

If a host-side script also sets `power-state=off`, the shutdown branch runs instead.
Once the Node is NotReady it probes before draining, because a dead kubelet would never finish terminating pods.

## Use case 4: resurrection

The Node no longer exists, so the pending pod is the only signal.
Its `nodeSelector` and required node affinity are matched against the static inventory.

```mermaid
sequenceDiagram
    autonumber
    participant KEDA
    participant API as Kubernetes API
    participant Sched as Scheduler
    participant WR as Wake reconciler
    participant Prov as MikroTik or PiKVM
    participant Host as Host OS

    KEDA->>API: scale Deployment to 1
    Sched->>API: set PodScheduled=False, reason Unschedulable
    API-->>WR: pod wants a wake
    WR->>WR: match pod against inventory labels
    WR->>API: get Node of each matching host

    alt a matching host has a live Node
        Note over WR: no wake, recheck in 30s
    else a wakeable host is in its cooldown
        Note over WR: no wake, recheck in 30s
    else every matching host is mid power off
        Note over WR: no wake, recheck in 30s
    else wakeable hosts available
        loop least recently tried first, until one call succeeds
            WR->>Prov: POST /rest/tool/wol or /api/atx/power?action=on
        end
        Prov->>Host: magic packet or ATX power on
        Note over WR: start cooldown, recheck in 30s
        Host->>Host: boot
        Host->>API: kubelet registers a fresh Node with its labels
        Sched->>API: bind pod to the new Node
    end
```

Pods without a `nodeSelector` or required node affinity, DaemonSet pods, and pods already bound to a node never wake anything.
