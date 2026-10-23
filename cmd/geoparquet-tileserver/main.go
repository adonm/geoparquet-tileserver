package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/paulmach/orb"
	"golang.org/x/sync/singleflight"

	"github.com/adonm/geoparquet-tileserver/internal/artifact"
	"github.com/adonm/geoparquet-tileserver/internal/catalog"
	"github.com/adonm/geoparquet-tileserver/internal/config"
	filt "github.com/adonm/geoparquet-tileserver/internal/filter"
	"github.com/adonm/geoparquet-tileserver/internal/tilecache"
	"github.com/adonm/geoparquet-tileserver/internal/tiles"
)

type layerEntry struct {
	name       string
	title      string
	kind       string
	collection *catalog.Collection
	group      *catalog.Group
	opts       tiles.Options

	once  sync.Once
	layer *tiles.Layer
	err   error

	artifacts      *artifact.Store
	indexKey       string
	catalogVersion string
}

func (e *layerEntry) get() (*tiles.Layer, error) {
	e.once.Do(func() {
		var items []catalog.Item
		var err error
		switch {
		case e.collection != nil:
			items, err = e.collection.Items()
		case e.group != nil:
			items, err = e.group.Items()
		default:
			err = fmt.Errorf("layer %q has no collection or group", e.name)
		}
		if err != nil {
			e.err = err
			return
		}
		e.layer, e.err = tiles.New(e.opts, items)
		if e.err == nil && e.artifacts != nil && e.indexKey != "" {
			if idx, ok := sLoadLayerIndex(e.artifacts, e.indexKey); ok {
				_ = e.layer.ApplyIndex(idx)
				slog.Info("layer index loaded", "collection", e.name, "key", e.indexKey)
			} else {
				go e.saveIndex()
			}
		}
	})
	return e.layer, e.err
}

func (e *layerEntry) saveIndex() {
	if e.layer == nil || e.artifacts == nil || e.indexKey == "" {
		return
	}
	idx, err := e.layer.BuildIndex(e.catalogVersion)
	if err != nil {
		return
	}
	_ = e.layer.ApplyIndex(idx)
	sSaveLayerIndex(e.artifacts, e.indexKey, idx)
	slog.Info("layer index saved", "collection", e.name, "key", e.indexKey)
}

func (e *layerEntry) bounds() orb.Bound {
	if e.layer != nil {
		return e.layer.Bounds
	}
	if e.collection != nil {
		return e.collection.Bound()
	}
	if e.group != nil {
		return e.group.Bound()
	}
	return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
}

func (e *layerEntry) minZoom() int { return e.opts.MinZoom }
func (e *layerEntry) maxZoom() int { return e.opts.MaxZoom }

// tileCacheSchema is bumped whenever the MVT rendering or cache-key semantics
// change, so encodings from an older build cannot be served.
const tileCacheSchema = "tile-v2"

// catalogVersion fingerprints the resolved catalog structure and its asset
// bases. For versioned STAC roots this changes when the selected release
// changes, which keeps tiles from different releases in separate cache keys.
func catalogVersion(source string, cat *catalog.Catalog) string {
	h := sha256.New()
	fmt.Fprintf(h, "source\x00%s\x00", source)
	fmt.Fprintf(h, "catalog\x00%s\x00%s\x00", cat.ID, cat.Base)
	for _, col := range cat.Collections {
		fmt.Fprintf(h, "collection\x00%s\x00%s\x00", col.ID, col.Base)
		names := make([]string, 0, len(col.Assets))
		for name := range col.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(h, "asset\x00%s\x00%s\x00", name, col.Assets[name].Href)
		}
	}
	for _, g := range cat.Groups {
		fmt.Fprintf(h, "group\x00%s\x00%s\x00%s\x00", g.ID, g.Base, g.PMTiles)
		for _, col := range g.Collections {
			fmt.Fprintf(h, "member\x00%s\x00%s\x00", col.ID, col.Base)
		}
	}
	sum := h.Sum(nil)
	return fmt.Sprintf("%x", sum[:8])
}

func tileCacheKey(st *catalogState, layer string, z uint8, x, y uint32, minCount int, filterLang, filter string) string {
	if filterLang == "" {
		filterLang = "cql2-text"
	}
	filterHash := sha256.Sum256([]byte(filterLang + "\x00" + normalizeFilter(filter)))
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d|%d|%d|%x",
		tileCacheSchema, st.name, st.version, layer, z, x, y, minCount, filterHash[:8])
}

// normalizeFilter collapses insignificant whitespace outside quoted strings so
// equivalent CQL2 text expressions share a cache entry without changing
// literal values.
func normalizeFilter(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	pendingSpace := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			if inQuote && i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				b.WriteByte('\'')
				i++
				pendingSpace = false
				continue
			}
			inQuote = !inQuote
		}
		if !inQuote {
			switch ch {
			case ' ', '\t', '\n', '\r':
				pendingSpace = true
				continue
			}
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteByte(ch)
	}
	return b.String()
}

type catalogState struct {
	name    string
	cat     *catalog.Catalog
	base    string
	version string
	layers  map[string]*layerEntry
	order   []string

	itemMu sync.Mutex
	items  map[string]*itemSet
}

type server struct {
	cfg        *config.Config
	states     map[string]*catalogState
	catalogURL map[string]string
	cache      *tilecache.Cache
	group      singleflight.Group
	artifacts  *artifact.Store
}

func (s *server) state(name string) *catalogState {
	if name == "" || name == "default" {
		return s.states["default"]
	}
	if st, ok := s.states[name]; ok {
		return st
	}
	if alias, ok := s.catalogURL[name]; ok {
		return s.states[alias]
	}
	return nil
}

func (s *server) catalogNames() []string {
	names := make([]string, 0, len(s.states))
	for name := range s.states {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "default" {
			return true
		}
		if names[j] == "default" {
			return false
		}
		return names[i] < names[j]
	})
	return names
}

type TileInput struct {
	Collection  string `path:"collection" doc:"Collection / layer id"`
	Z           uint8  `path:"z" doc:"Zoom level"`
	X           uint32 `path:"x" doc:"Tile column"`
	Y           string `path:"y" doc:"Tile row, optionally with .mvt suffix"`
	MinCount    int    `query:"min_count" default:"0" doc:"Only include features with count >= this value"`
	Filter      string `query:"filter" doc:"OGC API Features CQL2 filter expression"`
	FilterLang  string `query:"filter-lang" default:"cql2-text" doc:"Filter language"`
	Host        string `header:"Host"`
	Proto       string `header:"X-Forwarded-Proto"`
	Catalog     string `query:"catalog" doc:"Named catalog alias from config"`
	DebugTiming string `header:"X-Debug-Timing" doc:"Return render phase timings in response headers"`
	IfNoneMatch string `header:"If-None-Match" doc:"Conditional tile request"`
	AcceptEnc   string `header:"Accept-Encoding" doc:"Accepted response encodings"`
}

type TileOutput struct {
	ContentType     string `header:"Content-Type"`
	ContentEncoding string `header:"Content-Encoding"`
	CacheControl    string `header:"Cache-Control"`
	ETag            string `header:"ETag"`
	Vary            string `header:"Vary"`
	XCache          string `header:"X-Cache"`
	XQueryMS        string `header:"X-Query-Ms"`
	XOpenMS         string `header:"X-Open-Ms"`
	XBoundsMS       string `header:"X-Bounds-Ms"`
	XScanMS         string `header:"X-Scan-Ms"`
	XEncodeMS       string `header:"X-Encode-Ms"`
	XFeatureCount   string `header:"X-Features"`
	Body            []byte
}

type renderResult struct {
	body  []byte
	stats tiles.RenderStats
}

type TileJSONOutput struct {
	Body *TileJSON
}

type TileJSON struct {
	TileJSON     string        `json:"tilejson"`
	Name         string        `json:"name"`
	Tiles        []string      `json:"tiles"`
	MinZoom      int           `json:"minzoom"`
	MaxZoom      int           `json:"maxzoom"`
	Bounds       []float64     `json:"bounds"`
	Center       []float64     `json:"center,omitempty"`
	Scheme       string        `json:"scheme,omitempty"`
	Attribution  string        `json:"attribution,omitempty"`
	VectorLayers []VectorLayer `json:"vector_layers"`
}

type VectorLayer struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

type CollectionInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Kind        string    `json:"kind"`
	MinZoom     int       `json:"minzoom"`
	MaxZoom     int       `json:"maxzoom"`
	Bounds      []float64 `json:"bounds"`
}

type CollectionsOutput struct {
	Body []CollectionInfo
}

type CollectionDetail struct {
	ID          string            `json:"id"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Kind        string            `json:"kind"`
	MinZoom     int               `json:"minzoom"`
	MaxZoom     int               `json:"maxzoom"`
	Bounds      []float64         `json:"bounds"`
	TileJSON    string            `json:"tilejson"`
	Tiles       string            `json:"tiles"`
	Items       string            `json:"items"`
	STAC        string            `json:"stac"`
	Assets      map[string]string `json:"assets,omitempty"`
}

type CollectionOutput struct {
	Body CollectionDetail
}

type ItemInfo struct {
	ID     string            `json:"id"`
	BBox   []float64         `json:"bbox,omitempty"`
	STAC   string            `json:"stac"`
	Assets map[string]string `json:"assets,omitempty"`
}

type ItemsOutput struct {
	Body []ItemInfo
}

type HealthOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

func (s *server) entry(catalogName, collection string) (*catalogState, *layerEntry, error) {
	st := s.state(catalogName)
	if st == nil {
		return nil, nil, fmt.Errorf("unknown catalog: %s", catalogName)
	}
	entry, ok := st.layers[collection]
	if !ok {
		return st, nil, fmt.Errorf("unknown collection: %s", collection)
	}
	return st, entry, nil
}

func (s *server) tile(ctx context.Context, in *TileInput) (*TileOutput, error) {
	st, entry, err := s.entry(in.Catalog, in.Collection)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown catalog") {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, huma.Error404NotFound(err.Error())
	}
	if int(in.Z) < entry.minZoom() || int(in.Z) > entry.maxZoom() {
		return nil, huma.Error404NotFound("zoom out of range")
	}
	ys := strings.TrimSuffix(in.Y, ".mvt")
	y, err := strconv.ParseUint(ys, 10, 32)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid y")
	}
	minVal := in.MinCount
	var minPtr *int
	if minVal > 0 {
		minPtr = &minVal
	}
	var flt *filt.Filter
	if in.Filter != "" {
		if in.FilterLang != "" && in.FilterLang != "cql2-text" && in.FilterLang != "cql2-json" {
			return nil, huma.Error400BadRequest("unsupported filter-lang: " + in.FilterLang)
		}
		flt, err = filt.Compile(in.Filter)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid filter: " + err.Error())
		}
	}
	key := tileCacheKey(st, entry.name, in.Z, in.X, uint32(y), minVal, in.FilterLang, in.Filter)
	gzipOK := acceptsGzip(in.AcceptEnc)
	etag := tileETag(key, gzipOK)
	if in.IfNoneMatch != "" && etagMatches(in.IfNoneMatch, etag) {
		h := http.Header{}
		h.Set("ETag", etag)
		h.Set("Vary", "Accept-Encoding")
		return nil, huma.ErrorWithHeaders(huma.Status304NotModified(), h)
	}
	debug := in.DebugTiming != ""
	if !debug {
		if b, ok := s.cache.Get(key); ok {
			return tileResponse(b, "memory", gzipOK, etag, nil), nil
		}
	}
	v, err, _ := s.group.Do(key, func() (any, error) {
		layer, err := entry.get()
		if err != nil {
			return nil, err
		}
		if debug {
			b, stats, err := layer.RenderDetailed(ctx, in.Z, in.X, uint32(y), minPtr, flt)
			return renderResult{body: b, stats: stats}, err
		}
		b, err := layer.Render(ctx, in.Z, in.X, uint32(y), minPtr, flt)
		return renderResult{body: b}, err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	res := v.(renderResult)
	b := res.body
	if !debug {
		s.cache.Put(key, b)
	}
	var stats *tiles.RenderStats
	if debug {
		st := res.stats
		stats = &st
	}
	return tileResponse(b, "miss", gzipOK, etag, stats), nil
}

func tileResponse(b []byte, cache string, gzipOK bool, etag string, stats *tiles.RenderStats) *TileOutput {
	out := &TileOutput{
		ContentType:  "application/vnd.mapbox-vector-tile",
		CacheControl: "public, max-age=3600",
		ETag:         etag,
		Vary:         "Accept-Encoding",
		XCache:       cache,
		Body:         b,
	}
	if gzipOK {
		out.Body = gzipBytes(b)
		out.ContentEncoding = "gzip"
	}
	if stats != nil {
		out.XQueryMS = strconv.FormatFloat(stats.QueryMS(), 'f', 3, 64)
		out.XOpenMS = strconv.FormatFloat(stats.OpenMS, 'f', 3, 64)
		out.XBoundsMS = strconv.FormatFloat(stats.BoundsMS, 'f', 3, 64)
		out.XScanMS = strconv.FormatFloat(stats.ScanMS, 'f', 3, 64)
		out.XEncodeMS = strconv.FormatFloat(stats.EncodeMS, 'f', 3, 64)
		out.XFeatureCount = strconv.Itoa(stats.Features)
	}
	return out
}

func tileETag(key string, gzipOK bool) string {
	if gzipOK {
		key += "|gzip"
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("\"%x\"", sum[:8])
}

func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "gzip") {
			return !strings.Contains(part, "q=0")
		}
	}
	return false
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func (s *server) tileJSON(ctx context.Context, in *struct {
	Collection  string `path:"collection"`
	Host        string `header:"Host"`
	Proto       string `header:"X-Forwarded-Proto"`
	Catalog     string `query:"catalog" doc:"Named catalog alias from config"`
	DebugTiming string `header:"X-Debug-Timing" doc:"Return render phase timings in response headers"`
}) (*TileJSONOutput, error) {
	_, entry, err := s.entry(in.Catalog, in.Collection)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	b := entry.bounds()
	tileURL := "/collections/" + url.PathEscape(entry.name) + "/tiles/{z}/{x}/{y}.mvt"
	if base := strings.TrimRight(s.cfg.BaseURL, "/"); base != "" {
		tileURL = base + tileURL
	}
	if in.Catalog != "" {
		tileURL += "?catalog=" + url.QueryEscape(in.Catalog)
	}
	fields := map[string]string{}
	if layer, err := entry.get(); err == nil {
		for _, f := range layer.Fields() {
			if !f.Geometry {
				fields[f.Name] = f.Type
			}
		}
	}
	return &TileJSONOutput{Body: &TileJSON{
		TileJSON: "3.0.0",
		Name:     entry.name,
		Tiles:    []string{tileURL},
		MinZoom:  entry.minZoom(),
		MaxZoom:  entry.maxZoom(),
		Bounds:   []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()},
		Center:   []float64{(b.Min.X() + b.Max.X()) / 2, (b.Min.Y() + b.Max.Y()) / 2, float64(entry.minZoom())},
		Scheme:   "xyz",
		VectorLayers: []VectorLayer{{
			ID:     entry.name,
			Fields: fields,
		}},
	}}, nil
}

func (s *server) collections(_ context.Context, in *struct {
	Catalog string `query:"catalog"`
}) (*CollectionsOutput, error) {
	st := s.state(in.Catalog)
	if st == nil {
		return nil, huma.Error404NotFound("unknown catalog: " + in.Catalog)
	}
	out := &CollectionsOutput{}
	for _, name := range st.order {
		e := st.layers[name]
		b := e.bounds()
		kind := e.kind
		out.Body = append(out.Body, CollectionInfo{
			ID: e.name, Name: e.name, Title: e.title, Description: e.description(),
			Kind: kind, MinZoom: e.minZoom(), MaxZoom: e.maxZoom(),
			Bounds: []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()},
		})
	}
	return out, nil
}

func (e *layerEntry) description() string {
	if e.collection != nil {
		return e.collection.Description
	}
	if e.group != nil {
		return e.group.Description
	}
	return ""
}

func (s *server) collection(_ context.Context, in *struct {
	Collection string `path:"collection"`
	Catalog    string `query:"catalog"`
}) (*CollectionOutput, error) {
	_, entry, err := s.entry(in.Catalog, in.Collection)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	b := entry.bounds()
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	q := ""
	if in.Catalog != "" {
		q = "?catalog=" + url.QueryEscape(in.Catalog)
	}
	tileURL := base + "/collections/" + url.PathEscape(entry.name) + "/tiles/{z}/{x}/{y}.mvt" + q
	assets := map[string]string{}
	if entry.collection != nil {
		for name := range entry.collection.Assets {
			assets[name] = base + "/collections/" + url.PathEscape(entry.name) + "/assets/" + url.PathEscape(name) + q
		}
	}
	detail := CollectionDetail{
		ID: entry.name, Title: entry.title, Description: entry.description(), Kind: entry.kind,
		MinZoom: entry.minZoom(), MaxZoom: entry.maxZoom(),
		Bounds:   []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()},
		TileJSON: base + "/collections/" + url.PathEscape(entry.name) + "/tilejson.json" + q,
		Tiles:    tileURL,
	}
	if entry.collection != nil {
		detail.Items = base + "/collections/" + url.PathEscape(entry.name) + "/items" + q
		detail.Assets = assets
	}
	if entry.collection != nil && entry.collection.Base != "" {
		detail.STAC = base + "/collections/" + url.PathEscape(entry.name) + "/stac" + q
	}
	if entry.group != nil && entry.group.Base != "" {
		detail.STAC = base + "/collections/" + url.PathEscape(entry.name) + "/stac" + q
	}
	return &CollectionOutput{Body: detail}, nil
}

func (s *server) items(_ context.Context, in *struct {
	Collection string `path:"collection"`
	Catalog    string `query:"catalog"`
}) (*ItemsOutput, error) {
	_, entry, err := s.entry(in.Catalog, in.Collection)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	if entry.collection == nil {
		return nil, huma.Error400BadRequest("item listing is only available for STAC collections, not groups")
	}
	items, err := entry.collection.Items()
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	q := ""
	if in.Catalog != "" {
		q = "?catalog=" + url.QueryEscape(in.Catalog)
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	out := &ItemsOutput{}
	for _, it := range items {
		info := ItemInfo{ID: it.ID, BBox: it.BBox}
		info.STAC = base + "/collections/" + url.PathEscape(entry.name) + "/items/" + url.PathEscape(it.ID) + q
		if len(it.Assets) > 0 {
			info.Assets = map[string]string{}
			for name := range it.Assets {
				info.Assets[name] = base + "/collections/" + url.PathEscape(entry.name) + "/items/" + url.PathEscape(it.ID) + "/assets/" + url.PathEscape(name) + q
			}
		}
		out.Body = append(out.Body, info)
	}
	return out, nil
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, If-None-Match, If-Modified-Since, Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, ETag, Last-Modified")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func catalogAliasMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, api := range []string{"/stac-api", "/ogc"} {
			prefix := api + "/catalogs/"
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, prefix)
			catalogName, tail, _ := strings.Cut(rest, "/")
			if catalogName == "" {
				break
			}
			if decoded, err := url.PathUnescape(catalogName); err == nil {
				catalogName = decoded
			}
			path := api
			if tail != "" {
				path += "/" + tail
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = path
			q := r2.URL.Query()
			q.Set("catalog", catalogName)
			r2.URL.RawQuery = q.Encode()
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func htmlRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Accept"), "text/html") {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		target := ""
		switch {
		case p == "/collections":
			target = "/collections.html"
		case strings.HasPrefix(p, "/collections/") && strings.HasSuffix(p, "/items"):
			target = strings.TrimSuffix(p, "/items") + "/items.html"
		case strings.HasPrefix(p, "/collections/") && !strings.HasSuffix(p, ".html"):
			rest := strings.TrimPrefix(p, "/collections/")
			if rest != "" && !strings.Contains(rest, "/") {
				target = "/collections/" + rest + ".html"
			}
		}
		if target == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}

func main() {
	var configPath string
	var listen string
	var catalogOverride string
	flag.StringVar(&configPath, "config", "config.yaml", "path to config.yaml")
	flag.StringVar(&listen, "listen", "", "override listen address")
	flag.StringVar(&catalogOverride, "catalog", "", "override STAC catalog URL or local path")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	if listen != "" {
		cfg.Listen = listen
	}
	if catalogOverride != "" {
		cfg.Catalog = catalogOverride
	}
	globalMaxFeatures = cfg.MaxFeatures

	var artifacts *artifact.Store
	if cfg.ObjectStore != nil {
		artifacts, err = artifact.New(cfg.ObjectStore.URL, cfg.ObjectStore.ArtifactsBucket)
		if err != nil {
			slog.Error("object store", "err", err)
			os.Exit(1)
		}
		routes := make([]tiles.ProxyRoute, 0, len(cfg.ObjectStore.Routes))
		for _, r := range cfg.ObjectStore.Routes {
			routes = append(routes, tiles.ProxyRoute{Prefix: r.Prefix, Bucket: r.Bucket})
		}
		tiles.ConfigureSourceProxy(cfg.ObjectStore.URL, routes)
		slog.Info("object store configured", "url", cfg.ObjectStore.URL, "artifacts", cfg.ObjectStore.ArtifactsBucket, "proxy_routes", len(routes))
	}
	s := &server{
		cfg:        cfg,
		states:     map[string]*catalogState{},
		catalogURL: map[string]string{},
		cache:      tilecache.New(int64(cfg.CacheTilesMB) << 20),
		artifacts:  artifacts,
	}
	build := func(name, catalogURL string) error {
		cat, err := catalog.Load(catalogURL)
		if err != nil {
			return err
		}
		st := &catalogState{
			name:    name,
			cat:     cat,
			base:    catalogURL,
			version: catalogVersion(catalogURL, cat),
			layers:  map[string]*layerEntry{},
		}
		if len(cfg.Layers) > 0 {
			for _, lc := range cfg.Layers {
				col := cat.Collection(lc.Collection)
				grp := cat.Group(lc.Collection)
				if lc.Collection == "" {
					// Empty collection means "all items in this catalog".
					if len(cat.Collections) == 1 {
						col = cat.Collections[0]
					} else {
						grp = &catalog.Group{ID: lc.Name, Title: lc.Name, Collections: cat.Collections}
					}
				}
				if col == nil && grp == nil {
					return fmt.Errorf("layer %q: collection or group %q not found", lc.Name, lc.Collection)
				}
				e := &layerEntry{
					name: lc.Name, kind: "collection", collection: col,
					opts: tiles.Options{Name: lc.Name, Collection: lc.Collection, GeometryColumn: lc.GeometryColumn, MinZoom: lc.MinZoom, MaxZoom: lc.MaxZoom, Preload: cfg.Preload},
				}
				if col != nil {
					e.title = col.Title
				} else {
					e.kind = "group"
					e.group = grp
					e.title = grp.Title
				}
				st.layers[lc.Name] = e
				st.order = append(st.order, lc.Name)
			}
		} else {
			for _, col := range cat.Collections {
				if _, exists := st.layers[col.ID]; exists {
					continue
				}
				st.layers[col.ID] = &layerEntry{
					name: col.ID, title: col.Title, kind: "collection", collection: col,
					opts: tiles.Options{Name: col.ID, Collection: col.ID, MinZoom: 0, MaxZoom: 14, Preload: cfg.Preload},
				}
				st.order = append(st.order, col.ID)
			}
			for _, group := range cat.Groups {
				if _, exists := st.layers[group.ID]; exists {
					continue
				}
				st.layers[group.ID] = &layerEntry{
					name: group.ID, title: group.Title, kind: "group", group: group,
					opts: tiles.Options{Name: group.ID, MinZoom: 0, MaxZoom: 14, Preload: false},
				}
				st.order = append(st.order, group.ID)
			}
		}
		for _, e := range st.layers {
			e.opts.ExcludeProps = cfg.ExcludeProps
			e.opts.IDColumn = cfg.IDColumn
			e.opts.Simplify = cfg.Simplify
			e.artifacts = s.artifacts
			e.catalogVersion = st.version
			e.indexKey = "index/v1/" + st.version + "/" + e.name + ".json.gz"
		}
		if len(st.layers) == 0 {
			return fmt.Errorf("catalog %q has no usable collections", catalogURL)
		}
		if cfg.Preload {
			for _, e := range st.layers {
				if _, err := e.get(); err != nil {
					return fmt.Errorf("preload %q: %w", e.name, err)
				}
			}
		}
		s.states[name] = st
		s.catalogURL[catalogURL] = name
		return nil
	}
	if err := build("default", cfg.Catalog); err != nil {
		slog.Error("catalog", "url", cfg.Catalog, "err", err)
		os.Exit(1)
	}
	for name, catalogURL := range cfg.Catalogs {
		if err := build(name, catalogURL); err != nil {
			slog.Error("catalog alias", "name", name, "url", catalogURL, "err", err)
			os.Exit(1)
		}
	}

	router := chi.NewMux()
	router.Use(cors)
	router.Use(htmlRedirect)
	router.Use(catalogAliasMiddleware)
	s.registerSTACAPI(router)
	s.registerOGCAPI(router)
	router.Get("/", s.indexHTML)
	router.Get("/stac", s.catalogSTAC)
	router.Get("/index.html", s.indexHTML)
	router.Get("/collections.html", s.collectionsHTML)
	router.Get("/collections/{collection}.html", s.collectionHTML)
	router.Get("/collections/{collection}/items.html", s.itemsHTML)
	router.Get("/cache/stats", s.cacheStats)
	router.Get("/collections/{collection}/style.json", s.collectionStyle)
	router.Get("/collections/{collection}/stac", s.collectionSTAC)
	router.Get("/collections/{collection}/assets/{asset}", s.collectionAsset)
	router.Get("/collections/{collection}/assets/{asset}.parquet", s.collectionAsset)
	router.Get("/collections/{collection}/assets/{asset}.pmtiles", s.collectionAsset)
	router.Get("/collections/{collection}/pmtiles.pmtiles", s.groupPMTiles)
	router.Get("/collections/{collection}/items/{item}", s.itemSTAC)
	router.Get("/collections/{collection}/items/{item}/assets/{asset}", s.itemAsset)
	router.Get("/collections/{collection}/items/{item}/assets/{asset}.parquet", s.itemAsset)
	router.Get("/collections/{collection}/items/{item}/assets/{asset}.pmtiles", s.itemAsset)
	router.Get("/catalogs/{catalog}/stac", s.catalogSTAC)
	router.Get("/catalogs/{catalog}/collections/{collection}/stac", s.collectionSTAC)
	router.Get("/catalogs/{catalog}/collections/{collection}/style.json", s.collectionStyle)
	router.Get("/catalogs/{catalog}/collections/{collection}/assets/{asset}", s.collectionAsset)
	router.Get("/catalogs/{catalog}/collections/{collection}/assets/{asset}.parquet", s.collectionAsset)
	router.Get("/catalogs/{catalog}/collections/{collection}/assets/{asset}.pmtiles", s.collectionAsset)
	router.Get("/catalogs/{catalog}/collections/{collection}/pmtiles.pmtiles", s.groupPMTiles)
	router.Get("/catalogs/{catalog}/collections/{collection}/items/{item}", s.itemSTAC)
	router.Get("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}", s.itemAsset)
	router.Get("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}.parquet", s.itemAsset)
	router.Get("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}.pmtiles", s.itemAsset)
	router.Head("/stac", s.catalogSTAC)
	router.Head("/collections/{collection}/stac", s.collectionSTAC)
	router.Head("/collections/{collection}/style.json", s.collectionStyle)
	router.Head("/collections/{collection}/assets/{asset}", s.collectionAsset)
	router.Head("/collections/{collection}/assets/{asset}.parquet", s.collectionAsset)
	router.Head("/collections/{collection}/assets/{asset}.pmtiles", s.collectionAsset)
	router.Head("/collections/{collection}/pmtiles.pmtiles", s.groupPMTiles)
	router.Head("/collections/{collection}/items/{item}", s.itemSTAC)
	router.Head("/collections/{collection}/items/{item}/assets/{asset}", s.itemAsset)
	router.Head("/collections/{collection}/items/{item}/assets/{asset}.parquet", s.itemAsset)
	router.Head("/collections/{collection}/items/{item}/assets/{asset}.pmtiles", s.itemAsset)
	router.Head("/catalogs/{catalog}/stac", s.catalogSTAC)
	router.Head("/catalogs/{catalog}/collections/{collection}/stac", s.collectionSTAC)
	router.Head("/catalogs/{catalog}/collections/{collection}/style.json", s.collectionStyle)
	router.Head("/catalogs/{catalog}/collections/{collection}/assets/{asset}", s.collectionAsset)
	router.Head("/catalogs/{catalog}/collections/{collection}/assets/{asset}.parquet", s.collectionAsset)
	router.Head("/catalogs/{catalog}/collections/{collection}/assets/{asset}.pmtiles", s.collectionAsset)
	router.Head("/catalogs/{catalog}/collections/{collection}/pmtiles.pmtiles", s.groupPMTiles)
	router.Head("/catalogs/{catalog}/collections/{collection}/items/{item}", s.itemSTAC)
	router.Head("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}", s.itemAsset)
	router.Head("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}.parquet", s.itemAsset)
	router.Head("/catalogs/{catalog}/collections/{collection}/items/{item}/assets/{asset}.pmtiles", s.itemAsset)

	humaCfg := huma.DefaultConfig("GeoParquet Tile Server", "0.1.0")
	humaCfg.Info.Description = "A small STAC + GeoParquet vector tile server with browseable HTML views."
	api := humachi.New(router, humaCfg)

	huma.Register(api, huma.Operation{
		OperationID: "get-tile", Method: http.MethodGet,
		Path:    "/collections/{collection}/tiles/{z}/{x}/{y}",
		Summary: "Get an MVT tile",
		Responses: map[string]*huma.Response{
			"200": {Description: "MVT tile", Content: map[string]*huma.MediaType{
				"application/vnd.mapbox-vector-tile": {},
			}},
		},
	}, s.tile)

	huma.Register(api, huma.Operation{
		OperationID: "get-tilejson", Method: http.MethodGet,
		Path: "/collections/{collection}/tilejson.json", Summary: "Get TileJSON",
	}, s.tileJSON)

	huma.Register(api, huma.Operation{
		OperationID: "list-collections", Method: http.MethodGet,
		Path: "/collections", Summary: "List collections and virtual groups",
	}, s.collections)

	huma.Register(api, huma.Operation{
		OperationID: "get-collection", Method: http.MethodGet,
		Path: "/collections/{collection}", Summary: "Get collection metadata",
	}, s.collection)

	huma.Register(api, huma.Operation{
		OperationID: "list-items", Method: http.MethodGet,
		Path: "/collections/{collection}/items", Summary: "List STAC items and passthrough assets",
	}, s.items)

	huma.Register(api, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/healthz", Summary: "Health",
	}, func(context.Context, *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", cfg.Listen, "catalogs", len(s.states), "tile_cache_mb", cfg.CacheTilesMB)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func catalogFromRequest(r *http.Request) string {
	if c := chi.URLParam(r, "catalog"); c != "" {
		return c
	}
	return r.URL.Query().Get("catalog")
}

func (s *server) cacheStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.cache.Stats())
}

func contentRangeTotal(v string) (int64, bool) {
	i := strings.LastIndex(v, "/")
	if i < 0 || i+1 >= len(v) {
		return 0, false
	}
	n, err := strconv.ParseInt(v[i+1:], 10, 64)
	return n, err == nil && n > 0
}

// proxyAsset streams a local or remote STAC asset. Remote range headers are
// forwarded so Parquet consumers can keep using HTTP range reads.
func (s *server) serveAsset(w http.ResponseWriter, r *http.Request, href, contentType string) {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		href = tiles.ProxyURL(href)
		method := r.Method
		if method == "" {
			method = http.MethodGet
		}
		headRequest := method == http.MethodHead
		// Some CDNs (notably the Overture PMTiles bucket) reject HEAD and
		// open-ended ranges. A one-byte GET gives us the total length for a
		// standards-compliant HEAD response.
		req, err := http.NewRequestWithContext(r.Context(), method, href, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if headRequest {
			req.Method = http.MethodGet
			req.Header.Set("Range", "bytes=0-0")
		} else {
			for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since"} {
				if v := r.Header.Get(h); v != "" {
					req.Header.Set(h, v)
				}
			}
			// Overture's PMTiles CDN rejects full GETs. Tools such as
			// terrio/0.4 fetch the PMTiles header with a plain GET, so serve
			// a bounded prefix when there is no Range header.
			if req.Header.Get("Range") == "" && strings.HasSuffix(strings.Split(href, "?")[0], ".pmtiles") {
				req.Header.Set("Range", "bytes=0-16777215")
			}
		}
		req.Header.Set("User-Agent", "geoparquet-tileserver/0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		if contentType != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if headRequest {
			if total, ok := contentRangeTotal(resp.Header.Get("Content-Range")); ok {
				w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			} else if n := resp.ContentLength; n > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	f, err := os.Open(href)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, filepath.Base(href), st.ModTime(), f)
}

func (s *server) collectionStyle(w http.ResponseWriter, r *http.Request) {
	catalogName := catalogFromRequest(r)
	_, entry, err := s.entry(catalogName, chi.URLParam(r, "collection"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	id := url.PathEscape(entry.name)
	params := url.Values{}
	if catalogName != "" && catalogName != "default" {
		params.Set("catalog", catalogName)
	}
	if filter := r.URL.Query().Get("filter"); filter != "" {
		params.Set("filter", filter)
		if lang := r.URL.Query().Get("filter-lang"); lang != "" {
			params.Set("filter-lang", lang)
		}
	}
	if min := r.URL.Query().Get("min_count"); min != "" {
		params.Set("min_count", min)
	}
	tileURL := base + "/collections/" + id + "/tiles/{z}/{x}/{y}.mvt"
	if len(params) > 0 {
		tileURL += "?" + params.Encode()
	}
	b := entry.bounds()
	span := math.Max(b.Max.X()-b.Min.X(), b.Max.Y()-b.Min.Y())
	zoom := 4
	if span > 0 {
		zoom = int(math.Log2(360/span)) + 2
	}
	if zoom < 0 {
		zoom = 0
	}
	if zoom > 14 {
		zoom = 14
	}
	lon := (b.Min.X() + b.Max.X()) / 2
	lat := (b.Min.Y() + b.Max.Y()) / 2

	style := map[string]any{
		"version": 8,
		"name":    entry.title,
		"center":  []float64{lon, lat},
		"zoom":    zoom,
		"sources": map[string]any{
			"tiles": map[string]any{
				"type":    "vector",
				"tiles":   []string{tileURL},
				"minzoom": entry.minZoom(),
				"maxzoom": entry.maxZoom(),
				"bounds":  []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()},
			},
		},
		"layers": []any{
			map[string]any{"id": "background", "type": "background", "paint": map[string]any{"background-color": "#f4f1ea"}},
			map[string]any{"id": "fill", "type": "fill", "source": "tiles", "source-layer": entry.name,
				"paint": map[string]any{"fill-color": "#e4783d", "fill-opacity": 0.38, "fill-outline-color": "#8a4a1f"}},
			map[string]any{"id": "line", "type": "line", "source": "tiles", "source-layer": entry.name,
				"paint": map[string]any{"line-color": "#7a3c18", "line-width": 0.8}},
			map[string]any{"id": "point", "type": "circle", "source": "tiles", "source-layer": entry.name,
				"paint": map[string]any{"circle-color": "#d95f02", "circle-radius": 3, "circle-opacity": 0.85}},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	body, err := json.Marshal(style)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := fmt.Sprintf("\"%x\"", sum[:8])
	w.Header().Set("ETag", etag)
	if in := r.Header.Get("If-None-Match"); in != "" && etagMatches(in, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func (s *server) catalogSTAC(w http.ResponseWriter, r *http.Request) {
	st := s.state(catalogFromRequest(r))
	if st == nil || st.base == "" {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, st.base, "application/json")
}

func (s *server) groupPMTiles(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil || entry.group == nil || entry.group.PMTiles == "" {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, entry.group.PMTiles, "application/vnd.pmtiles")
}

func (s *server) collectionSTAC(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	href := ""
	if entry.collection != nil {
		href = entry.collection.Base
	} else if entry.group != nil {
		href = entry.group.Base
	}
	if href == "" {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, href, "application/json")
}

func (s *server) collectionAsset(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil || entry.collection == nil {
		http.NotFound(w, r)
		return
	}
	asset, ok := entry.collection.Assets[chi.URLParam(r, "asset")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, catalog.Resolve(entry.collection.Base, asset.Href), asset.Type)
}

func (s *server) findItem(entry *layerEntry, id string) (catalog.Item, bool) {
	if entry.collection == nil {
		return catalog.Item{}, false
	}
	items, err := entry.collection.Items()
	if err != nil {
		return catalog.Item{}, false
	}
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return catalog.Item{}, false
}

func (s *server) itemSTAC(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, ok := s.findItem(entry, chi.URLParam(r, "item"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, it.Base, "application/json")
}

func (s *server) itemAsset(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, ok := s.findItem(entry, chi.URLParam(r, "item"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	asset, ok := it.Assets[chi.URLParam(r, "asset")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, catalog.Resolve(it.Base, asset.Href), asset.Type)
}
