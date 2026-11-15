# Build stage. Pinned to the toolchain in go.mod so Cloud Build and a laptop produce
# the same binary.
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first, so a code-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static binary, because the runtime image has no libc.
# -trimpath keeps build paths out of the binary, which makes the image reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/agent ./cmd/agent

# Runtime stage. Distroless static: no shell, no package manager, no setuid binaries.
# The agent only makes outbound HTTPS calls, so it needs a binary and a CA bundle and
# genuinely nothing else.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/agent /agent

# Numeric, not `nonroot`: Kubernetes runAsNonRoot cannot verify a username.
USER 65532:65532

# Socket Mode dials out: no Slack traffic ever arrives inbound. The only listener is a
# health endpoint, which exists because Cloud Run instances refuse to start with
# --ingress configured and no serving port.
EXPOSE 8080
ENTRYPOINT ["/agent"]
