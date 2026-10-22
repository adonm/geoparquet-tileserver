package main

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/paulmach/orb"
)

type htmlAsset struct {
	Name       string
	Type       string
	URL        string
	RustGISURL string
}

type htmlLink struct {
	Name  string
	Title string
	URL   string
}

type htmlCollection struct {
	ID                string
	Title             string
	Description       string
	Kind              string
	Bounds            string
	MinZoom           int
	MaxZoom           int
	PageURL           string
	TileJSONURL       string
	TileURL           string
	ItemsURL          string
	STACURL           string
	PMTilesURL        string
	ParquetURL        string
	RustGISURL        string
	RustGISParquetURL string
	Assets            []htmlAsset
	Children          []htmlLink
}

type htmlCatalog struct {
	Name        string
	Title       string
	URL         string
	Current     bool
	Collections int
}

type collectionsPage struct {
	Title       string
	Catalog     string
	CatalogURL  string
	Catalogs    []htmlCatalog
	Collections []htmlCollection
	RustGISURL  string
}

type itemView struct {
	ID     string
	BBox   string
	STAC   string
	Assets []htmlAsset
}

type itemsPage struct {
	Title      string
	Catalog    string
	Collection string
	Items      []itemView
}

type collectionPage struct {
	Title      string
	Catalog    string
	CatalogURL string
	Collection htmlCollection
}

var collectionsTemplate = template.Must(template.New("collections").Parse(`<!doctype html>
<html lang="en" data-bs-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/css/bootstrap.min.css" rel="stylesheet" crossorigin="anonymous">
<style>code { word-break: break-word; } .table td { vertical-align: middle; }</style>
</head>
<body class="bg-body-tertiary">
<nav class="navbar navbar-expand-lg bg-body border-bottom">
  <div class="container">
    <a class="navbar-brand fw-semibold" href="/">GeoParquet Tile Server</a>
    <div class="d-flex flex-wrap gap-2 align-items-center">
      <a class="btn btn-sm btn-outline-secondary" href="/docs">API docs</a>
    </div>
  </div>
</nav>
<main class="container py-4">
  <div class="d-flex flex-wrap justify-content-between align-items-start gap-2 mb-3">
    <div>
      <h1 class="h3 mb-1">Browse catalog</h1>
      <p class="text-secondary mb-0">Collections, virtual groups, tiles, STAC documents, and raw assets.</p>
    </div>
    <div class="btn-group btn-group-sm" role="group" aria-label="Catalog">
      {{range .Catalogs}}<a class="btn {{if .Current}}btn-dark{{else}}btn-outline-dark{{end}}" href="{{.URL}}">{{.Name}}{{if .Title}} · {{.Title}}{{end}}</a>{{end}}
    </div>
  </div>
  <div class="card shadow-sm">
    <div class="card-header d-flex justify-content-between align-items-center">
      <span class="fw-semibold">Collections &amp; groups</span>
      <span class="badge text-bg-secondary">{{len .Collections}}</span>
    </div>
    <div class="table-responsive">
      <table class="table table-hover align-middle mb-0">
        <thead class="table-light"><tr><th>ID</th><th>Title</th><th>Kind</th><th>Bounds</th><th>Zooms</th><th>Links</th></tr></thead>
        <tbody>
        {{range .Collections}}
          <tr>
            <td><a class="fw-semibold text-decoration-none" href="{{.PageURL}}"><code>{{.ID}}</code></a></td>
            <td>{{.Title}}{{if .Description}}<div class="small text-secondary">{{.Description}}</div>{{end}}</td>
            <td><span class="badge {{if eq .Kind "group"}}text-bg-warning{{else}}text-bg-info{{end}}">{{.Kind}}</span></td>
            <td class="small text-secondary"><code>{{.Bounds}}</code></td>
            <td class="small">{{.MinZoom}}–{{.MaxZoom}}</td>
            <td>
              <div class="d-flex flex-wrap gap-2 small">
                <a href="{{.PageURL}}">Browse</a>
                <a href="{{.TileJSONURL}}">TileJSON</a>
                {{if .ItemsURL}}<a href="{{.ItemsURL}}">Items / assets</a>{{end}}
                {{if .STACURL}}<a href="{{.STACURL}}">STAC</a>{{end}}
                {{if .RustGISURL}}<a href="{{.RustGISURL}}" target="_blank" rel="noopener">Tiles in RustGIS ↗</a>{{end}}
                {{if .RustGISParquetURL}}<a href="{{.RustGISParquetURL}}" target="_blank" rel="noopener">Parquet in RustGIS ↗</a>{{end}}
              </div>
            </td>
          </tr>
        {{end}}
        </tbody>
      </table>
    </div>
  </div>
  <p class="small text-secondary mt-3 mb-0">Tile URL template: <code>/collections/{collection}/tiles/{z}/{x}/{y}.mvt</code></p>
</main>
<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/js/bootstrap.bundle.min.js" crossorigin="anonymous"></script>
</body>
</html>
`))

var collectionTemplate = template.Must(template.New("collection").Parse(`<!doctype html>
<html lang="en" data-bs-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/css/bootstrap.min.css" rel="stylesheet" crossorigin="anonymous">
<style>code { word-break: break-word; }</style>
</head>
<body class="bg-body-tertiary">
<nav class="navbar navbar-expand-lg bg-body border-bottom">
  <div class="container">
    <a class="navbar-brand fw-semibold" href="/">GeoParquet Tile Server</a>
    <div class="d-flex flex-wrap gap-2">
      <a class="btn btn-sm btn-outline-secondary" href="/collections.html{{.CatalogURL}}">Collections</a>
      <a class="btn btn-sm btn-outline-secondary" href="/docs">API docs</a>
    </div>
  </div>
</nav>
<main class="container py-4">
  <nav aria-label="breadcrumb"><ol class="breadcrumb"><li class="breadcrumb-item"><a href="/">Directory</a></li><li class="breadcrumb-item active" aria-current="page">{{.Collection.ID}}</li></ol></nav>
  <div class="d-flex flex-wrap justify-content-between align-items-start gap-2 mb-3">
    <div>
      <h1 class="h3 mb-1">{{.Collection.Title}} <span class="badge {{if eq .Collection.Kind "group"}}text-bg-warning{{else}}text-bg-info{{end}}">{{.Collection.Kind}}</span></h1>
      <div class="text-secondary"><code>{{.Collection.ID}}</code>{{if .Collection.Description}} · {{.Collection.Description}}{{end}}</div>
    </div>
    <div class="d-flex flex-wrap gap-2">
      <a class="btn btn-sm btn-outline-primary" href="{{.Collection.TileJSONURL}}">TileJSON</a>
      {{if .Collection.PMTilesURL}}<a class="btn btn-sm btn-outline-primary" href="{{.Collection.PMTilesURL}}">PMTiles passthrough</a>{{end}}
      {{if .Collection.ItemsURL}}<a class="btn btn-sm btn-outline-primary" href="{{.Collection.ItemsURL}}">Items / raw assets</a>{{end}}
      {{if .Collection.STACURL}}<a class="btn btn-sm btn-outline-primary" href="{{.Collection.STACURL}}">Raw STAC</a>{{end}}
      {{if .Collection.RustGISParquetURL}}<a class="btn btn-sm btn-success" href="{{.Collection.RustGISParquetURL}}" target="_blank" rel="noopener">Open Parquet in RustGIS ↗</a>{{end}}
      {{if .Collection.RustGISURL}}<a class="btn btn-sm btn-primary" href="{{.Collection.RustGISURL}}" target="_blank" rel="noopener">Open tiles in RustGIS ↗</a>{{end}}
    </div>
  </div>
  <div class="row g-3">
    <div class="col-lg-7">
      <div class="card shadow-sm h-100">
        <div class="card-body">
          <h2 class="h6 text-uppercase text-secondary">Tile URL</h2>
          <code>{{.Collection.TileURL}}</code>
          <hr>
          <dl class="row mb-0 small">
            <dt class="col-sm-3">Bounds</dt><dd class="col-sm-9"><code>{{.Collection.Bounds}}</code></dd>
            <dt class="col-sm-3">Zooms</dt><dd class="col-sm-9">{{.Collection.MinZoom}}–{{.Collection.MaxZoom}}</dd>
          </dl>
        </div>
      </div>
    </div>
    <div class="col-lg-5">
      <div class="card shadow-sm h-100">
        <div class="card-body">
          <h2 class="h6 text-uppercase text-secondary">STAC / passthrough</h2>
          <p class="small text-secondary">Raw documents and assets are streamed by this server. Remote assets support HTTP range requests.</p>
          <div class="d-grid gap-2">
            {{if .Collection.STACURL}}<a class="btn btn-sm btn-outline-secondary text-start" href="{{.Collection.STACURL}}">Raw STAC document</a>{{end}}
            {{if .Collection.ItemsURL}}<a class="btn btn-sm btn-outline-secondary text-start" href="{{.Collection.ItemsURL}}">Item and asset directory</a>{{end}}
          </div>
        </div>
      </div>
    </div>
  </div>
  {{if .Collection.Assets}}
  <h2 class="h5 mt-4">Collection assets</h2>
  <div class="card shadow-sm"><div class="table-responsive"><table class="table table-sm mb-0">
    <thead class="table-light"><tr><th>Name</th><th>Type</th><th>Passthrough</th><th>RustGIS</th></tr></thead>
    <tbody>{{range .Collection.Assets}}<tr><td><code>{{.Name}}</code></td><td class="text-secondary">{{.Type}}</td><td><a href="{{.URL}}">open</a></td><td>{{if .RustGISURL}}<a href="{{.RustGISURL}}" target="_blank" rel="noopener">open data ↗</a>{{else}}<span class="text-secondary">—</span>{{end}}</td></tr>{{end}}</tbody>
  </table></div></div>
  {{end}}
  {{if .Collection.Children}}
  <h2 class="h5 mt-4">Sub-collections</h2>
  <div class="list-group shadow-sm">
    {{range .Collection.Children}}<a class="list-group-item list-group-item-action d-flex justify-content-between align-items-center" href="{{.URL}}"><span>{{.Title}} <span class="text-secondary"><code>{{.Name}}</code></span></span><span>→</span></a>{{end}}
  </div>
  {{end}}
</main>
<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/js/bootstrap.bundle.min.js" crossorigin="anonymous"></script>
</body>
</html>
`))

var itemsTemplate = template.Must(template.New("items").Parse(`<!doctype html>
<html lang="en" data-bs-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/css/bootstrap.min.css" rel="stylesheet" crossorigin="anonymous">
<style>code { word-break: break-word; }</style>
</head>
<body class="bg-body-tertiary">
<nav class="navbar navbar-expand-lg bg-body border-bottom">
  <div class="container">
    <a class="navbar-brand fw-semibold" href="/">GeoParquet Tile Server</a>
    <div class="d-flex gap-2">
      <a class="btn btn-sm btn-outline-secondary" href="/collections/{{.Collection}}.html{{.Catalog}}">Collection</a>
      <a class="btn btn-sm btn-outline-secondary" href="/docs">API docs</a>
    </div>
  </div>
</nav>
<main class="container py-4">
  <h1 class="h3 mb-3">{{.Title}}</h1>
  <div class="card shadow-sm"><div class="table-responsive"><table class="table table-hover align-middle mb-0">
    <thead class="table-light"><tr><th>Item</th><th>BBox</th><th>STAC</th><th>Raw assets</th><th>RustGIS</th></tr></thead>
    <tbody>{{range .Items}}<tr>
      <td><code>{{.ID}}</code></td>
      <td class="small text-secondary"><code>{{.BBox}}</code></td>
      <td><a href="{{.STAC}}">STAC</a></td>
      <td class="small">{{range .Assets}}<a class="me-2" href="{{.URL}}">{{.Name}}</a>{{end}}</td>
      <td class="small">{{range .Assets}}{{if .RustGISURL}}<a class="me-2" href="{{.RustGISURL}}" target="_blank" rel="noopener">{{.Name}} ↗</a>{{end}}{{end}}</td>
    </tr>{{end}}</tbody>
  </table></div></div>
</main>
<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.8/dist/js/bootstrap.bundle.min.js" crossorigin="anonymous"></script>
</body>
</html>
`))

func (s *server) writeHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.Execute(w, data)
}

func (s *server) catalogQuery(name string) string {
	if name == "" || name == "default" {
		return ""
	}
	return "?catalog=" + url.QueryEscape(name)
}

func catalogPathPrefix(name string) string {
	if name == "" || name == "default" {
		return ""
	}
	return "/catalogs/" + url.PathEscape(name)
}

func rustgisURL(param, value string) string {
	if value == "" {
		return ""
	}
	return "https://web.rustgis.app/?" + param + "=" + url.QueryEscape(value)
}

func rustgisTilesURL(template string, b orb.Bound) string {
	u := rustgisURL("tiles", template)
	if u == "" {
		return ""
	}
	span := math.Max(b.Max.X()-b.Min.X(), b.Max.Y()-b.Min.Y())
	z := 4
	if span > 0 {
		z = int(math.Log2(360/span)) + 1
	}
	if z < 0 {
		z = 0
	}
	if z > 14 {
		z = 14
	}
	lat := (b.Min.Y() + b.Max.Y()) / 2
	lon := (b.Min.X() + b.Max.X()) / 2
	return fmt.Sprintf("%s#%d/%.4f/%.4f", u, z, lat, lon)
}

func rustgisDataURL(assetURL string) string {
	return rustgisURL("data", assetURL)
}

func isDataAsset(a htmlAsset) bool {
	name := strings.ToLower(a.Name)
	typ := strings.ToLower(a.Type)
	if strings.Contains(typ, "parquet") || strings.Contains(typ, "pmtiles") {
		return true
	}
	return strings.Contains(name, ".parquet") || strings.Contains(name, "parquet") || strings.HasSuffix(name, ".pmtiles") || strings.Contains(name, "pmtiles")
}

func dataAssetSuffix(a htmlAsset) string {
	name := strings.ToLower(a.Name)
	typ := strings.ToLower(a.Type)
	switch {
	case strings.Contains(typ, "pmtiles") || strings.Contains(name, "pmtiles"):
		return ".pmtiles"
	case strings.Contains(typ, "parquet") || strings.Contains(name, "parquet"):
		return ".parquet"
	}
	return ""
}

func (s *server) htmlCollection(_ *catalogState, e *layerEntry, catalog string) htmlCollection {
	q := s.catalogQuery(catalog)
	prefix := catalogPathPrefix(catalog)
	assetQ := q
	if prefix != "" {
		assetQ = ""
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	id := url.PathEscape(e.name)
	b := e.bounds()
	hc := htmlCollection{
		ID: e.name, Title: e.title, Description: e.description(), Kind: e.kind,
		Bounds:  fmt.Sprintf("[%g, %g, %g, %g]", b.Min.X(), b.Min.Y(), b.Max.X(), b.Max.Y()),
		MinZoom: e.minZoom(), MaxZoom: e.maxZoom(),
		PageURL:     "/collections/" + id + ".html" + q,
		TileJSONURL: base + "/collections/" + id + "/tilejson.json" + q,
		TileURL:     base + "/collections/" + id + "/tiles/{z}/{x}/{y}.mvt" + q,
	}
	if e.collection != nil {
		hc.STACURL = base + prefix + "/collections/" + id + "/stac" + assetQ
		hc.ItemsURL = base + "/collections/" + id + "/items.html" + q
		names := make([]string, 0, len(e.collection.Assets))
		for name := range e.collection.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			a := e.collection.Assets[name]
			assetURL := base + prefix + "/collections/" + id + "/assets/" + url.PathEscape(name) + assetQ
			ha := htmlAsset{Name: name, Type: a.Type, URL: assetURL}
			if suffix := dataAssetSuffix(ha); suffix != "" {
				assetURL = base + prefix + "/collections/" + id + "/assets/" + url.PathEscape(name) + suffix + assetQ
				ha.URL = assetURL
			}
			if isDataAsset(ha) {
				ha.RustGISURL = rustgisDataURL(assetURL)
			}
			hc.Assets = append(hc.Assets, ha)
		}
		var fallback string
		for _, a := range hc.Assets {
			if !strings.HasSuffix(strings.ToLower(a.URL), ".parquet") {
				continue
			}
			if !strings.Contains(strings.ToLower(a.Name), "hex") {
				hc.ParquetURL = a.URL
				break
			}
			if fallback == "" {
				fallback = a.URL
			}
		}
		if hc.ParquetURL == "" {
			hc.ParquetURL = fallback
		}
		if hc.ParquetURL != "" {
			hc.RustGISParquetURL = rustgisDataURL(hc.ParquetURL)
		}
	}
	if e.group != nil && e.group.Base != "" {
		hc.STACURL = base + prefix + "/collections/" + id + "/stac" + assetQ
	}
	if e.group != nil && e.group.PMTiles != "" {
		hc.PMTilesURL = base + prefix + "/collections/" + id + "/pmtiles.pmtiles" + assetQ
		hc.RustGISURL = rustgisDataURL(hc.PMTilesURL)
	}
	if hc.TileURL != "" && hc.RustGISURL == "" {
		hc.RustGISURL = rustgisTilesURL(hc.TileURL, b)
	}
	if e.group != nil {
		for _, child := range e.group.Collections {
			hc.Children = append(hc.Children, htmlLink{
				Name: child.ID, Title: child.Title,
				URL: "/collections/" + url.PathEscape(child.ID) + ".html" + q,
			})
		}
		sort.Slice(hc.Children, func(i, j int) bool { return hc.Children[i].Name < hc.Children[j].Name })
	}
	return hc
}

func (s *server) htmlCatalogList(selected string) ([]htmlCatalog, string) {
	var out []htmlCatalog
	for _, name := range s.catalogNames() {
		st := s.states[name]
		href := "/collections.html"
		if name != "default" {
			href += "?catalog=" + url.QueryEscape(name)
		}
		title := st.cat.Title
		if title == "" {
			title = st.cat.ID
		}
		out = append(out, htmlCatalog{
			Name: name, Title: title, URL: href,
			Current:     name == selected || (selected == "" && name == "default"),
			Collections: len(st.layers),
		})
	}
	return out, s.catalogQuery(selected)
}

func (s *server) collectionsHTML(w http.ResponseWriter, r *http.Request) {
	selected := r.URL.Query().Get("catalog")
	st := s.state(selected)
	if st == nil {
		http.NotFound(w, r)
		return
	}
	catalogs, q := s.htmlCatalogList(selected)
	page := collectionsPage{
		Title: "Collections · GeoParquet Tile Server", Catalog: selected, CatalogURL: q, Catalogs: catalogs,
	}
	for _, name := range st.order {
		page.Collections = append(page.Collections, s.htmlCollection(st, st.layers[name], selected))
	}
	s.writeHTML(w, http.StatusOK, collectionsTemplate, page)
}

func (s *server) indexHTML(w http.ResponseWriter, r *http.Request) {
	s.collectionsHTML(w, r)
}

func (s *server) collectionHTML(w http.ResponseWriter, r *http.Request) {
	selected := r.URL.Query().Get("catalog")
	st := s.state(selected)
	if st == nil {
		http.NotFound(w, r)
		return
	}
	e, ok := st.layers[chi.URLParam(r, "collection")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	catalogs, q := s.htmlCatalogList(selected)
	_ = catalogs
	s.writeHTML(w, http.StatusOK, collectionTemplate, collectionPage{
		Title: e.title + " · GeoParquet Tile Server", Catalog: selected, CatalogURL: q,
		Collection: s.htmlCollection(st, e, selected),
	})
}

func (s *server) itemsHTML(w http.ResponseWriter, r *http.Request) {
	selected := r.URL.Query().Get("catalog")
	_, e, err := s.entry(selected, chi.URLParam(r, "collection"))
	if err != nil || e.collection == nil {
		http.NotFound(w, r)
		return
	}
	items, err := e.collection.Items()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	q := s.catalogQuery(selected)
	id := url.PathEscape(e.name)
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	prefix := catalogPathPrefix(selected)
	assetQ := q
	if prefix != "" {
		assetQ = ""
	}
	page := itemsPage{Title: e.title + " items", Catalog: q, Collection: id}
	for _, it := range items {
		v := itemView{ID: it.ID, BBox: fmt.Sprintf("%v", it.BBox), STAC: "/collections/" + id + "/items/" + url.PathEscape(it.ID) + q}
		names := make([]string, 0, len(it.Assets))
		for name := range it.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			assetURL := base + prefix + "/collections/" + id + "/items/" + url.PathEscape(it.ID) + "/assets/" + url.PathEscape(name) + assetQ
			ha := htmlAsset{Name: name, URL: assetURL, Type: it.Assets[name].Type}
			if suffix := dataAssetSuffix(ha); suffix != "" {
				assetURL = base + prefix + "/collections/" + id + "/items/" + url.PathEscape(it.ID) + "/assets/" + url.PathEscape(name) + suffix + assetQ
				ha.URL = assetURL
			}
			if isDataAsset(ha) {
				ha.RustGISURL = rustgisDataURL(assetURL)
			}
			v.Assets = append(v.Assets, ha)
		}
		page.Items = append(page.Items, v)
	}
	s.writeHTML(w, http.StatusOK, itemsTemplate, page)
}
