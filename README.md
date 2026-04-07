# apoci

> **Status: Beta** -- Running in production is possible, but expect rough edges. APIs may change between minor versions.

You self-host Forgejo, your container registry, and everything else on one box. If that box dies, you need the container images to rebuild it -- but they lived on the box that just died. apoci solves this: federate your registry over ActivityPub so a handful of friends mirror your artifacts. When your server goes down, your peers still serve your images and you can bootstrap from any of them.

Each node is a single-user registry and an AP actor (`@registry@foo.com`). Push an artifact and it federates to your followers.

```
  foo.com               bar.com              baz.com
  ┌────────────┐       ┌────────────┐       ┌────────────┐
  │ OCI :5000  │       │ OCI :5000  │       │ OCI :5000  │
  │ SQLite+FS  │◄─────►│ SQLite+FS  │◄─────►│ SQLite+FS  │
  │ AP actor   │       │ AP actor   │       │ AP actor   │
  └────────────┘       └────────────┘       └────────────┘
```

**How it compares:** Like Harbor or Zot but federated; like Mastodon but for container images.

### Non-goals

apoci is not a multi-tenant registry, not a Harbor replacement, not a CDN. It doesn't try to federate with generic ActivityPub servers beyond discovery (you can find a node from Mastodon, but meaningful interaction is registry-to-registry).

## Quick start

### Install

```bash
go install git.erwanleboucher.dev/eleboucher/apoci/cmd/apoci@latest
```

Or build from source:

```bash
make build        # binary at ./bin/apoci
```

### Configure and run

```bash
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

> **Note:** Docker refuses plaintext registries by default. If you're not behind a TLS-terminating reverse proxy yet, add `"insecure-registries": ["foo.com:5000"]` to your Docker daemon config (`/etc/docker/daemon.json`), or set up TLS / a reverse proxy first (see [Deploy](#deploy)).

## Addressing

Repos are namespaced by domain, same idea as `@user@domain` in the fediverse. This prevents collisions when federating -- two operators on different domains can never clash.

```
docker pull <node>/<origin-domain>/<image>:<tag>
```

**Assume you operate `foo.com` and follow `bar.com`:**

```bash
docker pull foo.com/foo.com/myapp:v1       # your own image, from your node
docker pull bar.com/bar.com/myapp:v1       # bar's image, directly from bar
docker pull foo.com/bar.com/myapp:v1       # bar's image, from your node (federated copy)
```

The third case is the point: foo follows bar, so foo has bar's metadata. On pull, foo fetches the blob from bar, verifies the SHA-256, caches it, and serves it. Next pull is local.

Namespace is enforced on **pushes** only -- all repos must be prefixed with your domain. Pulls are unrestricted (any repo that exists locally or can be fetched from a peer is served).

```bash
docker push foo.com/myapp:v1               # DENIED: repository must start with "foo.com/"
docker push foo.com/foo.com/myapp:v1       # OK
```

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

When you push a manifest, three AP activities fire immediately on push:

| Push event | Activity | Effect on peer |
|-----------|----------|----------------|
| Manifest pushed | `Create` `OCIManifest` | Peer stores manifest metadata + content |
| Tag created/updated | `Update` `OCITag` | Peer maps tag to digest |
| Blob uploaded | `Announce` `OCIBlob` | Peer records blob location for pull-through |

Peers eagerly replicate blobs in the background. If the origin goes down, peers already have the data.

### Delivery

All outbound activities (including follow Accept/Reject) go through a persistent delivery queue with automatic retry and exponential backoff. Failed deliveries are retried up to 10 times before being marked as permanently failed. Completed deliveries are cleaned up after 7 days.

### Manage follows

```bash
apoci follow list
apoci follow remove bar.com
apoci identity show
```

### Remote CLI

All `follow` and `identity` subcommands can target a remote instance using `--remote` and `--token`:

```bash
ADMIN_TOKEN=$(cat ~/apoci/data/admin.token)
apoci follow list --remote https://registry.example.com --token "$ADMIN_TOKEN"
apoci follow add bar.com --remote https://registry.example.com --token "$ADMIN_TOKEN"
```

This hits the admin API (`/api/admin/...`) on the remote node, authenticated with the admin token (separate from the registry push token). Useful for managing headless or containerized instances.

## Security

1. **Follow gate** -- only approved peers can send activities
2. **Replay-resistant HTTP Signatures** -- RSA-SHA256 on every request; replays are rejected (5-minute validity window with a seen-signature cache)
3. **Namespace enforcement** -- writes are always scoped to the node's domain; the namespace is derived from `endpoint` automatically, so a node at `foo.com` can only push to `foo.com/*`
4. **Origin ownership** -- a followed peer can only write to repos it created
5. **Content addressing** -- SHA-256 verified on every blob fetch
6. **SSRF protection** -- private IPs blocked after DNS resolution (prevents rebinding)
7. **Rate limiting** -- mutating OCI requests (push blob, push manifest, start upload) are rate-limited per IP (5 req/s, burst 20). Not currently configurable; changing limits requires a code change.

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

## Monitoring

Enable metrics in config:

```yaml
metrics:
  enabled: true
  listen: ":9090"
  token: "your-metrics-bearer-token"
```

Metrics are served as JSON at `/debug/vars` on the metrics port, protected by bearer token authentication.

Key metrics to monitor:

| Metric | Type | Description |
|--------|------|-------------|
| `delivery_pending` | gauge | Undelivered activities in the outbound queue |
| `federation_followers` | gauge | Number of accepted followers |
| `federation_following` | gauge | Number of peers you follow |
| `inbox_rate_limited` | counter | Inbound activities rejected by rate limiter |
| `blob_replications_failed` | counter | Failed blob replication attempts |
| `gc_cycles_completed` | counter | Garbage collection runs |
| `delivery_failed` | counter | Permanently failed outbound deliveries |
| `registry_manifest_pushes` | counter | Total manifest pushes |
| `registry_blob_pull_throughs` | counter | Blobs fetched from peers on demand |

## Backup and restore

Back up the `dataDir` directory (default `/apoci/storage`). It contains:

- SQLite database (or use `pg_dump` for Postgres)
- Blob storage
- RSA keypair (`ap.key`)
- Registry token (`registry.token`)
- Admin token (`admin.token`)

Restore by stopping the node, replacing the directory, and restarting.

## Upgrades

Schema migrations run automatically on startup. Peer version skew is tolerated within the same major version. Check the changelog before upgrading across major versions.

## Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `endpoint` | *(required)* | Public URL; determines domain and namespace |
| `name` | domain | Display name (AP actor name) |
| `listen` | `:5000` | Bind address |
| `dataDir` | `/apoci/storage` | Database and blob storage |
| `database.driver` | `sqlite` | `sqlite` or `postgres` |
| `database.dsn` | | Postgres connection string (required when driver is `postgres`) |
| `database.maxOpenConns` | `4`/`25` | Max open DB connections (default: 4 for sqlite, 25 for postgres) |
| `database.maxIdleConns` | `4`/`10` | Max idle DB connections (default: 4 for sqlite, 10 for postgres) |
| `keyPath` | `{dataDir}/ap.key` | RSA key, generated on first run |
| `registryToken` | *(auto-generated)* | Bearer token for push; saved to `{dataDir}/registry.token`. Reads are unauthenticated. |
| `adminToken` | *(auto-generated)* | Bearer token for admin API; saved to `{dataDir}/admin.token`. Separate from registry token. |
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
| `metrics.enabled` | `false` | Expose `/debug/vars` on the metrics port |
| `metrics.listen` | `:9090` | Metrics bind address |
| `metrics.token` | | Bearer token for `/debug/vars` (unauthenticated if empty) |

Config lookup: `config/apoci.yaml` by default, override with `-c <path>` or `APOCI_CONFIG` env var.

## API

### OCI Distribution v2

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/v2/` | API version check |
| `GET` | `/v2/<name>/manifests/<ref>` | Pull manifest by tag or digest |
| `PUT` | `/v2/<name>/manifests/<ref>` | Push manifest (authenticated) |
| `GET` | `/v2/<name>/blobs/<digest>` | Pull blob |
| `POST` | `/v2/<name>/blobs/uploads/` | Start blob upload (authenticated) |
| `PATCH` | `/v2/<name>/blobs/uploads/<id>` | Upload blob chunk (authenticated) |
| `PUT` | `/v2/<name>/blobs/uploads/<id>` | Complete blob upload (authenticated) |

### ActivityPub

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/ap/actor` | Actor profile (`Accept: application/activity+json`) |
| `POST` | `/ap/inbox` | Receive activities from peers (HTTP Signature required) |
| `GET` | `/ap/outbox` | Published activities |
| `GET` | `/ap/followers` | Follower list |
| `GET` | `/ap/following` | Following list |

### Admin

All admin endpoints require the admin token as a bearer token (`{dataDir}/admin.token`).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/admin/identity` | Node identity info |
| `GET` | `/api/admin/follows` | List follows |
| `GET` | `/api/admin/follows/pending` | List pending follow requests |
| `POST` | `/api/admin/follows` | Follow a peer |
| `POST` | `/api/admin/follows/accept` | Accept a follow request |
| `POST` | `/api/admin/follows/reject` | Reject a follow request |
| `DELETE` | `/api/admin/follows` | Unfollow a peer |

### Discovery & Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/.well-known/webfinger` | WebFinger lookup |
| `GET` | `/.well-known/nodeinfo` | NodeInfo discovery |
| `GET` | `/ap/nodeinfo/2.1` | NodeInfo 2.1 document |
| `GET` | `/healthz` | Liveness check |
| `GET` | `/readyz` | Readiness check (verifies DB connection) |

## Contributing

Bug reports and feature requests: open an issue on the repository.

Run tests:

```bash
make test
```

Lint:

```bash
make lint
```

## License

MIT
