package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/clip"
	"github.com/paulmach/orb/geojson"

	"github.com/adonm/geoparquet-tileserver/internal/catalog"
	filt "github.com/adonm/geoparquet-tileserver/internal/filter"
)

const (
	stacVersion = "1.1.0"
	maxBodySize = 16 << 20
)

var stacConformance = []string{
	"https://api.stacspec.org/v1.0.0/core",
	"https://api.stacspec.org/v1.0.0/item-search",
	"https://api.stacspec.org/v1.0.0/item-search#fields",
	"https://api.stacspec.org/v1.0.0/item-search#sort",
	"https://api.stacspec.org/v1.0.0/ogcapi-features",
	"https://api.stacspec.org/v1.0.0/collections",
	"http://www.opengis.net/spec/ogcapi-common-2/1.0/conf/simple-query",
	"https://api.stacspec.org/v1.0.0-rc.1/collection-search",
	"https://api.stacspec.org/v1.0.0-rc.1/collection-search#free-text",
	"https://api.stacspec.org/v1.0.0-rc.1/collection-search#filter",
	"https://api.stacspec.org/v1.0.0-rc.1/collection-search#sort",
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/core",
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/geojson",
	"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/oas30",
	"http://www.opengis.net/spec/ogcapi-features-3/1.0/conf/filter",
	"http://www.opengis.net/spec/ogcapi-features-3/1.0/conf/features-filter",
	"http://www.opengis.net/spec/ogcapi-features-3/1.0/conf/queryables",
	"http://www.opengis.net/spec/cql2/1.0/conf/cql2-text",
	"http://www.opengis.net/spec/cql2/1.0/conf/cql2-json",
	"http://www.opengis.net/spec/cql2/1.0/conf/basic-cql2",
	"http://www.opengis.net/spec/cql2/1.0/conf/advanced-comparison-operators",
	"http://www.opengis.net/spec/ogcapi-common-3/1.0/conf/schemas",
	"http://www.opengis.net/spec/ogcapi-common-3/1.0/conf/queryables",
	"http://www.opengis.net/spec/ogcapi-common-3/1.0/conf/sortables",
	"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/core",
	"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/mvt",
	"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/tileset",
	"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/tilesets-list",
	"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/geodata-tilesets",
}

type stacLink struct {
	Href      string `json:"href"`
	Rel       string `json:"rel"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title,omitempty"`
	Method    string `json:"method,omitempty"`
	Merge     bool   `json:"merge,omitempty"`
	Body      any    `json:"body,omitempty"`
	Templated bool   `json:"templated,omitempty"`
}

type indexedItem struct {
	item    catalog.Item
	geom    orb.Geometry
	bound   orb.Bound
	start   time.Time
	end     time.Time
	hasTime bool
	props   map[string]any
}

type itemSet struct {
	items []indexedItem
	byID  map[string]int
}

func (s *server) itemSet(st *catalogState, entry *layerEntry) (*itemSet, error) {
	st.itemMu.Lock()
	defer st.itemMu.Unlock()
	if st.items == nil {
		st.items = map[string]*itemSet{}
	}
	if set := st.items[entry.name]; set != nil {
		return set, nil
	}
	var items []catalog.Item
	var err error
	switch {
	case entry.collection != nil:
		items, err = entry.collection.Items()
	case entry.group != nil:
		items, err = entry.group.Items()
	default:
		err = fmt.Errorf("layer %q has no collection", entry.name)
	}
	if err != nil {
		return nil, err
	}
	set := &itemSet{byID: map[string]int{}}
	for _, it := range items {
		rec := indexedItem{item: it, bound: it.Bound(), props: itemFilterProperties(it)}
		if g := itemGeometry(it); g != nil {
			rec.geom = g
			rec.bound = g.Bound()
		}
		if start, end, ok := itemTimes(it); ok {
			rec.start, rec.end, rec.hasTime = start, end, true
		}
		set.byID[it.ID] = len(set.items)
		set.items = append(set.items, rec)
	}
	sort.SliceStable(set.items, func(i, j int) bool { return set.items[i].item.ID < set.items[j].item.ID })
	// Rebuild byID after sorting.
	set.byID = make(map[string]int, len(set.items))
	for i := range set.items {
		set.byID[set.items[i].item.ID] = i
	}
	st.items[entry.name] = set
	return set, nil
}

func itemFilterProperties(it catalog.Item) map[string]any {
	props := make(map[string]any, len(it.Properties)*2+4)
	for k, v := range it.Properties {
		props[k] = v
		props["properties."+k] = v
	}
	props["id"] = it.ID
	props["collection"] = it.Collection
	if it.Collection == "" {
		props["collection"] = it.Collection
	}
	if t, ok := props["datetime"]; ok {
		props["properties.datetime"] = t
	}
	return props
}

func itemGeometry(it catalog.Item) orb.Geometry {
	if len(it.Raw) == 0 {
		return nil
	}
	var raw struct {
		Geometry json.RawMessage `json:"geometry"`
	}
	if json.Unmarshal(it.Raw, &raw) != nil || len(raw.Geometry) == 0 {
		return nil
	}
	g, err := geojson.UnmarshalGeometry(raw.Geometry)
	if err != nil {
		return nil
	}
	return g.Geometry()
}

func itemTimes(it catalog.Item) (time.Time, time.Time, bool) {
	get := func(name string) (time.Time, bool) {
		v, ok := it.Properties[name]
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
	if start, ok := get("start_datetime"); ok {
		end, ok := get("end_datetime")
		if !ok {
			end = start
		}
		return start, end, true
	}
	if t, ok := get("datetime"); ok {
		return t, t, true
	}
	return time.Time{}, time.Time{}, false
}

func parseTime(s string) (time.Time, error) {
	if t, err := parseQueryTime(s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(s)); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q", s)
}

func parseQueryTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, ",") {
		return time.Time{}, fmt.Errorf("invalid datetime %q", s)
	}
	if i := strings.IndexByte(s, 't'); i > 0 && !strings.Contains(s[:i], "T") {
		s = s[:i] + "T" + s[i+1:]
	}
	if strings.HasSuffix(s, "z") {
		s = s[:len(s)-1] + "Z"
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid datetime %q", s)
}

func (s *server) registerSTACAPI(router chi.Router) {
	router.Get("/stac-api", s.stacLanding)
	router.Get("/stac-api/", s.stacLanding)
	router.Get("/stac-api/conformance", s.stacConformance)
	router.Get("/stac-api/api", s.apiOpenAPI)
	router.Get("/stac-api/openapi.json", s.apiOpenAPI)
	router.Get("/stac-api/collections", s.stacCollections)
	router.Get("/stac-api/collections/{collection}", s.stacCollection)
	router.Get("/stac-api/collections/{collection}/items", s.stacItems)
	router.Get("/stac-api/collections/{collection}/items/{item}", s.stacItem)
	router.Get("/stac-api/collections/{collection}/queryables", s.ogcQueryables)
	router.Get("/stac-api/collections/{collection}/schema", s.ogcSchema)
	router.Get("/stac-api/collections/{collection}/sortables", s.ogcSortables)
	router.Get("/stac-api/collections/{collection}/tiles", s.ogcTilesets)
	router.Get("/stac-api/collections/{collection}/tiles/WebMercatorQuad", s.ogcTileset)
	router.Get("/stac-api/collections/{collection}/tiles/WebMercatorQuad/{z}/{y}/{x}", s.ogcTile)
	router.Get("/stac-api/search", s.stacSearchGET)
	router.Post("/stac-api/search", s.stacSearchPOST)
	router.Get("/stac-api/queryables", s.ogcQueryables)
	router.Options("/stac-api/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func (s *server) stacLanding(w http.ResponseWriter, r *http.Request) {
	st := s.state(catalogFromRequest(r))
	if st == nil {
		writeAPIError(w, http.StatusNotFound, "unknown catalog")
		return
	}
	base := s.apiURL(r, st, "/stac-api")
	links := []stacLink{
		{Href: base, Rel: "self", Type: "application/json", Title: "this document"},
		{Href: base, Rel: "root", Type: "application/json", Title: "STAC API root"},
		{Href: s.apiURL(r, st, "/stac-api/collections"), Rel: "data", Type: "application/json", Title: "Collections"},
		{Href: s.apiURL(r, st, "/stac-api/search"), Rel: "search", Type: "application/geo+json", Title: "Item search", Method: "GET"},
		{Href: s.apiURL(r, st, "/stac-api/search"), Rel: "search", Type: "application/geo+json", Title: "Item search", Method: "POST"},
		{Href: s.apiURL(r, st, "/stac-api/conformance"), Rel: "conformance", Type: "application/json", Title: "Conformance"},
		{Href: s.apiURL(r, st, "/stac-api/api"), Rel: "service-desc", Type: "application/vnd.oai.openapi+json;version=3.0", Title: "API definition"},
		{Href: s.absoluteURL(r, "/docs"), Rel: "service-doc", Type: "text/html", Title: "API documentation"},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "Catalog", "stac_version": stacVersion,
		"id":          firstNonEmptyString(st.cat.ID, "geoparquet-tileserver"),
		"title":       firstNonEmptyString(st.cat.Title, "GeoParquet OGC API"),
		"description": firstNonEmptyString(st.cat.Description, "STAC and OGC API services backed by GeoParquet"),
		"conformsTo":  stacConformance, "links": links,
	})
}

func (s *server) stacConformance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"conformsTo": stacConformance})
}

func (s *server) stacCollections(w http.ResponseWriter, r *http.Request) {
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
			start, end, ok := collectionTimes(entry)
			if ok && !tr.matches(start, end, true) {
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
	collections := make([]any, 0, end-offset)
	for _, entry := range matched[offset:end] {
		collections = append(collections, s.stacCollectionJSON(r, st, entry))
	}
	links := []stacLink{
		{Href: s.apiURL(r, st, "/stac-api/collections"), Rel: "self", Type: "application/json"},
		{Href: s.apiURL(r, st, "/stac-api"), Rel: "root", Type: "application/json"},
	}
	if end < total {
		u := *r.URL
		qq := u.Query()
		qq.Set("offset", strconv.Itoa(end))
		u.RawQuery = qq.Encode()
		links = append(links, stacLink{Href: s.absoluteURL(r, u.RequestURI()), Rel: "next", Type: "application/json"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"collections": collections, "links": links,
		"numberMatched": total, "numberReturned": len(collections),
	})
}

func collectionTimes(entry *layerEntry) (time.Time, time.Time, bool) {
	if entry.collection == nil || len(entry.collection.Raw) == 0 {
		return time.Time{}, time.Time{}, false
	}
	var raw struct {
		Extent struct {
			Temporal struct {
				Interval [][]*string `json:"interval"`
			} `json:"temporal"`
		} `json:"extent"`
	}
	if json.Unmarshal(entry.collection.Raw, &raw) != nil || len(raw.Extent.Temporal.Interval) == 0 {
		return time.Time{}, time.Time{}, false
	}
	iv := raw.Extent.Temporal.Interval[0]
	if len(iv) < 2 || iv[0] == nil {
		return time.Time{}, time.Time{}, false
	}
	start, err := parseTime(*iv[0])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	end := start
	if iv[1] != nil {
		if t, err := parseTime(*iv[1]); err == nil {
			end = t
		}
	}
	return start, end, true
}

func collectionTextMatch(entry *layerEntry, terms []string) bool {
	text := strings.ToLower(entry.name + " " + entry.title + " " + entry.description())
	if entry.collection != nil {
		text += " " + strings.Join(entry.collection.Keywords, " ")
	}
	for _, term := range terms {
		if term == "," {
			continue
		}
		if strings.Contains(text, strings.Trim(term, ",")) {
			return true
		}
	}
	return false
}

func collectionFilterProperties(entry *layerEntry) map[string]any {
	props := map[string]any{"id": entry.name, "title": entry.title, "description": entry.description()}
	if entry.collection != nil {
		props["keywords"] = entry.collection.Keywords
		props["license"] = entry.collection.License
	}
	return props
}

func collectionSortValue(entry *layerEntry, field string) string {
	switch field {
	case "id":
		return entry.name
	case "title":
		return entry.title
	case "datetime":
		if start, _, ok := collectionTimes(entry); ok {
			return start.Format(time.RFC3339Nano)
		}
	}
	return entry.name
}

func (s *server) stacCollection(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.stacCollectionJSON(r, st, entry))
}

func (s *server) stacCollectionJSON(r *http.Request, st *catalogState, entry *layerEntry) map[string]any {
	hc := s.htmlCollection(st, entry, st.name)
	b := entry.bounds()
	out := map[string]any{
		"type": "Collection", "stac_version": stacVersion,
		"id": entry.name, "title": entry.title, "description": entry.description(),
		"license": "other", "keywords": []string{},
		"extent": map[string]any{
			"spatial":  map[string]any{"bbox": [][]float64{{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()}}},
			"temporal": map[string]any{"interval": [][]any{{nil, nil}}},
		},
		"itemType":  "Feature",
		"crs":       []string{"http://www.opengis.net/def/crs/OGC/1.3/CRS84"},
		"summaries": map[string]any{},
	}
	if entry.collection != nil && len(entry.collection.Raw) > 0 {
		var raw map[string]any
		if json.Unmarshal(entry.collection.Raw, &raw) == nil {
			for _, k := range []string{"license", "keywords", "providers", "summaries", "extent", "assets", "item_assets"} {
				if v, ok := raw[k]; ok && v != nil {
					out[k] = v
				}
			}
		}
	}
	links := []stacLink{
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)), Rel: "self", Type: "application/json", Title: "this document"},
		{Href: s.apiURL(r, st, "/stac-api"), Rel: "root", Type: "application/json", Title: "STAC API root"},
		{Href: s.apiURL(r, st, "/stac-api/collections"), Rel: "parent", Type: "application/json", Title: "Collections"},
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)+"/items"), Rel: "items", Type: "application/geo+json", Title: "Items"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/queryables"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/queryables", Type: "application/schema+json", Title: "Queryables"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/schema"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/schema", Type: "application/schema+json", Title: "Schema"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/sortables"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/sortables", Type: "application/schema+json", Title: "Sortables"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/tiles"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json", Title: "Tilesets"},
		{Href: s.apiURL(r, st, "/ogc/collections/"+url.PathEscape(entry.name)+"/tiles/WebMercatorQuad"), Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json", Title: "WebMercatorQuad tileset"},
	}
	if entry.group != nil {
		for _, child := range hc.Children {
			links = append(links, stacLink{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(child.Name)), Rel: "child", Type: "application/json", Title: child.Title})
		}
	}
	out["links"] = links
	if len(hc.Assets) > 0 {
		assets := map[string]any{}
		for _, a := range hc.Assets {
			typ := a.Type
			if typ == "" {
				typ = assetType(a.Name)
			}
			assets[a.Name] = map[string]any{"href": a.URL, "type": typ}
		}
		out["assets"] = assets
	}
	if hc.PMTilesURL != "" {
		assets, _ := out["assets"].(map[string]any)
		if assets == nil {
			assets = map[string]any{}
		}
		assets["pmtiles"] = map[string]any{"href": hc.PMTilesURL, "type": "application/vnd.pmtiles"}
		out["assets"] = assets
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s *server) stacItems(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	params, err := parseSTACQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, total, err := s.searchItems(r.Context(), st, []*layerEntry{entry}, params)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := nextToken(params, total)
	links := []stacLink{
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)+"/items"), Rel: "self", Type: "application/geo+json"},
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)), Rel: "collection", Type: "application/json"},
		{Href: s.apiURL(r, st, "/stac-api"), Rel: "root", Type: "application/json"},
	}
	if next != "" {
		u := *r.URL
		q := u.Query()
		q.Set("token", next)
		u.RawQuery = q.Encode()
		links = append(links, stacLink{Href: s.absoluteURL(r, u.RequestURI()), Rel: "next", Type: "application/geo+json"})
	}
	features := make([]any, 0, len(result))
	for _, rec := range result {
		features = append(features, s.stacItemJSON(r, st, entry, rec, params.Fields))
	}
	writeJSONType(w, http.StatusOK, "application/geo+json", map[string]any{
		"type": "FeatureCollection", "features": features,
		"links": links, "numberMatched": total, "numberReturned": len(features),
		"timeStamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) stacItem(w http.ResponseWriter, r *http.Request) {
	st, entry, err := s.entry(catalogFromRequest(r), chi.URLParam(r, "collection"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	set, err := s.itemSet(st, entry)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	id := chi.URLParam(r, "item")
	i, ok := set.byID[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "item not found")
		return
	}
	writeJSONType(w, http.StatusOK, "application/geo+json", s.stacItemJSON(r, st, entry, set.items[i], nil))
}

func (s *server) stacItemJSON(r *http.Request, st *catalogState, entry *layerEntry, rec indexedItem, fields *fieldSelection) map[string]any {
	it := rec.item
	out := map[string]any{}
	if len(it.Raw) > 0 {
		_ = json.Unmarshal(it.Raw, &out)
	}
	out["type"] = "Feature"
	out["stac_version"] = stacVersion
	out["id"] = it.ID
	collection := it.Collection
	if collection == "" {
		collection = entry.name
	}
	out["collection"] = collection
	if _, ok := out["bbox"]; !ok || out["bbox"] == nil {
		b := it.Bound()
		out["bbox"] = []float64{b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()}
	}
	if _, ok := out["geometry"]; !ok || out["geometry"] == nil {
		if rec.geom != nil {
			out["geometry"] = geojson.NewGeometry(rec.geom)
		} else {
			b := it.Bound()
			out["geometry"] = geojson.NewGeometry(orb.Polygon{{
				{b.Min.X(), b.Min.Y()}, {b.Max.X(), b.Min.Y()},
				{b.Max.X(), b.Max.Y()}, {b.Min.X(), b.Max.Y()},
				{b.Min.X(), b.Min.Y()},
			}})
		}
	}
	out["properties"] = it.Properties
	if out["properties"] == nil {
		out["properties"] = map[string]any{}
	}
	assets := map[string]any{}
	for name, a := range it.Assets {
		typ := a.Type
		if typ == "" {
			typ = assetType(name)
		}
		assets[name] = map[string]any{
			"href": s.itemAssetURL(r, st, entry, it, name), "type": typ, "roles": a.Roles,
		}
	}
	out["assets"] = assets
	out["links"] = []stacLink{
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)+"/items/"+url.PathEscape(it.ID)), Rel: "self", Type: "application/geo+json", Title: "this document"},
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)), Rel: "parent", Type: "application/json", Title: entry.title},
		{Href: s.apiURL(r, st, "/stac-api/collections/"+url.PathEscape(entry.name)), Rel: "collection", Type: "application/json", Title: entry.title},
		{Href: s.apiURL(r, st, "/stac-api"), Rel: "root", Type: "application/json", Title: "STAC API root"},
	}
	if fields != nil {
		out = applyFieldSelection(out, fields)
	}
	return out
}

func (s *server) itemAssetURL(r *http.Request, st *catalogState, entry *layerEntry, it catalog.Item, name string) string {
	collection := it.Collection
	if collection == "" {
		collection = entry.name
	}
	if _, ok := st.layers[collection]; !ok {
		collection = entry.name
	}
	path := "/collections/" + url.PathEscape(collection) + "/items/" + url.PathEscape(it.ID) + "/assets/" + url.PathEscape(name)
	ha := htmlAsset{Name: name, Type: ""}
	if a, ok := it.Assets[name]; ok {
		ha.Type = a.Type
	}
	if suffix := dataAssetSuffix(ha); suffix != "" {
		path += suffix
	}
	return s.absoluteURL(r, path) + s.catalogQuery(st.name)
}

func assetType(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "parquet"):
		return "application/vnd.apache.parquet"
	case strings.Contains(n, "pmtiles"):
		return "application/vnd.pmtiles"
	}
	return "application/octet-stream"
}

type fieldSelection struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

func applyFieldSelection(m map[string]any, f *fieldSelection) map[string]any {
	if f == nil {
		return m
	}
	if len(f.Include) > 0 {
		out := map[string]any{}
		for _, path := range f.Include {
			if v, ok := getPath(m, path); ok {
				setPath(out, path, v)
			}
		}
		return out
	}
	for _, path := range f.Exclude {
		deletePath(m, path)
	}
	return m
}

func getPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	cur := m
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = v
}

func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	cur := m
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, parts[len(parts)-1])
}

type sortSpec struct {
	Field string
	Desc  bool
}

type stacSearchParams struct {
	Collections []string
	IDs         []string
	BBox        []float64
	Intersects  orb.Geometry
	Datetime    string
	Limit       int
	Offset      int
	Sort        []sortSpec
	Filter      string
	FilterLang  string
	Fields      *fieldSelection
	Token       string
}

func parseSTACQuery(r *http.Request) (*stacSearchParams, error) {
	q := r.URL.Query()
	p := &stacSearchParams{
		Collections: queryList(q, "collections"),
		IDs:         queryList(q, "ids"),
		Datetime:    q.Get("datetime"),
		Limit:       10,
		Filter:      q.Get("filter"),
		FilterLang:  q.Get("filter-lang"),
		Token:       q.Get("token"),
	}
	if rawLimit := q.Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("limit must be a positive integer")
		}
		p.Limit = n
	}
	if len(p.Collections) == 0 {
		p.Collections = queryList(q, "collection")
	}
	if p.Limit > sMaxFeatures() {
		p.Limit = sMaxFeatures()
	}
	if p.Limit < 1 {
		p.Limit = 1
	}
	if p.Limit > sMaxFeatures() {
		p.Limit = sMaxFeatures()
	}
	if bbox := q.Get("bbox"); bbox != "" {
		b, err := parseBBox(bbox)
		if err != nil {
			return nil, err
		}
		p.BBox = b
	}
	if raw := q.Get("intersects"); raw != "" {
		g, err := geojson.UnmarshalGeometry([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid intersects: %w", err)
		}
		p.Intersects = g.Geometry()
	}
	if p.BBox != nil && p.Intersects != nil {
		return nil, fmt.Errorf("bbox and intersects are mutually exclusive")
	}
	if p.Token != "" {
		off, err := decodeToken(p.Token)
		if err != nil {
			return nil, err
		}
		p.Offset = off
	} else if page := parseIntDefault(q.Get("page"), 0, 0); page > 0 {
		p.Offset = (page - 1) * p.Limit
	}
	if s := q.Get("sortby"); s != "" {
		p.Sort = parseSortby(s)
	}
	if f := q.Get("fields"); f != "" {
		p.Fields = parseFields(f)
	}
	if p.FilterLang == "" {
		p.FilterLang = "cql2-text"
	}
	return p, nil
}

func parseSTACBody(r *http.Request) (*stacSearchParams, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		return nil, err
	}
	var body struct {
		Collections []string        `json:"collections"`
		Collection  string          `json:"collection"`
		IDs         []string        `json:"ids"`
		BBox        []float64       `json:"bbox"`
		Intersects  json.RawMessage `json:"intersects"`
		Datetime    string          `json:"datetime"`
		Limit       int             `json:"limit"`
		SortBy      json.RawMessage `json:"sortby"`
		Filter      json.RawMessage `json:"filter"`
		FilterLang  string          `json:"filter-lang"`
		Fields      *fieldSelection `json:"fields"`
		Token       string          `json:"token"`
		Page        int             `json:"page"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	if len(body.Collections) == 0 && body.Collection != "" {
		body.Collections = []string{body.Collection}
	}
	p := &stacSearchParams{
		Collections: body.Collections, IDs: body.IDs, BBox: body.BBox,
		Datetime: body.Datetime, Limit: body.Limit, FilterLang: body.FilterLang,
		Fields: body.Fields, Token: body.Token,
	}
	if len(body.BBox) > 0 {
		b, err := normalizeBBox(body.BBox)
		if err != nil {
			return nil, err
		}
		p.BBox = b
	}
	if len(body.Filter) > 0 {
		var s string
		if json.Unmarshal(body.Filter, &s) == nil {
			p.Filter = s
		} else {
			p.Filter = string(body.Filter)
			p.FilterLang = "cql2-json"
		}
	}
	if len(body.Intersects) > 0 {
		g, err := geojson.UnmarshalGeometry(body.Intersects)
		if err != nil {
			return nil, fmt.Errorf("invalid intersects: %w", err)
		}
		p.Intersects = g.Geometry()
	}
	if p.BBox != nil && p.Intersects != nil {
		return nil, fmt.Errorf("bbox and intersects are mutually exclusive")
	}
	if body.Limit < 0 {
		return nil, fmt.Errorf("limit must be a positive integer")
	}
	p.Limit = clampLimit(body.Limit)
	if p.Token != "" {
		off, err := decodeToken(p.Token)
		if err != nil {
			return nil, err
		}
		p.Offset = off
	} else if body.Page > 0 {
		p.Offset = (body.Page - 1) * p.Limit
	}
	if len(body.SortBy) > 0 {
		var s string
		if json.Unmarshal(body.SortBy, &s) == nil {
			p.Sort = parseSortby(s)
		} else {
			var specs []struct {
				Field     string `json:"field"`
				Direction string `json:"direction"`
			}
			if json.Unmarshal(body.SortBy, &specs) == nil {
				for _, sp := range specs {
					p.Sort = append(p.Sort, sortSpec{Field: sp.Field, Desc: strings.EqualFold(sp.Direction, "desc")})
				}
			}
		}
	}
	if p.FilterLang == "" {
		p.FilterLang = "cql2-text"
	}
	return p, nil
}

func (s *server) stacSearchGET(w http.ResponseWriter, r *http.Request) {
	p, err := parseSTACQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.runSTACSearch(w, r, p)
}

func (s *server) stacSearchPOST(w http.ResponseWriter, r *http.Request) {
	p, err := parseSTACBody(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.runSTACSearch(w, r, p)
}

func (s *server) runSTACSearch(w http.ResponseWriter, r *http.Request, p *stacSearchParams) {
	st := s.state(catalogFromRequest(r))
	if st == nil {
		writeAPIError(w, http.StatusNotFound, "unknown catalog")
		return
	}
	var entries []*layerEntry
	if len(p.Collections) > 0 {
		for _, id := range p.Collections {
			entry, ok := st.layers[id]
			if !ok {
				writeAPIError(w, http.StatusNotFound, "unknown collection: "+id)
				return
			}
			entries = append(entries, entry)
		}
	} else {
		for _, id := range st.order {
			if e := st.layers[id]; e != nil && e.kind == "collection" {
				entries = append(entries, e)
			}
		}
		if len(entries) == 0 {
			for _, id := range st.order {
				if e := st.layers[id]; e != nil {
					entries = append(entries, e)
				}
			}
		}
	}
	result, total, err := s.searchItems(r.Context(), st, entries, p)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := nextToken(p, total)
	links := []stacLink{
		{Href: s.apiURL(r, st, "/stac-api/search"), Rel: "self", Type: "application/geo+json"},
		{Href: s.apiURL(r, st, "/stac-api"), Rel: "root", Type: "application/json"},
	}
	if next != "" {
		if r.Method == http.MethodPost {
			links = append(links, stacLink{Href: s.apiURL(r, st, "/stac-api/search"), Rel: "next", Type: "application/geo+json", Method: "POST", Merge: true, Body: map[string]any{"token": next}})
		} else {
			u := *r.URL
			q := u.Query()
			q.Set("token", next)
			u.RawQuery = q.Encode()
			links = append(links, stacLink{Href: s.absoluteURL(r, u.RequestURI()), Rel: "next", Type: "application/geo+json"})
		}
	}
	features := make([]any, 0, len(result))
	for _, rec := range result {
		entry := st.layers[rec.item.Collection]
		if entry == nil {
			entry = entries[0]
		}
		features = append(features, s.stacItemJSON(r, st, entry, rec, p.Fields))
	}
	writeJSONType(w, http.StatusOK, "application/geo+json", map[string]any{
		"type": "FeatureCollection", "features": features, "links": links,
		"numberMatched": total, "numberReturned": len(features),
		"timeStamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) searchItems(ctx context.Context, st *catalogState, entries []*layerEntry, p *stacSearchParams) ([]indexedItem, int, error) {
	var flt *filt.Filter
	if p.Filter != "" {
		switch p.FilterLang {
		case "", "cql2-text", "cql2-json", "cql-json":
		default:
			return nil, 0, fmt.Errorf("unsupported filter-lang: %s", p.FilterLang)
		}
		var err error
		flt, err = filt.Compile(p.Filter)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid filter: %w", err)
		}
	}
	var interval *timeRange
	if p.Datetime != "" {
		tr, err := parseTimeRange(p.Datetime)
		if err != nil {
			return nil, 0, err
		}
		interval = &tr
	}
	ids := map[string]bool{}
	for _, id := range p.IDs {
		ids[id] = true
	}
	var matched []indexedItem
	for _, entry := range entries {
		set, err := s.itemSet(st, entry)
		if err != nil {
			return nil, 0, err
		}
		for _, rec := range set.items {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			if len(ids) > 0 && !ids[rec.item.ID] {
				continue
			}
			if p.BBox != nil && !boundIntersects(rec.bound, p.BBox) {
				continue
			}
			if p.Intersects != nil {
				ib := p.Intersects.Bound()
				if !boundsOverlap(rec.bound, ib) {
					continue
				}
				if rec.geom != nil && clip.Geometry(ib, rec.geom) == nil {
					continue
				}
			}
			if interval != nil && !interval.matches(rec.start, rec.end, rec.hasTime) {
				continue
			}
			if flt != nil {
				ok, err := flt.Match(rec.props)
				if err != nil {
					return nil, 0, err
				}
				if !ok {
					continue
				}
			}
			matched = append(matched, rec)
		}
	}
	sortItems(matched, p.Sort)
	total := len(matched)
	if p.Offset >= len(matched) {
		return nil, total, nil
	}
	end := p.Offset + p.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[p.Offset:end], total, nil
}

func sortItems(items []indexedItem, specs []sortSpec) {
	if len(specs) == 0 {
		specs = []sortSpec{{Field: "id"}}
	}
	sort.SliceStable(items, func(i, j int) bool {
		for _, sp := range specs {
			a, b := sortValue(items[i], sp.Field), sortValue(items[j], sp.Field)
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

func sortValue(it indexedItem, field string) string {
	field = strings.TrimPrefix(field, "properties.")
	switch field {
	case "id":
		return it.item.ID
	case "collection":
		return it.item.Collection
	case "datetime":
		if it.hasTime {
			return it.start.Format(time.RFC3339Nano)
		}
	}
	if v, ok := it.props[field]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func nextToken(p *stacSearchParams, total int) string {
	if p.Offset+p.Limit >= total {
		return ""
	}
	return encodeToken(p.Offset + p.Limit)
}

func encodeToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeToken(token string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, fmt.Errorf("invalid token")
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid token")
	}
	return n, nil
}

type timeRange struct {
	start, end       time.Time
	hasStart, hasEnd bool
}

func parseTimeRange(s string) (timeRange, error) {
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		t, err := parseQueryTime(s)
		if err != nil {
			return timeRange{}, err
		}
		return timeRange{start: t, end: t, hasStart: true, hasEnd: true}, nil
	}
	if len(parts) != 2 {
		return timeRange{}, fmt.Errorf("invalid datetime")
	}
	var tr timeRange
	if parts[0] != ".." && parts[0] != "" {
		t, err := parseQueryTime(parts[0])
		if err != nil {
			return timeRange{}, err
		}
		tr.start, tr.hasStart = t, true
	}
	if parts[1] != ".." && parts[1] != "" {
		t, err := parseQueryTime(parts[1])
		if err != nil {
			return timeRange{}, err
		}
		tr.end, tr.hasEnd = t, true
	}
	if !tr.hasStart && !tr.hasEnd {
		return timeRange{}, fmt.Errorf("invalid datetime")
	}
	if tr.hasStart && tr.hasEnd && tr.start.After(tr.end) {
		return timeRange{}, fmt.Errorf("invalid datetime interval")
	}
	return tr, nil
}

func (tr timeRange) matches(start, end time.Time, has bool) bool {
	if !has {
		return false
	}
	if tr.hasStart && end.Before(tr.start) {
		return false
	}
	if tr.hasEnd && start.After(tr.end) {
		return false
	}
	return true
}

func parseBBox(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 && len(parts) != 6 {
		return nil, fmt.Errorf("bbox must have 4 or 6 numbers")
	}
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid bbox")
		}
		out = append(out, v)
	}
	return normalizeBBox(out)
}

func normalizeBBox(in []float64) ([]float64, error) {
	if len(in) == 4 {
		if in[0] > in[2] || in[1] > in[3] {
			return nil, fmt.Errorf("invalid bbox")
		}
		return in, nil
	}
	if len(in) == 6 {
		if in[0] > in[3] || in[1] > in[4] {
			return nil, fmt.Errorf("invalid bbox")
		}
		return []float64{in[0], in[1], in[3], in[4]}, nil
	}
	return nil, fmt.Errorf("bbox must have 4 or 6 numbers")
}

func boundIntersects(b orb.Bound, box []float64) bool {
	return boundsOverlap(b, orb.Bound{Min: orb.Point{box[0], box[1]}, Max: orb.Point{box[2], box[3]}})
}

func boundsOverlap(a, b orb.Bound) bool {
	return !(a.Max.X() < b.Min.X() || a.Min.X() > b.Max.X() || a.Max.Y() < b.Min.Y() || a.Min.Y() > b.Max.Y())
}

func queryList(q url.Values, key string) []string {
	var out []string
	for _, v := range q[key] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func parseIntDefault(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

func parseSortby(s string) []sortSpec {
	var out []sortSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sp := sortSpec{Field: part}
		if strings.HasPrefix(part, "-") {
			sp.Field, sp.Desc = part[1:], true
		} else {
			sp.Field = strings.TrimPrefix(part, "+")
		}
		out = append(out, sp)
	}
	return out
}

func parseFields(s string) *fieldSelection {
	var f fieldSelection
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "-") {
			f.Exclude = append(f.Exclude, part[1:])
		} else {
			f.Include = append(f.Include, strings.TrimPrefix(part, "+"))
		}
	}
	return &f
}

func clampLimit(n int) int {
	if n <= 0 {
		return 10
	}
	if n > sMaxFeatures() {
		return sMaxFeatures()
	}
	return n
}

var globalMaxFeatures = 100000

func sMaxFeatures() int { return globalMaxFeatures }

func (s *server) absoluteURL(r *http.Request, path string) string {
	if base := strings.TrimRight(s.cfg.BaseURL, "/"); base != "" {
		return base + path
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func (s *server) apiURL(r *http.Request, st *catalogState, path string) string {
	if st != nil && st.name != "" && st.name != "default" {
		switch {
		case strings.HasPrefix(path, "/stac-api"):
			path = "/stac-api/catalogs/" + url.PathEscape(st.name) + strings.TrimPrefix(path, "/stac-api")
		case strings.HasPrefix(path, "/ogc"):
			path = "/ogc/catalogs/" + url.PathEscape(st.name) + strings.TrimPrefix(path, "/ogc")
		}
		return s.absoluteURL(r, path)
	}
	return s.absoluteURL(r, path)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONType(w, status, "application/json", v)
}

func writeJSONType(w http.ResponseWriter, status int, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": strconv.Itoa(status), "description": msg})
}
