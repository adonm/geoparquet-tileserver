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
	Name string
	Type string
	URL  string
}

type htmlLink struct {
	Name  string
	Title string
	URL   string
}

type htmlCollection struct {
	ID          string
	Title       string
	Description string
	Kind        string
	Bounds      string
	MinZoom     int
	MaxZoom     int
	PageURL     string
	TileJSONURL string
	TileURL     string
	ItemsURL    string
	STACURL     string
	Assets      []htmlAsset
	Children    []htmlLink
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
<style>
:root { color-scheme: light dark; --muted:#68707c; --line:#d7dbe2; --accent:#0b6bcb; }
body { font: 15px/1.45 system-ui, -apple-system, Segoe UI, sans-serif; margin: 0; padding: 2rem; max-width: 1100px; }
h1 { margin: 0 0 .25rem; font-size: 1.7rem; }
p.lede { color: var(--muted); margin: 0 0 1.25rem; }
a { color: var(--accent); }
nav { display: flex; flex-wrap: wrap; gap: .4rem; margin: 1rem 0 1.5rem; }
nav a { border: 1px solid var(--line); border-radius: 999px; padding: .25rem .7rem; text-decoration: none; }
nav a.current { background: var(--accent); border-color: var(--accent); color: white; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; vertical-align: top; padding: .55rem .5rem; border-bottom: 1px solid var(--line); }
th { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--muted); }
td.id { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
.kind { font-size: .75rem; border: 1px solid var(--line); border-radius: 4px; padding: .05rem .35rem; color: var(--muted); }
.muted { color: var(--muted); }
.links { display: flex; flex-wrap: wrap; gap: .45rem; }
.links a { text-decoration: none; }
code { font-size: .9em; }
</style>
</head>
<body>
<h1>GeoParquet Tile Server</h1>
<p class="lede">Browse STAC collections, virtual groups, and raw GeoParquet assets.</p>
<nav>
{{range .Catalogs}}<a href="{{.URL}}"{{if .Current}} class="current"{{end}}>{{.Name}}{{if .Title}} · {{.Title}}{{end}}</a>{{end}}
<a href="/stac{{.CatalogURL}}">Raw STAC</a>
<a href="/docs">API docs</a>
</nav>
<h2>Collections &amp; groups <span class="muted">({{len .Collections}})</span></h2>
<table>
<thead><tr><th>ID</th><th>Title</th><th>Kind</th><th>Bounds</th><th>Zooms</th><th>Links</th></tr></thead>
<tbody>
{{range .Collections}}
<tr>
<td class="id"><a href="{{.PageURL}}">{{.ID}}</a></td>
<td>{{.Title}}{{if .Description}}<div class="muted">{{.Description}}</div>{{end}}</td>
<td><span class="kind">{{.Kind}}</span></td>
<td class="muted"><code>{{.Bounds}}</code></td>
<td>{{.MinZoom}}–{{.MaxZoom}}</td>
<td><div class="links">
<a href="{{.TileJSONURL}}">TileJSON</a>
{{if .ItemsURL}}<a href="{{.ItemsURL}}">Items / assets</a>{{end}}
{{if .STACURL}}<a href="{{.STACURL}}">STAC</a>{{end}}
</div></td>
</tr>
{{end}}
</tbody>
</table>
<p class="muted">Tile URL template: <code>/collections/{collection}/tiles/{z}/{x}/{y}.mvt</code>
{{if .CatalogURL}} · catalog query: <code>{{.CatalogURL}}</code>{{end}}</p>
</body>
</html>
`))

var collectionTemplate = template.Must(template.New("collection").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --muted:#68707c; --line:#d7dbe2; --accent:#0b6bcb; }
body { font: 15px/1.5 system-ui, -apple-system, Segoe UI, sans-serif; margin: 0; padding: 2rem; max-width: 1000px; }
h1 { margin: 0 0 .2rem; font-size: 1.7rem; }
h2 { margin-top: 1.8rem; }
a { color: var(--accent); }
.muted { color: var(--muted); }
nav { display: flex; flex-wrap: wrap; gap: .6rem; margin: 1rem 0 1.5rem; }
.box { border: 1px solid var(--line); border-radius: 8px; padding: 1rem; margin: 1rem 0; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: .45rem .4rem; border-bottom: 1px solid var(--line); }
th { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--muted); }
code { font-size: .9em; }
.kind { font-size: .75rem; border: 1px solid var(--line); border-radius: 4px; padding: .05rem .35rem; color: var(--muted); }
</style>
</head>
<body>
<nav><a href="/">← Directory</a><a href="/collections.html{{.CatalogURL}}">Collections</a><a href="/docs">API docs</a></nav>
<h1>{{.Collection.Title}} <span class="kind">{{.Collection.Kind}}</span></h1>
<p class="muted"><code>{{.Collection.ID}}</code>{{if .Collection.Description}} · {{.Collection.Description}}{{end}}</p>
<div class="box">
<div><strong>Bounds</strong> <code>{{.Collection.Bounds}}</code></div>
<div><strong>Zoom</strong> {{.Collection.MinZoom}}–{{.Collection.MaxZoom}}</div>
<div class="links" style="margin-top:.7rem">
<a href="{{.Collection.TileJSONURL}}">TileJSON</a>
{{if .Collection.ItemsURL}}<a href="{{.Collection.ItemsURL}}">Items / raw assets</a>{{end}}
{{if .Collection.STACURL}}<a href="{{.Collection.STACURL}}">Raw STAC</a>{{end}}
</div>
</div>
<p><strong>Tile URL</strong><br><code>{{.Collection.TileURL}}</code></p>
{{if .Collection.Assets}}
<h2>Collection assets</h2>
<table><thead><tr><th>Name</th><th>Type</th><th>Passthrough</th></tr></thead><tbody>
{{range .Collection.Assets}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td><a href="{{.URL}}">open</a></td></tr>{{end}}
</tbody></table>
{{end}}
{{if .Collection.Children}}
<h2>Sub-collections</h2>
<ul>{{range .Collection.Children}}<li><a href="{{.URL}}">{{.Title}}</a> <span class="muted"><code>{{.Name}}</code></span></li>{{end}}</ul>
{{end}}
</body>
</html>
`))

var itemsTemplate = template.Must(template.New("items").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --muted:#68707c; --line:#d7dbe2; --accent:#0b6bcb; }
body { font: 15px/1.45 system-ui, -apple-system, Segoe UI, sans-serif; margin: 0; padding: 2rem; max-width: 1100px; }
a { color: var(--accent); }
h1 { margin: 0 0 1rem; font-size: 1.6rem; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; vertical-align: top; padding: .5rem .4rem; border-bottom: 1px solid var(--line); }
th { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--muted); }
.muted { color: var(--muted); }
code { font-size: .9em; }
</style>
</head>
<body>
<nav><a href="/">← Directory</a><a href="/collections/{{.Collection}}.html{{.Catalog}}">Collection</a></nav>
<h1>{{.Title}}</h1>
<table><thead><tr><th>Item</th><th>BBox</th><th>STAC</th><th>Raw assets</th></tr></thead><tbody>
{{range .Items}}<tr>
<td><code>{{.ID}}</code></td>
<td class="muted"><code>{{.BBox}}</code></td>
<td><a href="{{.STAC}}">STAC</a></td>
<td>{{range .Assets}}<a href="{{.URL}}">{{.Name}}</a> {{end}}</td>
</tr>{{end}}
</tbody></table>
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

func (s *server) htmlCollection(_ *catalogState, e *layerEntry, catalog string) htmlCollection {
	q := s.catalogQuery(catalog)
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
		hc.STACURL = base + "/collections/" + id + "/stac" + q
		hc.ItemsURL = base + "/collections/" + id + "/items.html" + q
		names := make([]string, 0, len(e.collection.Assets))
		for name := range e.collection.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			a := e.collection.Assets[name]
			hc.Assets = append(hc.Assets, htmlAsset{
				Name: name, Type: a.Type,
				URL: base + "/collections/" + id + "/assets/" + url.PathEscape(name) + q,
			})
		}
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
	page := itemsPage{Title: e.title + " items", Catalog: q, Collection: id}
	for _, it := range items {
		v := itemView{ID: it.ID, BBox: fmt.Sprintf("%v", it.BBox), STAC: "/collections/" + id + "/items/" + url.PathEscape(it.ID) + q}
		names := make([]string, 0, len(it.Assets))
		for name := range it.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v.Assets = append(v.Assets, htmlAsset{Name: name, URL: "/collections/" + id + "/items/" + url.PathEscape(it.ID) + "/assets/" + url.PathEscape(name) + q})
		}
		page.Items = append(page.Items, v)
	}
	s.writeHTML(w, http.StatusOK, itemsTemplate, page)
}
