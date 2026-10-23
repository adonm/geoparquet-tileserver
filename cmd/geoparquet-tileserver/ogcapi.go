package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"

	filt "github.com/adonm/geoparquet-tileserver/internal/filter"
	"github.com/adonm/geoparquet-tileserver/internal/tiles"
)

var ogcConformance = append(append([]string{}, stacConformance...),
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/core",
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/geojson",
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/oas30",
)

func (s *server) registerOGCAPI(router chi.Router) {
	router.Get("/ogc", s.ogcLanding)
	router.Get("/ogc/", s.ogcLanding)
	router.Get("/ogc/conformance", s.ogcConformance)
	router.Get("/ogc/api", s.apiOpenAPI)
	router.Get("/ogc/openapi.json", s.apiOpenAPI)
	router.Get("/ogc/collections", s.ogcCollections)
	router.Get("/ogc/collections/{collection}", s.ogcCollection)
	router.Get("/ogc/collections/{collection}/items", s.ogcItems)
	router.Get("/ogc/collections/{collection}/items/{item}", s.ogcItem)
	router.Get("/ogc/collections/{collection}/queryables", s.ogcQueryables)
	router.Get("/ogc/collections/{collection}/schema", s.ogcSchema)
	router.Get("/ogc/collections/{collection}/sortables", s.ogcSortables)
	router.Get("/ogc/collections/{collection}/tiles", s.ogcTilesets)
	router.Get("/ogc/collections/{collection}/tiles/WebMercatorQuad", s.ogcTileset)
	router.Get("/ogc/collections/{collection}/tiles/WebMercatorQuad/{z}/{y}/{x}", s.ogcTile)
	router.Get("/ogc/tileMatrixSets", s.ogcTileMatrixSets)
	router.Get("/ogc/tileMatrixSets/WebMercatorQuad", s.ogcTileMatrixSet)
	router.Options("/ogc/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func (s *server) ogcLanding(w http.ResponseWriter, r *http.Request) {
	st := s.state(catalogFromRequest(r))
	if st == nil {
		writeAPIError(w, http.StatusNotFound, "unknown catalog")
		return
	}
	base := s.apiURL(r, st, "/ogc")
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       firstNonEmptyString(st.cat.Title, "GeoParquet OGC API"),
		"description": firstNonEmptyString(st.cat.Description, "OGC API Features and Tiles backed by GeoParquet"),
		"conformsTo":  ogcConformance,
		"links": []stacLink{
			{Href: base, Rel: "self", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc/conformance"), Rel: "conformance", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc/api"), Rel: "service-desc", Type: "application/vnd.oai.openapi+json;version=3.0"},
			{Href: s.absoluteURL(r, "/docs"), Rel: "service-doc", Type: "text/html"},
			{Href: s.apiURL(r, st, "/ogc/collections"), Rel: "data", Type: "application/json"},
		},
	})
}

func (s *server) ogcConformance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"conformsTo": ogcConformance})
}

func (s *server) ogcCollections(w http.ResponseWriter, r *http.Request) {
	st := s.state(catalogFromRequest(r))
	if st == nil {
		writeAPIError(w, http.StatusNotFound, "unknown catalog")
		return
	}
	q := r.URL.Query()
	limit := 1000
	if rawLimit := q.Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 {
			writeAPIError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > sMaxFeatures() {
		limit = sMaxFeatures()
	}
	offset := parseIntDefault(q.Get("offset"), 0, 0)
	if offset < 0 {
		offset = 0
	}
	var box *orb.Bound
	if raw := q.Get("bbox"); raw != "" {
		b, err := parseBBox(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		box = &orb.Bound{Min: orb.Point{b[0], b[1]}, Max: orb.Point{b[2], b[3]}}
	}
	var tr *timeRange
	if raw := q.Get("datetime"); raw != "" {
		parsed, err := parseTimeRange(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		tr = &parsed
	}
	var flt *filt.Filter
	if raw := q.Get("filter"); raw != "" {
		var err error
		flt, err = filt.Compile(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid filter: "+err.Error())
			return
		}
	}
	terms := strings.Fields(strings.ToLower(q.Get("q")))
	var matched []*layerEntry
	for _, name := range st.order {
		entry := st.layers[name]
		if entry == nil {
			continue
		}
		if box != nil && !boundsOverlap(entry.bounds(), *box) {
			continue
		}
		if tr != nil {
			if start, end, ok := collectionTimes(entry); ok && !tr.matches(start, end, true) {
				continue
			}
		}
		if len(terms) > 0 && !collectionTextMatch(entry, terms) {
			continue
		}
		if flt != nil {
			ok, err := flt.Match(collectionFilterProperties(entry))
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid filter: "+err.Error())
				return
			}
			if !ok {
				continue
			}
		}
		matched = append(matched, entry)
	}
	if specs := parseSortby(q.Get("sortby")); len(specs) > 0 {
		sort.SliceStable(matched, func(i, j int) bool {
			for _, sp := range specs {
				a, b := collectionSortValue(matched[i], sp.Field), collectionSortValue(matched[j], sp.Field)
				if a == b {
					continue
				}
				if sp.Desc {
					return a > b
				}
				return a < b
			}
			return false
		})
	}
	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]any, 0, end-offset)
	for _, entry := range matched[offset:end] {
		out = append(out, s.ogcCollectionJSON(r, st, entry))
	}
	links := []stacLink{
		{Href: s.apiURL(r, st, "/ogc/collections"), Rel: "self", Type: "application/json"},
		{Href: s.apiURL(r, st, "/ogc"), Rel: "root", Type: "application/json"},
	}
	if end < total {
		u := *r.URL
		qq := u.Query()
		qq.Set("offset", strconv.Itoa(end))
		u.RawQuery = qq.Encode()
		links = append(links, stacLink{Href: s.absoluteURL(r, u.RequestURI()), Rel: "next", Type: "application/json"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"collections": out, "links": links,
		"numberMatched": total, "numberReturned": len(out),
	})
}

func (s *server) ogcCollection(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.ogcCollectionJSON(r, st, entry))
}

func (s *server) ogcCollectionJSON(r *http.Request, st *catalogState, entry *layerEntry) map[string]any {
	b := entry.bounds()
	id := url.PathEscape(entry.name)
	return map[string]any{
		"id": entry.name, "title": entry.title, "description": entry.description(),
		"itemType": "feature",
		"crs":      []string{"http://www.opengis.net/def/crs/OGC/1.3/CRS84"},
		"extent": map[string]any{
			"spatial":  map[string]any{"bbox": [][]float64{{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()}}},
			"temporal": map[string]any{"interval": [][]any{{nil, nil}}},
		},
		"links": []stacLink{
			{Href: s.apiURL(r, st, "/ogc/collections/"+id), Rel: "self", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc/collections"), Rel: "parent", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc"), Rel: "root", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/items"), Rel: "items", Type: "application/geo+json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/queryables"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/queryables", Type: "application/schema+json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/schema"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/schema", Type: "application/schema+json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/sortables"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/sortables", Type: "application/schema+json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json"},
		},
	}
}

type ogcItemsParams struct {
	BBox       *orb.Bound
	Datetime   string
	Limit      int
	Offset     int
	Filter     string
	FilterLang string
}

func (s *server) ogcItems(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	p, err := s.parseOGCItemsParams(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	layer, err := entry.get()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	var flt *filt.Filter
	if p.Filter != "" {
		flt, err = filt.Compile(p.Filter)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid filter: "+err.Error())
			return
		}
	}
	bound := orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	if p.BBox != nil {
		bound = *p.BBox
	}
	sortSpecs := parseSortby(r.URL.Query().Get("sortby"))
	var tr *timeRange
	if p.Datetime != "" {
		parsed, _ := parseTimeRange(p.Datetime)
		tr = &parsed
	}
	var features []tiles.Feature
	var matched int
	var complete bool
	if tr != nil || len(sortSpecs) > 0 {
		all, _, scanned, err := layer.Features(r.Context(), bound, s.cfg.MaxFeatures, 0, flt)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		filtered := all[:0]
		for _, f := range all {
			if tr == nil || featureTimeMatches(f.Properties, *tr) {
				filtered = append(filtered, f)
			}
		}
		sortFeatures(filtered, sortSpecs)
		matched = len(filtered)
		complete = scanned && len(all) < s.cfg.MaxFeatures
		start := p.Offset
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + p.Limit
		if end > len(filtered) {
			end = len(filtered)
		}
		features = filtered[start:end]
	} else {
		features, matched, complete, err = layer.Features(r.Context(), bound, p.Limit, p.Offset, flt)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	out := make([]any, 0, len(features))
	for _, f := range features {
		out = append(out, featureJSON(f))
	}
	links := []stacLink{
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/items"), Rel: "self", Type: "application/geo+json"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)), Rel: "collection", Type: "application/json"},
		{Href: s.apiURL(r, st, "/ogc"), Rel: "root", Type: "application/json"},
	}
	if !complete {
		u := *r.URL
		q := u.Query()
		q.Set("offset", strconv.Itoa(p.Offset+len(features)))
		u.RawQuery = q.Encode()
		links = append(links, stacLink{Href: s.absoluteURL(r, u.RequestURI()), Rel: "next", Type: "application/geo+json"})
	}
	body := map[string]any{
		"type": "FeatureCollection", "features": out, "links": links,
		"numberReturned": len(out), "timeStamp": time.Now().UTC().Format(time.RFC3339),
	}
	if complete {
		body["numberMatched"] = matched
	}
	w.Header().Set("Content-Type", "application/geo+json")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *server) parseOGCItemsParams(r *http.Request) (*ogcItemsParams, error) {
	q := r.URL.Query()
	p := &ogcItemsParams{
		Datetime: q.Get("datetime"), Filter: q.Get("filter"), FilterLang: q.Get("filter-lang"),
		Limit:  10,
		Offset: parseIntDefault(q.Get("offset"), 0, 0),
	}
	if rawLimit := q.Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("limit must be a positive integer")
		}
		p.Limit = n
	}
	if p.Limit > s.cfg.MaxFeatures {
		p.Limit = s.cfg.MaxFeatures
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if raw := q.Get("bbox"); raw != "" {
		b, err := parseBBox(raw)
		if err != nil {
			return nil, err
		}
		p.BBox = &orb.Bound{Min: orb.Point{b[0], b[1]}, Max: orb.Point{b[2], b[3]}}
	}
	if p.FilterLang != "" && p.FilterLang != "cql2-text" && p.FilterLang != "cql2-json" {
		return nil, fmt.Errorf("unsupported filter-lang: %s", p.FilterLang)
	}
	if p.Datetime != "" {
		if _, err := parseTimeRange(p.Datetime); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func featureJSON(f tiles.Feature) map[string]any {
	return map[string]any{
		"type": "Feature", "id": f.ID,
		"geometry":   geojson.NewGeometry(f.Geometry),
		"properties": f.Properties,
		"bbox":       f.BBox,
	}
}

func featureTimeMatches(props map[string]any, tr timeRange) bool {
	for _, name := range []string{"datetime", "timestamp", "date", "time"} {
		if v, ok := props[name]; ok {
			if s, ok := v.(string); ok {
				if t, err := parseTime(s); err == nil {
					return tr.matches(t, t, true)
				}
			}
		}
	}
	start, startOK := propTime(props, "start_datetime")
	end, endOK := propTime(props, "end_datetime")
	if startOK || endOK {
		if !startOK {
			start = end
		}
		if !endOK {
			end = start
		}
		return tr.matches(start, end, true)
	}
	// No temporal property: keep the feature rather than silently dropping it.
	return true
}

func propTime(props map[string]any, name string) (time.Time, bool) {
	v, ok := props[name]
	if !ok {
		return time.Time{}, false
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := parseTime(s)
	return t, err == nil
}

func sortFeatures(features []tiles.Feature, specs []sortSpec) {
	if len(specs) == 0 {
		return
	}
	sort.SliceStable(features, func(i, j int) bool {
		for _, sp := range specs {
			a, b := featureSortValue(features[i], sp.Field), featureSortValue(features[j], sp.Field)
			if a == b {
				continue
			}
			if sp.Desc {
				return a > b
			}
			return a < b
		}
		return false
	})
}

func featureSortValue(f tiles.Feature, field string) string {
	field = strings.TrimPrefix(field, "properties.")
	switch field {
	case "id":
		return f.ID
	}
	if v, ok := f.Properties[field]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func (s *server) ogcItem(w http.ResponseWriter, r *http.Request) {
	_, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	layer, err := entry.get()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	id := chi.URLParam(r, "item")
	features, _, _, err := layer.Features(r.Context(), orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}, s.cfg.MaxFeatures, 0, nil)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, f := range features {
		if f.ID == id {
			w.Header().Set("Content-Type", "application/geo+json")
			_ = json.NewEncoder(w).Encode(featureJSON(f))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "feature not found")
}

func (s *server) ogcQueryables(w http.ResponseWriter, r *http.Request) {
	s.writeSchemaResource(w, r, "queryables")
}

func (s *server) ogcSchema(w http.ResponseWriter, r *http.Request) {
	s.writeSchemaResource(w, r, "schema")
}

func (s *server) ogcSortables(w http.ResponseWriter, r *http.Request) {
	s.writeSchemaResource(w, r, "sortables")
}

func (s *server) writeSchemaResource(w http.ResponseWriter, r *http.Request, kind string) {
	collection := chi.URLParam(r, "collection")
	if collection == "" {
		w.Header().Set("Content-Type", "application/schema+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object", "additionalProperties": true,
		})
		return
	}
	_, entry, err := s.entry(catalogFromRequest(r), collection)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	layer, err := entry.get()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	fields := layer.Fields()
	props := map[string]any{}
	for _, f := range fields {
		p := map[string]any{}
		if f.Geometry {
			p["format"] = f.Format
			p["x-ogc-role"] = "primary-geometry"
		} else {
			p["type"] = f.Type
			if f.Format != "" {
				p["format"] = f.Format
			}
		}
		props[f.Name] = p
	}
	if kind == "sortables" {
		props = map[string]any{"id": map[string]any{"type": "string"}, "collection": map[string]any{"type": "string"}}
		for _, f := range fields {
			if !f.Geometry {
				props[f.Name] = map[string]any{"type": f.Type}
			}
		}
	}
	out := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     s.apiURL(r, mustState(s, r), "/ogc/collections/"+url.PathEscape(entry.name)+"/"+kind),
		"type":    "object", "title": entry.title,
		"properties": props, "additionalProperties": false,
	}
	w.Header().Set("Content-Type", "application/schema+json")
	_ = json.NewEncoder(w).Encode(out)
}

func mustState(s *server, r *http.Request) *catalogState {
	if st := s.state(catalogFromRequest(r)); st != nil {
		return st
	}
	return s.states["default"]
}

func (s *server) ogcTilesets(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	id := url.PathEscape(entry.name)
	writeJSON(w, http.StatusOK, map[string]any{
		"tilesets": []any{map[string]any{
			"title":            entry.title,
			"dataType":         "vector",
			"crs":              "http://www.opengis.net/def/crs/OGC/1.3/CRS84",
			"tileMatrixSetURI": "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad",
			"links": []stacLink{
				{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles/WebMercatorQuad"), Rel: "self", Type: "application/json"},
				{Href: s.apiURL(r, st, "/ogc/tileMatrixSets/WebMercatorQuad"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/tiling-scheme", Type: "application/json"},
				{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles/WebMercatorQuad/{tileMatrix}/{tileRow}/{tileCol}"), Rel: "item", Type: "application/vnd.mapbox-vector-tile", Templated: true},
			},
		}},
		"links": []stacLink{
			{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles"), Rel: "self", Type: "application/json"},
			{Href: s.apiURL(r, st, "/ogc/collections/"+id), Rel: "collection", Type: "application/json"},
		},
	})
}

func (s *server) ogcTileset(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	tj := s.tileJSONFor(r, st, entry)
	id := url.PathEscape(entry.name)
	tj["tiles"] = []string{s.absoluteURL(r, "/ogc/collections/"+id+"/tiles/WebMercatorQuad/{z}/{y}/{x}") + s.catalogQuery(st.name)}
	tj["links"] = []stacLink{
		{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles/WebMercatorQuad"), Rel: "self", Type: "application/json"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+id), Rel: "collection", Type: "application/json"},
		{Href: s.apiURL(r, st, "/ogc/tileMatrixSets/WebMercatorQuad"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/tiling-scheme", Type: "application/json"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+id+"/tiles/WebMercatorQuad/{tileMatrix}/{tileRow}/{tileCol}"), Rel: "item", Type: "application/vnd.mapbox-vector-tile", Templated: true},
	}
	tj["tileMatrixSetURI"] = "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad"
	writeJSON(w, http.StatusOK, tj)
}

func (s *server) tileJSONFor(r *http.Request, st *catalogState, entry *layerEntry) map[string]any {
	b := entry.bounds()
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	if base == "" {
		base = s.absoluteURL(r, "")
	}
	q := ""
	if st.name != "" && st.name != "default" {
		q = "?catalog=" + url.QueryEscape(st.name)
	}
	tileURL := base + "/collections/" + url.PathEscape(entry.name) + "/tiles/{z}/{x}/{y}.mvt" + q
	fields := map[string]string{}
	if layer, err := entry.get(); err == nil {
		for _, f := range layer.Fields() {
			if !f.Geometry {
				fields[f.Name] = f.Type
			}
		}
	}
	return map[string]any{
		"tilejson": "3.0.0", "name": entry.name, "tiles": []string{tileURL},
		"minzoom": entry.minZoom(), "maxzoom": entry.maxZoom(),
		"bounds": []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()},
		"scheme": "xyz",
		"vector_layers": []map[string]any{{
			"id": entry.name, "fields": fields,
			"minzoom": entry.minZoom(), "maxzoom": entry.maxZoom(),
		}},
		"x-ogc-tileset": "WebMercatorQuad",
	}
}

func (s *server) ogcTile(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	z, err := strconv.ParseUint(chi.URLParam(r, "z"), 10, 8)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid z")
		return
	}
	y, err := strconv.ParseUint(chi.URLParam(r, "y"), 10, 32)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid y")
		return
	}
	x, err := strconv.ParseUint(chi.URLParam(r, "x"), 10, 32)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid x")
		return
	}
	q := r.URL.Query()
	minCount := parseIntDefault(q.Get("min_count"), 0, 0)
	key := tileCacheKey(st, entry.name, uint8(z), uint32(x), uint32(y), minCount, q.Get("filter-lang"), q.Get("filter"))
	gzipOK := acceptsGzip(r.Header.Get("Accept-Encoding"))
	etag := tileETag(key, gzipOK)
	if in := r.Header.Get("If-None-Match"); in != "" && etagMatches(in, etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	out, err := s.tile(r.Context(), &TileInput{
		Collection: entry.name, Z: uint8(z), X: uint32(x), Y: strconv.FormatUint(y, 10),
		MinCount: minCount, Filter: q.Get("filter"),
		FilterLang: q.Get("filter-lang"), Catalog: catalogFromRequest(r),
		AcceptEnc: r.Header.Get("Accept-Encoding"),
	})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(out.Body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	w.Header().Set("Content-Encoding", out.ContentEncoding)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("X-Cache", out.XCache)
	_, _ = w.Write(out.Body)
}

func (s *server) ogcTileMatrixSets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tileMatrixSets": []any{map[string]any{
			"id": "WebMercatorQuad", "title": "Google Maps Compatible",
			"uri": "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad",
			"links": []stacLink{
				{Href: s.absoluteURL(r, "/ogc/tileMatrixSets/WebMercatorQuad"), Rel: "self", Type: "application/json"},
			},
		}},
	})
}

func (s *server) ogcTileMatrixSet(w http.ResponseWriter, r *http.Request) {
	const origin = 20037508.342789244
	const baseScale = 559082264.0287178
	matrices := make([]any, 0, 25)
	for z := 0; z <= 24; z++ {
		n := int64(1) << z
		matrices = append(matrices, map[string]any{
			"id":               strconv.Itoa(z),
			"scaleDenominator": baseScale / float64(n),
			"cellSize":         156543.03392804097 / float64(n),
			"pointOfOrigin":    []float64{-origin, origin},
			"tileWidth":        256,
			"tileHeight":       256,
			"matrixWidth":      n,
			"matrixHeight":     n,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "WebMercatorQuad", "title": "Google Maps Compatible",
		"uri":               "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad",
		"crs":               "http://www.opengis.net/def/crs/EPSG/0/3857",
		"orderedAxes":       []string{"E", "N"},
		"wellKnownScaleSet": "http://www.opengis.net/def/wkss/OGC/1.0/GoogleMapsCompatible",
		"tileMatrices":      matrices,
	})
}

// apiOpenAPI returns a small, valid OpenAPI document. It is intentionally
// generated rather than using the Huma document so it describes the STAC/OGC
// routes that clients use for discovery.
func (s *server) apiOpenAPI(w http.ResponseWriter, r *http.Request) {
	base := s.absoluteURL(r, "")
	prefix := "/ogc"
	if strings.HasPrefix(r.URL.Path, "/stac-api") {
		prefix = "/stac-api"
	}
	base = s.absoluteURL(r, prefix)
	if st := s.state(catalogFromRequest(r)); st != nil {
		base = s.apiURL(r, st, prefix)
	}
	paths := map[string]any{}
	paths["/collections"] = map[string]any{"get": map[string]any{"summary": "Collections"}}
	paths["/collections/{collectionId}"] = map[string]any{"get": map[string]any{"summary": "Collection"}}
	paths["/collections/{collectionId}/items"] = map[string]any{"get": map[string]any{
		"summary": "Features",
		"parameters": []any{
			map[string]any{"name": "bbox", "in": "query", "schema": map[string]any{"type": "array", "items": map[string]any{"type": "number"}}},
			map[string]any{"name": "datetime", "in": "query", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}},
			map[string]any{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer"}},
			map[string]any{"name": "filter", "in": "query", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "filter-lang", "in": "query", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "sortby", "in": "query", "schema": map[string]any{"type": "string"}},
		},
	}}
	paths["/collections/{collectionId}/items/{featureId}"] = map[string]any{"get": map[string]any{"summary": "Feature"}}
	paths["/collections/{collectionId}/queryables"] = map[string]any{"get": map[string]any{"summary": "Queryables"}}
	paths["/collections/{collectionId}/schema"] = map[string]any{"get": map[string]any{"summary": "Schema"}}
	paths["/collections/{collectionId}/sortables"] = map[string]any{"get": map[string]any{"summary": "Sortables"}}
	paths["/collections/{collectionId}/tiles"] = map[string]any{"get": map[string]any{"summary": "Tilesets"}}
	paths["/collections/{collectionId}/tiles/WebMercatorQuad"] = map[string]any{"get": map[string]any{"summary": "Tileset"}}
	paths["/collections/{collectionId}/tiles/WebMercatorQuad/{tileMatrix}/{tileRow}/{tileCol}"] = map[string]any{"get": map[string]any{"summary": "Tile"}}
	paths["/search"] = map[string]any{"get": map[string]any{"summary": "STAC item search"}}
	writeJSONType(w, http.StatusOK, "application/vnd.oai.openapi+json;version=3.0", map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "GeoParquet STAC/OGC API", "version": "1.0.0"},
		"servers": []any{map[string]any{"url": base}},
		"paths":   paths,
	})
}
