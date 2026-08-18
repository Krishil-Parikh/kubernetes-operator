# UBS Agent Version Lifecycle — Simulation POC

Companion code to `docs/ARCHITECTURE.md`. This is a **simulation**: the real
UBS Blueprint API and Graph API are stood in for by a small Flask mock
server (`mock-api/`), and the "agent workload" itself is a plain `nginx`
container standing in for a real agent image. Everything else — the
controller, the labels convention, the Deployment lifecycle, the RBAC — is
built exactly as it would be against the real UK8s platform, with no CRDs
and no third-party controllers, per the constraints in the design doc.

## What this proves

1. A brand-new agent gets an agent ID minted and registered automatically
   on first deploy.
2. A new version applied alongside a healthy old version gets minted,
   health-gated, and registered — and **only after that succeeds** is the
   old version deregistered and deleted.
3. A **broken** new version never touches the old one — it just sits
   unready forever while the old version keeps serving, with zero manual
   intervention required to stay safe.

## Prerequisites

- [`kind`](https://kind.sigs.k8s.io/) and `kubectl`
- Docker
- Go 1.22+ (only needed if you want to rebuild the controller binary)

## 1. Create the local cluster

```bash
kind create cluster --name ubs-poc
kubectl cluster-info --context kind-ubs-poc
```

## 2. Build the two images and load them into kind

`kind` clusters can't pull from your local Docker daemon by default, so we
load the images directly.

```bash
# from the repo root
docker build -t ubs-poc/mock-api:latest ./mock-api
docker build -t ubs-poc/controller:latest ./controller

kind load docker-image ubs-poc/mock-api:latest --name ubs-poc
kind load docker-image ubs-poc/controller:latest --name ubs-poc
```

## 3. Deploy the namespace, RBAC, mock API, and controller

```bash
kubectl apply -f manifests/00-namespace-and-rbac.yaml
kubectl apply -f manifests/01-mock-api.yaml
kubectl apply -f manifests/02-controller.yaml

# wait for both to be ready
kubectl -n ubs-poc rollout status deployment/mock-ubs-api
kubectl -n ubs-poc rollout status deployment/ubs-agent-controller
```

Watch the controller's own logs in a separate terminal for the rest of the
demo:

```bash
kubectl -n ubs-poc logs -f deployment/ubs-agent-controller
```

## 4. Demo scenario A — first deploy

```bash
kubectl apply -f manifests/10-agent-v1.yaml
```

**Expected controller log output:**
```
[ubs-poc/bp-8891/stock-research-agent] first deploy detected (stock-research-agent-v1), minting agent id
[ubs-poc/bp-8891/stock-research-agent] agent 100 registered and live as v1 (stock-research-agent-v1)
```

Verify against the mock API directly:
```bash
kubectl -n ubs-poc port-forward svc/mock-ubs-api 5000:5000 &
curl -s http://localhost:5000/blueprint/bp-8891 | jq
# { "blueprintId": "bp-8891", "agentId": "100", "image": "nginx:1.25" }
```

## 5. Demo scenario B — a clean version transition

```bash
kubectl apply -f manifests/11-agent-v2.yaml
```

**Expected controller log output (in order):**
```
[ubs-poc/bp-8891/stock-research-agent] incoming deployment ... not yet Ready -- old deployment stock-research-agent-v1 left fully untouched
[ubs-poc/bp-8891/stock-research-agent] incoming deployment stock-research-agent-v2 is Ready, minting new agent id (old stock-research-agent-v1 still fully live)
[ubs-poc/bp-8891/stock-research-agent] new agent 101 registered and now the active agent for blueprint bp-8891
[ubs-poc/bp-8891/stock-research-agent] old agent 100 deregistered
[ubs-poc/bp-8891/stock-research-agent] old deployment stock-research-agent-v1 deleted -- exactly one version (stock-research-agent-v2) now running for this agent
```

Verify:
```bash
kubectl -n ubs-poc get deployments
# only stock-research-agent-v2 should remain

curl -s http://localhost:5000/blueprint/bp-8891 | jq
# { "blueprintId": "bp-8891", "agentId": "101", "image": "nginx:1.27" }

curl -s http://localhost:5000/_debug/state | jq
# retiredAgents should include "100"
```

## 6. Demo scenario C — a broken push never touches the working version

Reset to steady state first if you ran scenario B (you should already have
just `stock-research-agent-v2` running). Then:

```bash
kubectl apply -f manifests/12-agent-v3-broken.yaml
```

**Expected controller log output — repeating indefinitely, nothing else happening:**
```
[ubs-poc/bp-8891/stock-research-agent] incoming deployment stock-research-agent-v3-broken not yet Ready -- old deployment stock-research-agent-v2 left fully untouched
```

Confirm the old version is still the one actually registered and serving,
completely unaffected:
```bash
kubectl -n ubs-poc get pods -l ubs.io/agent-name=stock-research-agent
# stock-research-agent-v2-xxxx   Running
# stock-research-agent-v3-broken-xxxx   ImagePullBackOff

curl -s http://localhost:5000/blueprint/bp-8891 | jq
# still agentId 101 -- untouched
```

To "roll back," just delete the broken push — there's nothing else to undo:
```bash
kubectl delete -f manifests/12-agent-v3-broken.yaml
```

## 7. Tear down

```bash
kind delete cluster --name ubs-poc
```

## File map

```
mock-api/     Flask stand-in for UBS Blueprint API + Graph API
controller/   Go controller (client-go, no CRD, no controller-runtime)
manifests/    Namespace, RBAC, mock API, controller, and 3 demo scenarios
docs/         ARCHITECTURE.md -- full design writeup
```
