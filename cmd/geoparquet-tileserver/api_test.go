package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/parquet-go/parquet-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/encoding/wkb"

	"github.com/adonm/geoparquet-tileserver/internal/artifact"
	"github.com/adonm/geoparquet-tileserver/internal/catalog"
	"github.com/adonm/geoparquet-tileserver/internal/config"
	"github.com/adonm/geoparquet-tileserver/internal/tilecache"
	"github.com/adonm/geoparquet-tileserver/internal/tiles"
)

type testBBox struct {
	Xmin float64 `parquet:"xmin"`
	Ymin float64 `parquet:"ymin"`
	Xmax float64 `parquet:"xmax"`
	Ymax float64 `parquet:"ymax"`
}

type testRow struct {
	ID       string   `parquet:"id"`
	Name     string   `parquet:"name"`
	Geometry []byte   `parquet:"geometry"`
	BBox     testBBox `parquet:"bbox"`
}

func newTestServer(t *testing.T) (*server, chi.Router) {
	t.Helper()
	dir := t.TempDir()
	parquetPath := filepath.Join(dir, "data.parquet")
	rows := []testRow{}
	for i, pt := range []orb.Point{{1, 2}, {3, 4}} {
		geom, err := wkb.Marshal(pt)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, testRow{
			ID: fmt.Sprintf("%c", 'a'+i), Name: fmt.Sprintf("Place %c", 'A'+i), Geometry: geom,
			BBox: testBBox{Xmin: pt[0], Ymin: pt[1], Xmax: pt[0], Ymax: pt[1]},
		})
	}
	if err := parquet.WriteFile(parquetPath, rows); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(dir, "places", "collection.json"), map[string]any{
		"type": "Collection", "id": "places", "title": "Places", "description": "Test places",
		"extent": map[string]any{
			"spatial":  map[string]any{"bbox": [][]float64{{0, 0, 10, 10}}},
			"temporal": map[string]any{"interval": [][]any{{"2020-01-01T00:00:00Z", "2021-01-01T00:00:00Z"}}},
		},
		"links": []map[string]string{
			{"rel": "item", "href": "items/a.json"},
			{"rel": "item", "href": "items/b.json"},
		},
	})
	writeTestJSON(t, filepath.Join(dir, "places", "items", "a.json"), map[string]any{
		"type": "Feature", "stac_version": "1.1.0", "id": "a", "collection": "places",
		"bbox": []float64{1, 2, 1, 2}, "geometry": map[string]any{"type": "Point", "coordinates": []float64{1, 2}},
		"properties": map[string]any{"datetime": "2020-06-01T00:00:00Z", "name": "A"},
		"assets":     map[string]any{"data": map[string]any{"href": "../../data.parquet", "type": "application/vnd.apache.parquet"}},
	})
	writeTestJSON(t, filepath.Join(dir, "places", "items", "b.json"), map[string]any{
		"type": "Feature", "stac_version": "1.1.0", "id": "b", "collection": "places",
		"bbox": []float64{3, 4, 3, 4}, "geometry": map[string]any{"type": "Point", "coordinates": []float64{3, 4}},
		"properties": map[string]any{"datetime": "2020-07-01T00:00:00Z", "name": "B"},
		"assets":     map[string]any{"thumb": map[string]any{"href": "./thumb.png", "type": "image/png"}},
	})
	writeTestJSON(t, filepath.Join(dir, "catalog.json"), map[string]any{
		"type": "Catalog", "id": "test", "title": "Test",
		"links": []map[string]string{{"rel": "child", "href": "places/collection.json"}},
	})
	cat, err := catalog.Load(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry := &layerEntry{
		name: "places", title: "Places", kind: "collection", collection: cat.Collections[0],
		opts: tiles.Options{Name: "places", Collection: "places", GeometryColumn: "geometry", MinZoom: 0, MaxZoom: 14},
	}
	st := &catalogState{
		name: "default", cat: cat, base: filepath.Join(dir, "catalog.json"), version: "test",
		layers: map[string]*layerEntry{"places": entry}, order: []string{"places"},
	}
	s := &server{
		cfg:        &config.Config{BaseURL: "http://example.test", MaxFeatures: 100000, CacheTilesMB: 16},
		states:     map[string]*catalogState{"default": st},
		catalogURL: map[string]string{},
		cache:      tilecache.New(16 << 20),
	}
	globalMaxFeatures = s.cfg.MaxFeatures
	router := chi.NewMux()
	s.registerSTACAPI(router)
	s.registerOGCAPI(router)
	return s, router
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, router http.Handler, path string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, rec.Header().Get("Content-Type")
}

func TestSTACLandingAndCollectionSearch(t *testing.T) {
	_, router := newTestServer(t)
	code, body, ct := getJSON(t, router, "/stac-api")
	if code != 200 || ct != "application/json" {
		t.Fatalf("landing = %d %s", code, ct)
	}
	if body["type"] != "Catalog" || body["stac_version"] != "1.1.0" {
		t.Fatalf("landing body = %#v", body)
	}
	conforms, _ := body["conformsTo"].([]any)
	if len(conforms) < 5 {
		t.Fatalf("conformsTo = %#v", conforms)
	}
	code, body, _ = getJSON(t, router, "/stac-api/collections?q=places")
	if code != 200 {
		t.Fatalf("collections code = %d", code)
	}
	if body["numberMatched"] != float64(1) {
		t.Fatalf("collections = %#v", body)
	}
	col := body["collections"].([]any)[0].(map[string]any)
	if col["type"] != "Collection" || col["id"] != "places" {
		t.Fatalf("collection = %#v", col)
	}
}

func TestSTACItemsPagingAndSearch(t *testing.T) {
	_, router := newTestServer(t)
	code, body, ct := getJSON(t, router, "/stac-api/collections/places/items?limit=1")
	if code != 200 || ct != "application/geo+json" {
		t.Fatalf("items = %d %s", code, ct)
	}
	if body["numberMatched"] != float64(2) || body["numberReturned"] != float64(1) {
		t.Fatalf("items body = %#v", body)
	}
	links := body["links"].([]any)
	var next string
	for _, l := range links {
		if l.(map[string]any)["rel"] == "next" {
			next = l.(map[string]any)["href"].(string)
		}
	}
	if next == "" {
		t.Fatal("missing next link")
	}
	next = strings.TrimPrefix(next, "http://example.test")
	_, body, _ = getJSON(t, router, next)
	if body["numberReturned"] != float64(1) {
		t.Fatalf("next page = %#v", body)
	}
	code, body, _ = getJSON(t, router, "/stac-api/search?collections=places&bbox=2,3,5,5")
	if code != 200 || body["numberMatched"] != float64(1) {
		t.Fatalf("search bbox = %d %#v", code, body)
	}
	feature := body["features"].([]any)[0].(map[string]any)
	if feature["id"] != "b" || feature["geometry"] == nil {
		t.Fatalf("feature = %#v", feature)
	}
	assets := feature["assets"].(map[string]any)
	if len(assets) != 1 {
		t.Fatalf("assets = %#v", assets)
	}
}

func TestSTACPostSearch(t *testing.T) {
	_, router := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/stac-api/search", strings.NewReader(`{"collections":["places"],"limit":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post search = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["numberReturned"] != float64(1) {
		t.Fatalf("post search = %#v", body)
	}
}

func TestOGCFeaturesAndQueryables(t *testing.T) {
	_, router := newTestServer(t)
	code, body, ct := getJSON(t, router, "/ogc/collections/places/items?bbox=0,0,2,3&limit=10")
	if code != 200 || ct != "application/geo+json" {
		t.Fatalf("ogc items = %d %s %#v", code, ct, body)
	}
	if body["numberReturned"] != float64(1) {
		t.Fatalf("ogc items body = %#v", body)
	}
	feature := body["features"].([]any)[0].(map[string]any)
	if feature["id"] != "a" || feature["geometry"] == nil {
		t.Fatalf("feature = %#v", feature)
	}
	code, body, ct = getJSON(t, router, "/ogc/collections/places/queryables")
	if code != 200 || ct != "application/schema+json" {
		t.Fatalf("queryables = %d %s", code, ct)
	}
	props := body["properties"].(map[string]any)
	if _, ok := props["geometry"]; !ok {
		t.Fatalf("missing geometry queryable: %#v", props)
	}
	if _, ok := props["name"]; !ok {
		t.Fatalf("missing name queryable: %#v", props)
	}
}

func TestOGCTilesetAndTile(t *testing.T) {
	_, router := newTestServer(t)
	code, body, _ := getJSON(t, router, "/ogc/collections/places/tiles/WebMercatorQuad")
	if code != 200 {
		t.Fatalf("tileset = %d", code)
	}
	tilesURL, _ := body["tiles"].([]any)
	if len(tilesURL) != 1 {
		t.Fatalf("tiles = %#v", body["tiles"])
	}
	req := httptest.NewRequest(http.MethodGet, "/ogc/collections/places/tiles/WebMercatorQuad/0/0/0", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tile = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("tile content type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing tile ETag")
	}
	req = httptest.NewRequest(http.MethodGet, "/ogc/collections/places/tiles/WebMercatorQuad/0/0/0", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional tile = %d", rec.Code)
	}
}

func TestLayerIndexArtifact(t *testing.T) {
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/derived/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[key] = b
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			b, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	defer srv.Close()
	store, err := artifact.New(srv.URL, "derived")
	if err != nil {
		t.Fatal(err)
	}
	idx := &tiles.Index{
		SchemaVersion: tiles.IndexSchemaVersion, CatalogVersion: "v1", Collection: "places",
		Bounds: [4]float64{0, 0, 10, 10},
		Fields: []tiles.Field{{Name: "geometry", Format: "geometry-any", Geometry: true}},
		Partitions: []tiles.PartitionIndex{{
			Asset: "data.parquet", Bounds: [4]float64{0, 0, 10, 10},
			Levels: []tiles.LevelIndex{{Level: 0, RowGroups: []tiles.RowGroupIndex{{Index: 0, Bounds: [4]float64{0, 0, 10, 10}}}}},
		}},
	}
	sSaveLayerIndex(store, "index/v1/v1/places.json.gz", idx)
	got, ok := sLoadLayerIndex(store, "index/v1/v1/places.json.gz")
	if !ok || got.Collection != "places" || len(got.Partitions) != 1 || len(got.Fields) != 1 {
		t.Fatalf("index = %#v, ok=%v", got, ok)
	}
}

func TestLayerIndexRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	entry := s.states["default"].layers["places"]
	layer, err := entry.get()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := layer.BuildIndex("v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Partitions) != 1 || len(idx.Fields) == 0 {
		t.Fatalf("index = %#v", idx)
	}
	layer2 := &tiles.Layer{Name: "places"}
	if err := layer2.ApplyIndex(idx); err != nil {
		t.Fatal(err)
	}
	if fields := layer2.Fields(); len(fields) != len(idx.Fields) {
		t.Fatalf("applied fields = %#v", fields)
	}
}

func TestLayerPropertyProjection(t *testing.T) {
	s, _ := newTestServer(t)
	entry := s.states["default"].layers["places"]
	entry.opts.ExcludeProps = []string{"name"}
	entry.opts.Simplify = 1.0
	layer, err := entry.get()
	if err != nil {
		t.Fatal(err)
	}
	b, err := layer.Render(context.Background(), 0, 0, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := mvt.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) == 0 || len(layers[0].Features) != 2 {
		t.Fatalf("layers = %#v", layers)
	}
	for _, f := range layers[0].Features {
		if _, ok := f.Properties["name"]; ok {
			t.Fatalf("excluded property leaked: %#v", f.Properties)
		}
		if _, ok := f.Properties["id"]; !ok {
			t.Fatalf("id missing: %#v", f.Properties)
		}
	}
}

func TestLayerEntryLoadsArtifactIndex(t *testing.T) {
	s, _ := newTestServer(t)
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/derived/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[key] = b
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			b, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	defer srv.Close()
	store, err := artifact.New(srv.URL, "derived")
	if err != nil {
		t.Fatal(err)
	}
	idx := &tiles.Index{
		SchemaVersion: tiles.IndexSchemaVersion, CatalogVersion: "v1", Collection: "places",
		Bounds: [4]float64{0, 0, 10, 10},
		Fields: []tiles.Field{{Name: "marker", Type: "string"}},
		Partitions: []tiles.PartitionIndex{{
			Asset: "data.parquet", Bounds: [4]float64{0, 0, 10, 10},
			Levels: []tiles.LevelIndex{{Level: 0, RowGroups: []tiles.RowGroupIndex{{Index: 0, Bounds: [4]float64{0, 0, 10, 10}}}}},
		}},
	}
	sSaveLayerIndex(store, "index/v1/v1/places.json.gz", idx)
	entry := s.states["default"].layers["places"]
	entry.artifacts = store
	entry.indexKey = "index/v1/v1/places.json.gz"
	entry.catalogVersion = "v1"
	layer, err := entry.get()
	if err != nil {
		t.Fatal(err)
	}
	fields := layer.Fields()
	if len(fields) != 1 || fields[0].Name != "marker" {
		t.Fatalf("fields = %#v", fields)
	}
}
