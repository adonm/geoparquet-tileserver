# GeoParquet Tile Server

A small, fast Mapbox Vector Tile (MVT) server for STAC catalogs of optimized
GeoParquet files. Built in Go with [Huma](https://huma.rocks/),
[`parquet-go`](https://github.com/parquet-go/parquet-go), and
[`orb`](https://github.com/paulmach/orb).

It is deliberately narrower than a general tile platform such as Tegola:

- STAC + GeoParquet only
- WGS84 / OGC:CRS84
- MVT output
- local or HTTP(S) assets
- optional multi-resolution overviews via `geo:overviews`
- OGC API Features-style CQL2 filtering
- no PostGIS, GeoPackage, HANA, style rendering, or map composition

## Features

- STAC Catalog / Collection / Item traversal (`item` and `child` links)
- root catalogs are supported; the newest versioned release is selected
- collection URLs are generated from STAC collection IDs
- virtual groups stitch sibling collections (for example Overture themes)
  into one filterable MVT layer
- direct passthrough for STAC documents and all discovered assets
- browseable Bootstrap HTML directory with links to collections, groups,
  items, tiles, TileJSON, raw STAC, and passthrough files
- server-generated MapLibre styles for rendering arbitrary MVT layers in RustGIS
- "Open in RustGIS" links for catalogs, collections, and virtual groups
- CORS headers for browser-based clients such as RustGIS
- STAC and Parquet assets over local paths, `file://`, and HTTP(S)
- HTTP range requests with a chunked read cache
- lazy item loading and lazy Parquet dataset opening
- automatic geometry and bbox-covering column detection
- overview level selection via `geo:overviews` and `zoom` / `gsd`
- Parquet row-group bbox pruning
- WKB decode with `orb/encoding/wkb`
- tile clipping with `orb/clip`
- MVT encoding with `orb/encoding/mvt`
- all scalar Parquet properties become MVT properties
- Basic CQL2 text/JSON filtering via `?filter=...`
- LRU tile cache plus request coalescing
- named catalogs selectable with `?catalog=<name>`
- TileJSON, collections, health, OpenAPI docs

## Default source: Overture

Overture publishes GeoParquet through a STAC catalog:

- Human guide:
  <https://docs.overturemaps.org/getting-data/cloud-sources/>
- Machine-readable STAC:
  <https://stac.overturemaps.org/catalog.json>

The default `config.example.yaml` points at the Overture STAC **root**.
Overture's root has several dated releases; the server follows the newest
release automatically, then indexes its collections without downloading item
or Parquet data. Tiles are opened lazily on first use.

Overture themes such as `divisions`, `base`, `buildings`, and
`transportation` are exposed as virtual **groups**. A group MVT layer stitches
all sibling GeoParquet collections into one filterable layer:

```text
/collections/divisions/tiles/{z}/{x}/{y}.mvt
/collections/divisions/tiles/{z}/{x}/{y}.mvt?filter=...
```

For a small first HTTP test, use the `mappinginequality` catalog alias.

## Browseable HTML directory

Browse:

```text
/
/collections.html
/collections/{collection}.html
/collections/{collection}/items.html
```

The root page lists catalogs, collections, and virtual groups. Collection
pages link to TileJSON, raw STAC, items, assets, and the passthrough routes.
Browsers requesting `/collections`, `/collections/{collection}`, or
`/collections/{collection}/items` with `Accept: text/html` are redirected to
the corresponding `.html` page. JSON clients keep getting JSON from the same
paths.

The UI uses Bootstrap 5.3.8 from jsDelivr. Bootstrap 6 has not been released;
5.3.8 is the current stable release.

### RustGIS links

Catalog rows and collection pages include **Open tiles in RustGIS** links:

- Collections open the generated MVT layer through a server-generated MapLibre
  style, because RustGIS's generic `?tiles=` style does not know arbitrary MVT
  layer names:

  ```text
  https://web.rustgis.app/?blank&style=<url-encoded /collections/{id}/style.json>
  ```

- Virtual groups with a catalog-level Overture PMTiles link open that PMTiles
  archive through this server with `?data=`:

  ```text
  https://web.rustgis.app/?data=<url-encoded /collections/{group}/pmtiles.pmtiles>
  ```

- Parquet and PMTiles passthrough assets get **open data** links using
  `?data=` and filenames ending in `.parquet` or `.pmtiles`.

Collection pages and directory rows with a collection-level Parquet asset
also show a green **Open Parquet in RustGIS** link. Item pages expose links for
each item-level Parquet/PMTiles asset. The links open tile layers or files
directly; they do not send RustGIS back to the STAC catalog. The server sends
CORS headers and supports range requests so RustGIS can read the data.

## Passthrough routes

All discovered STAC documents and assets can be streamed through the server.
Remote files support HTTP range requests.

```text
GET /stac
GET /collections/{collection}/stac
GET /collections/{collection}/assets/{asset}
GET /collections/{collection}/items
GET /collections/{collection}/items/{item}
GET /collections/{collection}/items/{item}/assets/{asset}
GET /collections/{collection}/pmtiles.pmtiles
GET /collections/{collection}/style.json
```

Collection-level assets (common for Source Cooperative / fiboa datasets) and
item-level assets (common for partitioned catalogs such as Overture) both use
the same passthrough mechanism. Asset links are also present in the HTML item
listing and JSON item listing.

## Auto-generated collection URLs

With no `layers` configured, every STAC collection becomes a route using its
STAC `id`:

```text
/collections?catalog=mappinginequality
/collections/mappinginequality/tiles/8/66/102.mvt?catalog=mappinginequality
```

A catalog that directly contains two or more collections also becomes a group
route using the catalog's STAC `id`. Single-child groups are skipped to avoid
duplicate aliases.

Optional manual layers rename, subset, or constrain zoom ranges:

```yaml
layers:
  - name: places
    collection: place
    min_zoom: 0
    max_zoom: 14
```

An empty `collection` means all items in the selected catalog.

## Quick start

```bash
go build ./cmd/geoparquet-tileserver

cp config.example.yaml config.yaml
./geoparquet-tileserver --config config.yaml
```

Override the catalog at startup:

```bash
./geoparquet-tileserver --config config.yaml \
  --catalog https://stac.overturemaps.org/catalog.json
```

Named catalogs can be selected per request:

```text
/collections?catalog=mappinginequality
/collections/mappinginequality/tiles/8/66/102.mvt?catalog=mappinginequality
```

`catalog` can be a configured alias or that catalog's configured URL.
Catalogs are loaded once at startup; restart with `--catalog` to point at a
different source. Arbitrary per-request catalog URLs are intentionally not
loaded, to avoid SSRF and unbounded startup work.

Endpoints:

```text
GET /collections?catalog=<name>
GET /collections/{collection}
GET /collections/{collection}/tilejson.json?catalog=<name>
GET /collections/{collection}/tiles/{z}/{x}/{y}.mvt?catalog=<name>
GET /collections/{collection}/tiles/{z}/{x}/{y}.mvt?filter=...
GET /collections/{collection}/items?catalog=<name>
GET /healthz
GET /docs
GET /openapi.json
```

## Public datasets to test against

### Overture Maps

- Guide: <https://docs.overturemaps.org/getting-data/cloud-sources/>
- STAC root: <https://stac.overturemaps.org/catalog.json>
- Current release: `https://stac.overturemaps.org/2026-10-21.0/catalog.json`
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
/collections/mappinginequality/tiles/8/66/102.mvt?catalog=mappinginequality
/collections/mappinginequality/items.html?catalog=mappinginequality
```

Mapping Inequality is ~2.8 MB and WGS84, so it is a good first HTTP test.
Fiboa collections such as
`https://data.source.coop/fiboa/data/bg/stac/collection.json` are currently
projected (EPSG:3857), which this server does not reproject.

## Filtering

The server implements OGC API Features-style CQL2 filtering. The same filter
applies to a single collection or a stitched virtual group.

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
- numeric, string, and boolean comparisons

Not yet supported: spatial and temporal CQL2 operators, arithmetic, functions.

The cache key includes the catalog, collection/group, zoom/x/y, and the
raw filter query.

## STAC / GeoParquet contract

- WGS84 / OGC:CRS84
- geometry at the root, WKB or Parquet native `GEOMETRY`
- bbox covering column (`bbox` / `geom_bbox` / GeoParquet covering)
- spatial sort inside row groups
- overviews generated with `geo:overviews` metadata

`gpio publish stac` can generate the catalog. `gpio process overview` or
`tylertoo overview` can generate optimized overview GeoParquet.

## Container build with ko

```bash
# Install ko: https://ko.build
KO_DOCKER_REPO=ko.local ko build --local --base-import-paths --tags latest \
  ./cmd/geoparquet-tileserver

docker run -d --name geoparquet-tileserver \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ko.local/geoparquet-tileserver:latest --config /config.yaml --listen :8080
```

The image is a small static/distroless image. Put a TLS reverse proxy such as
Caddy in front of the container for HTTPS.

## Performance notes

- Warm tiles are sub-millisecond with the in-process tile cache.
- Remote HTTP reads are chunked (1 MiB) and cached in a 64 MiB read cache.
- Item documents and Parquet footers are opened only when a collection/group
  is requested.
- Prefer overview GeoParquet for low zoom.
- For selective filters, row-group statistics pushdown is still future work.
- Use a shared cache/CDN for multi-process deployments.

## Limitations

- WGS84 only; no reprojection.
- Basic CQL2 only.
- No S3/GCS native credentials; HTTPS assets only.
- Prototype; unit tests cover catalog discovery, latest-release selection,
  virtual groups, and the CQL2 filter subset.
- The first tile of a large Overture group or collection may be slow: item
  metadata is fetched once and bbox-column pages are scanned to build
  row-group bounds. Reuse the server process / tile cache for warm tiles.
- STAC item bboxes must be accurate. Antimeridian-crossing geometries can
  produce world-spanning bboxes and defeat partition pruning.
- Virtual groups are only generated for catalogs with two or more direct
  child collections; nested catalogs keep their own group/collection routes.

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
