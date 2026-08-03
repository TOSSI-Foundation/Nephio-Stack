# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Minimal base WITH iptables + iproute2. The node-agent (--node-agent mode) programs the host N6 NAT via
# go-iptables, which SHELLS OUT to the `iptables` binary — distroless/static has none, so the n6-nat step
# silently no-op'd and a UE got a PDU session but no internet (the UE-pool MASQUERADE was never applied).
# debian-slim ships iptables v1.8.x in nf_tables mode, matching CK8s hosts, so nat rules land in the shared
# `ip nat` table. Netlink-based steps (bridges/routes/ARP) don't need this, but NAT has no netlink verb.
# Keep the non-root default USER for the controller Deployment; the node-agent DaemonSet overrides to root
# (runAsUser:0) since programming the host firewall needs CAP_NET_ADMIN.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends iptables iproute2 ethtool \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
