# Architecture diagram (Mermaid)

## Full lifecycle flow, styled end-to-end

The same shape of diagram you'd draw for a controller-based system --
deploy event in, decision diamonds, retry loops, color-coded by
component -- but mapped onto what v2 actually contains. The dashed box at
the bottom lists what a queue/worker-pool/lock design would need that this
one doesn't, and why: there's exactly one writer per blueprint (Kubernetes'
own Deployment object), so there's nothing left to coordinate.

```mermaid
flowchart TD
    A["Developer / CI-CD"] -->|"kubectl apply / patch image"| B["Kubernetes Deployment<br/>RollingUpdate maxSurge=1, maxUnavailable=0"]

    B --> C["New pod scheduled --<br/>agent-supervisor starts as PID 1"]

    C --> D["Load identity: baked-in /.env<br/>(BLUEPRINT_ID, AGENT_IMAGE, UBS_API_BASE)<br/>+ BLUEPRINT_TOKEN from Secret"]

    D --> E["Graph API: mint new Agent ID"]

    E --> F{"Mint succeeded?"}
    F -->|"no, retry"| G["sleep 3s<br/>(in-process, up to 10x)"]
    G --> E
    F -->|"no, retries exhausted"| H(["log FATAL, exit 1 --<br/>kubelet restarts container,<br/>pod never goes Ready"])

    F -->|yes| I["Blueprint API: register<br/>agentId + image"]

    I --> J{"Register succeeded?"}
    J -->|"no, retry"| G
    J -->|"no, retries exhausted"| H
    J -->|yes| K["healthy = true --<br/>serve GET /healthz on :8080"]

    K --> L["exec nginx as child process --<br/>its stdout/stderr feed this<br/>container's own real stdout"]

    L --> M["Kubernetes readinessProbe:<br/>GET /healthz"]

    M --> N{"Ready?"}
    N -->|not yet| M
    N -->|yes| O["Traffic routed to new pod --<br/>old pod still fully serving in parallel"]

    O --> P["Kubernetes sends SIGTERM to OLD pod<br/>(STOPSIGNAL overridden to SIGTERM --<br/>nginx's own default SIGQUIT would<br/>never reach agent-supervisor's handler)"]

    P --> Q["Old pod: healthy = false immediately --<br/>readinessProbe fails, no new traffic"]

    Q --> R["Blueprint API: deregister old Agent ID"]

    R --> S{"Deregister succeeded?"}
    S -->|"no, after 5 attempts"| T["log WARNING, continue anyway --<br/>never block shutdown indefinitely"]
    T --> U
    S -->|yes| U["forward SIGTERM to nginx,<br/>wait for exit, then exit 0"]

    U --> V(["Kubernetes removes old pod --<br/>exactly one version now running"])

    B -.->|"bad image tag"| W["New pod: ImagePullBackOff --<br/>agent-supervisor never even starts"]
    W -.-> X["maxUnavailable=0 blocks any scale-down<br/>of old pod -- native Kubernetes,<br/>no code of ours involved"]
    X -.-> Y(["Old pod keeps serving indefinitely,<br/>never receives SIGTERM"])

    subgraph Absent["What a queue/worker-pool/lock design would need -- and why this doesn't"]
        direction LR
        Z1["Event Queue / Broker"]
        Z2["Go Worker Pool"]
        Z3["Acquire Blueprint Lock"]
        Z4["Periodic Reconciliation"]
        Z5["Controller Crash Recovery"]
        Z6["-- all replaced by: one Deployment object<br/>per blueprint is Kubernetes' own single<br/>writer, and there's no separate controller<br/>process that can crash or lose events"]
        Z1 -.-> Z6
        Z2 -.-> Z6
        Z3 -.-> Z6
        Z4 -.-> Z6
        Z5 -.-> Z6
    end

    classDef supervisor fill:#e1f5fe,stroke:#0277bd
    classDef external fill:#fff3e0,stroke:#ef6c00
    classDef state fill:#f3e5f5,stroke:#7b1fa2
    classDef decision fill:#fffde7,stroke:#f9a825
    classDef absent fill:#fafafa,stroke:#9e9e9e,stroke-dasharray: 4 3

    class C,K,L,Q,U supervisor
    class B,E,I,M,O,P,R,V,W,X,Y external
    class D state
    class F,J,N,S decision
    class Z1,Z2,Z3,Z4,Z5,Z6 absent
```

## agent-supervisor internal flow (this is the whole design)

Everything the agent does — register, serve, deregister — happens inside
this one process, which is the container's actual PID 1. There is no
Kubernetes lifecycle hook and no controller anywhere in this diagram.

```mermaid
flowchart TD
    Start(["Container starts<br/>agent-supervisor is PID 1"]) --> LoadEnv["Load /.env baked into image<br/>(BLUEPRINT_ID, AGENT_IMAGE, UBS_API_BASE)<br/>+ real env BLUEPRINT_TOKEN from Secret"]
    LoadEnv --> Mint["POST /graph/agents<br/>mint new agent ID"]

    Mint -->|201 + agentId| Register["POST /blueprint/&lt;id&gt;/register<br/>agentId + image"]
    Mint -->|error / no agentId| RetryMint{"attempt < 10?"}
    RetryMint -->|yes, sleep 3s| Mint
    RetryMint -->|no| FatalStart(["log FATAL, exit 1<br/>kubelet restarts container<br/>pod never goes Ready"])

    Register -->|200| MarkHealthy["healthy = true"]
    Register -->|error / non-200| RetryReg{"attempt < 10?"}
    RetryReg -->|yes, sleep 3s| Mint
    RetryReg -->|no| FatalStart

    MarkHealthy --> Health["serve GET /healthz on :8080<br/>-- now returns 200"]
    Health --> StartWorkload["exec nginx as a child process<br/>its stdout/stderr feed straight into<br/>this container's own stdout"]
    StartWorkload --> Serving(["agent live and serving --<br/>readinessProbe passes, traffic routed"])

    Serving --> WaitEvent{"blocking select on:"}
    WaitEvent -->|"SIGTERM from kubelet<br/>(pod being replaced or deleted)"| Term["healthy = false<br/>readiness fails immediately,<br/>no new traffic"]
    WaitEvent -->|nginx exits unexpectedly| CrashPath(["log FATAL, exit 1<br/>kubelet restarts container --<br/>ordinary container restart, not a hook"])

    Term --> Dereg["POST /agents/&lt;id&gt;/deregister"]
    Dereg -->|200| Forward["forward SIGTERM to nginx,<br/>wait for it to exit"]
    Dereg -->|"fails after 5 attempts"| WarnLog["log WARNING and continue anyway --<br/>don't block shutdown forever over<br/>a failed cleanup call"] --> Forward
    Forward --> CleanExit(["exit 0 -- pod terminates"])
```

## Sequence: version transition, end to end

```mermaid
sequenceDiagram
    participant CICD as CI/CD
    participant K8s as Kubernetes (native)
    participant NewPod as New pod's agent-supervisor
    participant OldPod as Old pod's agent-supervisor
    participant UBS as UBS Blueprint + Graph API

    CICD->>K8s: apply Deployment (new image, maxSurge=1, maxUnavailable=0)
    K8s->>NewPod: schedule + start container (agent-supervisor is PID 1)
    Note over NewPod: mint + register run BEFORE nginx even starts --<br/>no readiness window, just program order
    NewPod->>UBS: mint agent ID (Graph API)
    NewPod->>UBS: register agent ID (Blueprint API)
    UBS-->>NewPod: registered
    NewPod->>NewPod: start /healthz (200 now), exec nginx as child
    Note over NewPod: readinessProbe now passes (GET /healthz)
    K8s->>K8s: routes traffic to NewPod<br/>(old still fully serving in parallel)
    K8s->>OldPod: sends SIGTERM (STOPSIGNAL overridden to SIGTERM,<br/>not nginx's default SIGQUIT)
    Note over OldPod: own signal handler catches it directly --<br/>no preStop hook, no kubelet-mediated exec
    OldPod->>OldPod: healthy = false (readiness fails now)
    OldPod->>UBS: deregister old agent ID
    UBS-->>OldPod: deregistered
    OldPod->>OldPod: forward SIGTERM to nginx, wait, exit 0
    K8s->>OldPod: pod terminated
```

## Failure path: broken new version never disrupts old

```mermaid
sequenceDiagram
    participant CICD as CI/CD
    participant K8s as Kubernetes (native)
    participant NewPod as New pod (broken image)
    participant OldPod as Old pod's agent-supervisor

    CICD->>K8s: apply Deployment (bad image)
    K8s->>NewPod: schedule
    Note over NewPod: ImagePullBackOff -- container never starts,<br/>agent-supervisor never even runs, /healthz never exists
    K8s--xOldPod: maxUnavailable=0 blocks any scale-down<br/>of old while new is not Ready
    Note over OldPod: keeps serving indefinitely, never receives SIGTERM,<br/>fully untouched
    Note over NewPod: stuck until CI/CD patches image back<br/>or rolls forward with a fix -- no other action needed
```

## Why no external controller, and no lifecycle hooks either

```mermaid
flowchart LR
    subgraph Old["Controller-watch design (rejected)"]
        direction TB
        A1["CI/CD applies manifest"] --> A2["K8s API server"]
        A2 -->|watch event, informer resync delay| A3["External controller<br/>reconcile loop"]
        A3 -->|separate call| A4["UBS Blueprint API"]
        A3 -->|separate call| A5["Delete old Deployment"]
    end

    subgraph Mid["Lifecycle-hook design (superseded v2)"]
        direction TB
        B1["CI/CD applies manifest"] --> B2["Kubelet starts new pod"]
        B2 -->|"postStart (exec), output not in kubectl logs"| B3["UBS Blueprint API"]
        B3 -->|readinessProbe gate| B4["Traffic routed"]
        B4 -->|native RollingUpdate| B5["Kubelet runs preStop (exec)"]
        B5 -->|synchronous| B3
    end

    subgraph New["Custom agent-supervisor design (current)"]
        direction TB
        C1["CI/CD applies manifest"] --> C2["Kubelet starts new pod"]
        C2 -->|"agent-supervisor IS PID 1, calls API directly"| C3["UBS Blueprint API"]
        C3 -->|"own /healthz, readinessProbe gate"| C4["Traffic routed"]
        C4 -->|native RollingUpdate| C5["Kubelet sends SIGTERM"]
        C5 -->|"own signal handler, logs to real stdout"| C3
    end
```
