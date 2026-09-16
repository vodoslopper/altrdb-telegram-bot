# altrdb-telegram-bot — multi-stage build.
#
# The binary is pure Go with a pure-Go SQLite driver, so CGO stays off and the
# runtime image needs nothing but CA certificates and a timezone database.
# Pin the base images by digest in your own registry mirror if you need
# reproducible builds; the tags below are the tested combination.

# --- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

# ca-certificates is fetched into the build stage so the runtime stage can copy
# it instead of running a package manager as root at deploy time.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# Dependencies first: this layer is reused until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w \
        -X altrdb-telegram-bot/internal/config.Version=${VERSION} \
        -X altrdb-telegram-bot/internal/config.Commit=${COMMIT}" \
      -o /out/altrdb-telegram-bot ./cmd/altrdb-bot

# Run the test suite inside the build so a broken image cannot be produced.
# Remove this block if your CI already gates on `make test-race`.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go test ./...

# --- runtime ----------------------------------------------------------------
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 65532 -S altrdb \
    && adduser  -u 65532 -S -G altrdb -H -h /var/lib/altrdb-telegram-bot -s /sbin/nologin altrdb \
    && mkdir -p /var/lib/altrdb-telegram-bot \
    && chown -R 65532:65532 /var/lib/altrdb-telegram-bot

COPY --from=build /out/altrdb-telegram-bot /usr/local/bin/altrdb-telegram-bot

# The database lives on a named volume; SQLite also writes -wal and -shm files
# next to it, so the whole directory must be writable.
VOLUME ["/var/lib/altrdb-telegram-bot"]

# Inside a container the health endpoint must listen on all interfaces or the
# orchestrator cannot reach it; the published port is bound to 127.0.0.1 on the
# host by compose.yaml, so it is still not exposed publicly. Override this back
# to 127.0.0.1:8080 if you use host networking.
ENV DATABASE_PATH=/var/lib/altrdb-telegram-bot/bot.db \
    HTTP_LISTEN_ADDR=0.0.0.0:8080 \
    LOG_FORMAT=json

USER 65532:65532
WORKDIR /var/lib/altrdb-telegram-bot

EXPOSE 8080

# /livez answers without touching the database or Telegram, so a failing probe
# means the process itself is wedged. Use /healthz for readiness.
#
# NOTE: podman defaults to the OCI image format, which drops HEALTHCHECK. Build
# with `--format docker` (make docker does this for you) or rely on the
# healthcheck declared in compose.yaml.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/altrdb-telegram-bot", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/altrdb-telegram-bot"]
