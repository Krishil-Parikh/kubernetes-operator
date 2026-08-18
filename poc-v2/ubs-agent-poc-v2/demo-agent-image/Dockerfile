# Stand-in for a real agent workload image. Builds the custom
# agent-supervisor binary (see supervisor/main.go) and makes it the
# container's entrypoint: it registers this agent, starts nginx as a child
# process, and deregisters on shutdown -- entirely on its own, with no
# Kubernetes lifecycle hooks and no curl/shell dependency in the final
# image at all.
#
# In a real UBS deployment, every team's agent image would extend a shared
# internal base image that already bundles agent-supervisor -- see
# docs/ARCHITECTURE.md section 5.
FROM golang:1.22 AS build
WORKDIR /src
COPY supervisor/go.mod ./
RUN go mod download
COPY supervisor/main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /agent-supervisor .

FROM nginx:1.25
COPY --from=build /agent-supervisor /agent-supervisor
COPY .env /.env
# The nginx base image sets STOPSIGNAL SIGQUIT for its own entrypoint.
# agent-supervisor is PID 1 now and only handles SIGTERM/SIGINT, so this
# MUST be overridden -- otherwise docker/kubelet send SIGQUIT on shutdown,
# which Go's runtime treats as an unhandled fatal signal (stack dump +
# exit) and our deregister-before-stop logic never runs at all.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/agent-supervisor"]
