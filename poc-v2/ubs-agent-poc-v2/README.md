# UBS Agent Version Lifecycle — Custom-Supervisor Simulation POC (v2)

Companion code to `docs/ARCHITECTURE.md`. There is **no controller process
running in this design**, and — unlike the first cut of v2 — there are also
no Kubernetes `postStart`/`preStop` lifecycle hooks and no ConfigMap. The
entire register → serve → deregister lifecycle is owned by a small custom
Go binary, `agent-supervisor` (see `demo-agent-image/supervisor/main.go`),
that runs as the container's actual PID 1: it registers the agent, starts
the real workload (nginx, standing in for a real agent process) as its
child, and traps `SIGTERM` itself to deregister before the workload is
asked to stop.

Kubernetes is still the thing scheduling the pod and driving the rolling
update (that's unavoidable — it's what makes this a Kubernetes POC at
all), but every piece of *lifecycle intelligence* — registration order,
retries, deregistration, logging — is code we own end-to-end, not
delegated to kubelet-invoked exec hooks.

Read `docs/ARCHITECTURE.md` first for the full design reasoning, and
`docs/DIAGRAM.md` for the Mermaid sequence diagrams. `docs/ALTERNATIVES-CONSIDERED.md`
holds the earlier controller-based design for reference.

## Why this replaced the hook-based v2

The first cut of this POC used `postStart`/`preStop` exec hooks calling
shell scripts delivered via a ConfigMap. Testing it against a real `kind`
cluster surfaced two real problems:

1. **Hook output never reaches `kubectl logs`.** kubelet only surfaces
   exec-hook stdout/stderr on failure (as a truncated event message) — on
   success it's discarded entirely. The registration/deregistration
   narrative that makes this POC demoable was invisible during normal
   operation.
2. **ConfigMap-delivered config drifts.** `AGENT_IMAGE` had to be kept in
   sync with the Deployment's `image:` field by hand/CI templating — two
   independent places that could disagree.

`agent-supervisor` fixes both: it's the container's main process, so its
logs are the container's logs, no different from nginx's own; and
`BLUEPRINT_ID` / `AGENT_IMAGE` / `UBS_API_BASE` are now baked into a `.env`
file inside the image itself (see `demo-agent-image/.env`), so a version
bump can't drift out of sync with the image describing it. Only the
credential, `BLUEPRINT_TOKEN`, still comes from a real Kubernetes Secret —
baking a token into an image layer would be an actual security problem,
not a shortcut worth avoiding.

## What this proves

1. A brand-new agent registers itself (mint + register against the mock
   UBS/Graph API) **before** `agent-supervisor` ever starts the workload —
   enforced by our own program order, not by any Kubernetes hook.
2. A version bump (re-applying the same Deployment with a new image) keeps
   the old pod fully serving until the new pod's own `/healthz` goes green,
   then the old pod's `agent-supervisor` catches `SIGTERM`, deregisters,
   and only then lets nginx stop — with zero controller and zero extra
   Kubernetes objects beyond the pods Kubernetes itself churns through as
   part of a normal rolling update.
3. A **broken** new version never disrupts the old one — enforced by
   native `maxUnavailable: 0` semantics: a pod that never starts never
   passes `/healthz`, so Kubernetes never touches the old one.

## Prerequisites

- [`kind`](https://kind.sigs.k8s.io/) and `kubectl`
- Docker

## 1. Create the local cluster

```bash
kind create cluster --name ubs-poc-v2
kubectl cluster-info --context kind-ubs-poc-v2
```

## 2. Build and load the images

`agent-supervisor` is compiled inside the Docker build (multi-stage,
`golang:1.22` build stage) — you don't need Go installed locally to build
these images.

```bash
# from the repo root
docker build -t ubs-poc/mock-api:latest ./mock-api
docker build -t ubs-poc/demo-agent:v1 -f demo-agent-image/Dockerfile ./demo-agent-image
docker build -t ubs-poc/demo-agent:v2 -f demo-agent-image/Dockerfile.v2 ./demo-agent-image

kind load docker-image ubs-poc/mock-api:latest --name ubs-poc-v2
kind load docker-image ubs-poc/demo-agent:v1 --name ubs-poc-v2
kind load docker-image ubs-poc/demo-agent:v2 --name ubs-poc-v2
```

## 3. Deploy the namespace, mock API, secret, and the agent itself

```bash
kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-mock-api.yaml
kubectl apply -f manifests/05-blueprint-token-secret.yaml

kubectl -n ubs-poc-v2 rollout status deployment/mock-ubs-api

kubectl apply -f manifests/10-agent-deployment.yaml
kubectl -n ubs-poc-v2 rollout status deployment/stock-research-agent
```

Watch the agent pod's own logs — this now shows `agent-supervisor`'s
lifecycle narrative interleaved with nginx's own access/error logs,
because they're both writing to the same container stdout:

```bash
kubectl -n ubs-poc-v2 logs -f deployment/stock-research-agent
```

## 4. Demo scenario A — first deploy

The `10-agent-deployment.yaml` apply above **is** scenario A. Expected log
output from the pod itself:

```
2026-08-18T17:23:10Z agent-supervisor: starting up: blueprint=bp-8891 image=ubs-poc/demo-agent:v1 api=http://mock-ubs-api.ubs-poc-v2.svc.cluster.local:5000
2026-08-18T17:23:10Z agent-supervisor: attempt 1/10: minting agent id
2026-08-18T17:23:10Z agent-supervisor: minted agent id=100
2026-08-18T17:23:10Z agent-supervisor: registered agent id=100 against blueprint=bp-8891
2026-08-18T17:23:10Z agent-supervisor: workload started (pid=7), agent 100 is live and serving
```

Verify against the mock API directly:
```bash
kubectl -n ubs-poc-v2 port-forward svc/mock-ubs-api 5000:5000 &
curl -s http://localhost:5000/blueprint/bp-8891 | jq
# { "blueprintId": "bp-8891", "agentId": "100", "image": "ubs-poc/demo-agent:v1" }
```

Notice the pod only reaches `Ready` after registration succeeds — check
with:
```bash
kubectl -n ubs-poc-v2 get pods -w
```

## 5. Demo scenario B — a clean version transition, zero controller involved

```bash
kubectl patch deployment stock-research-agent -n ubs-poc-v2 \
  --patch-file manifests/11-agent-v2-patch.yaml
```

Watch both the new pod come up and the old pod terminate:
```bash
kubectl -n ubs-poc-v2 get pods -w
```

**Expected sequence, purely from pod-level logs (`kubectl -n ubs-poc-v2 logs <new-pod>` then `<old-pod>`):**

New pod:
```
... agent-supervisor: attempt 1/10: minting agent id
... agent-supervisor: minted agent id=101
... agent-supervisor: registered agent id=101 against blueprint=bp-8891
... agent-supervisor: workload started (pid=7), agent 101 is live and serving
```

Old pod (once the new one is Ready and Kubernetes sends it SIGTERM):
```
... agent-supervisor: received signal terminated -- failing readiness and deregistering agent 100 before shutdown
... agent-supervisor: agent id=100 deregistered successfully
... agent-supervisor: workload exited cleanly, shutdown complete
```

Verify:
```bash
kubectl -n ubs-poc-v2 get pods
# only the v2 pod should remain

curl -s http://localhost:5000/blueprint/bp-8891 | jq
# { "blueprintId": "bp-8891", "agentId": "101", "image": "ubs-poc/demo-agent:v2" }

curl -s http://localhost:5000/_debug/state | jq
# retiredAgents should include "100"
```

Notice there is **no controller pod anywhere in this cluster** — check:
```bash
kubectl get deployments -A
# only mock-ubs-api and stock-research-agent exist
```

## 6. Demo scenario C — a broken push never disrupts the running agent

```bash
kubectl patch deployment stock-research-agent -n ubs-poc-v2 \
  --patch-file manifests/12-agent-v3-broken-patch.yaml
```

```bash
kubectl -n ubs-poc-v2 get pods
# NAME                                    READY   STATUS             RESTARTS
# stock-research-agent-xxxxxxxxxx-yyyyy   1/1     Running            0    <- old, still serving
# stock-research-agent-zzzzzzzzzz-wwwww   0/1     ImagePullBackOff   0    <- broken new pod
```

The old pod keeps serving indefinitely — this isn't something our code
enforces, it's `maxUnavailable: 0` on the Deployment doing exactly what
it's built to do: a pod that never starts never passes `/healthz`, so
Kubernetes never sends the old pod's `agent-supervisor` a `SIGTERM`.
Confirm via the mock API that the registered agent hasn't changed:
```bash
curl -s http://localhost:5000/blueprint/bp-8891 | jq
# still agentId 101 -- completely untouched
```

To "roll back," just patch the image back:
```bash
kubectl patch deployment stock-research-agent -n ubs-poc-v2 \
  --patch-file manifests/11-agent-v2-patch.yaml
```

## 7. Tear down

```bash
kind delete cluster --name ubs-poc-v2
```

## File map

```
mock-api/               Flask stand-in for UBS Blueprint API + Graph API
demo-agent-image/
  supervisor/main.go    The custom agent-supervisor binary -- owns the whole
                        register/serve/deregister lifecycle, PID 1 in the container
  .env, .env.v2         Baked-in, per-version agent identity (blueprint id, image
                        tag, API base) -- no ConfigMap, no inline Deployment env:
  Dockerfile,
  Dockerfile.v2         Multi-stage builds: compile agent-supervisor, then
                        layer it + the matching .env onto nginx v1/v2
manifests/              Namespace, mock API, secret, agent Deployment, and the
                        two demo-scenario patches (clean v2, broken v3)
docs/
  ARCHITECTURE.md              Full design writeup (current, recommended design)
  DIAGRAM.md                   Mermaid sequence + comparison diagrams
  ALTERNATIVES-CONSIDERED.md   Earlier controller-based design, superseded, kept for record
```
