# Deployment

This is a security-first layout for running the tile server on a public host.

## Public edge: OpenTunnel

Inbound public HTTP is handled by [OpenTunnel](https://opentunnel.xyz), not by
Caddy. The tunnel client keeps an outbound encrypted connection to the relay
and terminates TLS locally; no public HTTP listener is required on this host.

Install it and create a route:

```bash
curl -fsSL https://opentunnel.xyz/install | sh
opentunnel route add 127.0.0.1:18110
```

A route without `--name` gets a random 16-character label. **Do not use a
readable route name for a public service.** OpenTunnel explicitly warns that
readable names are guessable.

Persist and inspect the tunnel:

```bash
opentunnel up
opentunnel status
opentunnel route list
```

The per-profile user service is enabled at login where systemd or launchd is
available. Keep that service enabled so the tunnel survives logout and reboot.

### Treat tunnel URLs as secrets

The full tunnel hostname is the only thing standing between a visitor and the
service. It is not authentication.

- Do not commit tunnel IDs, route URLs, or `config.host.yaml` to the repo.
- Do not paste them into public issues, screenshots, or logs.
- If a URL leaks, remove the route and add it again to rotate the route label.
- Put authentication in the application, or keep the service private.

The host-specific `config.host.yaml`, cache directory, and OpenTunnel state are
intentionally ignored by git.

## Caddy is internal only

Caddy is kept only as a loopback reverse proxy for pgpq so its `/metrics` block
remains in place. It must not bind `:80` or `:443`.

```caddyfile
{
    auto_https off
}

:18090 {
    bind 127.0.0.1
    encode zstd gzip
    @ops path /metrics
    respond @ops 404
    reverse_proxy 127.0.0.1:8081
}
```

Verify:

```bash
ss -ltnp | grep -E ':80 |:443 |:18090'
```

Only `127.0.0.1:18090` should appear.

## Tile server container

The container publishes only on loopback:

```bash
KO_DOCKER_REPO=ko.local ko build --local --base-import-paths --tags latest \
  ./cmd/geoparquet-tileserver

docker run -d --name geoparquet-tileserver-test \
  --restart unless-stopped \
  -p 127.0.0.1:18110:8080 \
  -v "$PWD/config.host.yaml:/config.yaml:ro" \
  ko.local/geoparquet-tileserver:latest \
  --config /config.yaml --listen :8080
```

Set `base_url` in the uncommitted `config.host.yaml` to the current random
OpenTunnel URL so TileJSON, styles, HTML links, and RustGIS links resolve
correctly. The tile cache is in-memory (`cache_tiles_mb`); no cache volume is
mounted.

## PostgreSQL

The benchmark PostgreSQL container is loopback-only:

```text
127.0.0.1:55432 -> 5432/tcp
```

Never publish a database on `0.0.0.0`. Do not expose PostgreSQL through the
public tunnel without a protocol-aware proxy and authentication.

## pgvs3 accelerator

The optional pgvs3 object-store cache/artifact service is loopback/private
only. It has no authentication by design; anyone who can reach `:8014` can
read or mutate every namespace. Do not put it behind OpenTunnel.

```text
127.0.0.1:8014 -> pgvs3 (HTTP/S3/RESP/SQS)
```

Recommended tileserver config:

```yaml
object_store:
  url: "http://127.0.0.1:8014"
  artifacts_bucket: "geoparquet-derived"
  routes:
    - prefix: "https://data.source.coop/"
      bucket: "sourcecoop"
    - prefix: "https://overturemaps-us-west-2.s3.us-west-2.amazonaws.com/"
      bucket: "overturemaps-us-west-2"
```

Run pgvs3 with `--range-cache-chunk 1048576` (or persist that value with
`pgvs3 origin --range-cache-chunk 1048576 set origin.yaml`) so large Parquet
range reads are cached as aligned chunks. The durable artifacts bucket holds
versioned collection indexes; the volatile cache namespaces hold range chunks.

## Admin and development services

OpenCode and other development/admin services must not listen on a public
interface. Bind them to `127.0.0.1`, and block their host port from external
interfaces as defense in depth. The OpenCode service configuration uses
`127.0.0.1:4096`; a firewall rule drops new external connections to port 4096.

SSH remains the only intentional direct public service. Harden it with keys,
disable password authentication where possible, and restrict source addresses
if your access pattern allows it.

## Verification checklist

```bash
opentunnel status
opentunnel route list
ss -ltnp
sudo iptables -S INPUT
curl -fsS https://<your-random-route>.<tunnel-id>.opentunnel.xyz/healthz
```

Expected:

- No process listening on public `:80` or `:443`.
- Only SSH is directly public.
- PostgreSQL and OpenCode are loopback-only or firewalled.
- Public web services are reachable only through random OpenTunnel routes.
