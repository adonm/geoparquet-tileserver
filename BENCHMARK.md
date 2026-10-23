# Tile loading benchmark notes

These are informal single-host measurements for choosing an architecture, not
a formal benchmark suite. The purpose is to show where the API path spends
time and how much caching helps.

> Historical note: the measurements below include a persistent disk cache that
> has since been removed. The server now keeps encoded tiles and HTTP ranges in
> memory only; persistent source caching belongs in an external object-store
> proxy.

## Test areas

- **Local overview dataset**: local GeoParquet, 16 tiles at z5 around the
  populated area. Compared against `go-pmtiles serve` for a local PMTiles
  archive.
- **Mapping Inequality over HTTPS**: 16 tiles at z8 around Birmingham,
  Alabama. Compared with the same archive served locally through
  `go-pmtiles serve`.

The PMTiles server was local-file based and had a 64 MB in-process cache. The
API was remote over HTTPS in the Mapping Inequality test, so the PMTiles
numbers are a best case for a precomputed archive.

## Results

### Local overview, 16 tiles at z5

| Path | Cold | Warm | Tile bytes |
|---|---:|---:|---:|
| PMTiles (`go-pmtiles`, local archive) | 11.4 ms | 7.7 ms | 1.22 MB |
| API MVT from local GeoParquet | 2,750 ms | 6.9 ms | 2.25 MB |
| API MVT after process restart, disk cache | — | 13.9 ms | 2.25 MB |

The API cold run is dominated by Parquet row-group scanning, geometry decoding,
clipping, and MVT encoding. A warm tile is actually slightly faster than the
PMTiles server because it is an in-process byte cache hit.

### Mapping Inequality over HTTPS, 16 tiles at z8

| Path | Cold | Warm | Tile bytes | Cache |
|---|---:|---:|---:|---|
| PMTiles (local archive) | 11.4 ms | 7.7 ms | 29.5 KB | archive cache |
| API MVT, no persistent cache | 5,952 ms | 24 ms | 33.3 KB | memory |
| API MVT, disk cache after restart | 26 ms | 23 ms | 33.3 KB | disk/memory |
| API MVT, new tile area with warm range cache | 1,879 ms | — | 84.9 KB | range cache |

The first API tile after a clean start took about 4.9 seconds. That is
dataset opening plus remote range reads and bbox-page scans, not MVT encoding
alone. Disk-caching the range chunks and tile bytes turns a restart into the
same order of magnitude as an in-memory hit.


## Query vs MVT encode

`X-Debug-Timing: 1` on a tile request returns phase timings. `query` is
dataset open + bbox/row-group discovery + row-group scan/decode/filter/clip;
`encode` is MVT projection, clipping, and `mvt.Marshal`.

### Local overview, 16 tiles at z5

| Phase | Total | Per tile | Share |
|---|---:|---:|---:|
| Query | 2,688 ms | 168.0 ms | 93.9% |
| MVT encode | 174.6 ms | 10.9 ms | 6.1% |
| Total | 2,883 ms | 180.2 ms | |

### Mapping Inequality over HTTPS, 16 tiles at z8

| Run | Query | Dataset open | Bounds | Scan/decode | Encode | Features |
|---|---:|---:|---:|---:|---:|---:|
| First pass | 4,265 ms | 2,908 ms | 1.4 ms | 1,356 ms | 3.1 ms | 241 |
| Second pass, warm range cache | 1,138 ms | 0 ms | 0 ms | 1,138 ms | 3.3 ms | 241 |
| Filtered `category = 'Best'` | 993 ms | 0 ms | 0 ms | 993 ms | 0.6 ms | 24 |

For the remote z8 area, only 241 features were returned across 16 tiles, so
MVT encoding is negligible. The filtered query still scanned/decoded the same
row groups; only the output size and encode time dropped.

This makes the priority clear: the bottleneck is the Parquet query path, not
MVT encoding. Geometry simplification will reduce payload bytes and some encode
time, but will not fix cold query latency.

## What needs improvement

1. **Cold first-hit latency is the main weakness.** PMTiles is already indexed
   and can serve a tile in under a millisecond locally; the API must open a
   Parquet dataset, find bbox bounds, read row groups, decode geometry, clip,
   filter, and encode MVT. The first tile of a new remote collection is the
   expensive one.
2. **MVT output is larger than the PMTiles equivalent.** The local overview
   produced 2.25 MB vs 1.22 MB for 16 tiles. Recommendations:
   - drop bbox helper columns such as `geom_bbox.*` from MVT properties;
   - simplify geometry per zoom;
   - add a `properties` projection option;
   - consider lower precision / quantization for geometry.
3. **Caching is essential.** The current cache stores generated tiles in a
   byte-bounded memory LRU and 1 MiB HTTP range chunks in a per-source memory
   cache. Persistent caching, if needed, should be provided by an external
   object-store proxy.
4. **Raw asset passthrough ranges stream directly.** They no longer use a disk
   chunk cache; upstream range support is forwarded unchanged.
5. **PMTiles is the better serving format for static layers.** Use PMTiles for
   precomputed, unfiltered map layers; use the GeoParquet API for filtering,
   dynamic properties, and data that changes too often to pre-tile.

## Memory tile cache configuration

```yaml
cache_tiles_mb: 256
max_features: 100000
```

Encoded MVT tiles are held in a byte-bounded in-memory LRU. Keys include the
resolved catalog fingerprint, renderer/cache schema version, zoom/x/y,
`min_count`, and a normalized filter hash. Responses expose an `X-Cache` header
of `memory` or `miss` for tile requests. `GET /cache/stats` returns hit, miss,
eviction, entry, and byte counters.
