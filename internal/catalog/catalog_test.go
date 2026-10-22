package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestLoadPicksLatestReleaseAndLoadsItemsLazily(t *testing.T) {
	var itemRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/catalog.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"type": "Catalog", "id": "releases",
			"links": []map[string]string{
				{"rel": "child", "href": "v1/catalog.json", "title": "2025-01-01.0"},
				{"rel": "child", "href": "v2/catalog.json", "title": "2026-02-02.0"},
			},
		})
	})
	mux.HandleFunc("/v1/catalog.json", func(w http.ResponseWriter, r *http.Request) {
		t.Error("old release should not be fetched")
		http.Error(w, "old", http.StatusInternalServerError)
	})
	mux.HandleFunc("/v2/catalog.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"type": "Catalog", "id": "2026-02-02.0",
			"links": []map[string]string{{"rel": "child", "href": "places/collection.json"}},
		})
	})
	mux.HandleFunc("/v2/places/collection.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"type": "Collection", "id": "places", "title": "Places",
			"extent": map[string]any{"spatial": map[string]any{"bbox": [][]float64{{-10, -10, 10, 10}}}},
			"links": []map[string]string{
				{"rel": "item", "href": "items/a.json"},
				{"rel": "item", "href": "items/b.json"},
			},
		})
	})
	mux.HandleFunc("/v2/places/items/a.json", func(w http.ResponseWriter, r *http.Request) {
		itemRequests.Add(1)
		writeJSON(w, map[string]any{"type": "Feature", "id": "a", "bbox": []float64{0, 0, 1, 1}, "assets": map[string]any{"data": map[string]any{"href": "a.parquet", "type": "application/vnd.apache.parquet"}}})
	})
	mux.HandleFunc("/v2/places/items/b.json", func(w http.ResponseWriter, r *http.Request) {
		itemRequests.Add(1)
		writeJSON(w, map[string]any{"type": "Feature", "id": "b", "bbox": []float64{1, 1, 2, 2}, "assets": map[string]any{"data": map[string]any{"href": "b.parquet", "type": "application/vnd.apache.parquet"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cat, err := Load(srv.URL + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Collections) != 1 || cat.Collections[0].ID != "places" {
		t.Fatalf("collections = %#v", cat.Collections)
	}
	if got := itemRequests.Load(); got != 0 {
		t.Fatalf("item requests before Items() = %d, want 0", got)
	}
	items, err := cat.Collections[0].Items()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "a" || items[1].ID != "b" {
		t.Fatalf("items = %#v", items)
	}
	if got := itemRequests.Load(); got != 2 {
		t.Fatalf("item requests = %d, want 2", got)
	}
}

func TestLoadBuildsGroupFromDirectChildCollections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/theme/catalog.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"type": "Catalog", "id": "theme", "title": "Theme",
			"links": []map[string]string{
				{"rel": "child", "href": "a.json"},
				{"rel": "child", "href": "b.json"},
			},
		})
	})
	for _, id := range []string{"a", "b"} {
		id := id
		mux.HandleFunc("/theme/"+id+".json", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"type": "Collection", "id": id, "title": "Collection " + id,
				"extent": map[string]any{"spatial": map[string]any{"bbox": [][]float64{{0, 0, 1, 1}}}},
				"assets": map[string]any{"data": map[string]any{"href": id + ".parquet", "type": "application/vnd.apache.parquet"}},
			})
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cat, err := Load(srv.URL + "/theme/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Groups) != 1 || cat.Groups[0].ID != "theme" {
		t.Fatalf("groups = %#v", cat.Groups)
	}
	group := cat.Groups[0]
	if len(group.Collections) != 2 {
		t.Fatalf("group collections = %d, want 2", len(group.Collections))
	}
	items, err := group.Items()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("group items = %d, want 2", len(items))
	}
}
