# Agent Version Lifecycle on UK8s — Design Proposal

**Author:** Krisha · AI Intern
**Status:** Proposal + working simulation POC (no CRDs, no third-party controllers)

---

## 1. Problem statement

On UK8s, an intern's namespace can contain multiple **blueprints**. Each
blueprint runs exactly one logical **agent**, but that agent evolves over
time — new capabilities, new prompts, new code — and each evolution is a new
**agent version**, minted with its own **agent ID** via the Graph API.

**Requirement:** at any moment, exactly one version of an agent may be live
under a given blueprint. When a new version is deployed successfully, the
previous version's Kubernetes resources must be removed and its agent ID
retired — but only *after* the new version is confirmed healthy. A failed
new version must never take down a working old one.

**Hard constraints, confirmed with the platform team:**

| Constraint | Detail |
|---|---|
| No CRDs | Developers must never author or maintain a custom resource. Everything they touch is a plain, stock `Deployment`. |
| No third-party controllers | No Argo Rollouts, Flagger, etc. Must be built in-house. |
| Blueprint state is external | The "current agent for this blueprint" fact lives inside UBS's own internal system, not in Kubernetes. We do not duplicate it. |
| No API locking | The UBS Blueprint API is plain REST — no transactions, no optimistic concurrency. Consistency is the controller's responsibility, not the API's. |
| Register/deregister are separate calls | There is no combined "swap" endpoint. Each is an independent REST call that can independently fail. |
| Trigger is CI/CD | A new version is triggered by CI/CD applying a new manifest, not a human running kubectl, and not the controller polling an external registry. |

---

## 2. Why not the two obvious approaches

### 2.1 A traditional Kubernetes Operator + CRD

The instinctive design: define an `AgentDeployment` custom resource, write a
`controller-runtime` operator that reconciles it, store rollout phase in
`.status`. This was our starting point and is architecturally sound *in
isolation* — but it fails constraint #1 outright: it requires developers to
learn a new `kind`, and any manifest-authoring mistake against that schema
is now a new class of human error UBS explicitly wants to avoid. Rejected.

### 2.2 A Mutating Admission Webhook

We reviewed a colleague's working webhook (`msfoundry/code/mutating-webhook`)
that injects a random GUID into every pod at admission time. It's a solid,
validated pattern for one specific job — **guaranteeing an env var lands on
a pod regardless of who created it** — but admission webhooks are
fundamentally **stateless, single-shot, fire-once** events. They cannot
compare against a previous version, cannot wait for readiness, cannot call
an external API and then act on a later signal, and cannot delete anything.
It solves a different, narrower problem than version lifecycle management.
We keep it, but only for its original narrow purpose (§6).

---

## 3. Proposed design: label-driven controller over native Deployments

### 3.1 The only thing a developer/CI-CD needs to know

Every agent `Deployment` carries two labels:

```yaml
metadata:
  labels:
    ubs.io/blueprint-id: bp-8891              # which blueprint this agent belongs to
    ubs.io/agent-name: stock-research-agent   # logical name, stable across every version
```

To ship a new version, CI/CD applies a **new Deployment object** with a new
name (e.g. suffixed by build number or image tag) but the **same two
labels**. This is a completely standard CI/CD templating convention —
nothing bespoke, nothing to install, nothing that looks unlike a normal
Kubernetes manifest.

No CRD. No new API group. No new verbs. No schema to get wrong.

### 3.2 What the controller does

A single controller process (built with `client-go`, no `controller-runtime`
CRD machinery needed since there's no custom resource) watches all
`Deployment`s carrying `ubs.io/blueprint-id`, and groups them by
`(namespace, blueprint-id, agent-name)`.

For each group, exactly two states are possible:

```
1 Deployment in the group  → steady state or brand-new agent
2 Deployments in the group → a version transition is in flight
```

**Case 1 — one Deployment:**
If it has no recorded agent ID yet, this is the very first deploy for this
blueprint: wait for it to become `Ready`, mint an agent ID via the Graph
API, register it against the blueprint. If it already has a recorded agent
ID, this is steady state — no-op.

**Case 2 — two Deployments (a transition):**
The older one (by `creationTimestamp`) is `old`; the newer is `incoming`.

```
 CI/CD applies "incoming"
        │
        ▼
 old keeps serving, completely untouched
        │
        ▼
 wait for incoming to become Ready  ──(never happens)──► stays here forever,
        │                                                 old still serving,
        │ Ready                                           nothing else occurs
        ▼
 mint new agent ID (Graph API)
        │
        ▼
 register new agent ID against blueprint (UBS API)  ──(fails)──► retry;
        │                                                         old still untouched
        │ success
        ▼
 deregister OLD agent ID (UBS API)  ──(fails)──► retry;
        │                                         new is live+registered,
        │                                         old Deployment stays running
        │                                         (harmless, just needs cleanup)
        │ success
        ▼
 delete OLD Deployment
        │
        ▼
 exactly one Deployment remains → back to steady state
```

The invariant that matters most: **the old Deployment is deleted in exactly
one place in the code, and only after both the readiness check and the
registration call have independently succeeded.** There is no path in this
design where old is deleted before new is proven live.

### 3.3 Where "phase" state lives (there is no ConfigMap, no CRD, no database)

This was a real design fork we considered and rejected: persisting rollout
phase in a controller-owned `ConfigMap`. We rejected it because it would be
a second, parallel source of truth alongside UBS's own internal blueprint
state — exactly the kind of drift risk a bank-grade system shouldn't carry.

Instead, every reconcile recomputes everything from two live sources:

1. **Kubernetes** — literally, how many Deployments currently share this
   `(blueprint-id, agent-name)` group. 1 = steady state, 2 = mid-transition.
2. **UBS's blueprint API** — which agent ID is currently registered.

The only controller-owned bookkeeping is a single annotation,
`ubs.io/agent-id`, written onto a Deployment *after* its agent ID is
successfully registered. This isn't "extra state" in the risky sense — it's
scoped to the object it describes, disappears when that object is deleted,
and is only ever used to know which ID to pass to the deregister call. If
the controller crashes and restarts mid-transition, it re-derives exactly
where it left off from this annotation plus the two live sources above —
nothing to resync, nothing to reconcile against a stale cache.

---

## 4. Failure and concurrency handling

### 4.1 Single-writer discipline (no locking on the UBS API, so we provide our own)

The controller runs as a single active replica (or with client-go leader
election if HA is later required). Reconciles are also serialized **per
group key**, via a `workqueue`, so two version transitions for the same
blueprint can never race each other even within one controller process.
This directly compensates for the UBS Blueprint API having no transactions
of its own — the API doesn't need locking if only one caller in the world
ever calls it for a given blueprint at a time.

### 4.2 Every external call is independently retryable

Minting, registering, and deregistering are three separate HTTP calls with
three separate failure modes. The design treats each as retryable in
isolation:

- **Mint fails** → nothing was created yet, reconcile just retries later.
- **Register fails** → `incoming` Deployment exists but isn't yet the active
  agent; `old` was never touched. Safe to retry indefinitely.
- **Deregister fails** → `new` is already live and correctly registered
  (the important thing, from a user-facing standpoint, already happened).
  `old`'s Deployment is left running rather than deleted — a harmless,
  visible, alertable state (duplicate compute cost, no correctness issue) —
  until the retry succeeds.

### 4.3 What "rollback" means here

Because old is never touched until new is fully proven, rollback in the
failure case is just: **delete the broken incoming Deployment.** There is
nothing to restore, because nothing about the old version ever changed.
This is a direct, deliberate simplification versus the original CRD-based
sketch, which is a genuine advantage of the "old survives untouched" design
over recycling a single Deployment via native `RollingUpdate` (which begins
tearing down old pods as soon as new ones pass readiness, and offers no
clean way to hold that back until an external registration call succeeds).

---

## 5. Component diagram

```
┌─────────────┐      applies       ┌───────────────────────────────┐
│   CI/CD      │ ─────────────────▶│  Deployment (plain, native)    │
│  pipeline    │  new manifest,     │  labels: blueprint-id,          │
│              │  same 2 labels     │          agent-name             │
└─────────────┘                    └───────────────┬─────────────────┘
                                                     │ watched via
                                                     │ label selector
                                                     ▼
                                    ┌───────────────────────────────┐
                                    │   ubs-agent-controller          │
                                    │   (client-go, single replica)   │
                                    │                                  │
                                    │  groups Deployments by           │
                                    │  (ns, blueprint-id, agent-name)  │
                                    └───────┬───────────────┬────────┘
                                            │               │
                             mint / register /        get / patch / delete
                             deregister (HTTP)         (Kubernetes API)
                                            │               │
                                            ▼               ▼
                              ┌───────────────────┐  ┌─────────────────┐
                              │  UBS Blueprint API  │  │  Deployments in   │
                              │  + Graph API         │  │  the namespace     │
                              │  (internal to UBS)   │  │                    │
                              └───────────────────┘  └─────────────────┘
```

---

## 6. Where the admission webhook still fits

Kept for its original, narrow purpose only: guaranteeing that
`BLUEPRINT_TOKEN` (and any other required env vars) land on every agent pod
at creation time, even if someone bypasses CI/CD and applies a raw manifest
by hand. It does **not** carry any version-lifecycle logic — that would
re-introduce the stateless/single-shot problem described in §2.2. Scoped
via `namespaceSelector` to skip system namespaces, exactly as in the
reviewed implementation.

---

## 7. Summary comparison

| | CRD + Operator | Webhook only | **This design** |
|---|---|---|---|
| New schema for developers | Yes | No | **No** |
| Handles multi-step version lifecycle | Yes | No (stateless) | **Yes** |
| Third-party dependency | No | No | **No** |
| Old survives a failed new deploy | Depends on implementation | N/A | **Yes, by construction** |
| Extra persisted state beyond k8s + UBS | CR `.status` | None | **None** (one annotation only) |
| What engineers actually maintain | Full controller + CRD schema + RBAC for new API group | Injection logic | Reconcile loop + 2 labels convention |

---

## 8. Open items for platform-team follow-up

- Confirm whether the UBS `register` call is idempotent (safe to call twice
  with the same agent ID) — affects retry safety after a partial failure.
- Confirm whether multiple controller replicas will ever be required (HA);
  if so, enable client-go leader election (`k8s.io/client-go/tools/leaderelection`).
- Confirm blueprint-token-to-Secret provisioning process for real UBS
  credentials (out of scope for this POC, which uses a mock API with no
  auth).
