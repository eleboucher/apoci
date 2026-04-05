FROM golang:1.26-bookworm AS builder

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

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    useradd -r -s /usr/sbin/nologin -d /var/lib/apoci apoci && \
    mkdir -p /var/lib/apoci /etc/apoci && \
    chown apoci:apoci /var/lib/apoci

COPY --from=builder /apoci /usr/local/bin/apoci

USER apoci
EXPOSE 5000

VOLUME /var/lib/apoci
VOLUME /etc/apoci

ENTRYPOINT ["apoci"]
CMD ["serve", "-c", "/etc/apoci/apoci.yaml"]
