# GeoParquet Tile Server

A small, fast Mapbox Vector Tile (MVT) server for a STAC collection of
optimized GeoParquet files. Built in Go with
[Huma](https://huma.rocks/), [`parquet-go`](https://github.com/parquet-go/parquet-go),
and [`orb`](https://github.com/paulmach/orb).

It is deliberately narrower than a general tile platform such as Tegola:

- one STAC collection per layer
- GeoParquet only
- WGS84 / OGC:CRS84
- MVT output
- optional multi-resolution overviews via `geo:overviews`
- no PostGIS, GeoPackage, HANA, style rendering, or map composition

## Features

- STAC Collection / Catalog / Item traversal (`item` and `child` links)
- `file://` and local relative/absolute asset paths
- automatic overview detection via the `geo:overviews` footer key
- zoom-to-level selection and Parquet row-group bbox pruning
- lazy loading for spatial partition catalogs
- optional startup preload of overview row-group bounds
- WKB decode with `orb/encoding/wkb`
- tile clipping with `orb/clip`
- MVT encoding with `orb/encoding/mvt`
- byte/LRU tile cache plus request coalescing
- typed `min_count` URL filter
- TileJSON, collections, health, OpenAPI docs from Huma

## Quick start

```bash
go build ./cmd/geoparquet-tileserver

cat > config.yaml <<'YAML'
listen: "127.0.0.1:8080"
catalog: "/path/to/collection.json"
cache_tiles: 4096
preload: true
layers:
  - name: cells
    min_zoom: 0
    max_zoom: 14
YAML

./geoparquet-tileserver --config config.yaml
```

Endpoints:

```text
GET /collections
GET /collections/{collection}/tilejson.json
GET /collections/{collection}/tiles/{z}/{x}/{y}
GET /collections/{collection}/tiles/{z}/{x}/{y}.mvt?min_count=500
GET /healthz
GET /docs
GET /openapi.json
```

## STAC / GeoParquet contract

The catalog may be a STAC Collection, Catalog, or Item. Item assets must point
at GeoParquet files (`type: application/vnd.apache.parquet` or `.parquet`).

Two layouts are supported.

### Overviews (recommended)

One or more items with a GeoParquet asset containing a `geo:overviews` footer
key. The server selects the level by zoom, prunes row groups using the bbox
covering, and reads only surviving row groups.

This is the lowest-latency layout for z0–z8 and beyond.

### Spatial partitions

Items with ordinary GeoParquet assets and accurate bboxes. The server selects
intersecting items and lazy-loads each partition.

Partition-only catalogs should not be used for low zoom without pre-aggregated
levels: every visible full-resolution feature would be encoded into the tile.

## Input requirements

- WGS84 / OGC:CRS84
- geometry at the root, WKB or Parquet native `GEOMETRY`
- bbox covering column (`xmin`, `ymin`, `xmax`, `ymax`)
- Hilbert/spatial sort inside row groups
- overviews generated with `geo:overviews` metadata

`gpio publish stac` can generate the catalog. `gpio process overview` or
`tylertoo overview` can generate optimized overview GeoParquet.

## Filtering

`min_count` is a typed map/query parameter:

```text
/tiles/8/41/135.mvt?min_count=500
```

The cache key includes the filter value. Additional filters can be added in
`internal/tiles` and the Huma input struct.

## Performance notes

- Warm overview tiles are in the low-millisecond range on a local dataset.
- The expensive part is geometry decoding, clipping, and MVT encoding.
- Cache encoded tiles and preload overview bounds.
- For high traffic, use a byte-weighted cache, ETags, and a CDN.
- For selective attribute filters, add row-group statistics pushdown or
  pre-materialized filtered levels.

## Limitations

- Local files only; no S3/HTTP asset support yet.
- WGS84 only.
- Fixed MVT properties (`count`, `sum_value`) and one `min_count` filter.
- Prototype; no test suite yet.
- STAC bboxes must be accurate. Antimeridian-crossing geometries can produce
  world-spanning item bboxes and defeat partition pruning.

## License

Apache License 2.0. See [LICENSE](LICENSE).

## Local benchmark snapshot

Warm z8 tile, local loopback, 200 concurrent requests:

| Server | p50 | p95 | Throughput |
|---|---:|---:|---:|
| standalone (cached) | 0.75 ms | 0.90 ms | 4,231 req/s |
| standalone (c=16) | 2.06 ms | 5.67 ms | 5,042 req/s |
| PMTiles (c=16) | 14.8 ms | 22.3 ms | 781 req/s |
| Tegola overview (c=16) | 6.75 ms | 17.3 ms | 1,668 req/s |
| Rust/tylertoo server (c=16) | 2.33 ms | 5.26 ms | 4,871 req/s |

Cold tile generation is ~80 ms for this z8 tile after preload; warm cache hits are
sub-millisecond. Results are environment-specific.
