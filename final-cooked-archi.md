# UBS Agent Deployment — Final Architecture (Detailed Flow)

Our proposed architecture (POC v2 native self-registering mechanism +
durable orchestration, no CRD, no idempotency store, automatic rollback),
drawn in a detailed end-to-end flow.

```mermaid
flowchart TD
    A[Developer / CI-CD] -->|Submit deployment request| B[Deployment Gateway]

    B -->|Authenticate, validate schema + image signature| C[Event Queue / Broker]

    C -->|Partition by Blueprint ID| D[Cell Worker Pool]

    D --> E[Deployment Workflow]

    E --> F{Acquire per-Blueprint Serialization Lock}

    F -->|Held by another operation| G[Retry / Requeue]
    G --> C

    F -->|Lock acquired| H[Read Current Agent from Blueprint]

    H --> I[Blueprint API]
    H --> J[Durable Workflow State]

    I --> K[Apply native Deployment to UK8s<br/>maxSurge=1, maxUnavailable=0]

    K --> L[UK8s starts new pod<br/>old pod keeps fully serving]

    L --> M[Pod supervisor: mint Agent ID]
    M --> N[Graph API]
    N --> O[Pod supervisor: register Agent ID<br/>against Blueprint]
    O --> P[Blueprint API]

    P --> Q[Pod passes readiness gate<br/>only after self-registration]

    Q --> R{New Version Healthy?<br/>readiness + health gates}

    R -->|No - failed| S[AUTOMATIC ROLLBACK pre-promotion<br/>delete broken candidate]
    S --> T[Old version still serving<br/>it was never touched]
    T --> J
    T --> U[Emit ROLLED_BACK event + alert]

    R -->|Yes| V[Promote: new pod already registered<br/>itself as current in Blueprint]

    V --> W{Old Version Exists?}

    W -->|No| X[Mark Deployment Complete]
    W -->|Yes| Y[Old pod scaled down by native rollout]

    Y --> Z[Old pod supervisor: deregister own Agent ID on preStop]
    Z --> AA{Deregister Successful?}

    AA -->|No| AB[Retry; if exhausted, leave orphaned ID<br/>for detection sweep]
    AB --> AC[Old pod exits anyway<br/>never block shutdown indefinitely]
    AA -->|Yes| AC
    AC --> X

    X --> J
    X --> AD[Release per-Blueprint Lock]
    AD --> AE[Emit Deployment Completed Event]

    AE --> AF{Post-promotion health breach<br/>in observation window?}
    AF -->|No| AG[Stable]
    AF -->|Yes, first time| AH[GUARDED AUTO-ROLLBACK<br/>redeploy previous known-good]
    AH --> C
    AF -->|Yes, second time in window| AI[Escalate to human<br/>break the flap loop]

    %% Reconciliation / detection sweep
    AJ[Periodic Reconciliation + Orphaned-ID Sweep] --> E
    AK[Worker Crash / Event Lost] --> AJ

    classDef controlplane fill:#e1f5fe,stroke:#0277bd
    classDef external fill:#fff3e0,stroke:#ef6c00
    classDef state fill:#f3e5f5,stroke:#7b1fa2
    classDef decision fill:#fffde7,stroke:#f9a825
    classDef native fill:#e8f5e9,stroke:#2e7d32
    classDef rollback fill:#fdecea,stroke:#c62828

    class B,D,E controlplane
    class I,N,P,K,L external
    class J,C state
    class F,R,W,AA,AF decision
    class M,O,Q,V,Y,Z,AC native
    class S,T,AH rollback
```

## Legend

- **Blue (control plane):** Deployment Gateway, cell worker pool, workflow —
  the durable orchestration layer.
- **Orange (external / cluster):** Blueprint API, Graph API, UK8s, and the
  native Deployment/pod actions.
- **Green (native mechanism):** the POC v2 core — pod self-mints,
  self-registers, self-deregisters; cutover driven by `maxUnavailable: 0`.
- **Red (rollback):** automatic pre-promotion rollback, and guarded
  post-promotion rollback.
- **Purple (state):** durable workflow state, event queue.
- **Yellow (decisions):** lock acquisition, health, old-version existence,
  deregister success, post-promotion breach.

## How this maps to the reference structure you liked

| Reference diagram node | Our architecture equivalent |
|---|---|
| Agent Lifecycle Gateway | Deployment Gateway |
| Event Queue / Broker → partition by Blueprint | Same, partition by Blueprint ID |
| Go Worker Pool / Reconciler | Cell Worker Pool + Deployment Workflow |
| Acquire Blueprint Lock | Per-Blueprint serialization lock |
| Generate Agent ID → Graph API | Done **inside the pod** (supervisor), not by the worker |
| Deploy New Agent / Wait for Deployment | Apply native Deployment + `maxUnavailable: 0` |
| New Version Healthy? | Readiness gate + health gates |
| Keep Existing Version (on fail) | **Automatic rollback** (old was never touched) |
| Promote / Delete Old | Native rollout retires old; pod self-deregisters |
| Periodic Reconciliation | Reconciliation + orphaned-Agent-ID detection sweep |

The two meaningful differences from the reference (both deliberate, both
core to our design): Agent ID minting/registration happens **inside the
pod** rather than in the worker, and the "keep existing version" branch is
an **automatic rollback** rather than a human-gated wait.
```