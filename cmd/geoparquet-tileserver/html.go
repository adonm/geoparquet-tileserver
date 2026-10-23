package main

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
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
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.css" crossorigin="anonymous">
<script src="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.js" crossorigin="anonymous" defer></script>
<style>
@layer utilities {
  code { word-break: break-word; }
  .site-nav { border-block-end: 1px solid var(--border); padding-block: var(--space-3); }
  .site-main { padding-block-end: var(--space-8); }
  .page-title { margin-block-start: 0; }
  .card-title { margin-block: 0; }
  .h-100 { height: 100%; }
  .table td { vertical-align: middle; }
}
</style>
</head>
<body>
<header class="site-nav">
  <div class="container hstack justify-between">
    <a class="unstyled" href="/"><strong>GeoParquet Tile Server</strong></a>
    <nav class="hstack gap-2" aria-label="Primary">
      <a class="button outline small" href="/docs">API docs</a>
    </nav>
  </div>
</header>
<main class="container site-main">
  <div class="hstack justify-between mb-4">
    <div>
      <h1 class="page-title">Browse catalog</h1>
      <p class="text-light">Collections, virtual groups, tiles, STAC documents, and raw assets.</p>
    </div>
    <menu class="buttons" aria-label="Catalog">
      {{range .Catalogs}}<li><a class="button small{{if .Current}}{{else}} outline{{end}}" href="{{.URL}}"{{if .Current}} aria-current="page"{{end}}>{{.Name}}{{if .Title}} · {{.Title}}{{end}}</a></li>{{end}}
    </menu>
  </div>
  <article class="card">
    <header class="hstack justify-between mb-4">
      <h3 class="card-title">Collections &amp; groups</h3>
      <span class="badge outline">{{len .Collections}}</span>
    </header>
    <div class="table">
      <table>
        <thead><tr><th>ID</th><th>Title</th><th>Kind</th><th>Bounds</th><th>Zooms</th><th>Links</th></tr></thead>
        <tbody>
        {{range .Collections}}
          <tr>
            <td><a href="{{.PageURL}}"><code>{{.ID}}</code></a></td>
            <td><strong>{{.Title}}</strong>{{if .Description}}<br><small class="text-light">{{.Description}}</small>{{end}}</td>
            <td><span class="badge" data-variant="{{if eq .Kind "group"}}warning{{else}}secondary{{end}}">{{.Kind}}</span></td>
            <td><small class="text-light"><code>{{.Bounds}}</code></small></td>
            <td><small>{{.MinZoom}}–{{.MaxZoom}}</small></td>
            <td>
              <div class="hstack gap-2">
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
  </article>
  <p class="text-light mt-4"><small>Tile URL template: <code>/collections/{collection}/tiles/{z}/{x}/{y}.mvt</code></small></p>
</main>
</body>
</html>
`))

var collectionTemplate = template.Must(template.New("collection").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.css" crossorigin="anonymous">
<script src="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.js" crossorigin="anonymous" defer></script>
<style>
@layer utilities {
  code { word-break: break-word; }
  .site-nav { border-block-end: 1px solid var(--border); padding-block: var(--space-3); }
  .site-main { padding-block-end: var(--space-8); }
  .page-title { margin-block-start: 0; }
  .card-title { margin-block: 0; }
  .h-100 { height: 100%; }
  .table td { vertical-align: middle; }
}
</style>
</head>
<body>
<header class="site-nav">
  <div class="container hstack justify-between">
    <a class="unstyled" href="/"><strong>GeoParquet Tile Server</strong></a>
    <nav class="hstack gap-2" aria-label="Primary">
      <a class="button outline small" href="/collections.html{{.CatalogURL}}">Collections</a>
      <a class="button outline small" href="/docs">API docs</a>
    </nav>
  </div>
</header>
<main class="container site-main">
  <nav aria-label="Breadcrumb">
    <ol class="unstyled hstack" style="font-size: var(--text-7)">
      <li><a class="unstyled" href="/">Directory</a></li>
      <li aria-hidden="true">/</li>
      <li><span aria-current="page"><strong>{{.Collection.ID}}</strong></span></li>
    </ol>
  </nav>
  <div class="hstack justify-between mb-4">
    <div>
      <h1 class="page-title">{{.Collection.Title}} <span class="badge" data-variant="{{if eq .Collection.Kind "group"}}warning{{else}}secondary{{end}}">{{.Collection.Kind}}</span></h1>
      <p class="text-light"><code>{{.Collection.ID}}</code>{{if .Collection.Description}} · {{.Collection.Description}}{{end}}</p>
    </div>
    <div class="hstack gap-2">
      <a class="button outline small" href="{{.Collection.TileJSONURL}}">TileJSON</a>
      {{if .Collection.PMTilesURL}}<a class="button outline small" href="{{.Collection.PMTilesURL}}">PMTiles passthrough</a>{{end}}
      {{if .Collection.ItemsURL}}<a class="button outline small" href="{{.Collection.ItemsURL}}">Items / raw assets</a>{{end}}
      {{if .Collection.STACURL}}<a class="button outline small" href="{{.Collection.STACURL}}">Raw STAC</a>{{end}}
      {{if .Collection.RustGISParquetURL}}<a class="button outline small" href="{{.Collection.RustGISParquetURL}}" target="_blank" rel="noopener">Open Parquet in RustGIS ↗</a>{{end}}
      {{if .Collection.RustGISURL}}<a class="button small" href="{{.Collection.RustGISURL}}" target="_blank" rel="noopener">Open tiles in RustGIS ↗</a>{{end}}
    </div>
  </div>
  <div class="row">
    <div class="col-7">
      <article class="card h-100">
        <h3 class="card-title">Tile URL</h3>
        <p class="mt-2"><code>{{.Collection.TileURL}}</code></p>
        <div class="vstack gap-2">
          <div class="hstack justify-between"><small class="text-light">Bounds</small><code>{{.Collection.Bounds}}</code></div>
          <div class="hstack justify-between"><small class="text-light">Zooms</small><span>{{.Collection.MinZoom}}–{{.Collection.MaxZoom}}</span></div>
        </div>
      </article>
    </div>
    <div class="col-5">
      <article class="card h-100">
        <h3 class="card-title">STAC / passthrough</h3>
        <p class="text-light mt-2"><small>Raw documents and assets are streamed by this server. Remote assets support HTTP range requests.</small></p>
        <div class="vstack gap-2">
          {{if .Collection.STACURL}}<a class="button outline small w-100" href="{{.Collection.STACURL}}">Raw STAC document</a>{{end}}
          {{if .Collection.ItemsURL}}<a class="button outline small w-100" href="{{.Collection.ItemsURL}}">Item and asset directory</a>{{end}}
        </div>
      </article>
    </div>
  </div>
  {{if .Collection.Assets}}
  <h2>Collection assets</h2>
  <article class="card">
    <div class="table">
      <table>
        <thead><tr><th>Name</th><th>Type</th><th>Passthrough</th><th>RustGIS</th></tr></thead>
        <tbody>{{range .Collection.Assets}}<tr>
          <td><code>{{.Name}}</code></td>
          <td><small class="text-light">{{.Type}}</small></td>
          <td><a href="{{.URL}}">open</a></td>
          <td>{{if .RustGISURL}}<a href="{{.RustGISURL}}" target="_blank" rel="noopener">open data ↗</a>{{else}}<small class="text-light">—</small>{{end}}</td>
        </tr>{{end}}</tbody>
      </table>
    </div>
  </article>
  {{end}}
  {{if .Collection.Children}}
  <h2>Sub-collections</h2>
  <article class="card">
    <div class="vstack gap-2">
      {{range .Collection.Children}}<a class="hstack justify-between" href="{{.URL}}"><span><strong>{{.Title}}</strong> <small class="text-light"><code>{{.Name}}</code></small></span><span aria-hidden="true">→</span></a>{{end}}
    </div>
  </article>
  {{end}}
</main>
</body>
</html>
`))

var itemsTemplate = template.Must(template.New("items").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.css" crossorigin="anonymous">
<script src="https://cdn.jsdelivr.net/npm/@knadh/oat@1.0.0/oat.min.js" crossorigin="anonymous" defer></script>
<style>
@layer utilities {
  code { word-break: break-word; }
  .site-nav { border-block-end: 1px solid var(--border); padding-block: var(--space-3); }
  .site-main { padding-block-end: var(--space-8); }
  .page-title { margin-block-start: 0; }
  .table td { vertical-align: middle; }
}
</style>
</head>
<body>
<header class="site-nav">
  <div class="container hstack justify-between">
    <a class="unstyled" href="/"><strong>GeoParquet Tile Server</strong></a>
    <nav class="hstack gap-2" aria-label="Primary">
      <a class="button outline small" href="/collections/{{.Collection}}.html{{.Catalog}}">Collection</a>
      <a class="button outline small" href="/docs">API docs</a>
    </nav>
  </div>
</header>
<main class="container site-main">
  <h1 class="page-title">{{.Title}}</h1>
  <article class="card">
    <div class="table">
      <table>
        <thead><tr><th>Item</th><th>BBox</th><th>STAC</th><th>Raw assets</th><th>RustGIS</th></tr></thead>
        <tbody>{{range .Items}}<tr>
          <td><code>{{.ID}}</code></td>
          <td><small class="text-light"><code>{{.BBox}}</code></small></td>
          <td><a href="{{.STAC}}">STAC</a></td>
          <td><div class="hstack gap-2">{{range .Assets}}<a href="{{.URL}}">{{.Name}}</a>{{end}}</div></td>
          <td><div class="hstack gap-2">{{range .Assets}}{{if .RustGISURL}}<a href="{{.RustGISURL}}" target="_blank" rel="noopener">{{.Name}} ↗</a>{{end}}{{end}}</div></td>
        </tr>{{end}}</tbody>
      </table>
    </div>
  </article>
</main>
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

func rustgisStyleURL(styleURL string) string {
	if styleURL == "" {
		return ""
	}
	return "https://web.rustgis.app/?blank&style=" + url.QueryEscape(styleURL)
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
	if hc.RustGISURL == "" {
		styleURL := base + prefix + "/collections/" + id + "/style.json" + assetQ
		hc.RustGISURL = rustgisStyleURL(styleURL)
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
