# Tile loading benchmark notes

These are informal single-host measurements for choosing an architecture, not
a formal benchmark suite. The purpose is to show where the API path spends
time and how much persistent caching helps.

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
3. **Disk caching is essential.** The implemented cache stores both generated
   tiles and 1 MiB HTTP range chunks. After a restart, the tested remote view
   went from ~5.9 s to ~26 ms.
4. **Raw asset passthrough is still streamed.** The API's Parquet reader and
   generated tiles use the disk range cache, but a client reading a raw
   Parquet/PMTiles asset through the passthrough route still goes to the
   upstream server for each request. A chunked range cache for passthrough
   assets is the next caching improvement.
5. **PMTiles is the better serving format for static layers.** Use PMTiles for
   precomputed, unfiltered map layers; use the GeoParquet API for filtering,
   dynamic properties, and data that changes too often to pre-tile.

## Persistent cache configuration

```yaml
cache_dir: "/cache"
cache_disk_mb: 2048
```

The disk cache is content-addressed and bounded. When the size limit is
exceeded, the least-recently-used files are removed. Mount `/cache` as a
persistent Docker volume. The ko image runs as UID/GID `65532`, so the host
cache directory must be writable by that user:

```bash
mkdir -p cache
sudo chown 65532:65532 cache
```

Responses expose an `X-Cache` header of `memory`, `disk`, or `miss` for tile
requests, which makes cache validation easy.
