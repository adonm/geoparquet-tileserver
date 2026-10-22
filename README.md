# GeoParquet Tile Server

A small, fast Mapbox Vector Tile (MVT) server for a STAC collection of
optimized GeoParquet files. Built in Go with
[Huma](https://huma.rocks/), [`parquet-go`](https://github.com/parquet-go/parquet-go),
and [`orb`](https://github.com/paulmach/orb).

It is deliberately narrower than a general tile platform such as Tegola:

- STAC + GeoParquet only
- WGS84 / OGC:CRS84
- MVT output
- local or HTTP(S) assets
- optional multi-resolution overviews via `geo:overviews`
- OGC API Features-style CQL2 filtering
- no PostGIS, GeoPackage, HANA, style rendering, or map composition

## Features

- STAC Collection / Catalog / Item traversal (`item` and `child` links)
- STAC and Parquet assets over local paths, `file://`, and HTTP(S)
- HTTP range requests with a chunked read cache
- automatic geometry and bbox-covering column detection
- overview level selection via `geo:overviews` and `zoom` / `gsd`
- Parquet row-group bbox pruning
- WKB decode with `orb/encoding/wkb`
- tile clipping with `orb/clip`
- MVT encoding with `orb/encoding/mvt`
- all scalar Parquet properties become MVT properties
- Basic CQL2 text filtering via `?filter=...`
- `?filter-lang=cql2-text` or `cql2-json`
- LRU tile cache plus request coalescing
- named catalogs selectable with `?catalog=<name>`
- TileJSON, collections, health, OpenAPI docs

## Default source: Overture

Overture publishes GeoParquet through a STAC catalog:

- Human guide:
  <https://docs.overturemaps.org/getting-data/cloud-sources/>
- Machine-readable STAC:
  <https://stac.overturemaps.org/catalog.json>

The default `config.example.yaml` points at a release-specific Overture
collection. Overture files are global and large, so use a tile bbox to read
only the relevant row groups. For a smaller first test, use the
`mappinginequality` catalog alias.

## Quick start

```bash
go build ./cmd/geoparquet-tileserver

cp config.example.yaml config.yaml
# edit the catalog / layer as needed
./geoparquet-tileserver --config config.yaml
```

Override the catalog at startup:

```bash
./geoparquet-tileserver --config config.yaml \
  --catalog https://stac.overturemaps.org/catalog.json
```

Endpoints:

```text
GET /collections?catalog=<name>
GET /collections/{collection}/tilejson.json?catalog=<name>
GET /collections/{collection}/tiles/{z}/{x}/{y}.mvt?catalog=<name>
GET /collections/{collection}/tiles/{z}/{x}/{y}.mvt?filter=...
GET /healthz
GET /docs
GET /openapi.json
```

`catalog` is optional; omitting it uses the default catalog. The value can be
a configured alias (`mappinginequality`) or that catalog's configured URL.
Catalogs are loaded once at startup; restart with `--catalog` to point at a
different source. Arbitrary per-request catalog URLs are intentionally not
loaded, to avoid SSRF and unbounded startup work.

## Public datasets to test against

### Overture Maps

- Guide: <https://docs.overturemaps.org/getting-data/cloud-sources/>
- STAC root: <https://stac.overturemaps.org/catalog.json>
- Latest release: `https://stac.overturemaps.org/<release>/catalog.json`
- Example collection:
  `https://stac.overturemaps.org/2026-10-21.0/divisions/division_area/collection.json`

Assets are HTTPS GeoParquet on S3/Azure; the server reads them with range
requests and row-group pruning.

### Source Cooperative / GLEN

Small, public, WGS84 GeoParquet STAC collections:

- Mapping Inequality:
  `https://data.source.coop/cboettig/mappinginequality/stac-collection.json`
- California Protected Areas:
  `https://data.source.coop/cboettig/cpad/stac-collection.json`
- CalEnviroScreen:
  `https://data.source.coop/cboettig/calenviroscreen/stac-collection.json`
- GLEN root:
  `https://data.source.coop/cboettig/glen/catalog.json`

Example alias in `config.example.yaml`:

```yaml
catalogs:
  mappinginequality: "https://data.source.coop/cboettig/mappinginequality/stac-collection.json"
```

Then:

```text
/collections?catalog=mappinginequality
/collections/features/tiles/8/66/102.mvt?catalog=mappinginequality
```

Mapping Inequality is ~2.8 MB and WGS84, so it is a good first HTTP test.
Fiboa collections such as
`https://data.source.coop/fiboa/data/bg/stac/collection.json` are currently
projected (EPSG:3857), which this server does not reproject.

## Filtering

The server implements OGC API Features-style CQL2 filtering.

```text
?filter=category='Best'
?filter=state='AL' AND metrics:area > 1000
?filter=crop:name IN ('Wheat','Corn')
?filter=name LIKE 'A%'
?filter=category IS NOT NULL
```

`filter-lang` can be `cql2-text` (default) or `cql2-json`.

Supported subset:

- `AND`, `OR`, `NOT`
- `=`, `<>`, `<`, `<=`, `>`, `>=`
- `LIKE`, `BETWEEN`, `IN`, `IS [NOT] NULL`
- numeric and string comparisons

Not yet supported: spatial and temporal CQL2 operators, arithmetic, functions.

The cache key includes the catalog, collection, zoom/x/y, and the normalized
filter query.

## STAC / GeoParquet contract

- WGS84 / OGC:CRS84
- geometry at the root, WKB or Parquet native `GEOMETRY`
- bbox covering column (`bbox` / `geom_bbox` / GeoParquet covering)
- spatial sort inside row groups
- overviews generated with `geo:overviews` metadata

`gpio publish stac` can generate the catalog. `gpio process overview` or
`tylertoo overview` can generate optimized overview GeoParquet.

## Performance notes

- Warm tiles are sub-millisecond with the in-process tile cache.
- Remote HTTP reads are chunked (1 MiB) and cached in a 64 MiB read cache.
- Prefer overview GeoParquet for low zoom.
- For selective filters, row-group statistics pushdown is still future work.
- Use a shared cache/CDN for multi-process deployments.

## Limitations

- WGS84 only; no reprojection.
- Basic CQL2 only.
- No S3/GCS native credentials; HTTPS assets only.
- Prototype; unit tests currently cover the CQL2 filter subset only.
- The first Overture tile can be slow: the server scans bbox-column pages to
  build row-group bounds. Reuse the server process / tile cache for warm tiles.
- STAC item bboxes must be accurate. Antimeridian-crossing geometries can
  produce world-spanning bboxes and defeat partition pruning.

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

Results are environment-specific.
