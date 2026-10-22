// Package catalog loads a STAC Collection/Catalog/Item tree and exposes the
// GeoParquet items needed by the tile server.
package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/paulmach/orb"
)

type Link struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
	Type string `json:"type"`
}

type Asset struct {
	Href  string   `json:"href"`
	Type  string   `json:"type"`
	Roles []string `json:"roles"`
}

type Item struct {
	Type       string           `json:"type"`
	ID         string           `json:"id"`
	Collection string           `json:"collection"`
	BBox       []float64        `json:"bbox"`
	Assets     map[string]Asset `json:"assets"`
	Links      []Link           `json:"links"`
	Properties map[string]any   `json:"properties"`
	Base       string           `json:"-"`
}

type Catalog struct {
	Items []Item
}

func Load(path string) (*Catalog, error) {
	seen := map[string]bool{}
	var items []Item
	var walk func(string, int) error
	walk = func(p string, depth int) error {
		if depth > 16 || seen[p] {
			return nil
		}
		seen[p] = true
		data, err := Fetch(p)
		if err != nil {
			return err
		}
		var head struct {
			Type   string           `json:"type"`
			ID     string           `json:"id"`
			Links  []Link           `json:"links"`
			BBox   []float64        `json:"bbox"`
			Assets map[string]Asset `json:"assets"`
			Extent struct {
				Spatial struct {
					BBox [][]float64 `json:"bbox"`
				} `json:"spatial"`
			} `json:"extent"`
			Collection string `json:"collection"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			return err
		}
		if head.Type == "Feature" {
			var it Item
			if err := json.Unmarshal(data, &it); err != nil {
				return err
			}
			it.Base = p
			items = append(items, it)
			return nil
		}
		// Collection-level GeoParquet assets (e.g. fiboa collections).
		if len(head.Assets) > 0 {
			it := Item{Type: "Feature", ID: head.ID, Collection: head.ID, Assets: head.Assets, Base: p}
			if len(head.BBox) >= 4 {
				it.BBox = head.BBox
			} else if len(head.Extent.Spatial.BBox) > 0 {
				it.BBox = head.Extent.Spatial.BBox[0]
			}
			items = append(items, it)
		}
		for _, l := range head.Links {
			if l.Rel != "item" && l.Rel != "child" {
				continue
			}
			hp := Resolve(p, l.Href)
			if hp == "" {
				continue
			}
			if err := walk(hp, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(Resolve(".", path), 0); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return &Catalog{Items: items}, nil
}

func Fetch(path string) ([]byte, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Get(path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(path)
}

func Resolve(base, href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if strings.HasPrefix(href, "file://") {
		u, err := url.Parse(href)
		if err == nil {
			return u.Path
		}
		return strings.TrimPrefix(href, "file://")
	}
	if strings.Contains(href, "://") {
		return ""
	}
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		b, err := url.Parse(base)
		if err != nil {
			return ""
		}
		r, err := url.Parse(href)
		if err != nil {
			return ""
		}
		return b.ResolveReference(r).String()
	}
	if filepath.IsAbs(href) {
		return href
	}
	return filepath.Join(filepath.Dir(base), href)
}

func (it Item) Bound() orb.Bound {
	if len(it.BBox) >= 4 {
		return orb.Bound{Min: orb.Point{it.BBox[0], it.BBox[1]}, Max: orb.Point{it.BBox[2], it.BBox[3]}}
	}
	return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
}

func (it Item) ParquetAsset() (string, bool) {
	var best string
	for _, a := range it.Assets {
		if contains(a.Roles, "overview") || strings.Contains(a.Href, "*") {
			continue
		}
		if strings.Contains(a.Type, "parquet") || strings.HasSuffix(a.Href, ".parquet") {
			href := Resolve(it.Base, a.Href)
			if href == "" {
				continue
			}
			if best == "" || len(href) < len(best) {
				best = href
			}
		}
	}
	return best, best != ""
}

func (c *Catalog) Bounds() orb.Bound {
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	for _, it := range c.Items {
		ib := it.Bound()
		if ib.Min.X() < minX {
			minX = ib.Min.X()
		}
		if ib.Min.Y() < minY {
			minY = ib.Min.Y()
		}
		if ib.Max.X() > maxX {
			maxX = ib.Max.X()
		}
		if ib.Max.Y() > maxY {
			maxY = ib.Max.Y()
		}
	}
	if minX > maxX {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (c *Catalog) Filter(collection string) []Item {
	if collection == "" {
		return c.Items
	}
	out := make([]Item, 0, len(c.Items))
	for _, it := range c.Items {
		if it.Collection == "" || it.Collection == collection {
			out = append(out, it)
		}
	}
	return out
}
