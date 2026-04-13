# apoci

> **Status: Beta** -- Running in production is possible, but expect rough edges. APIs may change between minor versions.

You self-host Forgejo, your container registry, and everything else on one homelab. If that homelab dies, how will you rebootstrap it. apoci solves this: federate your registry over ActivityPub so a handful of friends mirror your artifacts. When your server goes down, your peers still serve your images and you can bootstrap from any of them.

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
TOKEN=$(cat /apoci/storage/registry.token)
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

The domain prefix is added automatically when the repo path is not domain-scoped, so you don't need to type it:

```bash
docker push foo.com/myapp:v1               # stored as foo.com/myapp
docker push foo.com/foo.com/myapp:v1       # also works (prefix already present)
docker push foo.com/foreign.dev/app:v1     # DENIED: foreign domain
```

Pulls require the repository to exist in the database (created by a prior push or federation). Blobs cannot be read from arbitrary or non-existent repository paths.

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
apoci follow remove bar.com --force   # remove local records even if the peer is unreachable
apoci identity show
```

### Remote CLI

All `follow` and `identity` subcommands can target a remote instance using `--remote` and `--token`:

```bash
ADMIN_TOKEN=$(cat /apoci/storage/admin.token)
apoci follow list --remote https://registry.example.com --token "$ADMIN_TOKEN"
apoci follow add bar.com --remote https://registry.example.com --token "$ADMIN_TOKEN"
```

Or set `APOCI_REMOTE_URL` and `APOCI_ADMIN_TOKEN` to avoid repeating flags:

```bash
export APOCI_REMOTE_URL=https://registry.example.com
export APOCI_ADMIN_TOKEN=$(cat /apoci/storage/admin.token)
apoci follow list
apoci follow add bar.com
apoci identity show
```

This hits the admin API (`/api/admin/...`) on the remote node, authenticated with the admin token (separate from the registry push token). Useful for managing headless or containerized instances.

## Upstream proxy

apoci can act as a pull-through cache for any external OCI registry — Docker Hub, GHCR, Quay, a private Harbor, or anything speaking OCI Distribution v2. Configure the registries you want to proxy, then pull images through your node using the upstream registry hostname as the leading path segment:

```bash
docker pull foo.com/docker.io/library/nginx:latest
docker pull foo.com/ghcr.io/user/repo:tag
docker pull foo.com/quay.io/prometheus/prometheus:v2.51.0
```

On first pull, apoci fetches the manifest and streams the blobs from the upstream, caches them locally, and serves them. Subsequent pulls are fully local — the upstream never sees the second request.

### Configure

```yaml
upstreams:
  enabled: true
  fetchTimeout: 60s
  registries:
    - name: docker.io
      endpoint: "https://registry-1.docker.io"
      auth: token         # Bearer challenge; works anonymously for public images

    - name: ghcr.io
      endpoint: "https://ghcr.io"
      auth: token
      username: myuser
      password: "ghp_yourtoken"

    - name: harbor.corp.example
      endpoint: "https://harbor.corp.example"
      auth: basic
      username: robot$apoci
      password: "robot-password"
      private: true    # require auth to pull images from this upstream through apoci
```

Alternatively, configure via env vars using a JSON array:

```bash
APOCI_UPSTREAMS_ENABLED=true
APOCI_UPSTREAMS_REGISTRIES='[{"name":"docker.io","endpoint":"https://registry-1.docker.io","auth":"token"},{"name":"ghcr.io","endpoint":"https://ghcr.io","auth":"token","username":"myuser","password":"ghp_yourtoken","private":true}]'
```

`auth` must be one of:

| Value | Description |
|-------|-------------|
| `none` | No authentication |
| `basic` | HTTP Basic Auth (`username` + `password` required) |
| `token` | Docker Bearer token challenge (used by Docker Hub, GHCR, Quay) |

### Private upstream images

By default, apoci serves cached images to anyone (no auth required for pulls), matching the behavior of a public registry. If you configure an upstream with credentials to access **private** images, set `private: true` — apoci will then require your registry token to pull any image cached from that upstream.

`private` is enforced at the registry level, not per-image. Registries like GHCR or Docker Hub host both public and private packages under the same hostname. If you need to pull private packages from such a registry, your options are:

- `private: true` — all images cached from that registry require auth on your node (public packages included)
- `private: false` (default) — cached images are publicly accessible; only use this if you are not proxying private content

### Pull syntax

```
docker pull <your-node>/<upstream-registry>/<image>:<tag>
```

The first path segment after your node must match a configured registry `name`. Any path that does not match a known registry name falls through to normal local/federated lookup.

### Circuit breaker

If an upstream becomes unreachable, apoci opens a circuit breaker for that registry. Pulls that would hit an open circuit return a 404 immediately rather than blocking. The circuit probes the upstream again after a backoff period and closes on success.

## Security

1. **Follow gate** -- only approved peers can send activities
2. **Replay-resistant HTTP Signatures** -- RSA-SHA256 on every request; replays are rejected (5-minute validity window with a seen-signature cache)
3. **Namespace enforcement** -- writes are scoped to the node's domain; reads require the repository to exist. Blobs and manifests are only served from repositories that were created by a prior push or federation
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

WebFinger accepts both `acct:registry@example.com` and `acct:registry@registry.example.com`. Repos are namespaced under the `accountDomain` (`example.com/*`). Federated peers read the `ociNamespace` field in the actor document to validate ownership.

The domain prefix is added automatically on push/pull, so you don't need to type it:

```bash
docker push registry.example.com/myteam/myapp:v1
# stored and federated as example.com/myteam/myapp
```

Federated repos from other domains keep their full path:

```bash
docker pull registry.example.com/other.dev/user/repo:latest
```

Writes to a foreign namespace (`other.dev/...`) are rejected; reads work for any repo in the database.

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

## Notifications

Send alerts to Discord, Slack, Telegram, email, and more via [shoutrrr](https://github.com/nicholas-fedor/shoutrrr) URLs.

```yaml
notifications:
  urls:
    - "discord://token@id"
    - "slack://token:token:token@channel"
  events:
    - peer_health
    - follow_request
    - replication_failure
    - gc_error
```

| Event | Description |
|-------|-------------|
| `peer_health` | A federation peer went up or down |
| `follow_request` | New follow request pending operator approval |
| `replication_failure` | Blob replication from a peer failed |
| `gc_error` | Garbage collection encountered an error |

No events are enabled by default. See the [shoutrrr docs](https://nicholas-fedor.github.io/shoutrrr) for supported services and URL formats.

Env vars: `APOCI_NOTIFICATIONS_URLS` (comma-separated) and `APOCI_NOTIFICATIONS_EVENTS` (comma-separated).

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
| `upstream_blob_pull_throughs_total` | counter | Blobs fetched from upstream registries (labelled by registry) |
| `upstream_manifest_pull_throughs_total` | counter | Manifests fetched from upstream registries (labelled by registry) |
| `upstream_circuit_open` | gauge | Circuit breaker state per upstream registry (1 = open, 0 = closed) |

## Web UI

Enable a browser-based image browser at the root path:

```yaml
ui:
  enabled: true
```

When enabled, visiting your registry in a browser shows:

- **My Images** — repositories you've pushed locally
- **Federated Images** — repositories mirrored from peers, grouped by source

Features:

- Server-side search with instant filtering
- Pull commands for easy copy-paste
- Automatic dark/light mode based on system preference
- No JavaScript required for basic browsing (htmx enhances search)

Private repositories (marked `private: true` in the database) are excluded from the listing. The UI is read-only — it doesn't expose any write operations.

When disabled (default), the root path returns a minimal JSON status response.

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

All settings can be configured via YAML file, environment variables, or both. Environment variables take precedence over YAML values. The YAML file is optional -- you can run purely from env vars.

| Field | Env Var | Default | Description |
|-------|---------|---------|-------------|
| `endpoint` | `APOCI_ENDPOINT` | *(required)* | Public URL; determines domain and namespace |
| `name` | `APOCI_NAME` | domain | Display name (AP actor name) |
| `listen` | `APOCI_LISTEN` | `:5000` | Bind address |
| `dataDir` | `APOCI_DATA_DIR` | `/apoci/storage` | Database and blob storage |
| `database.driver` | `APOCI_DB_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `database.dsn` | `APOCI_DB_DSN` | | Postgres connection string (required when driver is `postgres`) |
| `database.maxOpenConns` | `APOCI_DB_MAX_OPEN_CONNS` | `4`/`25` | Max open DB connections (default: 4 for sqlite, 25 for postgres) |
| `database.maxIdleConns` | `APOCI_DB_MAX_IDLE_CONNS` | `4`/`10` | Max idle DB connections (default: 4 for sqlite, 10 for postgres) |
| `keyPath` | `APOCI_KEY_PATH` | `{dataDir}/ap.key` | RSA key, generated on first run |
| `registryToken` | `APOCI_REGISTRY_TOKEN` | *(auto-generated)* | Bearer token for push; saved to `{dataDir}/registry.token`. Reads are unauthenticated. |
| `adminToken` | `APOCI_ADMIN_TOKEN` | *(auto-generated)* | Bearer token for admin API; saved to `{dataDir}/admin.token`. Separate from registry token. |
| `accountDomain` | `APOCI_ACCOUNT_DOMAIN` | endpoint domain | Vanity domain for `@registry@<domain>` handle |
| `immutableTags` | `APOCI_IMMUTABLE_TAGS` | `^v[0-9]` | Regex, matching tags can't be overwritten |
| `logLevel` | `APOCI_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `logFormat` | `APOCI_LOG_FORMAT` | `json` | `json` / `text` |
| `tls.cert` | `APOCI_TLS_CERT` | | TLS cert path |
| `tls.key` | `APOCI_TLS_KEY` | | TLS key path |
| `peering.healthCheckInterval` | `APOCI_PEERING_HEALTH_CHECK_INTERVAL` | `30s` | Peer health poll interval |
| `peering.fetchTimeout` | `APOCI_PEERING_FETCH_TIMEOUT` | `60s` | Blob fetch timeout |
| `limits.maxManifestSize` | `APOCI_MAX_MANIFEST_SIZE` | `10485760` | Max manifest size in bytes (10 MB) |
| `limits.maxBlobSize` | `APOCI_MAX_BLOB_SIZE` | `536870912` | Max blob size in bytes (512 MB) |
| `gc.enabled` | `APOCI_GC_ENABLED` | `true` | Set to `false` to disable background garbage collection |
| `gc.interval` | `APOCI_GC_INTERVAL` | `6h` | How often to run GC |
| `gc.stalePeerBlobAge` | `APOCI_GC_STALE_PEER_BLOB_AGE` | `720h` | Remove peer blob refs older than this (720h = 30 days) |
| `gc.orphanBatchSize` | `APOCI_GC_ORPHAN_BATCH_SIZE` | `500` | Max orphaned blobs to process per cycle |
| `federation.autoAccept` | `APOCI_FEDERATION_AUTO_ACCEPT` | `none` | `none`, `mutual` (peers you follow), or `all` (public) |
| `federation.allowedDomains` | `APOCI_FEDERATION_ALLOWED_DOMAINS` | `[]` | Always auto-accept follows from these domains (comma-separated in env) |
| `federation.blockedDomains` | `APOCI_FEDERATION_BLOCKED_DOMAINS` | `[]` | Silently drop all activities from these domains (comma-separated in env) |
| `federation.blockedActors` | `APOCI_FEDERATION_BLOCKED_ACTORS` | `[]` | Silently drop all activities from these actor URLs (comma-separated in env) |
| `notifications.urls` | `APOCI_NOTIFICATIONS_URLS` | `[]` | Shoutrrr notification URLs (comma-separated in env) |
| `notifications.events` | `APOCI_NOTIFICATIONS_EVENTS` | `[]` | Events to notify on: `peer_health`, `follow_request`, `replication_failure`, `gc_error` (comma-separated in env) |
| `metrics.enabled` | `APOCI_METRICS_ENABLED` | `false` | Expose `/debug/vars` on the metrics port |
| `metrics.listen` | `APOCI_METRICS_LISTEN` | `:9090` | Metrics bind address |
| `metrics.token` | `APOCI_METRICS_TOKEN` | | Bearer token for `/debug/vars` (unauthenticated if empty) |
| `upstreams.enabled` | `APOCI_UPSTREAMS_ENABLED` | `false` | Enable pull-through proxy for upstream registries |
| `upstreams.fetchTimeout` | `APOCI_UPSTREAMS_FETCH_TIMEOUT` | `60s` | Timeout for upstream registry requests |
| `upstreams.registries` | `APOCI_UPSTREAMS_REGISTRIES` | `[]` | JSON array of upstream registry configs — each entry: `name`, `endpoint`, `auth`, `username`, `password`, `private` |
| `ui.enabled` | `APOCI_UI_ENABLED` | `false` | Enable web UI at `/` to browse images |

**CLI-only env vars** (no YAML equivalent, used by `follow` and `identity` subcommands):

| Env Var | Description |
|---------|-------------|
| `APOCI_REMOTE_URL` | Default `--remote` URL for CLI subcommands |
| `APOCI_ADMIN_TOKEN` | Default `--token` for CLI subcommands (falls back to the server config value when running locally) |

Config lookup: `config/apoci.yaml` by default, override with `-c <path>` or `APOCI_CONFIG` env var. If the config file is not found, apoci runs purely from environment variables and defaults.

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
| `DELETE` | `/api/admin/follows` | Unfollow a peer. Body: `{"target": "<actor-url>"}`. Pass `"force": true` to remove local records even when the peer is unreachable (skips sending an ActivityPub `Undo Follow`). |

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
