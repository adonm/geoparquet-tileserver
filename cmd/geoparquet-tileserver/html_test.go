package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

func TestHTMLTemplatesUseOat(t *testing.T) {
	collection := htmlCollection{
		ID: "place", Title: "Place", Kind: "collection", Bounds: "[0, 0, 1, 1]",
		MinZoom: 0, MaxZoom: 14,
		PageURL: "/collections/place.html", TileJSONURL: "/tilejson.json",
		TileURL: "/collections/place/tiles/{z}/{x}/{y}.mvt",
		Assets:  []htmlAsset{{Name: "data.parquet", Type: "application/vnd.apache.parquet", URL: "/data.parquet"}},
		Children: []htmlLink{
			{Name: "child", Title: "Child", URL: "/collections/child.html"},
		},
	}
	tests := []struct {
		name string
		tmpl *template.Template
		data any
	}{
		{"collections", collectionsTemplate, collectionsPage{
			Title:    "Collections",
			Catalogs: []htmlCatalog{{Name: "default", Title: "Default", URL: "/collections.html", Current: true}},
			Collections: []htmlCollection{
				collection,
			},
		}},
		{"collection", collectionTemplate, collectionPage{Title: "Place", Collection: collection}},
		{"items", itemsTemplate, itemsPage{
			Title: "Place items", Collection: "place",
			Items: []itemView{{ID: "item", BBox: "[]", STAC: "/item", Assets: []htmlAsset{
				{Name: "data.parquet", URL: "/data.parquet", RustGISURL: "/rustgis"},
			}}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tt.tmpl.Execute(&buf, tt.data); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if !strings.Contains(out, "@knadh/oat@1.0.0/oat.min.css") {
				t.Error("missing pinned Oat stylesheet")
			}
			if strings.Contains(strings.ToLower(out), "bootstrap") {
				t.Error("output still contains Bootstrap")
			}
			if !strings.Contains(out, "<main class=\"container site-main\">") {
				t.Error("missing Oat main container")
			}
		})
	}
}
