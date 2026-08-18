# Agent Version Lifecycle on UK8s — Design Proposal (v2)

**Author:** Krisha · AI Intern
**Status:** Proposal + working simulation POC (no CRDs, no controllers, no third-party dependencies)
**Supersedes:** v1 draft (custom controller watching labeled Deployments) — kept in
`docs/ALTERNATIVES-CONSIDERED.md` as a documented, deliberately-rejected alternative.

---

## 1. Problem statement and constraints

On UK8s, an intern's namespace can contain multiple **blueprints**. Each
blueprint runs exactly one logical **agent**, but that agent evolves over
time, and each evolution is a new **agent version** with its own **agent
ID** minted via the Graph API. At any moment, exactly one version of an
agent may be live under a given blueprint. When a new version deploys
successfully, the previous version's resources are removed and its agent
ID retired — but only after the new version is confirmed healthy, and a
failed new version must never take down a working old one.

**Confirmed hard constraints:**

| Constraint | Detail |
|---|---|
| No CRDs | Developers only ever author plain, stock Kubernetes objects. |
| No third-party controllers | Everything is either native Kubernetes or built in-house. |
| No custom controller either | This is the change from the v1 draft — see §3. |
| Blueprint state is external | UBS's own internal system is the source of truth for "current agent"; we never duplicate it in-cluster. |
| No API locking | UBS Blueprint API is plain REST, no transactions, no optimistic concurrency. |
| Register/deregister are separate calls | No combined atomic "swap" endpoint exists. |
| Trigger is CI/CD | New versions arrive via CI/CD applying a manifest. |
| **Always available** | The mechanism itself must never become a single point of failure. |
| **Consistent** | Exactly one agent ID registered per blueprint at (near enough) all times. |
| **Minimal deployment latency** | Time from "CI/CD pushes" to "new version live and old retired" should be as short as physically possible. |

The last three rows are new asks that materially changed the design, and
are the reason this version supersedes the original controller-based
sketch — see §3 for exactly why.

---

## 2. The core design decision: no watcher, no reconcile loop

Every design considered before this one — a CRD-based Operator, and then a
label-driven custom controller watching Deployments — shares one structural
property: **a separate process observes cluster state from the outside and
reacts to it after the fact.** That pattern is inherently a source of
latency (informer resync intervals, workqueue processing, reconcile
scheduling) and a second availability surface to reason about (what happens
if the controller itself is down when a transition needs to happen?).

The lowest-latency, zero-extra-hop mechanism for "run some logic exactly
when a pod starts, and exactly when a pod stops, before it's allowed to
serve traffic / before it's allowed to fully terminate" is not something
you need to build — Kubernetes ships it natively: **container lifecycle
hooks**, combined with a **readiness probe** and the **native
RollingUpdate strategy**.

### 2.1 postStart — registration happens inside the new pod's own startup

`postStart` is a kubelet-invoked hook that runs immediately after a
container starts. The pod is not marked `Ready` — and therefore receives
**zero traffic** — until both the hook completes and the readiness probe
passes. We make the readiness probe check a file the hook writes on
success, so there is no window, ever, where an unregistered agent can
receive a request. This is enforced by the kubelet itself, not by
application-level coordination.

### 2.2 preStop — deregistration happens inside the old pod's own shutdown

`preStop` runs the instant a pod is selected for termination and **blocks
actual container shutdown** until it completes (bounded by
`terminationGracePeriodSeconds`). Crucially, by the time `preStop` even
fires, the new pod has *already* passed its own readiness check — that
ordering is enforced natively by `maxUnavailable: 0` in the RollingUpdate
strategy, not by anything we wrote. `preStop`'s only remaining job is
cleanup: retire the old agent ID so it doesn't linger as a duplicate
registration.

### 2.3 What a developer/CI-CD actually touches

Still just a plain `Deployment`, with the same two labels as before, plus
a `lifecycle` block and a `readinessProbe` — all standard Kubernetes
fields, nothing bespoke:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: stock-research-agent
  labels:
    ubs.io/blueprint-id: bp-8891
    ubs.io/agent-name: stock-research-agent
spec:
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0     # old never drops below capacity while new comes up
  template:
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: agent
          image: registry.ubs.internal/agents/stock-research:2.4.1
          envFrom:
            - secretRef: { name: bp-8891-token }
          lifecycle:
            postStart:
              exec: { command: ["/bin/register-agent.sh"] }
            preStop:
              exec: { command: ["/bin/deregister-agent.sh"] }
          readinessProbe:
            exec: { command: ["cat", "/tmp/registered"] }
```

To ship a new version, CI/CD changes `spec.template.spec.containers[0].image`
on this **same** Deployment object and re-applies it. That's the entire
trigger — Kubernetes' own RollingUpdate machinery does the rest natively.

---

## 3. Why this beats the controller-watch design on every stated axis

| Requirement | Controller-watch (v1 draft) | Lifecycle-hook (this version) |
|---|---|---|
| **Deployment latency** | Bound by informer resync period + workqueue processing time — inherently reactive | Registration happens synchronously inside pod startup, before readiness — no external hop, no polling delay |
| **Consistency** | Old deleted by a separate process after observing new's success — a window exists where correctness depends on that process running correctly | Kubernetes' own readiness gate refuses to route traffic before registration completes, enforced by kubelet — not by an external observer |
| **Availability** | The controller pod itself is now a dependency — if it's down, transitions cannot proceed at all | No separate control-plane component exists to be unavailable. The mechanism lives inside the workload being deployed, not beside it |
| **What survives a crash mid-transition** | Controller crash mid-reconcile can leave a phase "stuck" until the controller restarts and resumes | Kubelet retries hook invocation per its own pod lifecycle guarantees; a script that fails just fails its own retry loop, contained to one pod |
| **Extra objects to run/monitor** | A controller Deployment + RBAC (ClusterRole/Binding) to keep healthy | None — zero extra long-running components |

**The honest tradeoff, stated plainly:** `postStart`/`preStop` hooks have
no built-in retry or backoff of their own beyond a single invocation — the
kubelet calls each exactly once per pod lifecycle event. There's no
external controller left to pick up a failed call later. This design
therefore pushes retry responsibility **into the hook scripts themselves**
(bounded retry loops with sleep/backoff, see §4), rather than relying on a
controller's requeue mechanism. This is the correct tradeoff for latency
and availability, but it's a deliberate design choice worth stating
explicitly rather than an oversight — reviewers should know the retry
logic now lives in a shell script per pod instead of a centralized queue.

---

## 4. Failure handling, by phase

### 4.1 New agent fails to register (postStart never succeeds)

`register-agent.sh` retries internally (bounded attempts, fixed backoff)
against the UBS Blueprint + Graph API. If it exhausts retries, it exits
non-zero. Kubelet's response to a failing `postStart` hook is to restart
the container per the pod's restart policy — so a persistently broken new
version simply **never becomes Ready**, and therefore (via
`maxUnavailable: 0`) Kubernetes **never touches the old pod**. No explicit
rollback step is needed: the old version keeps serving indefinitely until
someone fixes or reverts the image. This is the strongest form of the
"old survives a broken new deploy" guarantee across every design we've
considered — it isn't implemented by our code at all, it falls directly
out of native `RollingUpdate` semantics once registration is folded into
the readiness gate.

### 4.2 Old agent fails to deregister (preStop call fails)

`deregister-agent.sh` retries internally, then — deliberately — **exits 0
anyway** rather than blocking pod termination indefinitely. Blocking
shutdown on a failed cleanup call would itself become an availability
problem (a pod stuck `Terminating`, holding resources, potentially
delaying future rollouts). The result of a failure here is a harmless
orphaned registration: the old agent ID still shows as registered in UBS
even though nothing is running for it. This is a monitoring/alerting
concern, not a correctness or availability one, since the new agent is
already live and serving by the time `preStop` even runs.

### 4.3 What "rollback" means here

For a broken new version: nothing to do except fix or revert the image and
re-apply — old was never touched, so there's nothing to restore. For a
failed deregister: nothing to roll back either, since the failure occurs
strictly after the new version is already the correct, serving agent — the
only follow-up is a cleanup alert for the orphaned old agent ID in UBS.

---

## 5. Production packaging note

This POC delivers the two hook scripts via a mounted `ConfigMap` for
demo convenience. In a real UBS rollout, the recommendation is to bake
`register-agent.sh` / `deregister-agent.sh` (or a small compiled binary
doing the same two HTTP calls, avoiding a shell+curl runtime dependency)
directly into a **shared internal base image** that every team's agent
Dockerfile extends (`FROM registry.ubs.internal/base/agent:latest`). That
removes the ConfigMap + volume mount entirely, guarantees every agent image
has the hooks available regardless of what a given team's Dockerfile
otherwise contains, and centralizes any future change to the
registration/deregistration protocol to one base image rebuild rather than
N ConfigMaps.

---

## 6. Where the admission webhook still fits

Unchanged from the earlier proposal: kept narrowly for guaranteeing
`BLUEPRINT_TOKEN` and related env vars land on every agent pod even if
someone bypasses CI/CD and applies a raw manifest by hand. It carries no
version-lifecycle logic — that would reintroduce exactly the
stateless/single-shot mismatch that ruled webhooks out as a primary
mechanism in the first place.

---

## 7. Summary comparison across all three designs considered

| | CRD + Operator | Label-driven controller | **Lifecycle hooks (this proposal)** |
|---|---|---|---|
| New schema for developers | Yes | No | No |
| Extra long-running component to keep available | Yes (operator) | Yes (controller) | **None** |
| Deployment latency | Reconcile-bound | Reconcile-bound | **Kubelet-native, synchronous** |
| Old survives a broken new version | Depends on implementation | By explicit code path | **By native RollingUpdate semantics, not our code** |
| Where retry logic lives | Controller reconcile loop | Controller reconcile loop | **Per-pod hook script** |
| Third-party dependency | No | No | No |

---

## 8. Open items for platform-team follow-up

- Confirm the UBS `register` call's idempotency (safe to call twice with
  the same agent ID) — affects how aggressively the hook script can retry.
- Confirm acceptable bound for `terminationGracePeriodSeconds` platform-wide,
  since it caps how long `preStop`'s deregister retries can run before
  Kubernetes force-kills the pod anyway.
- Decide on shared base-image ownership (§5) — which team maintains
  `registry.ubs.internal/base/agent`, and what the upgrade path looks like
  for existing agent images once it exists.
- Confirm whether curl+shell hooks are acceptable long-term or whether a
  small compiled sidecar/init binary is preferred for the register/deregister
  calls (removes a runtime dependency on `curl` inside every agent image).
