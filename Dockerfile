# syntax=docker/dockerfile:1

# Build stage. CGO is off because the SQLite driver is pure Go (modernc.org/sqlite), which is why
# the runtime image can be distroless static rather than a full libc base.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILT=unknown
# Always linux/amd64: an Apple Silicon build defaults to arm64 and will not run on the cluster.
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.built=${BUILT}" \
    -o /out/gateway ./cmd/gateway

RUN CGO_ENABLED=0 go test ./...

# Runtime stage.
# Pinned, not inherited. TARGETARCH below fixes the BINARY's architecture, but the image manifest
# still takes the builder's default platform — so a plain `docker build .` on an Apple Silicon host
# produced an arm64-labelled image containing an x86-64 binary, which the cluster pulls and cannot
# execute. make image passes --platform too; this makes the bare command safe as well.
FROM --platform=linux/amd64 gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /usr/local/bin/gateway

# The PVC mounts here. nonroot is uid 65532; the volume must be writable by that uid, which
# fsGroup: 65532 in the pod spec arranges.
VOLUME ["/data"]
ENV DATA_DIR=/data \
    GATEWAY_ADDR=:8080 \
    LOG_FORMAT=json

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["serve"]
