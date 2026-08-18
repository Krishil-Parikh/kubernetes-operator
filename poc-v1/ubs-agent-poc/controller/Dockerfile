FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /ubs-agent-controller .

FROM gcr.io/distroless/static-debian12
COPY --from=build /ubs-agent-controller /ubs-agent-controller
ENTRYPOINT ["/ubs-agent-controller"]
