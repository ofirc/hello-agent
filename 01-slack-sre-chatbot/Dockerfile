# Build stage. Pinned to the toolchain in go.mod so Cloud Build and a laptop produce
# the same binary.
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first, so a code-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static binary, so the agent itself needs no libc. The skills under
# skills/ are embedded into it, so nothing else has to be copied into the runtime image.
# -trimpath keeps build paths out of the binary, which makes the image reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/agent ./cmd/agent

# Runtime stage. Debian slim rather than distroless, for one reason: python3 and bash.
# `drift debug` lets the model run short scripts, and Cloud Run sandboxes run them against
# this image's filesystem, which they see read-only. The sandbox can only launch
# interpreters that already exist here, at /usr/bin/python3 and /usr/bin/bash.
#
# Because the sandbox reads this filesystem, nothing secret may be written to the image
# or to disk. Tokens arrive as environment variables from Secret Manager, and the sandbox
# is started with no environment, no network and a single read-only data mount.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 bash ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/agent /agent

# Numeric, not `nonroot`: Kubernetes runAsNonRoot cannot verify a username.
USER 65532:65532

# Socket Mode dials out: no Slack traffic ever arrives inbound. The only listener is a
# health endpoint, which exists because Cloud Run instances refuse to start with
# --ingress configured and no serving port.
EXPOSE 8080
ENTRYPOINT ["/agent"]
