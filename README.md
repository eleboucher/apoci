# apoci

Federated OCI registry over ActivityPub. Each node is a single-user registry and an AP actor (`@registry@foo.com`). Push an artifact and it federates to your followers. If your node goes down, peers still serve your images.

```
  foo.com               bar.com              baz.com
  ┌────────────┐       ┌────────────┐       ┌────────────┐
  │ OCI :5000  │       │ OCI :5000  │       │ OCI :5000  │
  │ SQLite+FS  │◄─────►│ SQLite+FS  │◄─────►│ SQLite+FS  │
  │ AP actor   │       │ AP actor   │       │ AP actor   │
  └────────────┘       └────────────┘       └────────────┘
```

Docker, Helm, Flux, ORAS -- any OCI client works. Discoverable via WebFinger and NodeInfo, so Mastodon can follow your registry and see pushes in its feed.

## Quick start

```bash
make build
cp configs/apoci.example.yaml apoci.yaml
```

```yaml
# apoci.yaml -- only endpoint is required
endpoint: "https://foo.com"
```

The domain becomes your identity (`@registry@foo.com`) and your repo namespace (`foo.com/*`).

```bash
apoci serve
```

Looks for `apoci.yaml` in the current directory by default. Override with `-c` or `APOCI_CONFIG`.

On first run, a registry token is auto-generated at `{dataDir}/registry.token`. Use it to push:

```bash
cat ~/apoci/data/registry.token
```

Push something:

```bash
TOKEN=$(cat ~/apoci/data/registry.token)
docker login foo.com -u registry -p "$TOKEN"
docker push foo.com/foo.com/myapp:v1
```

## Addressing

Repos are namespaced by domain, same idea as `@user@domain` in the fediverse. This prevents collisions when federating -- two operators on different domains can never clash.

```
docker pull <node>/<origin-domain>/<image>:<tag>
```

```bash
docker pull foo.com/foo.com/myapp:v1       # your own image, from your node
docker pull bar.com/bar.com/myapp:v1       # bar's image, from bar
docker pull foo.com/bar.com/myapp:v1       # bar's image, from your node (federated)
```

The third case is the point: foo follows bar, so foo has bar's metadata. On pull, foo fetches the blob from bar, verifies the SHA-256, caches it, and serves it. Next pull is local.

## Federation

### Follow a peer

```bash
apoci follow add bar.com
# or: apoci follow add @registry@bar.com
# or: apoci follow add https://bar.com/ap/actor
```

Bar must accept before anything flows:

```bash
# on bar
apoci follow pending
apoci follow accept foo.com
```

By default, every follow requires operator approval. Set `federation.autoAccept: mutual` to auto-accept peers you already follow, `all` for a public profile, or use `federation.allowedDomains` to trust specific domains.

### What gets federated

When you push a manifest, three AP activities go out to followers:

| Push event | Activity | Effect on peer |
|-----------|----------|----------------|
| Manifest | `Create` `OCIManifest` | Peer stores manifest metadata + content |
| Tag | `Update` `OCITag` | Peer maps tag to digest |
| Blob | `Announce` `OCIBlob` | Peer records blob location for pull-through |

Peers eagerly replicate blobs in the background. If the origin goes down, peers already have the data.

### Manage follows

```bash
apoci follow list
apoci follow remove bar.com
apoci identity show
```

## Security

1. **Follow gate** -- only approved peers can send activities
2. **HTTP Signatures** -- RSA-SHA256 on every request, replay-protected (5min window + seen-signature cache)
3. **Origin ownership** -- a followed peer can only write to repos it created
4. **Content addressing** -- SHA-256 verified on every blob fetch
5. **SSRF protection** -- private IPs blocked after DNS resolution (prevents rebinding)

## ActivityPub

Each node is an `Application` actor (`@registry@<domain>`). Discoverable via:

```
GET /.well-known/webfinger?resource=acct:registry@foo.com
GET /.well-known/nodeinfo
GET /ap/actor    (Accept: application/activity+json)
```

Search `@registry@foo.com` in Mastodon to follow a node from the fediverse.

`name` in the config is a display label only. The repo namespace is always the domain.

## Deploy

### Docker

```bash
make docker
docker run -d -p 5000:5000 \
  --user 1000:1000 \
  -v ~/apoci/data:/apoci/storage \
  -v $(pwd)/apoci.yaml:/apoci/config/apoci.yaml:ro \
  apoci:latest
```

### Docker Compose (SQLite)

```bash
docker compose up --build -d
```

### Docker Compose (PostgreSQL)

```bash
docker compose -f docker-compose.postgres.yml up --build -d
```

### Systemd

```ini
[Unit]
Description=apoci
After=network.target

[Service]
ExecStart=/usr/local/bin/apoci serve -c /etc/apoci/apoci.yaml
User=apoci
Restart=on-failure
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

### Reverse proxy

nginx:

```nginx
server {
    listen 443 ssl;
    server_name foo.com;
    ssl_certificate /etc/letsencrypt/live/foo.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/foo.com/privkey.pem;
    client_max_body_size 0;  # Let apoci enforce limits via limits.maxBlobSize

    location / {
        proxy_pass http://127.0.0.1:5000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 900;
    }
}
```

Caddy:

```
foo.com {
    reverse_proxy 127.0.0.1:5000
}
```

## Split-domain

`accountDomain` lets you be `@registry@example.com` while running on `registry.example.com`:

```yaml
endpoint: "https://registry.example.com"
accountDomain: "example.com"
```

WebFinger accepts both `acct:registry@example.com` and `acct:registry@registry.example.com`. AP URLs and OCI namespace stay on the service domain.

You need to proxy `/.well-known/webfinger` from the vanity domain to the service:

```
example.com {
    handle /.well-known/webfinger {
        reverse_proxy registry.example.com:443
    }
    respond 404
}
```

Path-prefix proxying (`example.com/registry/...`) is not supported.

## Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `endpoint` | *(required)* | Public URL; determines domain and namespace |
| `name` | domain | Display name (AP actor name) |
| `listen` | `:5000` | Bind address |
| `dataDir` | `/var/lib/apoci` | Database and blob storage |
| `database.driver` | `sqlite` | `sqlite` or `postgres` |
| `database.dsn` | | Postgres connection string (required when driver is `postgres`) |
| `database.maxOpenConns` | `4`/`25` | Max open DB connections (default: 4 for sqlite, 25 for postgres) |
| `database.maxIdleConns` | `4`/`10` | Max idle DB connections (default: 4 for sqlite, 10 for postgres) |
| `keyPath` | `{dataDir}/ap.key` | RSA key, generated on first run |
| `registryToken` | *(auto-generated)* | Bearer token for push; saved to `{dataDir}/registry.token`. Reads are unauthenticated. |
| `accountDomain` | endpoint domain | Vanity domain for `@registry@<domain>` handle |
| `immutableTags` | `^v[0-9]` | Regex, matching tags can't be overwritten |
| `logLevel` | `info` | `debug` / `info` / `warn` / `error` |
| `logFormat` | `json` | `json` / `text` |
| `tls.cert` | | TLS cert path |
| `tls.key` | | TLS key path |
| `peering.healthCheckInterval` | `30s` | Peer health poll interval |
| `peering.fetchTimeout` | `60s` | Blob fetch timeout |
| `limits.maxManifestSize` | `10485760` | Max manifest size in bytes (10 MB) |
| `limits.maxBlobSize` | `536870912` | Max blob size in bytes (512 MB) |
| `federation.autoAccept` | `none` | `none`, `mutual` (peers you follow), or `all` (public) |
| `federation.allowedDomains` | `[]` | Always auto-accept follows from these domains |
| `federation.blockedDomains` | `[]` | Silently drop all activities from these domains |
| `federation.blockedActors` | `[]` | Silently drop all activities from these actor URLs |
| `metrics.enabled` | `false` | Expose `/debug/vars` |
| `metrics.listen` | `:9090` | Metrics bind address |
| `metrics.token` | | Bearer token for metrics |

Config lookup: `./apoci.yaml` by default, override with `-c <path>` or `APOCI_CONFIG` env var.

Requires Go 1.26+ and a C compiler (CGO for SQLite).

## API

**OCI v2:** `GET /v2/`, `GET|PUT /v2/<name>/manifests/<ref>`, `GET /v2/<name>/blobs/<digest>`, `POST /v2/<name>/blobs/uploads/`

**ActivityPub:** `GET /ap/actor`, `POST /ap/inbox`, `GET /ap/outbox`, `GET /ap/followers`, `GET /ap/following`

**Discovery:** `GET /.well-known/webfinger`, `GET /.well-known/nodeinfo`

**Health:** `GET /healthz`, `GET /readyz`

## License

MIT
