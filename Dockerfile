# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-bookworm AS builder

# Native gcc plus cross C compilers: the Go toolchain runs on $BUILDPLATFORM and
# cross-compiles cgo to $TARGETARCH, avoiding QEMU emulation of the whole build.
RUN apt-get update && apt-get install -y --no-install-recommends \
      gcc libc6-dev \
      gcc-aarch64-linux-gnu libc6-dev-arm64-cross \
      gcc-x86-64-linux-gnu libc6-dev-amd64-cross \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETARCH
ARG VERSION=docker
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-${TARGETARCH} \
    CC=$(case "${TARGETARCH}" in \
          arm64) echo aarch64-linux-gnu-gcc ;; \
          amd64) echo x86_64-linux-gnu-gcc ;; \
          *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
        esac) \
    CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /apoci \
    ./cmd/apoci

FROM public.ecr.aws/docker/library/debian:bookworm-slim

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=
LABEL org.opencontainers.image.title="apoci" \
      org.opencontainers.image.description="apoci — federated, self-hostable multi-format (OCI, npm, Cargo, PyPI) registry that publishes artifacts as an ActivityPub actor" \
      org.opencontainers.image.source="https://git.erwanleboucher.dev/eleboucher/apoci" \
      org.opencontainers.image.url="https://git.erwanleboucher.dev/eleboucher/apoci" \
      org.opencontainers.image.documentation="https://git.erwanleboucher.dev/eleboucher/apoci/src/branch/main/README.md" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.authors="Erwan Leboucher <erwanleboucher@gmail.com>" \
      org.opencontainers.image.vendor="Erwan Leboucher" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

ENV PATH="/apoci:${PATH}" \
    APOCI_DATA_DIR="/apoci/storage" \
    APOCI_CONFIG="/apoci/config/apoci.yaml"

USER 1000:1000

WORKDIR "/apoci/storage"
WORKDIR "/apoci/config"
WORKDIR "/apoci"

COPY --chown=1000:1000 --from=builder /apoci /apoci/apoci

VOLUME "/apoci/storage"
EXPOSE 5000

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://localhost:5000/healthz || exit 1

# Config path comes from APOCI_CONFIG, so every subcommand resolves it.
ENTRYPOINT ["apoci"]
CMD ["serve"]
