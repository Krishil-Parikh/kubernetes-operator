# Deliberately broken: this image doesn't exist in any registry.
# Used to demonstrate that a broken push never disrupts the running agent
# because Kubernetes' own maxUnavailable:0 RollingUpdate refuses to scale
# down the old pod until a new one passes readiness -- which never happens
# here since the pod can't even be scheduled/pulled.
#
# Nothing to build here -- this is referenced directly as an image tag in
# manifests/12-agent-v3-broken-patch.yaml, no Dockerfile needed.
