FROM public.ecr.aws/docker/library/golang:1.26-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=1 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /apoci \
    ./cmd/apoci

FROM public.ecr.aws/docker/library/debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

USER 1000:1000

WORKDIR "/apoci/storage"
WORKDIR "/apoci/config"
WORKDIR "/apoci"

COPY --chown=1000:1000 --from=builder /apoci /apoci/apoci

VOLUME "/apoci/storage"
EXPOSE 5000

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://localhost:5000/healthz || exit 1

ENTRYPOINT ["/apoci/apoci"]
CMD ["serve", "-c", "/apoci/config/apoci.yaml"]
